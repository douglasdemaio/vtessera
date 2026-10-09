package trade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/gagliardetto/solana-go"
)

// ReconcilePolicy bounds the background worker. The buyer calling /confirm is
// always the fast path; this exists so a signature that was invisible then, or
// a service that restarted mid-confirmation, still reaches a verdict without a
// human.
type ReconcilePolicy struct {
	Interval time.Duration
	// Batch bounds one pass so a large backlog cannot starve the request path.
	Batch int
}

// DefaultReconcilePolicy polls often enough to settle a confirmation promptly
// and rarely enough to leave the RPC endpoint to actual buyers.
func DefaultReconcilePolicy() ReconcilePolicy {
	return ReconcilePolicy{Interval: 15 * time.Second, Batch: 50}
}

func (p ReconcilePolicy) interval() time.Duration {
	if p.Interval <= 0 {
		return DefaultReconcilePolicy().Interval
	}
	return p.Interval
}

func (p ReconcilePolicy) batch() int {
	if p.Batch < 1 {
		return 1
	}
	return p.Batch
}

// TradeLister is the slice of the store the worker needs to find trades whose
// settlement outcome is still outstanding.
type TradeLister interface {
	TradesInState(ctx context.Context, state domain.TradeState) ([]domain.Trade, error)
}

// ReconcileStats reports one pass, so a caller can log outcomes without the
// worker needing a logger of its own.
type ReconcileStats struct {
	Examined int
	Settled  int
	Disputed int
	Expired  int
	Pending  int
	// ExpiredClusterMismatch counts requests withdrawn because they were
	// compiled for a cluster this process is not running. It is reported
	// separately from Expired because it is not an ordinary timeout: it means
	// the operator repointed the service, and the count is how they see that
	// the backlog they inherited belongs to another chain.
	ExpiredClusterMismatch int
}

// ReconcileOutstanding drives one pass over trades awaiting an on-chain verdict.
// A pass never fails as a whole: one unreachable trade must not stop the rest,
// and an RPC outage must not be recorded as a dispute.
//
// It returns an error only when the trade list itself cannot be read.
func (s *Service) ReconcileOutstanding(ctx context.Context, policy ReconcilePolicy) (ReconcileStats, error) {
	var stats ReconcileStats
	if !s.settlement.Reconciles() {
		return stats, nil
	}
	trades, err := s.settlement.TradeList.TradesInState(ctx, domain.TradeSettlementPending)
	if err != nil {
		return stats, err
	}
	// Re-read the endpoint's identity once per pass, before touching any trade.
	// A worker that keeps polling a URL that now points somewhere else is worse
	// than idle: it would fetch a "transaction not found" for every pending
	// signature and report the whole backlog as still pending, which reads like
	// chain congestion rather than a configuration fault.
	if err := s.settlement.CheckCluster(ctx); err != nil {
		return stats, err
	}
	limit := policy.batch()
	if len(trades) > limit {
		trades = trades[:limit]
	}
	for _, tr := range trades {
		stats.Examined++
		outcome, err := s.reconcileTrade(ctx, tr)
		if err != nil {
			// Leave the trade pending and try again next pass. An operator sees a
			// pending trade, which is the honest state for an unknown outcome.
			continue
		}
		switch outcome {
		case reconcileSettled:
			stats.Settled++
		case reconcileDisputed:
			stats.Disputed++
		case reconcileExpired:
			stats.Expired++
		case reconcileExpiredClusterMismatch:
			stats.Expired++
			stats.ExpiredClusterMismatch++
		default:
			stats.Pending++
		}
	}
	return stats, nil
}

// RunReconciler reconciles outstanding settlements until the context is done. It
// is a no-op when on-chain settlement is not configured, so a deployment that
// never enables settlement does not need to special-case it.
func (s *Service) RunReconciler(ctx context.Context, policy ReconcilePolicy) {
	if !s.settlement.Reconciles() {
		return
	}
	ticker := time.NewTicker(policy.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.ReconcileOutstanding(ctx, policy)
		}
	}
}

type reconcileOutcome int

const (
	reconcilePending reconcileOutcome = iota
	reconcileSettled
	reconcileDisputed
	reconcileExpired
	// reconcileExpiredClusterMismatch is counted apart from reconcileExpired so
	// the reason a request was withdrawn survives into the pass report.
	reconcileExpiredClusterMismatch
)

