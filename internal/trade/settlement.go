package trade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
)

var (
	// ErrNotBuyer marks a settlement action taken by the wrong party.
	ErrNotBuyer = errors.New("only the buyer may request settlement")
	// ErrNotOnchain marks a settlement action on an off-chain trade.
	ErrNotOnchain = errors.New("trade is not an on-chain trade")
	// ErrSettlementLive blocks cancellation while an unexpired settlement
	// transaction exists, per design spec §5.
	ErrSettlementLive = errors.New("trade has a live settlement transaction")
	// ErrSettlementPending means the signature is not visible on chain yet. It
	// is not a mismatch: the caller retries, and a worker re-polls.
	ErrSettlementPending = errors.New("settlement transaction is not confirmed on chain yet")
	// ErrSettlementFailed means the transaction landed but failed on chain, so
	// its blockhash is spent and the buyer must request a fresh transaction.
	ErrSettlementFailed = errors.New("settlement transaction failed on chain")
	// ErrInvalidSignature marks a submitted signature the service cannot even
	// parse. It is a bad request, never a dispute.
	ErrInvalidSignature = errors.New("settlement signature is not valid")
	// ErrClusterMismatch marks a signed request whose terms were compiled for a
	// different cluster than the one running. It is neither a settlement nor a
	// dispute: the trade is fine and the operator's configuration is wrong.
	ErrClusterMismatch = errors.New("settlement request was issued for a different cluster")
)

// ConfirmPolicy bounds how long a confirm waits for RPC finality lag before
// reporting the signature as still pending. The design spec calls for retries
// with exponential backoff; the client always remains authoritative.
type ConfirmPolicy struct {
	Attempts int
	Backoff  time.Duration
	MaxDelay time.Duration
}

// DefaultConfirmPolicy retries briefly; a background worker reconciles anything
// still outstanding afterwards, so a long block here buys little.
func DefaultConfirmPolicy() ConfirmPolicy {
	return ConfirmPolicy{Attempts: 3, Backoff: 500 * time.Millisecond, MaxDelay: 2 * time.Second}
}

func (p ConfirmPolicy) attempts() int {
	if p.Attempts < 1 {
		return 1
	}
	return p.Attempts
}

func (p ConfirmPolicy) delayFor(attempt int) time.Duration {
	delay := p.Backoff
	for i := 1; i < attempt && delay > 0; i++ {
		delay *= 2
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	return delay
}

// SettlementStore persists issued settlement requests and the signature that
// later proves settlement.
type SettlementStore interface {
	CreateSettlementRequest(ctx context.Context, r domain.SettlementRequest) (domain.SettlementRequest, error)
	GetSettlementRequest(ctx context.Context, tradeID string) (domain.SettlementRequest, error)
	LiveSettlementRequest(ctx context.Context, tradeID string, now time.Time) (domain.SettlementRequest, error)
	ListSettlementRequests(ctx context.Context, tradeID string) ([]domain.SettlementRequest, error)
	SetSettlementSignature(ctx context.Context, id, signature string, at time.Time) error
	ConfirmSettlementRequest(ctx context.Context, id, signature string, at time.Time) error
	ExpireSettlementRequests(ctx context.Context, tradeID string, at time.Time) error
}

// SettlementBuilder assembles the unsigned settlement transaction.
type SettlementBuilder interface {
	Build(ctx context.Context, t settlement.Terms) (settlement.Build, error)
}

// SettlementVerifier proves a submitted transaction settles the terms.
type SettlementVerifier interface {
	Verify(t settlement.Terms, tx *solana.Transaction) error
}

// ChainReader fetches a submitted transaction.
type ChainReader interface {
	Transaction(ctx context.Context, sig solana.Signature) (settlement.Fetched, error)
}

// ClusterReader reports the chain identity behind the RPC endpoint, so the
// reconciler can re-check it on every tick rather than trusting the answer the
// process got at boot.
type ClusterReader interface {
	GetGenesisHash(ctx context.Context) (string, error)
}

// MintVerifier re-checks a governed mint against the chain immediately before
// a request is built from it. It is narrower than the startup preflight by
// design: at request time only the properties settlement correctness depends on
// are load-bearing, and a governance rotation must not become a settlement
// outage.
type MintVerifier interface {
	Verify(ctx context.Context, mint tokens.Token) error
}

// SettlementDeps are the on-chain capabilities of the service. They are absent
// unless the deployment is configured with an RPC endpoint and a fee policy,
// which is what keeps `settlementMode: onchain` a 501 when it cannot be honoured.
type SettlementDeps struct {
	Registry tokens.Registry
	Policy   fees.Policy
	Builder  SettlementBuilder
	Verifier SettlementVerifier
	Chain    ChainReader
	Store    SettlementStore
	// Cluster is the identity every request is stamped with and every request is
	// checked against. It is required when settlement is enabled, not optional:
	// a request that does not name the cluster it was compiled for cannot be
	// proved safe to execute.
	Cluster cluster.Cluster
	// Genesis re-reads the endpoint's chain identity. Required for the same
	// reason as Cluster, and it is what makes a mid-flight repoint detectable
	// instead of silent.
	Genesis ClusterReader
	// Mints re-verifies the mint on the build path. When nil the boot-time
	// preflight is the only mint check, which is weaker but not incorrect for
	// a deployment that has not wired it yet.
	Mints MintVerifier
	// TradeList is optional: without it the reconciliation worker cannot run,
	// and settlement itself still works for callers that use /confirm.
	TradeList TradeLister
	Confirm   ConfirmPolicy
	Sleep     func(ctx context.Context, d time.Duration) error
}

// Enabled reports whether on-chain settlement can be honoured.
func (d SettlementDeps) Enabled() bool {
	return d.Registry != nil && d.Builder != nil && d.Verifier != nil && d.Chain != nil && d.Store != nil && d.Cluster != ""
}

// Reconciles reports whether outstanding settlements can be reconciled in the
// background. Reconciliation is additive: a deployment without it still settles
// whenever the buyer calls /confirm.
func (d SettlementDeps) Reconciles() bool {
	return d.Enabled() && d.TradeList != nil && d.Genesis != nil
}

// CheckCluster re-reads the endpoint's genesis hash and compares it with the
// declared cluster. It is the check that keeps a fixed URL honest: a proxy or a
// mistyped endpoint can repoint at another cluster without the process
// restarting, and the only way to notice is to keep asking.
func (d SettlementDeps) CheckCluster(ctx context.Context) error {
	if !d.Enabled() {
		return ErrSettlementUnconfigured
	}
	if d.Genesis == nil {
		// Without a reader there is nothing to compare, so the declared cluster
		// is all there is. Say so rather than implying a check happened.
		return nil
	}
	hash, err := d.Genesis.GetGenesisHash(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOnchainUnavailable, err)
	}
	if err := d.Cluster.Verify(hash); err != nil {
		return fmt.Errorf("%w: %v", ErrOnchainUnavailable, err)
	}
	return nil
}

