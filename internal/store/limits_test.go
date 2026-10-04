package store

import (
	"context"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// sumSpend totals the buyer's committed spend as a USD-shaped string, summed in
// the same six-decimal scale the caps use so the comparison cannot lose a unit.
func mustAmount(t *testing.T, a money.Amount, err error) money.Amount {
	t.Helper()
	if err != nil {
		t.Fatalf("build amount: %v", err)
	}
	return a
}

func sumSpend(t *testing.T, s *Store, buyer string, since time.Time) string {
	t.Helper()
	rows, err := s.CommittedSpendSince(context.Background(), buyer, since, "")
	if err != nil {
		t.Fatalf("committed spend for %s: %v", buyer, err)
	}
	var total uint64
	for _, row := range rows {
		units, err := row.Amount.BaseUnits(6)
		if err != nil {
			t.Fatalf("sum %s: %v", row.Amount, err)
		}
		total += units
	}
	amount, err := money.NewFromBaseUnits(total, 6)
	return mustAmount(t, amount, err).String()
}

// sumSpendExcluding totals a buyer's committed spend with one trade left out.
func sumSpendExcluding(t *testing.T, s *Store, buyer string, since time.Time, excludeTradeID string) string {
	t.Helper()
	rows, err := s.CommittedSpendSince(context.Background(), buyer, since, excludeTradeID)
	if err != nil {
		t.Fatalf("committed spend for %s excluding %s: %v", buyer, excludeTradeID, err)
	}
	var total uint64
	for _, row := range rows {
		units, err := row.Amount.BaseUnits(6)
		if err != nil {
			t.Fatalf("sum %s: %v", row.Amount, err)
		}
		total += units
	}
	amount, err := money.NewFromBaseUnits(total, 6)
	return mustAmount(t, amount, err).String()
}

// backdateCommitEvents rewrites the timestamps of the transitions into
// spend-committing states, so a window can be exercised without waiting for it.
func backdateCommitEvents(t *testing.T, s *Store, tradeID string, at time.Time) {
	t.Helper()
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE trade_events SET created_at = ? WHERE trade_id = ? AND to_state IN (?, ?, ?, ?)`,
		nanos(at), tradeID, committedStates[0], committedStates[1], committedStates[2], committedStates[3])
	if err != nil {
		t.Fatalf("backdate commit events for %s: %v", tradeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("trade %s has no commit event to backdate", tradeID)
	}
}

// driveSpendTrade walks a trade the way the trade service does: every transition
// is recorded in the event log, because that log is where the cap window reads
// commit times from. SetTradeState alone would leave the query with nothing to
// find, and a test that passes against a stub of the real path proves nothing.
func driveSpendTrade(t *testing.T, s *Store, tradeID string, path ...domain.TradeState) {
	t.Helper()
	ctx := context.Background()
	from := domain.TradeProposed
	for _, to := range path {
		changed, err := s.SetTradeState(ctx, tradeID, from, to, usageBase)
		if err != nil {
			t.Fatalf("%s: %s->%s: %v", tradeID, from, to, err)
		}
		if !changed {
			t.Fatalf("%s: transition %s->%s matched no row", tradeID, from, to)
		}
		if _, err := s.AppendTradeEvent(ctx, domain.TradeEvent{
			TradeID:      tradeID,
			ActorAgentID: "buyer",
			Type:         domain.TradeEventType(to),
			FromState:    from,
			ToState:      to,
			CreatedAt:    usageBase,
		}); err != nil {
			t.Fatalf("%s: append %s->%s event: %v", tradeID, from, to, err)
		}
		from = to
	}
}

// backdateEngagement rewrites a trade's creation time, so the window can be
// exercised without waiting for it.
func backdateEngagement(t *testing.T, s *Store, tradeID string, at time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE trades SET created_at = ? WHERE id = ?`, nanos(at), tradeID); err != nil {
		t.Fatalf("backdate engagement of %s: %v", tradeID, err)
	}
}

