package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "vtessera.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testAgent(id string) domain.Agent {
	return domain.Agent{
		ID:     id,
		Card:   domain.AgentCard{Name: "agent " + id, PublicKey: id},
		Status: domain.AgentActive,
	}
}

const validMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

func testOffer(id, agentID string) domain.Offer {
	return domain.Offer{
		ID:              id,
		AgentID:         agentID,
		Direction:       domain.DirectionAsk,
		Description:     "sentiment dataset",
		Capabilities:    []string{"nlp", "datasets"},
		PriceAmount:     money.MustParse("25"),
		PriceMint:       validMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		Status:          domain.OfferOpen,
		CreatedAt:       time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestAgentsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAgent("J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh")
	a.Card = domain.AgentCard{
		Name:            "alice",
		Description:     "research agent",
		PublicKey:       a.ID,
		Capabilities:    []string{"nlp"},
		Currencies:      []string{validMint},
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		Skills:          []domain.AgentSkill{{ID: "summarize", Name: "Summarize"}},
	}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Card.Name != "alice" || got.Status != domain.AgentActive {
		t.Errorf("got = %+v, want name alice active", got)
	}
	if len(got.Card.Currencies) != 1 || got.Card.Currencies[0] != validMint {
		t.Errorf("currencies = %v, want [%s]", got.Card.Currencies, validMint)
	}
	if len(got.Card.Skills) != 1 || got.Card.Skills[0].ID != "summarize" {
		t.Errorf("skills = %+v, want one summarize skill", got.Card.Skills)
	}
	if _, err := s.GetAgent(ctx, "missing"); err != ErrNotFound {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
}

func TestSearchOffersFilters(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"alice", "bob", "carol"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	alice := testOffer("o1", "alice")
	if err := s.CreateOffer(ctx, alice, ""); err != nil {
		t.Fatal(err)
	}
	bob := testOffer("o2", "bob")
	bob.Description = "translation service (100% offline)"
	bob.Capabilities = []string{"translation"}
	bob.PriceMint = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc"
	bob.SettlementModes = []domain.SettlementMode{domain.SettlementOffchain, domain.SettlementOnchain}
	if err := s.CreateOffer(ctx, bob, ""); err != nil {
		t.Fatal(err)
	}
	carol := testOffer("o3", "carol")
	carol.Direction = domain.DirectionBid
	carol.Status = domain.OfferClosed
	if err := s.CreateOffer(ctx, carol, ""); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query domain.OfferQuery
		want  []string
	}{
		{"open asks", domain.OfferQuery{Status: domain.OfferOpen, Direction: domain.DirectionAsk}, []string{"o1", "o2"}},
		{"capability nlp", domain.OfferQuery{Status: domain.OfferOpen, Capability: "nlp"}, []string{"o1"}},
		{"capability translation", domain.OfferQuery{Status: domain.OfferOpen, Capability: "translation"}, []string{"o2"}},
		{"mint eurc", domain.OfferQuery{Status: domain.OfferOpen, Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc"}, []string{"o2"}},
		{"onchain mode", domain.OfferQuery{Status: domain.OfferOpen, Mode: domain.SettlementOnchain}, []string{"o2"}},
		{"offchain mode", domain.OfferQuery{Status: domain.OfferOpen, Mode: domain.SettlementOffchain}, []string{"o1", "o2"}},
		{"text search", domain.OfferQuery{Status: domain.OfferOpen, Text: "offline"}, []string{"o2"}},
		{"exclude self", domain.OfferQuery{Status: domain.OfferOpen, ExcludeAgent: "bob"}, []string{"o1"}},
		{"by agent", domain.OfferQuery{Status: domain.OfferClosed, AgentID: "carol"}, []string{"o3"}},
		{"defaults to open", domain.OfferQuery{Text: "offline"}, []string{"o2"}},
		{"all statuses explicitly", domain.OfferQuery{Status: domain.OfferStatus("")}, []string{"o1", "o2"}},
		{"closed only", domain.OfferQuery{Status: domain.OfferClosed}, []string{"o3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.SearchOffers(ctx, c.query)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d offers %v, want %d", len(got), ids(got), len(c.want))
			}
			for i, id := range c.want {
				if got[i].ID != id {
					t.Errorf("offer[%d] = %s, want %s", i, got[i].ID, id)
				}
			}
		})
	}
}

func ids(offers []domain.Offer) []string {
	out := []string{}
	for _, o := range offers {
		out = append(out, o.ID)
	}
	return out
}

func TestSearchOffersEscapesLike(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	o := testOffer("o1", "alice")
	o.Description = "100% coverage"
	if err := s.CreateOffer(ctx, o, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchOffers(ctx, domain.OfferQuery{Status: domain.OfferOpen, Text: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("literal %% search got %v, want the offer with 100%%", ids(got))
	}
	got, err = s.SearchOffers(ctx, domain.OfferQuery{Status: domain.OfferOpen, Text: "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("no match expected, got %v", ids(got))
	}
}

func TestOfferIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOffer(ctx, testOffer("o1", "alice"), "key-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOffer(ctx, testOffer("o2", "alice"), "key-1"); err == nil {
		t.Fatal("expected conflict on duplicate idempotency key")
	}
	back, err := s.GetOfferByIdempotencyKey(ctx, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != "o1" {
		t.Errorf("idempotent fetch = %s, want o1", back.ID)
	}
}

func TestTradeLifecycleStorage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"alice", "bob"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateOffer(ctx, testOffer("o1", "alice"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tr := domain.Trade{
		ID:             "t1",
		OfferID:        "o1",
		BuyerAgentID:   "bob",
		SellerAgentID:  "alice",
		Description:    "sentiment dataset",
		Amount:         money.MustParse("25"),
		Mint:           validMint,
		SettlementMode: domain.SettlementOffchain,
		State:          domain.TradeProposed,
		CreatedAt:      now,
	}
	if err := s.CreateTrade(ctx, tr, "trade-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTrade(ctx, tr, "trade-key"); err == nil {
		t.Fatal("expected conflict on duplicate trade idempotency key")
	}

	inserted, err := s.AddTradeAcceptance(ctx, "t1", "bob", now)
	if err != nil || !inserted {
		t.Fatalf("first acceptance inserted=%v err=%v", inserted, err)
	}
	inserted, err = s.AddTradeAcceptance(ctx, "t1", "bob", now)
	if err != nil || inserted {
		t.Fatalf("repeat acceptance inserted=%v err=%v, want false", inserted, err)
	}
	if _, err := s.AddTradeAcceptance(ctx, "t1", "alice", now); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTrade(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Acceptances) != 2 {
		t.Errorf("acceptances = %v, want 2", got.Acceptances)
	}
	if got.Amount.String() != "25" {
		t.Errorf("amount = %s, want 25", got.Amount)
	}

	changed, err := s.SetTradeState(ctx, "t1", domain.TradeProposed, domain.TradeNegotiating, now)
	if err != nil || !changed {
		t.Fatalf("proposed->negotiating changed=%v err=%v", changed, err)
	}
	changed, err = s.SetTradeState(ctx, "t1", domain.TradeProposed, domain.TradeAccepted, now)
	if err != nil || changed {
		t.Fatalf("stale transition changed=%v err=%v, want false", changed, err)
	}
	if _, err := s.AppendTradeEvent(ctx, domain.TradeEvent{
		TradeID: "t1", ActorAgentID: "bob", Type: domain.EventNegotiating,
		FromState: domain.TradeProposed, ToState: domain.TradeNegotiating, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListTradeEvents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.EventNegotiating {
		t.Errorf("events = %+v, want one negotiating event", events)
	}
}

func TestLedgerChainAndReceipt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	hashOf := func(seq int64, prev, payloadHash string) string {
		return "h-" + prev + "-" + payloadHash
	}
	if _, found, err := s.HeadLedgerEntry(ctx); err != nil || found {
		t.Fatalf("empty head found=%v err=%v", found, err)
	}
	first, err := s.AppendLedgerEntry(ctx, domain.LedgerAppend{
		TradeID: "t1", Payload: []byte(`{"a":1}`), PayloadHash: "p1", PrevHash: domain.GenesisHash, CreatedAt: time.Now().UTC(),
	}, hashOf)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || first.PrevHash != domain.GenesisHash {
		t.Errorf("first entry = %+v, want seq 1 prev genesis", first)
	}
	if _, err := s.AppendLedgerEntry(ctx, domain.LedgerAppend{
		TradeID: "t2", Payload: []byte(`{"a":2}`), PayloadHash: "p2", PrevHash: domain.GenesisHash, CreatedAt: time.Now().UTC(),
	}, hashOf); err != ErrStale {
		t.Errorf("stale prev hash = %v, want ErrStale", err)
	}
	second, err := s.AppendLedgerEntry(ctx, domain.LedgerAppend{
		TradeID: "t2", Payload: []byte(`{"a":2}`), PayloadHash: "p2", PrevHash: first.Hash, CreatedAt: time.Now().UTC(),
	}, hashOf)
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != 2 || second.PrevHash != first.Hash {
		t.Errorf("second entry = %+v, want seq 2 chained", second)
	}
	if _, err := s.AppendLedgerEntry(ctx, domain.LedgerAppend{
		TradeID: "t2", Payload: []byte(`{}`), PayloadHash: "p3", PrevHash: second.Hash, CreatedAt: time.Now().UTC(),
	}, hashOf); err == nil {
		t.Error("expected conflict on duplicate trade in ledger")
	}
	entries, err := s.ListLedgerEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2", len(entries))
	}

	receipt := domain.Receipt{ID: "r1", TradeID: "t1", JWS: "a.b.c", IssuedAt: time.Now().UTC()}
	if err := s.SaveReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReceiptByTrade(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.JWS != receipt.JWS {
		t.Errorf("receipt = %q, want %q", got.JWS, receipt.JWS)
	}
	if _, err := s.GetReceiptByTrade(ctx, "nope"); err != ErrNotFound {
		t.Errorf("missing receipt = %v, want ErrNotFound", err)
	}
}

func TestChallengeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()
	c := domain.Challenge{ID: "c1", AgentID: "alice", Nonce: "bm9uY2U=", ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeChallenge(ctx, "c1", now); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := s.ConsumeChallenge(ctx, "c1", now); err != ErrStale {
		t.Errorf("second consume = %v, want ErrStale", err)
	}
	expired := domain.Challenge{ID: "c2", AgentID: "alice", Nonce: "bm9uY2U=", ExpiresAt: now.Add(-time.Minute)}
	if err := s.CreateChallenge(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeChallenge(ctx, "c2", now); err != ErrStale {
		t.Errorf("expired consume = %v, want ErrStale", err)
	}
	if _, err := s.ConsumeChallenge(ctx, "missing", now); err != ErrNotFound {
		t.Errorf("missing consume = %v, want ErrNotFound", err)
	}
}

func TestUpdateAndListAgents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAgent("alice")
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := testAgent("bob")
	if err := s.CreateAgent(ctx, b); err != nil {
		t.Fatal(err)
	}

	a.Card.Name = "alice v2"
	a.UpdatedAt = time.Now().UTC()
	if err := s.UpdateAgent(ctx, a); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Card.Name != "alice v2" {
		t.Errorf("name = %q, want alice v2", got.Card.Name)
	}

	missing := testAgent("missing")
	if err := s.UpdateAgent(ctx, missing); err != ErrNotFound {
		t.Errorf("update missing agent = %v, want ErrNotFound", err)
	}

	if err := s.SetAgentStatus(ctx, "alice", domain.AgentSuspended, time.Now().UTC()); err != nil {
		t.Fatalf("set agent status: %v", err)
	}
	if err := s.SetAgentStatus(ctx, "missing", domain.AgentSuspended, time.Now().UTC()); err != ErrNotFound {
		t.Errorf("set status on missing agent = %v, want ErrNotFound", err)
	}

	active, err := s.ListAgents(ctx, domain.AgentActive)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "bob" {
		t.Errorf("active agents = %v, want [bob]", active)
	}
	suspended, err := s.ListAgents(ctx, domain.AgentSuspended)
	if err != nil {
		t.Fatal(err)
	}
	if len(suspended) != 1 || suspended[0].ID != "alice" {
		t.Errorf("suspended agents = %v, want [alice]", suspended)
	}
}

func TestListAgentsByIDs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(ctx, testAgent("bob")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListAgentsByIDs(ctx, []string{"alice", "missing", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2: %v", len(got), got)
	}
	if got["alice"].ID != "alice" || got["bob"].ID != "bob" {
		t.Errorf("got = %v, want alice and bob", got)
	}
	if _, ok := got["missing"]; ok {
		t.Errorf("got unexpected entry for missing agent")
	}

	empty, err := s.ListAgentsByIDs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("empty ids = %v, want empty map", empty)
	}
}

func TestOfferLookupsAndStatus(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	o := testOffer("o1", "alice")
	if err := s.CreateOffer(ctx, o, ""); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetOffer(ctx, "o1")
	if err != nil {
		t.Fatalf("get offer: %v", err)
	}
	if got.Description != o.Description {
		t.Errorf("description = %q, want %q", got.Description, o.Description)
	}
	if _, err := s.GetOffer(ctx, "missing"); err != ErrNotFound {
		t.Errorf("get missing offer = %v, want ErrNotFound", err)
	}

	open, err := s.ListOpenOffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "o1" {
		t.Errorf("open offers = %v, want [o1]", ids(open))
	}

	if err := s.SetOfferStatus(ctx, "o1", domain.OfferClosed, time.Now().UTC()); err != nil {
		t.Fatalf("set offer status: %v", err)
	}
	if err := s.SetOfferStatus(ctx, "missing", domain.OfferClosed, time.Now().UTC()); err != ErrNotFound {
		t.Errorf("set status on missing offer = %v, want ErrNotFound", err)
	}
	open, err = s.ListOpenOffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("open offers after close = %v, want none", ids(open))
	}
}

func TestGetTradeByIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"alice", "bob"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateOffer(ctx, testOffer("o1", "alice"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tr := domain.Trade{
		ID:             "t1",
		OfferID:        "o1",
		BuyerAgentID:   "bob",
		SellerAgentID:  "alice",
		Description:    "sentiment dataset",
		Amount:         money.MustParse("25"),
		Mint:           validMint,
		SettlementMode: domain.SettlementOffchain,
		State:          domain.TradeProposed,
		CreatedAt:      now,
	}
	if err := s.CreateTrade(ctx, tr, "trade-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTradeAcceptance(ctx, "t1", "bob", now); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTradeByIdempotencyKey(ctx, "trade-key")
	if err != nil {
		t.Fatalf("get by idempotency key: %v", err)
	}
	if got.ID != "t1" {
		t.Errorf("id = %s, want t1", got.ID)
	}
	if len(got.Acceptances) != 1 || got.Acceptances[0] != "bob" {
		t.Errorf("acceptances = %v, want [bob]", got.Acceptances)
	}
	if _, err := s.GetTradeByIdempotencyKey(ctx, "missing-key"); err != ErrNotFound {
		t.Errorf("missing key = %v, want ErrNotFound", err)
	}
}

func TestPing(t *testing.T) {
	s := testStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}
}
