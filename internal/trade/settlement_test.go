package trade_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/gagliardetto/solana-go"
)

const signature = "3n1kM1LPRPRTzXcQKCSGyHZUnQVXPPiQjRHDXpNzWJmEfjJcEbLKLzsjpmpfd1GJPPfM6P2KSPfrRtGYEPrTQ3mB"

type fakeBuilder struct {
	ttl    time.Duration
	err    error
	builds int
	terms  []settlement.Terms
}

func (b *fakeBuilder) Build(_ context.Context, t settlement.Terms) (settlement.Build, error) {
	b.builds++
	b.terms = append(b.terms, t)
	if b.err != nil {
		return settlement.Build{}, b.err
	}
	ttl := b.ttl
	if ttl == 0 {
		ttl = 90 * time.Second
	}
	now := time.Now().UTC()
	return settlement.Build{
		Request: settlement.Request{
			TradeID:    t.TradeID,
			Blockhash:  "9zjePBqC5DBShJ7wzSjPQFPQjk6jJKAJgYbn3yiv1sPk",
			LastValid:  987654,
			UnsignedTx: "AQID",
			BuyerATA:   "buyer-ata",
			SellerATA:  "seller-ata",
			CreatedATA: true,
			CreatedAt:  now,
			ExpiresAt:  now.Add(ttl),
		},
		Terms: t,
	}, nil
}

type fakeVerifier struct {
	err   error
	calls int
	terms []settlement.Terms
}

func (v *fakeVerifier) Verify(t settlement.Terms, _ *solana.Transaction) error {
	v.calls++
	v.terms = append(v.terms, t)
	return v.err
}

// chainResult is one scripted answer from the fake chain: either a transaction
// or the RPC lag error the confirm flow is expected to retry.
type chainResult struct {
	fetched settlement.Fetched
	err     error
}

type fakeChain struct {
	results []chainResult
	calls   int
	// genesis is what the endpoint claims to be. Empty string reports a chain
	// identity that matches no cluster, which is how a repoint is simulated.
	genesis      string
	genesisErr   error
	genesisCalls int
}

// GetGenesisHash satisfies the reconciler's per-tick cluster re-check. An unset
// genesis reports the cluster the harness declares, so a test that is not about
// cluster identity does not have to restate it; a test that is sets it
// deliberately.
func (c *fakeChain) GetGenesisHash(context.Context) (string, error) {
	c.genesisCalls++
	if c.genesisErr != nil {
		return "", c.genesisErr
	}
	if c.genesis == "" {
		return cluster.MainnetBeta.GenesisHash(), nil
	}
	return c.genesis, nil
}

func (c *fakeChain) Transaction(_ context.Context, sig solana.Signature) (settlement.Fetched, error) {
	c.calls++
	if len(c.results) == 0 {
		return settlement.Fetched{}, settlement.ErrTransactionNotFound
	}
	next := c.results[0]
	if len(c.results) > 1 {
		c.results = c.results[1:]
	}
	return next.fetched, next.err
}

func (c *fakeChain) add(f settlement.Fetched, err error) *fakeChain {
	c.results = append(c.results, chainResult{fetched: f, err: err})
	return c
}

// landed is a transaction the chain accepted.
func landed() chainResult {
	return chainResult{fetched: settlement.Fetched{Transaction: &solana.Transaction{}}}
}

// notVisible is the signature not being on chain yet, which is expected.
func notVisible() chainResult {
	return chainResult{err: settlement.ErrTransactionNotFound}
}