// WithSettlement attaches on-chain settlement capabilities. Until it is called,
// on-chain trades are refused rather than half-handled.
func (s *Service) WithSettlement(deps SettlementDeps) *Service {
	s.settlement = deps
	return s
}

// SettlementFee reports the fee policy this service enforces on on-chain
// settlements, so the API can publish exactly what will be charged.
func (s *Service) SettlementFee() (fees.Policy, bool) {
	if !s.settlement.Enabled() {
		return fees.Policy{}, false
	}
	return s.settlement.Policy, true
}

// verifyMint re-checks the governed mint against the chain, mapping the three
// outcomes onto three different operational meanings.
//
// A hard invariant failing is a 409: the token the offer was priced in is not
// the token that exists, and neither party can fix it by retrying. A governance
// pin drifting still settles, because the token is the same token and a
// legitimate issuer rotation must not halt the marketplace. The chain being
// unreachable is a 503, because nothing is known and retrying is exactly right.
func (s *Service) verifyMint(ctx context.Context, address string) error {
	if s.settlement.Mints == nil {
		return nil
	}
	mint, err := s.settlement.Registry.Enabled(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMintUngoverned, err)
	}
	err = s.settlement.Mints.Verify(ctx, mint)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, settlement.ErrMintUnverified):
		return fmt.Errorf("%w: %v", ErrMintUngoverned, err)
	case errors.Is(err, settlement.ErrMintUnreachable):
		return fmt.Errorf("%w: %v", ErrOnchainUnavailable, err)
	default:
		return err
	}
}

// SettlementRequestFor returns the current settlement request for a trade.
func (s *Service) SettlementRequestFor(ctx context.Context, actorID, tradeID string) (domain.SettlementRequest, error) {
	if _, err := s.partyTrade(ctx, actorID, tradeID); err != nil {
		return domain.SettlementRequest{}, err
	}
	if err := s.requireSettlement(ctx); err != nil {
		return domain.SettlementRequest{}, err
	}
	return s.settlement.Store.GetSettlementRequest(ctx, tradeID)
}

