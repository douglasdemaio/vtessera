package trade_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
)

// unpricedMint is a syntactically valid address this deployment declares no rate
// for. It exists so a test can build a real offer row in it without inventing a
// schema hook to do it.
const unpricedMint = "9n4xM2fPP91Zm3fDc7rT2QkWrB1sRdVvNcR6cMDcyGDQ"

// capSetup is setup with spending caps in force. Every test in this file differs
// from the rest of the package only by that, which is deliberate: a cap tested
// somewhere other than the trade path is not known to work on the trade path.
func capSetup(t *testing.T, adjust func(limits.Policy) limits.Policy) harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "trade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	policy := limits.DefaultPolicy(limits.DefaultRates())
	if adjust != nil {
		policy = adjust(policy)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("cap policy: %v", err)
	}
	registrySvc := registry.New(db, mints, registry.WithPricer(policy))
	for _, id := range []string{buyerKey, sellerKey, outsider} {
		if _, _, err := registrySvc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	led := ledger.New(db, signer)
	return harness{
		svc:      trade.New(db, db, db, led).WithLimits(policy, db),
		registry: registrySvc,
		led:      led,
		db:       db,
	}
}

// offerFor publishes an off-chain offer at a chosen price, so a test can put the
// amount above or below a cap rather than working with whatever the package's
// shared fixture charges.
func offerFor(t *testing.T, h harness, amount string) string {
	t.Helper()
	ctx := context.Background()
	offer, _, err := h.registry.PublishOffer(ctx, sellerKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     amount,
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if err != nil {
		t.Fatalf("publish offer at %s: %v", amount, err)
	}
	return offer.ID
}

// commit walks a trade to recorded, which is the point at which the spend counts.
func commit(t *testing.T, h harness, tradeID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, buyerKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Record(ctx, buyerKey, tradeID); err != nil {
		t.Fatalf("commit %s: %v", tradeID, err)
	}
}

func TestATradeAboveThePerTradeCapIsRejected(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	// The default cap is five dollars and USDC is priced at par, so anything above
	// five USDC must be refused before the trade row exists.
	overCap := offerFor(t, h, "5.01")
	if _, _, err := h.svc.Create(ctx, buyerKey, overCap, domain.SettlementOffchain, ""); !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("Create at 5.01 against a 5.00 cap = %v, want ErrSpendCapExceeded", err)
	}

	// Exactly at the cap is allowed. A cap that refuses its own boundary is an
	// off-by-one dressed as a limit.
	atCap := offerFor(t, h, "5.00")
	if _, isNew, err := h.svc.Create(ctx, buyerKey, atCap, domain.SettlementOffchain, ""); err != nil || !isNew {
		t.Fatalf("Create at exactly the cap = %v, %v", isNew, err)
	}
}

func TestATradeAboveTheDailyCapIsRejected(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	// Four five-dollar trades is exactly twenty dollars. The fifth, at any
	// amount, has to be refused.
	offerID := offerFor(t, h, "5.00")
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		tr, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
		if err != nil {
			t.Fatalf("trade %d of 4 against the daily cap: %v", i+1, err)
		}
		ids = append(ids, tr.ID)
	}
	for _, id := range ids {
		commit(t, h, id)
	}

	oneCent := offerFor(t, h, "0.01")
	_, _, err := h.svc.Create(ctx, buyerKey, oneCent, domain.SettlementOffchain, "")
	if !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("Create with 20.00 already committed = %v, want ErrSpendCapExceeded", err)
	}
	if !strings.Contains(err.Error(), "20.00") {
		t.Errorf("refusal %q does not name the daily cap the buyer ran into", err)
	}
}

func TestAnOpenNegotiationReservesItsBudgetUntilItIsCancelled(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	// A proposed trade is not money that has moved, but it is exposure the buyer
	// has taken on. Five of them exhaust twenty dollars, because a buyer that
	// could open unlimited negotiations against a spent budget has no cap at all.
	offerID := offerFor(t, h, "5.00")
	opened := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		tr, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
		if err != nil {
			t.Fatalf("proposed trade %d was refused: %v", i+1, err)
		}
		opened = append(opened, tr.ID)
	}
	fifth := offerFor(t, h, "5.00")
	if _, _, err := h.svc.Create(ctx, buyerKey, fifth, domain.SettlementOffchain, ""); !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("a fifth open five-dollar negotiation = %v, want ErrSpendCapExceeded", err)
	}

	// Cancelling one releases the reservation, so the buyer can start again. This
	// is the escape that makes the reservation tolerable: a negotiation that goes
	// nowhere does not hold the budget for the rest of the day.
	if _, err := h.svc.Cancel(ctx, buyerKey, opened[0], "not proceeding"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Create(ctx, buyerKey, fifth, domain.SettlementOffchain, ""); err != nil {
		t.Fatalf("a fifth negotiation after cancelling one: %v", err)
	}
}

func TestACancelledTradeReleasesItsBudget(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	offerID := offerFor(t, h, "5.00")
	for i := 0; i < 5; i++ {
		tr, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
		if err != nil {
			t.Fatalf("trade %d was refused: %v", i+1, err)
		}
		if _, err := h.svc.Cancel(ctx, buyerKey, tr.ID, "agent chose not to proceed"); err != nil {
			t.Fatalf("cancel trade %d: %v", i+1, err)
		}
	}
}