func onchainHarness(t *testing.T, opts ...func(*trade.SettlementDeps)) (harness, *trade.SettlementDeps) {
	t.Helper()
	h := setup(t)
	// mainnet-beta, not localnet: localnet has no genesis pin, so Verify accepts
	// anything and the per-tick re-check would be a no-op that proved nothing.
	// A pinned cluster is what makes the identity assertions below real.
	registry, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatalf("test mints: %v", err)
	}
	policy := fees.Default()
	// Cluster and Genesis are populated with the declared value rather than left
	// empty, because Enabled() now requires them: a request that does not name
	// the cluster it was compiled for cannot be proved safe to execute.
	chain := &fakeChain{genesis: cluster.MainnetBeta.GenesisHash()}
	deps := trade.SettlementDeps{
		Registry: registry,
		Policy:   policy,
		Builder:  &fakeBuilder{},
		Verifier: &fakeVerifier{},
		Chain:    chain,
		Store:    h.db,
		Cluster:  cluster.MainnetBeta,
		Genesis:  chain,
		Confirm:  trade.ConfirmPolicy{Attempts: 3, Backoff: time.Millisecond},
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	for _, opt := range opts {
		opt(&deps)
	}
	return h, &deps
}

// withChain swaps in a fake endpoint. It sets Genesis as well as Chain because
// both come from the same node: a test that swapped only one would be asserting
// against a service with two different clusters, which cannot be configured.
func withChain(f *fakeChain) func(*trade.SettlementDeps) {
	return func(d *trade.SettlementDeps) {
		d.Chain = f
		d.Genesis = f
	}
}

func withVerifier(v *fakeVerifier) func(*trade.SettlementDeps) {
	return func(d *trade.SettlementDeps) { d.Verifier = v }
}

func withBuilder(b *fakeBuilder) func(*trade.SettlementDeps) {
	return func(d *trade.SettlementDeps) { d.Builder = b }
}

func acceptedOnchainTrade(t *testing.T, h harness) domain.Trade {
	t.Helper()
	ctx := context.Background()
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOnchain, "onchain-idem")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	accepted, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

// TestOnchainTradeRefusedWhenSettlementIsNotConfigured is the 501 case: no
// cluster and no endpoint, so the feature is not switched on here. It is a
// different refusal from a 503, where the feature is on and the chain is
// merely unreachable, and conflating them would tell an operator to fix their
// configuration during a network outage.
func TestOnchainTradeRefusedWhenSettlementIsNotConfigured(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	_, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOnchain, "idem-1")
	if !errors.Is(err, trade.ErrSettlementUnconfigured) {
		t.Errorf("err = %v, want ErrSettlementUnconfigured so on-chain is never half-handled", err)
	}
	if errors.Is(err, trade.ErrOnchainUnavailable) {
		t.Error("an unconfigured deployment must not report a 503-style outage")
	}
}

func TestBuildSettlementIssuesUnsignedTransactionToTheBuyer(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)

	request, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if request.UnsignedTx == "" || request.Blockhash == "" {
		t.Fatalf("request = %+v, want an unsigned transaction and blockhash", request)
	}
	if request.Signature != "" {
		t.Errorf("signature = %q, want none: the service never signs", request.Signature)
	}
	if request.Status != domain.SettlementIssued {
		t.Errorf("status = %s, want issued", request.Status)
	}
	if request.FeeLamports != fees.DefaultLamports {
		t.Errorf("fee = %d, want %d", request.FeeLamports, fees.DefaultLamports)
	}
	if request.FeeWallet != fees.DefaultWallet {
		t.Errorf("fee wallet = %s, want %s", request.FeeWallet, fees.DefaultWallet)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending", tr.State)
	}
	// The terms the buyer will sign are the frozen trade terms.
	terms := deps.Builder.(*fakeBuilder).terms[0]
	if terms.TradeID != accepted.ID || terms.Amount != 12_500_000 || terms.Decimals != 6 {
		t.Errorf("terms = %+v, want 12.50 USDC in base units for trade %s", terms, accepted.ID)
	}
	if !terms.Buyer.Equals(mustKey(t, buyerKey)) || !terms.Seller.Equals(mustKey(t, sellerKey)) {
		t.Errorf("parties = %s/%s, want the agent ids as wallets", terms.Buyer, terms.Seller)
	}
}

func TestBuildSettlementRefusesTheSeller(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, sellerKey, accepted.ID); !errors.Is(err, trade.ErrNotBuyer) {
		t.Errorf("err = %v, want ErrNotBuyer: the buyer is the fee payer and signer", err)
	}
}

func TestBuildSettlementRefusesOutsiders(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, outsider, accepted.ID); !errors.Is(err, trade.ErrNotParty) {
		t.Errorf("err = %v, want ErrNotParty", err)
	}
}