// BuildSettlement implements the design spec §6.2 build flow. Only the buyer may
// ask, and the service still never signs: it returns an unsigned transaction
// plus the blockhash expiry that bounds it.
func (s *Service) BuildSettlement(ctx context.Context, actorID, tradeID string) (domain.SettlementRequest, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.SettlementRequest{}, err
	}
	if err := s.requireSettlement(ctx); err != nil {
		return domain.SettlementRequest{}, err
	}
	if tr.BuyerAgentID != actorID {
		return domain.SettlementRequest{}, fmt.Errorf("%w: %s is the buyer", ErrNotBuyer, tr.BuyerAgentID)
	}
	if tr.SettlementMode != domain.SettlementOnchain {
		return domain.SettlementRequest{}, fmt.Errorf("%w: %s is %s", ErrNotOnchain, tr.ID, tr.SettlementMode)
	}
	switch tr.State {
	case domain.TradeAccepted, domain.TradeSettlementPending:
	default:
		return domain.SettlementRequest{}, fmt.Errorf("%w: cannot build settlement in state %s", ErrIllegalState, tr.State)
	}
	terms, err := settlement.NewTerms(tr, s.settlement.Registry, s.settlement.Policy)
	if err != nil {
		return domain.SettlementRequest{}, err
	}
	// The registry answers from boot-time configuration; the chain is what the
	// buyer will actually be paid in. Re-checking here is what stops a request
	// being compiled against a mint that stopped being that mint after startup.
	if err := s.verifyMint(ctx, tr.Mint); err != nil {
		return domain.SettlementRequest{}, err
	}
	built, err := s.settlement.Builder.Build(ctx, terms)
	if err != nil {
		return domain.SettlementRequest{}, err
	}
	now := s.now().UTC()
	// A new request supersedes any previous one in the same transaction, so the
	// trade never holds two live requests.
	request, err := s.settlement.Store.CreateSettlementRequest(ctx, domain.SettlementRequest{
		ID:          uuid.NewString(),
		TradeID:     tr.ID,
		Cluster:     s.settlement.Cluster,
		UnsignedTx:  built.Request.UnsignedTx,
		Blockhash:   built.Request.Blockhash,
		LastValid:   built.Request.LastValid,
		BuyerATA:    built.Request.BuyerATA,
		SellerATA:   built.Request.SellerATA,
		CreatedATA:  built.Request.CreatedATA,
		FeeLamports: s.settlement.Policy.Lamports(),
		FeeWallet:   s.settlement.Policy.WalletAddress(),
		Status:      domain.SettlementIssued,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   built.Request.ExpiresAt,
	})
	if err != nil {
		return domain.SettlementRequest{}, err
	}
	if tr.State == domain.TradeAccepted {
		if _, err := s.apply(ctx, tr, actorID, domain.TradeSettlementPending, settlementDetail(built)); err != nil {
			return domain.SettlementRequest{}, err
		}
	}
	return request, nil
}

// ConfirmSettlement implements the design spec §6.3 confirm flow. Verification is
// exact and canonical: anything other than the agreed transaction is a dispute,
// never a settlement. Only the chain can prove settlement, so a signature that
// is not yet visible stays pending and is retried rather than disputed.
func (s *Service) ConfirmSettlement(ctx context.Context, actorID, tradeID, signature string) (domain.Trade, domain.Receipt, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if err := s.requireSettlement(ctx); err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if tr.SettlementMode != domain.SettlementOnchain {
		return domain.Trade{}, domain.Receipt{}, fmt.Errorf("%w: %s is %s", ErrNotOnchain, tr.ID, tr.SettlementMode)
	}
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, fmt.Errorf("%w: %q is not base58: %v", ErrInvalidSignature, signature, err)
	}
	switch tr.State {
	case domain.TradeSettled, domain.TradeRecorded:
		receipt, err := s.ledger.Receipt(ctx, tradeID)
		return tr, receipt, err
	case domain.TradeSettlementPending:
	case domain.TradeDisputed:
		return domain.Trade{}, domain.Receipt{}, fmt.Errorf("%w: trade %s is disputed and needs operator review", ErrIllegalState, tradeID)
	default:
		return domain.Trade{}, domain.Receipt{}, fmt.Errorf("%w: cannot confirm settlement in state %s", ErrIllegalState, tr.State)
	}
	// Record the signature first so a crash between here and the verdict still
	// leaves evidence for the reconciliation worker to re-poll.
	requests, err := s.settlement.Store.ListSettlementRequests(ctx, tradeID)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	currentID := ""
	if request, err := s.settlement.Store.GetSettlementRequest(ctx, tradeID); err == nil {
		currentID = request.ID
		// Refuse a request built for another cluster before recording anything or
		// touching the chain. Recording the signature first would be evidence for
		// a transaction this process can never settle, and the reconciliation
		// worker would then re-poll it forever; polling it here would report a
		// signature this cluster has never seen as merely not-yet-visible.
		if err := s.checkRequestCluster(requests, currentID); err != nil {
			return domain.Trade{}, domain.Receipt{}, err
		}
		// Record the signature first so a crash between here and the verdict still
		// leaves evidence for the reconciliation worker to re-poll.
		if err := s.settlement.Store.SetSettlementSignature(ctx, request.ID, sig.String(), s.now().UTC()); err != nil {
			return domain.Trade{}, domain.Receipt{}, err
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Trade{}, domain.Receipt{}, err
	}
	fetched, err := s.awaitTransaction(ctx, sig)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	return s.settleFetched(ctx, tr, requests, currentID, sig, fetched)
}

