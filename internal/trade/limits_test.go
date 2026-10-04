package trade_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// acceptAt walks a trade to accepted, which is where a deadline starts running.
func acceptAt(t *testing.T, h harness, offerID string) domain.Trade {
	t.Helper()
	ctx := context.Background()
	tr, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
	if err != nil {
		t.Fatalf("create trade: %v", err)
	}
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	accepted, err := h.svc.Accept(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != domain.TradeAccepted {
		t.Fatalf("state = %s, want accepted", accepted.State)
	}
	return accepted
}

// commitAmount opens an offer at a price and takes one trade all the way to
// recorded. It exists because the file's commit helper takes a trade ID and these
// tests think in amounts: five four-dollar trades is the shape of the scenario,
// not five trades with opaque IDs.
func commitAmount(t *testing.T, h harness, amount string) {
	t.Helper()
	ctx := context.Background()
	tr, _, err := h.svc.Create(ctx, buyerKey, offerFor(t, h, amount), domain.SettlementOffchain, "")
	if err != nil {
		t.Fatalf("create at %s: %v", amount, err)
	}
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Record(ctx, buyerKey, tr.ID); err != nil {
		t.Fatalf("record at %s: %v", amount, err)
	}
}

// clockSetup is capSetup with both a deadline and a clock the test drives, which
// is what it takes to put a trade on either side of its acceptance deadline.
func clockSetup(t *testing.T, ttl time.Duration, adjust func(limits.Policy) limits.Policy) (harness, *time.Time) {
	t.Helper()
	h := capSetup(t, adjust)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h.svc = h.svc.WithAcceptanceTTL(ttl).WithClock(func() time.Time { return now })
	return h, &now
}

func TestAnAcceptedTradeCannotBeCancelledBeforeItsDeadline(t *testing.T) {
	ctx := context.Background()
	h, now := clockSetup(t, 24*time.Hour, nil)

	tr := acceptAt(t, h, offerFor(t, h, "2.00"))

	// Acceptance is a commitment. Letting a party walk away from it the moment
	// they regret it would leave the counterparty with no way to plan, so the
	// refusal has to name when the trade does become walk-away.
	_, err := h.svc.Cancel(ctx, buyerKey, tr.ID, "changed my mind")
	if !errors.Is(err, trade.ErrNotExpiredYet) {
		t.Fatalf("Cancel before the deadline = %v, want ErrNotExpiredYet", err)
	}
	after, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeAccepted {
		t.Errorf("state = %s, want the refusal to leave it accepted", after.State)
	}

	// Either party, not just the buyer: an accepted trade the seller never
	// intends to deliver is exactly as dead as the reverse.
	if _, err := h.svc.Cancel(ctx, sellerKey, tr.ID, "never mind"); !errors.Is(err, trade.ErrNotExpiredYet) {
		t.Fatalf("seller Cancel before the deadline = %v, want ErrNotExpiredYet", err)
	}

	*now = now.Add(24*time.Hour + time.Second)
	cancelled, err := h.svc.Cancel(ctx, buyerKey, tr.ID, "")
	if err != nil {
		t.Fatalf("Cancel after the deadline: %v", err)
	}
	if cancelled.State != domain.TradeCancelled {
		t.Errorf("state = %s, want cancelled", cancelled.State)
	}
}

func TestAnAcceptedTradeAtItsDeadlineCanStillBeCommitted(t *testing.T) {
	ctx := context.Background()
	h, now := clockSetup(t, 24*time.Hour, nil)

	tr := acceptAt(t, h, offerFor(t, h, "2.00"))

	// The deadline is the last instant at which a commitment still counts, not
	// the first at which it is refused. A buyer who commits at 23:59:59 and is
	// refused a second later would lose a trade both parties had already agreed.
	*now = now.Add(24 * time.Hour)
	if _, _, err := h.svc.Record(ctx, buyerKey, tr.ID); err != nil {
		t.Fatalf("Record at the deadline: %v", err)
	}
}

func TestTheDailyCapIsEnforcedWhenTheBuyerCommits(t *testing.T) {
	ctx := context.Background()
	// A one-hour window, so a test can roll it by advancing the clock rather than
	// by waiting. The window is the whole mechanism: the trade is checked against
	// one window and committed against another.
	h, now := clockSetup(t, 24*time.Hour, func(p limits.Policy) limits.Policy {
		return p.WithWindow(time.Hour)
	})

	// Accepted with the buyer's daily budget almost empty...
	tr := acceptAt(t, h, offerFor(t, h, "4.00"))

	// ...then the window rolls, which drops the accepted trade's reservation out of
	// the counted window, and the buyer spends that window's whole budget on other
	// trades. Twenty dollars is exactly the cap, so all five are accepted.
	*now = now.Add(2 * time.Hour)
	for range 5 {
		commitAmount(t, h, "4.00")
	}

	// Committing the first trade now would take the buyer to twenty-four dollars
	// across the boundary. It is refused, which is the whole point: the old
	// behaviour let this through, and twenty-four is inside the "one window of
	// overshoot" that was documented as a known bound.
	_, _, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if !errors.Is(err, trade.ErrSpendCapExceeded) {
		t.Fatalf("Record after the window rolled = %v, want ErrSpendCapExceeded", err)
	}

	// Refused, and still cancellable once its deadline passes. A commit that is
	// turned down must never leave a buyer holding a trade they can neither finish
	// nor escape, so the refusal and the way out have to arrive together.
	refused, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refused.State != domain.TradeAccepted {
		t.Errorf("state = %s, want the refused commit to leave it accepted", refused.State)
	}
	*now = now.Add(24 * time.Hour)
	cancelled, err := h.svc.Cancel(ctx, buyerKey, tr.ID, "")
	if err != nil {
		t.Fatalf("Cancel after the refused commit: %v", err)
	}
	if cancelled.State != domain.TradeCancelled {
		t.Errorf("state = %s, want cancelled", cancelled.State)
	}
}

func TestATradeInsideItsCapIsStillCommittableAfterTheWindowRolls(t *testing.T) {
	ctx := context.Background()
	h, now := clockSetup(t, 24*time.Hour, func(p limits.Policy) limits.Policy {
		return p.WithWindow(time.Hour)
	})

	tr := acceptAt(t, h, offerFor(t, h, "4.00"))

	// One trade at four dollars, an hour and a half later, is still inside a
	// twenty-dollar cap. Re-checking the cap at the commit must not turn the cap
	// into a rule that any elapsed time invalidates a trade both parties agreed.
	*now = now.Add(90 * time.Minute)
	if _, _, err := h.svc.Record(ctx, buyerKey, tr.ID); err != nil {
		t.Fatalf("Record after the window rolled: %v", err)
	}
}

func TestAnExpiredAcceptedTradeIsSweptAndReleasesItsBudget(t *testing.T) {
	ctx := context.Background()
	h, now := clockSetup(t, time.Hour, nil)

	stale := acceptAt(t, h, offerFor(t, h, "4.00"))
	// A trade accepted after the sweep's cutoff must survive it.
	*now = now.Add(30 * time.Minute)
	fresh := acceptAt(t, h, offerFor(t, h, "4.00"))

	*now = now.Add(45 * time.Minute)
	expired, err := h.svc.ExpireAccepted(ctx, 100)
	if err != nil {
		t.Fatalf("ExpireAccepted: %v", err)
	}
	if expired != 1 {
		t.Errorf("expired = %d, want 1", expired)
	}

	swept, err := h.svc.Get(ctx, buyerKey, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if swept.State != domain.TradeCancelled {
		t.Errorf("state = %s, want the stale trade cancelled", swept.State)
	}
	kept, err := h.svc.Get(ctx, buyerKey, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != domain.TradeAccepted {
		t.Errorf("state = %s, want the young trade untouched", kept.State)
	}

	// The sweep is how the cap is more than a promise: a budget that never comes
	// back is a queue, not a cap.
	spent, _, err := h.svc.Create(ctx, buyerKey, offerFor(t, h, "4.00"), domain.SettlementOffchain, "")
	if err != nil {
		t.Fatalf("Create after the sweep released the budget: %v", err)
	}
	if spent.State != domain.TradeProposed {
		t.Errorf("state = %s, want proposed", spent.State)
	}

	// Running it again changes nothing and must not fail, because it runs on a
	// timer for the life of the process.
	if again, err := h.svc.ExpireAccepted(ctx, 100); err != nil || again != 0 {
		t.Errorf("second sweep = %d, %v, want 0 and no error", again, err)
	}
}

func TestTheSweepIsBounded(t *testing.T) {
	ctx := context.Background()
	h, now := clockSetup(t, time.Hour, nil)

	for range 5 {
		acceptAt(t, h, offerFor(t, h, "1.00"))
	}
	*now = now.Add(2 * time.Hour)
	expired, err := h.svc.ExpireAccepted(ctx, 2)
	if err != nil {
		t.Fatalf("ExpireAccepted: %v", err)
	}
	if expired != 2 {
		t.Errorf("expired = %d, want the batch limit of 2", expired)
	}
	// The remainder is swept on the next tick rather than all at once, which is
	// what keeps a large backlog from holding a write lock.
	if rest, err := h.svc.ExpireAccepted(ctx, 2); err != nil || rest != 2 {
		t.Errorf("second sweep = %d, %v, want 2 and no error", rest, err)
	}
	if last, err := h.svc.ExpireAccepted(ctx, 2); err != nil || last != 1 {
		t.Errorf("third sweep = %d, %v, want 1 and no error", last, err)
	}
}

func TestNoDeadlineMeansNoCancellation(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)

	tr := acceptAt(t, h, offerFor(t, h, "2.00"))

	// With no deadline configured the transition stays refused, so the service
	// behaves exactly as it did before this change rather than quietly widening
	// who can cancel. The configuration refuses to boot in this state.
	if _, err := h.svc.Cancel(ctx, buyerKey, tr.ID, ""); !errors.Is(err, trade.ErrIllegalState) {
		t.Fatalf("Cancel with no deadline = %v, want ErrIllegalState", err)
	}
	if n, err := h.svc.ExpireAccepted(ctx, 100); err != nil || n != 0 {
		t.Errorf("sweep with no deadline = %d, %v, want 0 and no error", n, err)
	}
}
