package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
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

// attestSigner is a throwaway Ed25519 key standing in for an agent or for the
// marketplace. The tests that use it are about what was stored, not about who
// signed, so the key does not need to persist anywhere.
func attestSigner(t *testing.T) *attest.SigningKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := attest.NewSigningKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// seedAttestAgent registers an agent whose ID is the signer's own public key.
//
// The ID has to be the key rather than an arbitrary string because the card
// statement names an agent and the verifier requires that the key which signed it
// be that agent. A store that accepted a mismatch would let one agent sign a
// card claiming to be another.
func seedAttestAgent(t *testing.T, s *Store, key *attest.SigningKey) domain.Agent {
	t.Helper()
	id := key.PublicKeyBase58()
	agent := testAgent(id)
	agent.Card = domain.AgentCard{
		Name: "alice", URL: "https://trader.example.com", PublicKey: id,
		Currencies: []string{validMint},
	}
	if err := s.CreateAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

// attestCard is the statement the marketplace and the agent both sign, built the
// way registry.Register builds it so a signature written by one is verifiable
// through the other.
func attestCard(agent domain.Agent) attest.Card {
	skills := make([]attest.Skill, 0, len(agent.Card.Skills))
	for _, sk := range agent.Card.Skills {
		skills = append(skills, attest.Skill{
			ID: sk.ID, Name: sk.Name, Tags: sk.Tags, Input: sk.Input, Output: sk.Output,
		})
	}
	modes := make([]string, 0, len(agent.Card.SettlementModes))
	for _, m := range agent.Card.SettlementModes {
		modes = append(modes, string(m))
	}
	return attest.Card{
		AgentID:         agent.ID,
		Name:            agent.Card.Name,
		Description:     agent.Card.Description,
		Version:         agent.Card.Version,
		URL:             agent.Card.URL,
		Capabilities:    agent.Card.Capabilities,
		Skills:          skills,
		Currencies:      agent.Card.Currencies,
		SettlementModes: modes,
	}
}

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
	bob.PriceMint = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"
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
		{"mint eurc", domain.OfferQuery{Status: domain.OfferOpen, Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"}, []string{"o2"}},
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

func TestTradeAcceptanceReportsTheEarliestAcceptanceOrNone(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	tr := domain.Trade{
		ID: "t1", OfferID: "o1", BuyerAgentID: "bob", SellerAgentID: "alice",
		Description: "sentiment dataset", Amount: money.MustParse("25"), Mint: validMint,
		SettlementMode: domain.SettlementOffchain, State: domain.TradeProposed, CreatedAt: now,
	}
	if err := s.CreateTrade(ctx, tr, "trade-key"); err != nil {
		t.Fatal(err)
	}

	if _, found, err := s.TradeAcceptance(ctx, "t1"); err != nil || found {
		t.Fatalf("TradeAcceptance before any acceptance: found=%v err=%v, want false, nil", found, err)
	}

	later := now.Add(time.Hour)
	if _, err := s.AddTradeAcceptance(ctx, "t1", "bob", later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTradeAcceptance(ctx, "t1", "alice", now); err != nil {
		t.Fatal(err)
	}

	at, found, err := s.TradeAcceptance(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("TradeAcceptance after two acceptances: found=%v err=%v, want true, nil", found, err)
	}
	if !at.Equal(now) {
		t.Errorf("TradeAcceptance = %v, want the earliest acceptance %v", at, now)
	}
}

func TestAcceptedBeforeFiltersByStateDeadlineAndOrdersOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []string{"alice", "bob"} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	deadline := now.Add(time.Hour)

	// t1: accepted well before the deadline, moved to accepted first so it is the
	// oldest by updated_at.
	// t2: accepted exactly at the deadline, which must still count ("<=").
	// t3: accepted after the deadline, which must not be returned.
	// t4: never moved out of proposed, which must not be returned even though it
	// has an acceptance row.
	cases := []struct {
		id, offer    string
		acceptedAt   time.Time
		moveAccepted bool
	}{
		{"t1", "o1", now.Add(-time.Hour), true},
		{"t2", "o2", deadline, true},
		{"t3", "o3", deadline.Add(time.Hour), true},
		{"t4", "o4", now.Add(-time.Hour), false},
	}
	for i, c := range cases {
		if err := s.CreateOffer(ctx, testOffer(c.offer, "alice"), ""); err != nil {
			t.Fatal(err)
		}
		tr := domain.Trade{
			ID: c.id, OfferID: c.offer, BuyerAgentID: "bob", SellerAgentID: "alice",
			Description: "sentiment dataset", Amount: money.MustParse("25"), Mint: validMint,
			SettlementMode: domain.SettlementOffchain, State: domain.TradeProposed,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateTrade(ctx, tr, "trade-key-"+c.id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddTradeAcceptance(ctx, c.id, "bob", c.acceptedAt); err != nil {
			t.Fatal(err)
		}
		if c.moveAccepted {
			// Stagger updated_at so t1 is unambiguously the oldest row.
			moveAt := now.Add(time.Duration(i) * time.Minute)
			if _, err := s.SetTradeState(ctx, c.id, domain.TradeProposed, domain.TradeAccepted, moveAt); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := s.AcceptedBefore(ctx, deadline, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("AcceptedBefore returned %d trades, want 2 (t1, t2): %+v", len(got), got)
	}
	if got[0].ID != "t1" || got[1].ID != "t2" {
		t.Errorf("AcceptedBefore = [%s, %s], want [t1, t2] oldest first", got[0].ID, got[1].ID)
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

// The card and its signatures are written together, so a reader must never be
// able to observe one without the others. The migration's own reason for
// existing.
func TestACardAndItsAttestationsAreStoredTogether(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	key := attestSigner(t)
	market := attestSigner(t)

	agent := seedAttestAgent(t, s, key)
	sig, err := key.SignCard(attestCard(agent), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	marketSig, err := market.AttestCard(attestCard(agent), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAgentCard(ctx, agent, &sig, marketSig, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.CardAttestation(ctx, agent.ID)
	if err != nil || !found {
		t.Fatalf("CardAttestation = %v, %v; want a record", found, err)
	}
	if got.Agent == nil {
		t.Fatal("the agent signature is missing from a card that was signed")
	}
	if got.Market.Value == "" {
		t.Error("the marketplace signature is missing from a card this marketplace published")
	}
}

// A card the agent did not sign still has a marketplace attestation, and the two
// answers stay distinguishable. Collapsing them would tell a directory that a
// capability list is unvouched-for when the marketplace did in fact publish it.
func TestAnUnsignedCardKeepsItsMarketplaceAttestation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	market := attestSigner(t)

	agent := seedAttestAgent(t, s, market)
	marketSig, err := market.AttestCard(attestCard(agent), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAgentCard(ctx, agent, nil, marketSig, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.CardAttestation(ctx, agent.ID)
	if err != nil || !found {
		t.Fatalf("CardAttestation = %v, %v; want a record", found, err)
	}
	if got.Agent != nil {
		t.Error("an unsigned card reports an agent signature")
	}
	if got.Market.Value == "" {
		t.Error("the marketplace attestation was dropped along with the agent's")
	}
}

// Replacing a signed card with an unsigned one clears the agent's signature in
// the same write. A stale one would be a signature over content no longer stored,
// and a verifier checking it would be checking a card that does not exist.
func TestAnUnsignedReplacementClearsTheAgentSignatureInOneWrite(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	key := attestSigner(t)
	market := attestSigner(t)

	agent := seedAttestAgent(t, s, key)
	now := time.Now().UTC()
	sig, err := key.SignCard(attestCard(agent), now)
	if err != nil {
		t.Fatal(err)
	}
	marketSig, err := market.AttestCard(attestCard(agent), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAgentCard(ctx, agent, &sig, marketSig, now); err != nil {
		t.Fatal(err)
	}

	renamed := domain.AgentCard{
		Name: "renamed", URL: "https://trader.example.com", PublicKey: agent.ID,
		Currencies: []string{validMint},
	}
	agent.Card = renamed
	statement := attest.Card{
		AgentID: agent.ID, Name: renamed.Name, URL: renamed.URL,
		Currencies: []string{validMint},
	}
	newMarket, err := market.AttestCard(statement, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAgentCard(ctx, agent, nil, newMarket, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.CardAttestation(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != nil {
		t.Error("the previous agent signature survived an unsigned replacement")
	}
	// The marketplace re-signed, so its signature describes the stored card.
	if err := attest.VerifyCardAttestedBy(statement, got.Market, market.PublicKeyBase58()); err != nil {
		t.Errorf("the marketplace signature does not describe the card that replaced the one it signed: %v", err)
	}
}

// An offer and its seller's signature are one write, so an offer cannot exist
// unsigned when its signature was sent, or signed when it does not.
func TestAnOfferAndItsSignatureAreStoredTogether(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seller := attestSigner(t)

	agent := seedAttestAgent(t, s, seller)
	now := time.Now().UTC()
	offer := domain.Offer{
		ID:              "offer-1",
		AgentID:         agent.ID,
		Direction:       domain.DirectionAsk,
		Description:     "summarize",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     money.MustParse("1.00"),
		PriceMint:       validMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		Status:          domain.OfferOpen,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	statement := attest.Offer{
		ID: offer.ID, Seller: agent.ID, Description: offer.Description,
		Direction: "ask", Capabilities: offer.Capabilities, Mint: validMint,
		Scale: "6", AmountBaseUnits: "1000000", UnitAmount: "1.00",
		SettlementModes: []string{"offchain"},
	}
	sig, err := seller.SignOffer(statement, offer.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOfferWithAttestation(ctx, offer, "", &sig); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetOffer(ctx, offer.ID); err != nil {
		t.Fatalf("the offer was not stored alongside its signature: %v", err)
	}
	got, found, err := s.OfferAttestation(ctx, offer.ID)
	if err != nil || !found {
		t.Fatalf("OfferAttestation = %v, %v; want a signature", found, err)
	}
	if err := attest.VerifyOffer(statement, got); err != nil {
		t.Errorf("the stored signature does not describe the offer: %v", err)
	}
}

// A card from before attestations existed has no row at all, which is different
// from a row saying the card is unsigned. A reader has to be able to tell.
func TestACardFromBeforeAttestationsHasNoRecordRatherThanAnEmptyOne(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	agent := seedAttestAgent(t, s, attestSigner(t))

	got, found, err := s.CardAttestation(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("an agent registered before attestations reports an attestation record")
	}
	if got.Market.Value != "" || got.Agent != nil {
		t.Error("an absent record returned signature values")
	}
}

// One agent signing a card that names another is the whole reason the statement
// carries the agent separately from the key that signed it. Without the check a
// verifiable card would prove nothing about who published it.
func TestACardSignedByAnotherKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	agentKey := attestSigner(t)
	impostor := attestSigner(t)
	agent := seedAttestAgent(t, s, agentKey)

	// Signing the raw canonical bytes is what an attacker would do. The
	// convenience method refuses a card naming somebody else, but nothing stops a
	// caller building the signature by hand, so the check that matters is the one
	// at verification.
	at := time.Now().UTC()
	sig, err := impostor.Sign(attest.CardBytes(attestCard(agent), at), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCard(attestCard(agent), sig); err == nil {
		t.Fatal("a signature over a card naming another agent verifies; the test would not be testing anything")
	}
	marketSig, err := agentKey.AttestCard(attestCard(agent), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAgentCard(ctx, agent, &sig, marketSig, at); err == nil {
		t.Error("a card signed by a key other than the agent it names was stored")
	}
}

// The mirror of the card rule for offers: a signature from a key that is not the
// seller is not a seller attestation, and storing it would create a row that
// claims a signature the seller never made.
func TestAnOfferSignedByAnotherKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seller := attestSigner(t)
	impostor := attestSigner(t)
	agent := seedAttestAgent(t, s, seller)

	now := time.Now().UTC()
	offer := domain.Offer{
		ID:              "offer-1",
		AgentID:         agent.ID,
		Direction:       domain.DirectionAsk,
		Description:     "summarize",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     money.MustParse("1.00"),
		PriceMint:       validMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		Status:          domain.OfferOpen,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	statement := attest.Offer{
		ID: offer.ID, Seller: agent.ID, Description: offer.Description,
		Direction: "ask", Capabilities: offer.Capabilities, Mint: validMint,
		Scale: "6", AmountBaseUnits: "1000000", UnitAmount: "1.00",
		SettlementModes: []string{"offchain"},
	}
	sig, err := impostor.Sign(attest.OfferBytes(statement, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOfferWithAttestation(ctx, offer, "", &sig); err == nil {
		t.Error("an offer was stored with a signature from a key that is not the seller")
	}
	if _, err := s.GetOffer(ctx, offer.ID); err == nil {
		t.Error("the refused offer was stored anyway")
	}
}

// The refusal has to leave nothing behind. A signature rejected on its way in
// must not leave an offer the marketplace will later list as signed.
func TestARefusedOfferAttestationWritesNoOffer(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seller := attestSigner(t)
	impostor := attestSigner(t)
	agent := seedAttestAgent(t, s, seller)

	now := time.Now().UTC()
	offer := domain.Offer{
		ID: "offer-1", AgentID: agent.ID, Direction: domain.DirectionAsk,
		Description: "summarize", Capabilities: []string{"summarize:document"},
		PriceAmount: money.MustParse("1.00"), PriceMint: validMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		Status:          domain.OfferOpen, CreatedAt: now, UpdatedAt: now,
	}
	statement := attest.Offer{
		ID: offer.ID, Seller: agent.ID, Direction: "ask", Mint: validMint,
	}
	sig, err := impostor.Sign(attest.OfferBytes(statement, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOfferWithAttestation(ctx, offer, "", &sig); err == nil {
		t.Fatal("the mismatched signature was accepted, so nothing was refused")
	}
	if _, found, err := s.OfferAttestation(ctx, offer.ID); err != nil || found {
		t.Errorf("OfferAttestation = %v, %v; want no signature for an offer that does not exist", found, err)
	}
}

// The live-trade refusal is the store's own, made inside the transaction that
// writes the retirement. A caller that checked first would leave a window in which
// a trade accepted after the check strands a buyer with a withdrawn seller, so the
// test calls RetireAgent directly with nothing in front of it.
func TestRetirementIsRefusedByTheStoreItselfWhileATradeIsLive(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(ctx, testAgent("bob")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOffer(ctx, testOffer("o1", "alice"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	trade := domain.Trade{
		ID: "t-live", OfferID: "o1", BuyerAgentID: "bob", SellerAgentID: "alice",
		Description: "summarize a document", Amount: money.MustParse("10"), Mint: validMint,
		SettlementMode: domain.SettlementOffchain, State: domain.TradeAccepted, CreatedAt: now,
	}
	if err := s.CreateTrade(ctx, trade, "live-key"); err != nil {
		t.Fatal(err)
	}

	_, err := s.RetireAgent(ctx, "alice", "withdrawn", "ops", now)
	var refusal *LiveTradesError
	if !errors.As(err, &refusal) {
		t.Fatalf("RetireAgent = %v, want a live-trade refusal", err)
	}
	if len(refusal.TradeIDs) != 1 || refusal.TradeIDs[0] != trade.ID {
		t.Errorf("refusal names %v, want the blocking trade %s", refusal.TradeIDs, trade.ID)
	}
	// Nothing was written: a refused retirement must not have closed the listing
	// it was told not to close.
	agent, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != domain.AgentActive {
		t.Errorf("status = %s, want the agent left active by a refused retirement", agent.Status)
	}
	if _, found, err := s.Retirement(ctx, "alice"); err != nil || found {
		t.Errorf("a refused retirement left an audit record: found=%v err=%v", found, err)
	}
}

// A trade that has finished does not block a withdrawal. Recorded, settled,
// disputed and cancelled are all finished, and refusing on those would make an
// agent impossible to withdraw.
func TestAFinishedTradeDoesNotBlockARetirement(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(ctx, testAgent("bob")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOffer(ctx, testOffer("o1", "alice"), ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, state := range []domain.TradeState{
		domain.TradeRecorded, domain.TradeSettled, domain.TradeDisputed, domain.TradeCancelled,
	} {
		trade := domain.Trade{
			ID: "t-" + string(state), OfferID: "o1", BuyerAgentID: "bob", SellerAgentID: "alice",
			Description: "summarize a document", Amount: money.MustParse("10"), Mint: validMint,
			SettlementMode: domain.SettlementOffchain, State: state, CreatedAt: now,
		}
		if err := s.CreateTrade(ctx, trade, "key-"+string(state)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RetireAgent(ctx, "alice", "withdrawn", "ops", now); err != nil {
		t.Fatalf("a finished trade blocked the retirement: %v", err)
	}
}

// Restoring records who reversed the withdrawal, separately from who made it.
func TestRestoringRecordsTheOperatorWhoReversedTheWithdrawal(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.CreateAgent(ctx, testAgent("alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.RetireAgent(ctx, "alice", "withdrawn", "ops-a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreAgent(ctx, "alice", "ops-b", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	record, found, err := s.Retirement(ctx, "alice")
	if err != nil || !found {
		t.Fatalf("record found=%v err=%v", found, err)
	}
	if record.Actor != "ops-a" {
		t.Errorf("actor = %q, want ops-a", record.Actor)
	}
	if record.RestoredBy != "ops-b" {
		t.Errorf("restoredBy = %q, want ops-b", record.RestoredBy)
	}
	if record.RestoredAt == nil {
		t.Error("restoredAt is not set although a restore happened")
	}
}
