package trade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	// TradeList is optional: without it the reconciliation worker cannot run,
	// and settlement itself still works for callers that use /confirm.
	TradeList TradeLister
	Confirm   ConfirmPolicy
	Sleep     func(ctx context.Context, d time.Duration) error
}

// Enabled reports whether on-chain settlement can be honoured.
func (d SettlementDeps) Enabled() bool {
	return d.Registry != nil && d.Builder != nil && d.Verifier != nil && d.Chain != nil && d.Store != nil
}

// Reconciles reports whether outstanding settlements can be reconciled in the
// background. Reconciliation is additive: a deployment without it still settles
// whenever the buyer calls /confirm.
func (d SettlementDeps) Reconciles() bool {
	return d.Enabled() && d.TradeList != nil
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

// SettlementRequestFor returns the current settlement request for a trade.
func (s *Service) SettlementRequestFor(ctx context.Context, actorID, tradeID string) (domain.SettlementRequest, error) {
	if _, err := s.partyTrade(ctx, actorID, tradeID); err != nil {
		return domain.SettlementRequest{}, err
	}
	if err := s.requireSettlement(); err != nil {
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
	if err := s.requireSettlement(); err != nil {
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
	if err := s.requireSettlement(); err != nil {
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

func (s *Service) requireSettlement() error {
	if !s.settlement.Enabled() {
		return ErrOnchainUnavailable
	}
	return nil
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
// the mint must be on the service's allowlist, and both party agent IDs must be
// Solana addresses, because the buyer and seller wallets are the agent IDs.
func (s *Service) checkSettleable(buyer, seller, mint string) error {
	if _, err := s.settlement.Registry.Enabled(mint); err != nil {
		return fmt.Errorf("%w: %v", ErrModeNotAccepted, err)
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