func TestBuildSettlementRefusesOffchainTrades(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOffchain, "offchain-1")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID); !errors.Is(err, trade.ErrNotOnchain) {
		t.Errorf("err = %v, want ErrNotOnchain", err)
	}
}

// TestOnchainTradeRefusesAnUngovernedMint is a 409, not a 400: the mint was the
// seller's choice and the buyer cannot correct it, so it is a conflict with
// server state rather than a malformed request.
func TestOnchainTradeRefusesAnUngovernedMint(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	// A valid Solana address that the service does not govern.
	offer, _, err := h.registry.PublishOffer(ctx, sellerKey, registryOffer(t, outsider), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.svc.Create(ctx, buyerKey, offer.ID, domain.SettlementOnchain, "unlisted-1")
	if !errors.Is(err, trade.ErrMintUngoverned) {
		t.Errorf("err = %v, want ErrMintUngoverned", err)
	}
}

func TestConfirmSettlementSettlesTheTradeAndIssuesATessera(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	request, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}

	settled, receipt, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != domain.TradeSettled {
		t.Errorf("state = %s, want settled", settled.State)
	}
	if receipt.TradeID != accepted.ID {
		t.Errorf("receipt trade = %s, want %s", receipt.TradeID, accepted.ID)
	}
	stored, err := h.svc.SettlementRequestFor(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != request.ID {
		t.Errorf("request = %s, want the confirmed %s", stored.ID, request.ID)
	}
	if stored.Status != domain.SettlementConfirmed || stored.Signature != signature {
		t.Errorf("request = %+v, want confirmed against signature %s", stored, signature)
	}
	// The tessera must carry the signature that proved settlement.
	if receipt.JWS == "" {
		t.Error("tessera is missing its JWS")
	}
	offer, err := h.db.GetOffer(ctx, accepted.OfferID)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want closed after settlement", offer.Status)
	}
}

func TestConfirmSettlementMismatchDisputesAndIssuesNoTessera(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	verifier := &fakeVerifier{err: settlement.ErrMismatch}
	h, deps := onchainHarness(t, withChain(chain), withVerifier(verifier))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature); !errors.Is(err, settlement.ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch", err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed: a mismatch is never a settlement", tr.State)
	}
	if _, err := h.svc.Tessera(ctx, buyerKey, accepted.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("tessera err = %v, want none issued for a disputed trade", err)
	}
	// A dispute is terminal: no retry may turn it into a settlement.
	if _, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature); !errors.Is(err, trade.ErrIllegalState) {
		t.Errorf("second confirm err = %v, want ErrIllegalState", err)
	}
}

func TestConfirmSettlementRetriesWhileTheSignatureIsNotVisible(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{}
	chain.add(settlement.Fetched{}, settlement.ErrTransactionNotFound)
	chain.add(landed().fetched, nil)
	sleeps := 0
	h, deps := onchainHarness(t, withChain(chain))
	deps.Sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature); err != nil {
		t.Fatal(err)
	}
	if chain.calls != 2 || sleeps != 1 {
		t.Errorf("chain calls = %d with %d backoffs, want 2 calls and 1 backoff", chain.calls, sleeps)
	}
}

func TestConfirmSettlementReportsStillPendingRatherThanDisputing(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{}
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature)
	if !errors.Is(err, trade.ErrSettlementPending) {
		t.Fatalf("err = %v, want ErrSettlementPending: an invisible signature is not a mismatch", err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending so the worker can re-poll", tr.State)
	}
	// The signature is recorded even though the trade has not settled, so a
	// reconciliation worker can re-poll it after a crash.
	stored, err := h.svc.SettlementRequestFor(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Signature != signature {
		t.Errorf("signature = %q, want the submitted %s recorded for reconciliation", stored.Signature, signature)
	}
}

func TestConfirmSettlementOnChainFailureKeepsTheTradeSettleable(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{}
	chain.add(settlement.Fetched{Transaction: &solana.Transaction{}, ExecErr: errors.New("insufficient funds")}, nil)
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature)
	if !errors.Is(err, trade.ErrSettlementFailed) {
		t.Fatalf("err = %v, want ErrSettlementFailed", err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending so a fresh transaction can be issued", tr.State)
	}
	// The spent blockhash is retired, so the buyer can ask again immediately.
	built := deps.Builder.(*fakeBuilder)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if built.builds != 2 {
		t.Errorf("builds = %d, want a fresh transaction after the failure", built.builds)
	}
}

func TestConfirmSettlementRejectsAMalformedSignature(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, "not-a-signature"); err == nil {
		t.Fatal("expected a malformed signature to be rejected")
	} else if !strings.Contains(err.Error(), "base58") {
		t.Errorf("err = %v, want a base58 complaint", err)
	}
}