// setCommitTime rewrites the timestamps of the transitions into a
// spend-committing state, so a window can be exercised without waiting for it.
func setCommitTime(t *testing.T, s *Store, tradeID string, at time.Time) {
	t.Helper()
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE trade_events SET created_at = ? WHERE trade_id = ? AND to_state IN (?, ?, ?, ?)`,
		nanos(at), tradeID, committedStates[0], committedStates[1], committedStates[2], committedStates[3])
	if err != nil {
		t.Fatalf("set commit time for %s: %v", tradeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("trade %s has no commit event to move", tradeID)
	}
}

func seedSpend(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"buyer", "other", "seller"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatalf("create agent %s: %v", id, err)
		}
	}
	for _, o := range []struct{ id, agent string }{{"o1", "seller"}, {"o2", "seller"}} {
		if err := s.CreateOffer(ctx, testOffer(o.id, o.agent), ""); err != nil {
			t.Fatalf("create offer %s: %v", o.id, err)
		}
	}
	// Committed by the buyer under test.
	seedTrade(t, s, "t1", "o1", "buyer", "seller")
	driveSpendTrade(t, s, "t1", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeRecorded)

	// Still a proposal: no tokens have moved, so it spends nothing.
	seedTrade(t, s, "t2", "o1", "buyer", "seller")

	// Walked to negotiating and then abandoned: still spends nothing.
	seedTrade(t, s, "t3", "o1", "buyer", "seller")
	driveSpendTrade(t, s, "t3", domain.TradeNegotiating)

	// Cancelled before anything committed: spends nothing.
	seedTrade(t, s, "t4", "o1", "buyer", "seller")
	driveTrade(t, s, "t4", domain.TradeCancelled)

	// A settlement the buyer can sign and broadcast right now, so it counts even
	// though no receipt exists.
	seedTrade(t, s, "t5", "o2", "buyer", "seller")
	driveSpendTrade(t, s, "t5", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeSettlementPending)

	// Committed by somebody else, which must not appear against this buyer.
	seedTrade(t, s, "t6", "o2", "other", "seller")
	driveSpendTrade(t, s, "t6", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeRecorded)
}

func TestCommittedSpendChargesEveryEngagementExceptACancelledOne(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	// t2, t3 and t4 are also charged: they are engagements the buyer still holds,
	// and t4 releases only because it was cancelled. t6 belongs to another buyer.
	if got := sumSpend(t, s, "buyer", usageBase.Add(-time.Hour)); got != "100.000000" {
		t.Errorf("engaged spend for the buyer = %s, want 100", got)
	}
	if got := sumSpend(t, s, "other", usageBase.Add(-time.Hour)); got != "25.000000" {
		t.Errorf("committed spend for the other buyer = %s, want 25", got)
	}
}

func TestExcludingATradeLeavesItOutOfItsOwnTotal(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	// A caller that is about to account for a trade itself excludes it and adds
	// its own amount instead. Not excluding it charges the buyer twice, which
	// refuses a trade that was inside its cap when it was created: a settlement
	// re-check on t5 that counted the full hundred and then added t5 again would
	// refuse a buyer who has not gone over anything.
	full, err := s.CommittedSpendSince(context.Background(), "buyer", usageBase.Add(-time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	withoutT5, err := s.CommittedSpendSince(context.Background(), "buyer", usageBase.Add(-time.Hour), "t5")
	if err != nil {
		t.Fatal(err)
	}
	if len(full)-len(withoutT5) != 1 {
		t.Fatalf("excluding t5 removed %d rows, want 1", len(full)-len(withoutT5))
	}
	// Four seeded trades at twenty-five each, so leaving t5 out is seventy-five.
	if got := sumSpendExcluding(t, s, "buyer", usageBase.Add(-time.Hour), "t5"); got != "75.000000" {
		t.Errorf("engaged spend without t5 = %s, want 75", got)
	}

	// Excluding something that is not there changes nothing, so a caller cannot
	// quietly subtract another buyer's trade or invent headroom for itself.
	absent, err := s.CommittedSpendSince(context.Background(), "buyer", usageBase.Add(-time.Hour), "no-such-trade")
	if err != nil {
		t.Fatal(err)
	}
	if len(absent) != len(full) {
		t.Errorf("excluding an unknown trade changed the total from %d rows to %d", len(full), len(absent))
	}
}

func TestCommittedSpendExpiresOnceAnEngagementIsOlderThanTheWindow(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	// Everything was opened and committed at usageBase. Five minutes later it is
	// all older than a one-minute window.
	if got := sumSpend(t, s, "buyer", usageBase.Add(5*time.Minute)); got != "0.000000" {
		t.Errorf("spend five minutes after commit = %s, want 0", got)
	}
	if got := sumSpend(t, s, "buyer", usageBase.Add(-time.Minute)); got != "100.000000" {
		t.Errorf("spend inside a one-minute window = %s, want 100", got)
	}
}

func TestCommittingLaterMovesTheExposureIntoTheNewerWindow(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	// t1 was opened yesterday and commits half an hour from now; the rest stay
	// engaged at usageBase. Everything else in the seed is unaffected.
	backdateEngagement(t, s, "t1", usageBase.Add(-18*time.Hour))
	setCommitTime(t, s, "t1", usageBase.Add(30*time.Minute))

	// A window covering everything, including t1's later commit.
	if got := sumSpend(t, s, "buyer", usageBase.Add(-10*time.Minute)); got != "100.000000" {
		t.Errorf("spend in a window covering t1's commit = %s, want 100", got)
	}
	// A window whose cutoff falls between the other engagements and t1's commit
	// contains t1 alone. This is the anchoring: t1 left the window when the
	// others did not, because its exposure did not start until it could move
	// money.
	if got := sumSpend(t, s, "buyer", usageBase.Add(10*time.Minute)); got != "25.000000" {
		t.Errorf("spend in a window covering only the later commit = %s, want 25", got)
	}
	// And once the window has moved past it again, the exposure is gone.
	if got := sumSpend(t, s, "buyer", usageBase.Add(2*time.Hour)); got != "0.000000" {
		t.Errorf("spend long after every engagement = %s, want 0", got)
	}
}

func TestCommittingTodayChargesADayAfterOpening(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	// The case the window exists for: opened eighteen hours ago, commits now. A
	// cap measured from the opening would let it escape every daily limit.
	backdateEngagement(t, s, "t1", usageBase.Add(-18*time.Hour))
	setCommitTime(t, s, "t1", usageBase)

	// The window that matters: it opened seventeen hours ago, after t1 was opened
	// but before it committed. A cap measured from the opening would not see this
	// trade at all, which is the escape the commit timestamp exists to close.
	if got := sumSpend(t, s, "buyer", usageBase.Add(-17*time.Hour)); got != "100.000000" {
		t.Errorf("spend in a window that opened after t1 was created = %s, want 100", got)
	}
	if got := sumSpend(t, s, "buyer", usageBase.Add(-24*time.Hour)); got != "100.000000" {
		t.Errorf("spend in a 24h window = %s, want 100", got)
	}
	// Every engagement here is older than that cutoff, so the window is empty.
	if got := sumSpend(t, s, "buyer", usageBase.Add(10*time.Minute)); got != "0.000000" {
		t.Errorf("spend in a window past every engagement = %s, want 0", got)
	}
}

func TestCommittedSpendCountsATradeOnceWhenItIsLaterDisputed(t *testing.T) {
	s := testStore(t)
	seedSpend(t, s)

	if _, err := s.SetTradeState(context.Background(), "t1", domain.TradeRecorded, domain.TradeDisputed, usageBase); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTradeEvent(context.Background(), domain.TradeEvent{
		TradeID: "t1", ActorAgentID: "buyer", Type: domain.EventDisputed,
		FromState: domain.TradeRecorded, ToState: domain.TradeDisputed, CreatedAt: usageBase,
	}); err != nil {
		t.Fatal(err)
	}
	// Two commit transitions, one exposure. Charging it twice would punish a buyer
	// for raising a dispute.
	if got := sumSpend(t, s, "buyer", usageBase.Add(-time.Hour)); got != "100.000000" {
		t.Errorf("spend after a dispute = %s, want 100: one trade is charged once", got)
	}
}

func TestAgentLimitsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("buyer")); err != nil {
		t.Fatal(err)
	}

	// Absent is not an error: it means the agent is on the deployment defaults.
	if _, err := s.GetAgentLimits(ctx, "buyer"); err != ErrNotFound {
		t.Errorf("GetAgentLimits with no row = %v, want ErrNotFound", err)
	}

	want := domain.AgentLimits{
		AgentID:     "buyer",
		PerTradeUSD: money.MustParse("50.00"),
		PerDayUSD:   money.MustParse("200.00"),
		RaisedAt:    usageBase,
	}
	if err := s.SetAgentLimits(ctx, want); err != nil {
		t.Fatalf("SetAgentLimits: %v", err)
	}
	got, err := s.GetAgentLimits(ctx, "buyer")
	if err != nil {
		t.Fatalf("GetAgentLimits: %v", err)
	}
	if got.PerTradeUSD.String() != "50.00" || got.PerDayUSD.String() != "200.00" {
		t.Errorf("stored limits = %s per trade, %s per day", got.PerTradeUSD, got.PerDayUSD)
	}
	if !got.RaisedAt.Equal(usageBase) {
		t.Errorf("raisedAt = %s, want %s", got.RaisedAt, usageBase)
	}

	// A second raise replaces the first rather than accumulating.
	want.PerTradeUSD = money.MustParse("10.00")
	if err := s.SetAgentLimits(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAgentLimits(ctx, "buyer")
	if err != nil {
		t.Fatal(err)
	}
	if got.PerTradeUSD.String() != "10.00" {
		t.Errorf("per-trade cap after a second raise = %s, want 10.00", got.PerTradeUSD)
	}
}

func TestAgentLimitsRefusesAnUnknownAgent(t *testing.T) {
	s := testStore(t)
	err := s.SetAgentLimits(context.Background(), domain.AgentLimits{
		AgentID:     "nobody",
		PerTradeUSD: money.MustParse("50.00"),
		PerDayUSD:   money.MustParse("200.00"),
		RaisedAt:    usageBase,
	})
	if err == nil {
		t.Fatal("a cap was recorded for an agent that does not exist")
	}
}