// reconcileTrade settles one trade if its recorded signature has landed. It is
// deliberately conservative: a signature that is not visible yet is not a
// dispute, and a request with no signature is the buyer's to submit. It is not
// quite a no-op, though: a settlement request has a finite life, and once that
// life is over the trade cannot settle by that transaction. At that point the
// trade is cancelled so its buyer's reservation comes back instead of hanging
// forever.
func (s *Service) reconcileTrade(ctx context.Context, tr domain.Trade) (reconcileOutcome, error) {
	if tr.SettlementMode != domain.SettlementOnchain {
		return reconcilePending, nil
	}
	requests, err := s.settlement.Store.ListSettlementRequests(ctx, tr.ID)
	if err != nil {
		return reconcilePending, err
	}
	now := s.now().UTC()
	signed, ok := latestSignedRequest(requests)
	if !ok {
		// Nothing was ever submitted. If the last request's transaction is past
		// its blockhash lifetime it can no longer be submitted either, so the
		// trade is cancelled rather than left to reserve the buyer's budget for
		// good. This cannot strand a buyer who paid: a payment the service can
		// see is a recorded signature, and that case is handled below.
		if last, ok := latestRequest(requests); ok && last.Status == domain.SettlementIssued && !now.Before(last.ExpiresAt) {
			return s.reconcileLapsed(ctx, tr, now)
		}
		return reconcilePending, nil
	}
	// Withdraw a request built for another cluster instead of polling for it.
	// The pass already re-read the endpoint's identity, so this is about the
	// trade's own history rather than the URL. Expiring rather than skipping is
	// what unblocks the buyer: an issued request holds the trade's partial
	// unique index, so leaving it in place means a fresh request on the correct
	// cluster is rejected forever. The trade itself stays pending and a human
	// decides, but the buyer is not left with a permanently stuck trade.
	if err := s.checkRequestCluster(requests, signed.ID); err != nil {
		if !errors.Is(err, ErrClusterMismatch) {
			return reconcilePending, err
		}
		if err := s.settlement.Store.ExpireSettlementRequests(ctx, tr.ID, now); err != nil {
			return reconcilePending, err
		}
		return reconcileExpiredClusterMismatch, nil
	}
	sig, err := solana.SignatureFromBase58(signed.Signature)
	if err != nil {
		return reconcilePending, err
	}
	// One attempt only: the ticker is the retry, so a signature that is still
	// propagating costs a single RPC call per interval instead of a backoff
	// storm across every pending trade.
	fetched, err := s.settlement.Chain.Transaction(ctx, sig)
	if errors.Is(err, settlement.ErrTransactionNotFound) {
		// The chain does not know this transaction. If the request is still
		// issued and the blockhash it was built against has lapsed, the
		// transaction can no longer be submitted and waiting longer only holds
		// the buyer's budget. A request the chain already resolved as expired is
		// left alone: that is the failed-execution case, where the buyer may
		// rebuild against the same trade rather than losing it.
		if signed.Status == domain.SettlementIssued && !now.Before(signed.ExpiresAt) {
			return s.reconcileLapsed(ctx, tr, now)
		}
		return reconcilePending, nil
	}
	if err != nil {
		return reconcilePending, err
	}
	if _, _, err := s.settleFetched(ctx, tr, requests, signed.ID, sig, fetched); err != nil {
		switch {
		case errors.Is(err, settlement.ErrMismatch), errors.Is(err, ErrSettlementFailed):
			// The trade already carries its terminal state; the outcome is known.
			if errors.Is(err, settlement.ErrMismatch) {
				return reconcileDisputed, nil
			}
			return reconcileExpired, nil
		case errors.Is(err, ErrIllegalState):
			// Another pass or the buyer got there first.
			return reconcilePending, nil
		}
		return reconcilePending, err
	}
	return reconcileSettled, nil
}

// reconcileLapsed cancels a settlement_pending trade whose settlement
// transaction can no longer land and expires its outstanding requests so a
// partial unique index does not outlive the attempt. It is reported as an
// expiry rather than a dispute: nobody is at fault when a blockhash runs out
// before a transaction is submitted. A trade another path has already moved
// past settlement_pending is left alone.
func (s *Service) reconcileLapsed(ctx context.Context, tr domain.Trade, now time.Time) (reconcileOutcome, error) {
	if err := s.settlement.Store.ExpireSettlementRequests(ctx, tr.ID, now); err != nil {
		return reconcilePending, err
	}
	detail := reasonDetail("the settlement transaction's blockhash lapsed before it landed")
	if _, err := s.applyAs(ctx, tr, "", domain.TradeCancelled, detail, domain.EventExpired); err != nil {
		if errors.Is(err, ErrIllegalState) {
			return reconcilePending, nil
		}
		return reconcilePending, err
	}
	return reconcileExpired, nil
}

// latestSignedRequest returns the most recent request that carries a signature.
// Older requests are evidence, not the current attempt.
func latestSignedRequest(requests []domain.SettlementRequest) (domain.SettlementRequest, bool) {
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Signature != "" {
			return requests[i], true
		}
	}
	return domain.SettlementRequest{}, false
}