func TestConcurrentTradesCannotBothTakeTheSameDollar(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	// Five simultaneous five-dollar offers against a twenty-dollar budget. Exactly
	// four may be created. Reading the cap and then reserving against it are two
	// separate statements, and without the lock between them every one of these
	// can read a budget with room left before any of them has reserved anything.
	offerID := offerFor(t, h, "5.00")
	const attempts = 5
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		created    int
		refusedFor int
		otherErr   error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, trade.ErrSpendCapExceeded):
				refusedFor++
			default:
				otherErr = err
			}
		}()
	}
	wg.Wait()
	if otherErr != nil {
		t.Fatalf("a concurrent create failed for a reason other than the cap: %v", otherErr)
	}
	if created != 4 || refusedFor != 1 {
		t.Fatalf("created %d and refused %d of %d simultaneous five-dollar trades, want 4 and 1",
			created, refusedFor, attempts)
	}
}

func TestAnUnpricedCurrencyIsRefusedRatherThanTradedUncapped(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	// A mint with no declared rate cannot be capped. Allowing it would hand every
	// agent a way around the cap by choosing a currency, so the offer is refused
	// at the point the seller names the currency.
	if _, _, err := h.registry.PublishOffer(ctx, sellerKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "priced in something we cannot measure",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     "1.00",
		PriceMint:       unpricedMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, ""); !errors.Is(err, registry.ErrMintUnpriced) {
		t.Fatalf("publishing an offer in an unpriced mint = %v, want registry.ErrMintUnpriced", err)
	}

	// The refusal is not limited to publishing. An offer published before the
	// policy existed must also be refused at trade time, because an agent that
	// can find one will try.
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	legacy := registry.New(h.db, mints)
	offer, _, err := legacy.PublishOffer(ctx, sellerKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "published before caps existed",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     "1000000.00",
		PriceMint:       unpricedMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if err != nil {
		t.Fatalf("publish through an unpriced registry: %v", err)
	}
	_, _, err = h.svc.Create(ctx, buyerKey, offer.ID, domain.SettlementOffchain, "")
	if !errors.Is(err, trade.ErrMintUnpriced) {
		t.Fatalf("Create in an unpriced mint = %v, want trade.ErrMintUnpriced", err)
	}
}

func TestRaisingACapTakesOptInAndStaysUnderTheCeiling(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, func(p limits.Policy) limits.Policy {
		return p.WithCeilings(money.MustParse("50.00"), money.MustParse("200.00"))
	})

	offerID := offerFor(t, h, "25.00")
	if _, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, ""); !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("a 25.00 trade was allowed under the default cap: %v", err)
	}

	raised, err := h.svc.RaiseLimits(ctx, buyerKey, money.MustParse("50.00"), money.MustParse("200.00"))
	if err != nil {
		t.Fatalf("RaiseLimits: %v", err)
	}
	if raised.PerTrade.String() != "50.00" || raised.PerDay.String() != "200.00" {
		t.Errorf("RaiseLimits reported %s per trade and %s per day, want the values asked for",
			raised.PerTrade, raised.PerDay)
	}
	if _, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, ""); err != nil {
		t.Fatalf("a 25.00 trade after opting in: %v", err)
	}

	// Above the ceiling is refused rather than clamped. Clamping would tell the
	// agent its cap was raised when it was not, and it would go on to spend
	// against a number it chose.
	_, err = h.svc.RaiseLimits(ctx, buyerKey, money.MustParse("500.00"), money.MustParse("200.00"))
	if !errors.Is(err, limits.ErrAboveCeiling) {
		t.Fatalf("raising to 500.00 against a 50.00 ceiling = %v, want ErrAboveCeiling", err)
	}

	// A raise belongs to the agent that asked for it.
	if _, _, err := h.svc.Create(ctx, outsider, offerID, domain.SettlementOffchain, ""); !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("another agent inherited the raise: %v", err)
	}
}

func TestRaisingOnlyOneCapLeavesTheOtherAlone(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, func(p limits.Policy) limits.Policy {
		return p.WithCeilings(money.MustParse("50.00"), money.MustParse("200.00"))
	})

	raised, err := h.svc.RaiseLimits(ctx, buyerKey, money.MustParse("50.00"), money.Amount{})
	if err != nil {
		t.Fatalf("RaiseLimits: %v", err)
	}
	// An omitted figure means "leave this one as it is", so a caller raising only
	// the per-trade cap does not silently drop its daily cap to the default.
	if raised.PerDay.String() != "20.00" {
		t.Errorf("daily cap after raising only the per-trade cap = %s, want the 20.00 default", raised.PerDay)
	}
}

func TestCapsAreOffUntilAPolicyIsWired(t *testing.T) {
	ctx := context.Background()
	h := setup(t)

	if _, ok := h.svc.Limits(); ok {
		t.Fatal("a service built without WithLimits reports caps in force")
	}
	// Same shape of trade, same amount, no policy: it proceeds. Every other test
	// in this package relies on that, so stating it here means turning caps on by
	// default would break this test rather than quietly change them all.
	offerID := offerFor(t, h, "500.00")
	if _, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, ""); err != nil {
		t.Fatalf("uncapped Create: %v", err)
	}
}
