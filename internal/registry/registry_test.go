package registry_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/google/uuid"
)

const (
	aliceKey = "HofWGrRkGfQy5qnFXkV1MR1PbnixJ4xMPhsmywrpEqYd"
	bobKey   = "4u9MuUGc6GUZMKxguayJXPw8a54quV53DnPQzii2fA3P"
	usdc     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

func newService(t *testing.T) *registry.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return registry.New(db)
}

func card(name string) domain.AgentCard {
	return domain.AgentCard{
		Name:        name,
		Description: name + " trading agent",
		Version:     "0.1.0",
		URL:         "https://" + name + ".example.com",
		Skills:      []domain.AgentSkill{{ID: "summarize", Name: "Summarize", Tags: []string{"text"}}, {ID: "translate", Name: "Translate", Tags: []string{"text"}}},
	}
}

func TestRegisterIsIdempotentAndUpdatesCard(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	created, isNew, err := svc.Register(ctx, aliceKey, card("alice"))
	if err != nil || !isNew {
		t.Fatalf("Register = %v, %v, want created", isNew, err)
	}
	if created.Status != domain.AgentActive {
		t.Errorf("status = %s, want active", created.Status)
	}
	updated := card("alice-renamed")
	again, isNew, err := svc.Register(ctx, aliceKey, updated)
	if err != nil || isNew {
		t.Fatalf("second Register = %v, %v, want update", isNew, err)
	}
	if again.Card.Name != "alice-renamed" {
		t.Errorf("name = %s, want the re-registered card", again.Card.Name)
	}
	if again.CreatedAt != created.CreatedAt {
		t.Error("createdAt changed on update")
	}
}

func TestRegisterRejectsMismatchedPublicKey(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	bad := card("alice")
	bad.PublicKey = bobKey
	if _, _, err := svc.Register(ctx, aliceKey, bad); err == nil {
		t.Fatal("want error for publicKey that does not match the authenticated agent")
	}
}

func TestRegisterRejectsInvalidCard(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	bad := card("alice")
	bad.URL = "not-a-url"
	if _, _, err := svc.Register(ctx, aliceKey, bad); err == nil {
		t.Fatal("want error for invalid card url")
	}
}

func TestPublishOfferRequiresRegisteredActiveAgent(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	_, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "1.50",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PublishOffer err = %v, want not found for unknown agent", err)
	}
}

func TestPublishOfferRejectsPriceBeyondCardCurrencies(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	withCurrencies := card("alice")
	withCurrencies.Currencies = []string{usdc}
	if _, _, err := svc.Register(ctx, aliceKey, withCurrencies); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "forbidden currency",
		PriceAmount:     "1.00",
		PriceMint:       "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr",
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if !errors.Is(err, registry.ErrCurrencyNotAccepted) {
		t.Fatalf("err = %v, want ErrCurrencyNotAccepted", err)
	}
}

func TestPublishOfferRejectsTooPreciseOnchainAmount(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice")); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "sub-cent precision cannot settle on chain",
		PriceAmount:     "1.000000001",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOnchain},
	}, "")
	if !errors.Is(err, registry.ErrAmountTooPrecise) {
		t.Fatalf("err = %v, want ErrAmountTooPrecise", err)
	}
}

func TestPublishOfferIdempotency(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice")); err != nil {
		t.Fatal(err)
	}
	in := registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "translate text",
		Capabilities:    []string{"translate"},
		PriceAmount:     "2.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}
	first, isNew, err := svc.PublishOffer(ctx, aliceKey, in, "idem-1")
	if err != nil || !isNew {
		t.Fatalf("PublishOffer = %v, %v", isNew, err)
	}
	second, isNew, err := svc.PublishOffer(ctx, aliceKey, in, "idem-1")
	if err != nil || isNew {
		t.Fatalf("replay = %v, %v, want the same offer", isNew, err)
	}
	if first.ID != second.ID {
		t.Errorf("replay created %s, want %s", second.ID, first.ID)
	}
}

