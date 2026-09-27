package store

import (
	"context"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

var usageBase = time.Unix(1759000000, 0).UTC()

func driveTrade(t *testing.T, s *Store, tradeID string, path ...domain.TradeState) {
	t.Helper()
	driveTradeFrom(t, s, tradeID, domain.TradeProposed, path...)
}

func driveTradeFrom(t *testing.T, s *Store, tradeID string, from domain.TradeState, path ...domain.TradeState) {
	t.Helper()
	ctx := context.Background()
	for _, to := range path {
		changed, err := s.SetTradeState(ctx, tradeID, from, to, usageBase)
		if err != nil {
			t.Fatalf("%s: %s->%s: %v", tradeID, from, to, err)
		}
		if !changed {
			t.Fatalf("%s: transition %s->%s matched no row", tradeID, from, to)
		}
		from = to
	}
}

func issueReceipt(t *testing.T, s *Store, tradeID string, at time.Time) {
	t.Helper()
	if err := s.SaveReceipt(context.Background(), domain.Receipt{
		ID:       "r-" + tradeID,
		TradeID:  tradeID,
		JWS:      "jws." + tradeID,
		IssuedAt: at,
	}); err != nil {
		t.Fatalf("save receipt for %s: %v", tradeID, err)
	}
}

func seedTrade(t *testing.T, s *Store, id, offerID, buyer, seller string) {
	t.Helper()
	if err := s.CreateTrade(context.Background(), domain.Trade{
		ID:             id,
		OfferID:        offerID,
		BuyerAgentID:   buyer,
		SellerAgentID:  seller,
		Description:    "sentiment dataset",
		Amount:         money.MustParse("25"),
		Mint:           validMint,
		SettlementMode: domain.SettlementOffchain,
		State:          domain.TradeProposed,
		CreatedAt:      usageBase,
		UpdatedAt:      usageBase,
	}, "key-"+id); err != nil {
		t.Fatalf("create trade %s: %v", id, err)
	}
}

// Seeds a marketplace where the delivered trades belong to one agent and the
// failed trades belong to another, so that a per-agent breakdown which omits
// failure-only agents cannot pass by accident.
func seedUsage(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatalf("create agent %s: %v", id, err)
		}
	}
	for _, o := range []struct{ id, agent string }{{"o1", "alice"}, {"o2", "alice"}, {"o3", "dave"}} {
		if err := s.CreateOffer(ctx, testOffer(o.id, o.agent), ""); err != nil {
			t.Fatalf("create offer %s: %v", o.id, err)
		}
	}

	seedTrade(t, s, "t1", "o1", "bob", "alice")
	driveTrade(t, s, "t1", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeRecorded)
	issueReceipt(t, s, "t1", usageBase.Add(time.Minute))

	seedTrade(t, s, "t2", "o2", "carol", "alice")
	driveTrade(t, s, "t2", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeSettlementPending, domain.TradeSettled)
	issueReceipt(t, s, "t2", usageBase.Add(2*time.Minute))

	seedTrade(t, s, "t3", "o1", "bob", "alice")
	driveTrade(t, s, "t3", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeRecorded)
	issueReceipt(t, s, "t3", usageBase.Add(3*time.Minute))

	seedTrade(t, s, "t4", "o3", "bob", "dave")
	driveTrade(t, s, "t4", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeDisputed)

	seedTrade(t, s, "t5", "o3", "carol", "dave")
	driveTrade(t, s, "t5", domain.TradeCancelled)

	seedTrade(t, s, "t6", "o3", "bob", "dave")
	driveTrade(t, s, "t6", domain.TradeNegotiating)
}

func TestUsageMetricsCountsDeliveriesOutcomesAndDistinctParties(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}

	want := domain.UsageTotals{
		Delivered: 3,
		Disputed:  1,
		Cancelled: 1,
		Consumers: 2,
		Services:  2,
	}
	if m.Totals != want {
		t.Errorf("totals = %+v, want %+v", m.Totals, want)
	}

	// o3 is only ever traded in failed and in-flight trades, so it must not
	// inflate the count of services actually delivered.
	if m.Totals.Services != 2 {
		t.Errorf("services = %d, want 2: only delivered offers count", m.Totals.Services)
	}
}