func TestConfirmSettlementIsIdempotentOnceSettled(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	first, firstReceipt, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature)
	if err != nil {
		t.Fatal(err)
	}
	second, secondReceipt, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || firstReceipt.ID != secondReceipt.ID {
		t.Errorf("repeat confirm returned a different trade or receipt: %+v %+v", second, secondReceipt)
	}
	if chain.calls != 1 {
		t.Errorf("chain calls = %d, want the settled trade to be answered from the ledger", chain.calls)
	}
}

func TestSettlementIsBlockedWhileTheTransactionIsLive(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Cancel(ctx, buyerKey, accepted.ID, "changed my mind"); !errors.Is(err, trade.ErrSettlementLive) {
		t.Fatalf("err = %v, want ErrSettlementLive: a live transaction may be in flight", err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want the trade still settlement_pending", tr.State)
	}
}

func TestSettlementIsCancellableOnceTheBlockhashLapses(t *testing.T) {
	ctx := context.Background()
	// A blockhash that has already expired: the live transaction is spent.
	h, deps := onchainHarness(t, withBuilder(&fakeBuilder{ttl: -time.Second}))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.svc.Cancel(ctx, buyerKey, accepted.ID, "expired anyway")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != domain.TradeCancelled {
		t.Errorf("state = %s, want cancelled once the blockhash expired", cancelled.State)
	}
}

func TestRebookingAfterExpiryKeepsOneLiveRequest(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	first, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Error("a re-request must be a new request")
	}
	// The trade stays settlement_pending across a re-request.
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending", tr.State)
	}
	pending := 0
	for _, r := range mustListRequests(t, h, accepted.ID) {
		if r.Status == domain.SettlementIssued {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("issued requests = %d, want exactly 1 live", pending)
	}
}

func TestSettlementEventTrailRecordsTheSignature(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, accepted.ID, signature); err != nil {
		t.Fatal(err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawPending, sawSettled bool
	for _, e := range tr.Events {
		if e.Type == domain.EventSettlementPending {
			sawPending = true
			if e.ToState != domain.TradeSettlementPending {
				t.Errorf("pending event to state = %s", e.ToState)
			}
		}
		if e.Type == domain.EventSettled {
			sawSettled = true
			if !strings.Contains(string(e.Detail), signature) {
				t.Errorf("settled event detail = %s, want the signature", e.Detail)
			}
		}
	}
	if !sawPending || !sawSettled {
		t.Errorf("events = %+v, want settlement_pending and settled", tr.Events)
	}
}

func registryOffer(t *testing.T, mint string) registry.NewOffer {
	t.Helper()
	return registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     "1.00",
		PriceMint:       mint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOnchain},
	}
}