// latestRequest returns the most recent request of any kind. It is what an
// unsigned request's expiry is read from: the signature is absent, but the
// deadline the request was built with is not.
func latestRequest(requests []domain.SettlementRequest) (domain.SettlementRequest, bool) {
	if len(requests) == 0 {
		return domain.SettlementRequest{}, false
	}
	return requests[len(requests)-1], true
}

// settleFetched applies the verdict for a transaction that has landed and
// completes the books. The buyer's /confirm call and the reconciliation worker
// both come through here, so a trade can never settle by one path and dispute by
// the other. currentID names the request that carried the signature.
func (s *Service) settleFetched(ctx context.Context, tr domain.Trade, requests []domain.SettlementRequest, currentID string, sig solana.Signature, fetched settlement.Fetched) (domain.Trade, domain.Receipt, error) {
	actor := tr.BuyerAgentID
	if err := s.checkRequestCluster(requests, currentID); err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if fetched.ExecErr != nil {
		if err := s.settlement.Store.ExpireSettlementRequests(ctx, tr.ID, s.now().UTC()); err != nil {
			return domain.Trade{}, domain.Receipt{}, err
		}
		return domain.Trade{}, domain.Receipt{}, fmt.Errorf("%w: %v", ErrSettlementFailed, fetched.ExecErr)
	}
	terms, err := settlement.NewTerms(tr, s.settlement.Registry, s.settlement.Policy)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if err := s.settlement.Verifier.Verify(terms, fetched.Transaction); err != nil {
		if !errors.Is(err, settlement.ErrMismatch) {
			return domain.Trade{}, domain.Receipt{}, err
		}
		// Any mismatch is a dispute: no tessera, and never silently settled.
		if _, err := s.apply(ctx, tr, actor, domain.TradeDisputed, disputeDetail(sig.String(), err)); err != nil {
			return domain.Trade{}, domain.Receipt{}, err
		}
		return domain.Trade{}, domain.Receipt{}, err
	}
	if err := s.confirmRequests(ctx, requests, currentID, sig.String(), s.now().UTC()); err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	settled, err := s.apply(ctx, tr, actor, domain.TradeSettled, confirmedDetail(sig.String()))
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	receipt, err := s.ledger.Record(ctx, settled, sig.String(), string(s.settlement.Cluster))
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if err := s.offers.SetOfferStatus(ctx, settled.OfferID, domain.OfferClosed, s.now().UTC()); err != nil {
		return settled, receipt, err
	}
	return settled, receipt, nil
}

// checkRequestCluster refuses to honour a request that was not compiled for the
// cluster this process is running.
//
// The trade's history can predate the current configuration: a request issued
// while the operator pointed at devnet is still in the table after they repoint
// at mainnet, and its unsigned transaction names devrent-minted ATAs that do not
// exist on mainnet-beta. Executing or settling it here would either fail on chain
// or, worse, succeed against addresses nobody priced. A `pre-phase-3` request
// predates the column and is refused for the same reason, more bluntly: nothing
// recorded which cluster it was built for, so nothing can vouch for it.
func (s *Service) checkRequestCluster(requests []domain.SettlementRequest, currentID string) error {
	if s.settlement.Cluster == "" {
		return nil
	}
	// An empty currentID means the caller could not identify a current request,
	// which happens only on a trade with no request at all. The verifier then
	// finds nothing to match and disputes, which is already the safe answer.
	if currentID == "" {
		return nil
	}
	want := s.settlement.Cluster
	for _, request := range requests {
		if request.ID != currentID {
			continue
		}
		if request.Cluster == domain.PrePhase3Cluster {
			return fmt.Errorf("%w: request %s predates cluster awareness and cannot be attributed to %s",
				ErrClusterMismatch, request.ID, want)
		}
		if request.Cluster != want {
			return fmt.Errorf("%w: request %s was issued for %s, this process runs %s",
				ErrClusterMismatch, request.ID, request.Cluster, want)
		}
		return nil
	}
	return nil
}

// confirmRequests marks the request that carried this signature as confirmed and
// expires the ones it superseded, so a trade cannot show two live requests.
func (s *Service) confirmRequests(ctx context.Context, requests []domain.SettlementRequest, currentID, signature string, at time.Time) error {
	if currentID == "" {
		return nil
	}
	// Confirm before sweeping: the sweep expires every still-issued request for
	// the trade, including the one being confirmed if the order were reversed.
	found := false
	for _, request := range requests {
		if request.ID != currentID {
			continue
		}
		found = true
		if err := s.settlement.Store.ConfirmSettlementRequest(ctx, request.ID, signature, at); err != nil {
			return err
		}
		break
	}
	if !found {
		return s.settlement.Store.ConfirmSettlementRequest(ctx, currentID, signature, at)
	}
	if len(requests) > 1 {
		return s.settlement.Store.ExpireSettlementRequests(ctx, requests[0].TradeID, at)
	}
	return nil
}