func TestUsageMetricsListAnAgentWhoseTradesAllFailed(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}

	if len(m.Agents) != 2 {
		t.Fatalf("agents = %+v, want 2 entries", m.Agents)
	}
	if m.Agents[0] != (domain.AgentUsage{AgentID: "alice", Delivered: 3}) {
		t.Errorf("agents[0] = %+v, want alice with 3 delivered", m.Agents[0])
	}
	if m.Agents[1] != (domain.AgentUsage{AgentID: "dave", Disputed: 1, Cancelled: 1}) {
		t.Errorf("agents[1] = %+v, want dave with 1 disputed and 1 cancelled", m.Agents[1])
	}
}

func TestUsageMetricsPerAgentCountsSumToTotals(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}

	var delivered, disputed, cancelled int
	for _, a := range m.Agents {
		delivered += a.Delivered
		disputed += a.Disputed
		cancelled += a.Cancelled
	}
	if delivered != m.Totals.Delivered {
		t.Errorf("sum of per-agent delivered = %d, totals = %d", delivered, m.Totals.Delivered)
	}
	if disputed != m.Totals.Disputed {
		t.Errorf("sum of per-agent disputed = %d, totals = %d", disputed, m.Totals.Disputed)
	}
	if cancelled != m.Totals.Cancelled {
		t.Errorf("sum of per-agent cancelled = %d, totals = %d", cancelled, m.Totals.Cancelled)
	}
}

func TestUsageMetricsPartitionTheTerminalTrades(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}

	var terminal int
	row := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM trades WHERE state IN (?, ?, ?, ?)`,
		string(domain.TradeRecorded), string(domain.TradeSettled),
		string(domain.TradeDisputed), string(domain.TradeCancelled))
	if err := row.Scan(&terminal); err != nil {
		t.Fatal(err)
	}

	// recorded and settled are terminal and are the only states that issue a
	// receipt, so a delivered trade can never also be disputed or cancelled.
	got := m.Totals.Delivered + m.Totals.Disputed + m.Totals.Cancelled
	if got != terminal {
		t.Errorf("delivered+disputed+cancelled = %d, want %d terminal trades", got, terminal)
	}
}

func TestUsageMetricsAsOfIsNewestReceiptAndIgnoresStateChanges(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}
	want := usageBase.Add(3 * time.Minute)
	if m.AsOf == nil {
		t.Fatal("asOf is nil, want the newest receipt time")
	}
	if !m.AsOf.Equal(want) {
		t.Errorf("asOf = %v, want %v", m.AsOf, want)
	}

	// A later state transition bumps trades.updated_at and must not be mistaken
	// for a delivery.
	driveTradeFrom(t, s, "t6", domain.TradeNegotiating, domain.TradeAccepted, domain.TradeDisputed)
	after, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}
	if after.AsOf == nil || !after.AsOf.Equal(want) {
		t.Errorf("asOf = %v after a state change, want unchanged %v", after.AsOf, want)
	}
}

func TestUsageMetricsIgnoreASecondReceiptForOneTrade(t *testing.T) {
	s := testStore(t)
	seedUsage(t, s)

	before, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}

	if err := s.SaveReceipt(context.Background(), domain.Receipt{
		ID: "r-t1-again", TradeID: "t1", JWS: "jws.again",
		IssuedAt: usageBase.Add(4 * time.Minute),
	}); err == nil {
		t.Fatal("expected a second receipt for one trade to be rejected")
	}

	after, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics: %v", err)
	}
	if after.Totals != before.Totals {
		t.Errorf("totals = %+v after a rejected duplicate, want %+v", after.Totals, before.Totals)
	}
}

func TestUsageMetricsOnAnEmptyMarketplace(t *testing.T) {
	s := testStore(t)

	m, err := s.UsageMetrics(context.Background())
	if err != nil {
		t.Fatalf("usage metrics on empty store: %v", err)
	}
	if m.Totals != (domain.UsageTotals{}) {
		t.Errorf("totals = %+v, want all zero", m.Totals)
	}
	if m.AsOf != nil {
		t.Errorf("asOf = %v, want nil so the site can tell no delivery from an old one", m.AsOf)
	}
	if m.Agents == nil {
		t.Error("agents is nil, want an empty slice so it serialises as []")
	}
	if len(m.Agents) != 0 {
		t.Errorf("agents = %+v, want empty", m.Agents)
	}
}