func mustKey(t *testing.T, address string) solana.PublicKey {
	t.Helper()
	key, err := solana.PublicKeyFromBase58(address)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustListRequests(t *testing.T, h harness, tradeID string) []domain.SettlementRequest {
	t.Helper()
	requests, err := h.db.ListSettlementRequests(context.Background(), tradeID)
	if err != nil {
		t.Fatal(err)
	}
	return requests
}

// --- cluster identity ---

// fakeMints is a mint verifier whose verdict the test chooses, so the three
// outcomes can each be provoked without a chain that misbehaves on demand.
type fakeMints struct {
	err   error
	calls int
}

func (f *fakeMints) Verify(context.Context, tokens.Token) error {
	f.calls++
	return f.err
}

func withMints(m *fakeMints) func(*trade.SettlementDeps) {
	return func(d *trade.SettlementDeps) { d.Mints = m }
}

func withCluster(c cluster.Cluster) func(*trade.SettlementDeps) {
	return func(d *trade.SettlementDeps) { d.Cluster = c }
}

func TestBuildSettlementRecordsTheClusterItWasCompiledFor(t *testing.T) {
	ctx := context.Background()
	h, deps := onchainHarness(t)
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)

	request, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Cluster != cluster.MainnetBeta {
		t.Errorf("cluster = %q, want %q", request.Cluster, cluster.MainnetBeta)
	}
}

// TestBuildSettlementVerifiesTheMintOnUse is the property that makes the boot
// check insufficient on its own: a mint that stopped being that mint after
// startup must not be used to compile a transaction.
func TestBuildSettlementVerifiesTheMintOnUse(t *testing.T) {
	ctx := context.Background()
	mints := &fakeMints{}
	h, deps := onchainHarness(t, withMints(mints))
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)

	if _, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if mints.calls == 0 {
		t.Error("the mint was not re-verified before the transaction was built")
	}
}

func TestBuildSettlementRefusesAMintThatNoLongerHolds(t *testing.T) {
	// A 409, not a 503: the token is not the one that was priced, and no amount
	// of retrying changes that.
	ctx := context.Background()
	h, deps := onchainHarness(t, withMints(&fakeMints{err: settlement.ErrMintUnverified}))
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)

	_, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID)
	if !errors.Is(err, trade.ErrMintUngoverned) {
		t.Errorf("err = %v, want ErrMintUngoverned", err)
	}
	if errors.Is(err, trade.ErrOnchainUnavailable) {
		t.Error("a wrong mint must not be reported as a chain outage")
	}
}

func TestBuildSettlementReportsAnUnreachableChainAsRetryable(t *testing.T) {
	// The opposite case: nothing is known, so the caller should come back rather
	// than conclude the token is wrong.
	ctx := context.Background()
	h, deps := onchainHarness(t, withMints(&fakeMints{err: settlement.ErrMintUnreachable}))
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)

	_, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID)
	if !errors.Is(err, trade.ErrOnchainUnavailable) {
		t.Errorf("err = %v, want ErrOnchainUnavailable", err)
	}
	if errors.Is(err, trade.ErrMintUngoverned) {
		t.Error("an unreachable chain must not be reported as an ungoverned mint")
	}
}

func TestBuildSettlementRefusesWhenTheEndpointIsNotTheDeclaredCluster(t *testing.T) {
	// A fixed URL can be repointed behind the process, so the identity is
	// re-read on the request path rather than trusted from boot.
	ctx := context.Background()
	chain := &fakeChain{}
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)

	// The endpoint now points at devnet while the service declares mainnet-beta.
	chain.genesis = cluster.Devnet.GenesisHash()

	_, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID)
	if !errors.Is(err, trade.ErrOnchainUnavailable) {
		t.Errorf("err = %v, want ErrOnchainUnavailable", err)
	}
	if chain.genesisCalls == 0 {
		t.Error("the cluster identity was not re-checked on the request path")
	}
}