// awaitTransaction polls for the transaction with exponential backoff, so RPC
// finality lag does not turn into a spurious dispute.
func (s *Service) awaitTransaction(ctx context.Context, sig solana.Signature) (settlement.Fetched, error) {
	policy := s.settlement.Confirm
	for attempt := 1; ; attempt++ {
		fetched, err := s.settlement.Chain.Transaction(ctx, sig)
		if err == nil {
			return fetched, nil
		}
		if !errors.Is(err, settlement.ErrTransactionNotFound) {
			return settlement.Fetched{}, err
		}
		if attempt >= policy.attempts() {
			return settlement.Fetched{}, fmt.Errorf("%w: %s", ErrSettlementPending, sig)
		}
		if err := s.sleep(ctx, policy.delayFor(attempt)); err != nil {
			return settlement.Fetched{}, err
		}
	}
}

func (s *Service) sleep(ctx context.Context, d time.Duration) error {
	if s.settlement.Sleep != nil {
		return s.settlement.Sleep(ctx, d)
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// requireSettlement reports why an on-chain trade cannot be handled, or nil when
// it can. The two refusals are distinct on purpose: no cluster is a 501, while
// a cluster that cannot be reached is a 503 with retry semantics.
func (s *Service) requireSettlement(ctx context.Context) error {
	if !s.settlement.Enabled() {
		return ErrSettlementUnconfigured
	}
	return s.settlement.CheckCluster(ctx)
}

// liveSettlement reports the trade's unexpired request, if any.
func (s *Service) liveSettlement(ctx context.Context, tradeID string) (domain.SettlementRequest, bool, error) {
	if !s.settlement.Enabled() {
		return domain.SettlementRequest{}, false, nil
	}
	request, err := s.settlement.Store.LiveSettlementRequest(ctx, tradeID, s.now().UTC())
	if errors.Is(err, domain.ErrNotFound) {
		return domain.SettlementRequest{}, false, nil
	}
	if err != nil {
		return domain.SettlementRequest{}, false, err
	}
	return request, true, nil
}

// checkSettleable refuses to open an on-chain trade that could not be settled:
// the mint must be on the cluster's allowlist, and both party agent IDs must be
// Solana addresses, because the buyer and seller wallets are the agent IDs.
func (s *Service) checkSettleable(buyer, seller, mint string) error {
	// The mint is the seller's choice, so a mint this cluster does not govern is
	// a conflict the buyer cannot resolve by editing their request. It is
	// reported separately from the other two checks because it is the one an
	// operator fixes by configuration rather than by the caller.
	if _, err := s.settlement.Registry.Enabled(mint); err != nil {
		return fmt.Errorf("%w: %v", ErrMintUngoverned, err)
	}
	for _, party := range []string{buyer, seller} {
		if _, err := solana.PublicKeyFromBase58(party); err != nil {
			return fmt.Errorf("%w: agent %q cannot settle on chain: %v", ErrModeNotAccepted, party, err)
		}
	}
	return nil
}

func settlementDetail(b settlement.Build) json.RawMessage {
	detail, err := json.Marshal(map[string]any{
		"blockhash":    b.Request.Blockhash,
		"unsignedTx":   b.Request.UnsignedTx,
		"createdAta":   b.Request.CreatedATA,
		"blockhashTtl": settlement.DefaultBlockhashTTL.String(),
		"lastValid":    b.Request.LastValid,
	})
	if err != nil {
		return nil
	}
	return detail
}

func confirmedDetail(signature string) json.RawMessage {
	detail, err := json.Marshal(map[string]string{"solanaSignature": signature})
	if err != nil {
		return nil
	}
	return detail
}

func disputeDetail(signature string, cause error) json.RawMessage {
	detail, err := json.Marshal(map[string]string{
		"solanaSignature": signature,
		"reason":          cause.Error(),
	})
	if err != nil {
		return nil
	}
	return detail
}