func TestSearchAndClose(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, id := range []string{aliceKey, bobKey} {
		if _, _, err := svc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	offer, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a long document",
		Capabilities:    []string{"summarize"},
		PriceAmount:     "3.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	found, err := svc.Search(ctx, domain.OfferQuery{Text: "document"})
	if err != nil || len(found) != 1 {
		t.Fatalf("Search = %d offers, %v", len(found), err)
	}
	if found[0].ID != offer.ID {
		t.Errorf("found %s, want %s", found[0].ID, offer.ID)
	}
	if _, err := svc.CloseOffer(ctx, bobKey, offer.ID); !errors.Is(err, registry.ErrNotOwner) {
		t.Fatalf("non-owner close err = %v, want ErrNotOwner", err)
	}
	closed, err := svc.CloseOffer(ctx, aliceKey, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != domain.OfferClosed {
		t.Errorf("status = %s, want closed", closed.Status)
	}
	after, err := svc.Search(ctx, domain.OfferQuery{Text: "document"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("closed offer still discoverable: %d results", len(after))
	}
}

func TestAnnouncementSourceSkipsClosedAndInactive(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, id := range []string{aliceKey, bobKey} {
		if _, _, err := svc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "3.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PublishOffer(ctx, bobKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "1.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, ""); err != nil {
		t.Fatal(err)
	}
	announcements, err := svc.AnnouncementSource().Announcements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(announcements) != 2 {
		t.Fatalf("got %d announcements, want 2", len(announcements))
	}
	for _, a := range announcements {
		if a.Policy[agp.PolicyCurrencies] == nil || (a.CostAmount != "3.00" && a.CostAmount != "1.00") {
			t.Errorf("announcement %s missing truthful policy: %+v", a.AnnouncementID, a.Policy)
		}
	}
	if _, err := svc.CloseOffer(ctx, bobKey, ""); err == nil {
		t.Fatal("closing an empty offer id should fail")
	}
	offers, err := svc.Search(ctx, domain.OfferQuery{AgentID: bobKey})
	if err != nil || len(offers) != 1 {
		t.Fatalf("search bob = %d, %v", len(offers), err)
	}
	if _, err := svc.CloseOffer(ctx, bobKey, offers[0].ID); err != nil {
		t.Fatal(err)
	}
	announcements, err = svc.AnnouncementSource().Announcements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(announcements) != 1 {
		t.Errorf("got %d announcements after closing one offer, want 1", len(announcements))
	}
}

func TestAnnouncementSourceRoutesToCheapestAgent(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, id := range []string{aliceKey, bobKey} {
		if _, _, err := svc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	for _, spec := range []struct {
		agent  string
		amount string
	}{{aliceKey, "9.00"}, {bobKey, "2.50"}} {
		if _, _, err := svc.PublishOffer(ctx, spec.agent, registry.NewOffer{
			Direction:       domain.DirectionAsk,
			Description:     "summarize a document",
			Capabilities:    []string{"summarize:document"},
			PriceAmount:     spec.amount,
			PriceMint:       usdc,
			SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		}, ""); err != nil {
			t.Fatal(err)
		}
	}
	table, err := agp.NewRouting().BuildTable(ctx, svc.AnnouncementSource())
	if err != nil {
		t.Fatal(err)
	}
	result, err := table.Route(agp.Intent{TargetCapability: "summarize:document", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.AgentID != bobKey {
		t.Errorf("routed to %s, want the cheaper agent %s", result.Route.AgentID, bobKey)
	}
}

func TestAnnouncementCountStaysBounded(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card(aliceKey)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
			Direction:       domain.DirectionAsk,
			Description:     "summarize " + uuid.NewString(),
			PriceAmount:     "1.00",
			PriceMint:       usdc,
			SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		}, ""); err != nil {
			t.Fatal(err)
		}
	}
	announcements, err := svc.AnnouncementSource().Announcements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(announcements) > agp.MaxRoutes {
		t.Errorf("got %d announcements, above the %d table cap", len(announcements), agp.MaxRoutes)
	}
}

func TestAgentReturnsRegisteredAgentOrNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice")); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Agent(ctx, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != aliceKey || got.Card.Name != "alice" {
		t.Errorf("Agent = %+v, want the registered alice agent", got)
	}
	if _, err := svc.Agent(ctx, bobKey); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Agent(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestAgentsFiltersByStatusAndDefaultsToActive(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := registry.New(db)
	for _, id := range []string{aliceKey, bobKey} {
		if _, _, err := svc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetAgentStatus(ctx, bobKey, domain.AgentSuspended, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	active, err := svc.Agents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != aliceKey {
		t.Fatalf("Agents(\"\") = %v, want only alice", active)
	}
	suspended, err := svc.Agents(ctx, domain.AgentSuspended)
	if err != nil {
		t.Fatal(err)
	}
	if len(suspended) != 1 || suspended[0].ID != bobKey {
		t.Fatalf("Agents(suspended) = %v, want only bob", suspended)
	}
}

func TestOfferReturnsPublishedOfferOrNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice")); err != nil {
		t.Fatal(err)
	}
	published, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "1.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Offer(ctx, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != published.ID {
		t.Errorf("Offer = %s, want %s", got.ID, published.ID)
	}
	if _, err := svc.Offer(ctx, uuid.NewString()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Offer(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestWithMintsGovernsOnchainPrecisionCheck(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const testMint = "11111111111111111111111111111111"
	mints, err := tokens.New([]tokens.Token{{Address: testMint, Symbol: "TEST", Decimals: 2, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	svc := registry.New(db, registry.WithMints(mints))
	withCurrencies := card("alice")
	withCurrencies.Currencies = []string{testMint}
	if _, _, err := svc.Register(ctx, aliceKey, withCurrencies); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "more precision than the governed mint supports",
		PriceAmount:     "1.005",
		PriceMint:       testMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOnchain},
	}, "")
	if !errors.Is(err, registry.ErrAmountTooPrecise) {
		t.Fatalf("PublishOffer err = %v, want ErrAmountTooPrecise for the 2-decimal test mint", err)
	}
}