func TestConfirmSettlementRefusesARequestIssuedForAnotherCluster(t *testing.T) {
	// A request compiled against mainnet-beta mints is still in the table after
	// the operator repoints at devnet. Its transaction names accounts that do
	// not exist on the new chain, so honouring it here would either fail on
	// chain or, worse, succeed against addresses nobody priced.
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, deps := onchainHarness(t, withChain(chain))
	h.svc.WithSettlement(*deps)
	tr := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}

	// Repoint the operator's configuration without touching the stored request.
	// The endpoint now honestly reports devnet, so the identity re-check passes
	// and the only thing standing between the request and execution is the
	// cluster stamped on the request itself.
	deps.Cluster = cluster.Devnet
	deps.Genesis = &fakeChain{genesis: cluster.Devnet.GenesisHash()}
	h.svc.WithSettlement(*deps)

	_, _, err := h.svc.ConfirmSettlement(ctx, buyerKey, tr.ID, signature)
	if !errors.Is(err, trade.ErrClusterMismatch) {
		t.Errorf("err = %v, want ErrClusterMismatch", err)
	}
	// The refusal has to come before the chain is consulted. Polling first would
	// report a signature this cluster has never seen as merely not-yet-visible,
	// which is an outage the operator would go looking for on the wrong host.
	if chain.calls != 0 {
		t.Errorf("chain reads = %d, want 0: a request from another cluster must not be polled", chain.calls)
	}
	// And the signature must not be recorded, or the reconciliation worker would
	// re-poll it every interval forever.
	stored, err := h.svc.SettlementRequestFor(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Signature != "" {
		t.Errorf("signature = %q, want it left unrecorded for a wrong-cluster request", stored.Signature)
	}
}

func TestReconcilerRechecksTheClusterBeforeTouchingAnyTrade(t *testing.T) {
	// A worker that keeps polling a repointed URL fetches "not found" for every
	// pending signature and reports the whole backlog as still pending, which
	// reads like chain congestion rather than a configuration fault.
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, _ := reconcilingHarness(t, withChain(chain))
	pendingWithSignature(t, h)

	// The endpoint now points at devnet while the service declares mainnet-beta.
	chain.genesis = cluster.Devnet.GenesisHash()
	before := chain.genesisCalls
	if _, err := h.svc.ReconcileOutstanding(ctx, trade.ReconcilePolicy{Batch: 10}); err == nil {
		t.Error("ReconcileOutstanding should report a repointed endpoint")
	}
	if chain.genesisCalls <= before {
		t.Error("the reconciler did not re-read the cluster identity")
	}
}

func TestReconcilerProceedsWhenTheClusterStillMatches(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, _ := reconcilingHarness(t, withChain(chain))
	pendingWithSignature(t, h)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.ReconcilePolicy{Batch: 10})
	if err != nil {
		t.Fatalf("ReconcileOutstanding: %v", err)
	}
	if stats.Settled != 1 {
		t.Errorf("stats = %+v, want the pending trade settled by the worker", stats)
	}
}

func TestReconcilerWithdrawsARequestIssuedForAnotherClusterAndUnblocksTheTrade(t *testing.T) {
	// The reconciliation worker runs unattended. Left to re-poll a mainnet-beta
	// signature on a devnet process, it would spend one RPC call per interval per
	// trade indefinitely, and every result would read as "not yet visible" — an
	// operator would conclude the chain was congested.
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, deps := reconcilingHarness(t, withChain(chain))
	tr := pendingWithSignature(t, h)

	// The stored request still says mainnet-beta; the process now says devnet.
	deps.Cluster = cluster.Devnet
	deps.Genesis = &fakeChain{genesis: cluster.Devnet.GenesisHash()}
	h.svc.WithSettlement(*deps)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.ReconcilePolicy{Batch: 10})
	if err != nil {
		t.Fatalf("ReconcileOutstanding: %v", err)
	}
	if chain.calls != 0 {
		t.Errorf("chain reads = %d, want 0: the worker must not poll another cluster's signature", chain.calls)
	}
	// Withdrawn, not settled and not disputed: the operator's configuration
	// changed, which says nothing about the trade.
	if stats.Settled != 0 || stats.Disputed != 0 {
		t.Errorf("stats = %+v, want neither settled nor disputed", stats)
	}
	if stats.ExpiredClusterMismatch != 1 {
		t.Errorf("stats = %+v, want one request expired for a cluster mismatch", stats)
	}

	// The trade stays pending — a human decides, not a code path — and the
	// withdrawn request is what unblocks it: an issued request holds the trade's
	// partial unique index, so leaving it would make every future request fail.
	after, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want the trade left pending for an operator to decide", after.State)
	}
	fresh, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatalf("a fresh request on the correct cluster must be possible: %v", err)
	}
	if fresh.Cluster != cluster.Devnet {
		t.Errorf("cluster = %q, want the replacement request stamped devnet", fresh.Cluster)
	}
}
