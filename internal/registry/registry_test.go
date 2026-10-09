package registry_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/probe"
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
	return registry.New(db, mainnetMints(t), marketSigner(t))
}

// marketSigner is the marketplace's own key. Every card this registry publishes
// is signed with it, so a test that reads a card's provenance has something to
// verify against.
func marketSigner(t *testing.T) *attest.SigningKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := attest.NewSigningKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// mainnetMints is the governed set these tests run against. It is built from
// the production table rather than a fixture, because the registry's job is to
// enforce that table: a fixture would let the tests pass while the real
// governed set said something different.
func mainnetMints(t *testing.T) tokens.Registry {
	t.Helper()
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatalf("governed mints: %v", err)
	}
	return mints
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

	created, isNew, err := svc.Register(ctx, aliceKey, card("alice"), nil)
	if err != nil || !isNew {
		t.Fatalf("Register = %v, %v, want created", isNew, err)
	}
	if created.Status != domain.AgentActive {
		t.Errorf("status = %s, want active", created.Status)
	}
	updated := card("alice-renamed")
	again, isNew, err := svc.Register(ctx, aliceKey, updated, nil)
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
	if _, _, err := svc.Register(ctx, aliceKey, bad, nil); err == nil {
		t.Fatal("want error for publicKey that does not match the authenticated agent")
	}
}

func TestRegisterRejectsInvalidCard(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	bad := card("alice")
	bad.URL = "not-a-url"
	if _, _, err := svc.Register(ctx, aliceKey, bad, nil); err == nil {
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
	}, "", "", nil)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PublishOffer err = %v, want not found for unknown agent", err)
	}
}

func TestPublishOfferRejectsPriceBeyondCardCurrencies(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	withCurrencies := card("alice")
	withCurrencies.Currencies = []string{usdc}
	if _, _, err := svc.Register(ctx, aliceKey, withCurrencies, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "forbidden currency",
		PriceAmount:     "1.00",
		PriceMint:       "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr",
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil)
	if !errors.Is(err, registry.ErrCurrencyNotAccepted) {
		t.Fatalf("err = %v, want ErrCurrencyNotAccepted", err)
	}
}

func TestPublishOfferRejectsTooPreciseOnchainAmount(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice"), nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "sub-cent precision cannot settle on chain",
		PriceAmount:     "1.000000001",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOnchain},
	}, "", "", nil)
	if !errors.Is(err, registry.ErrAmountTooPrecise) {
		t.Fatalf("err = %v, want ErrAmountTooPrecise", err)
	}
}

func TestPublishOfferIdempotency(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	if _, _, err := svc.Register(ctx, aliceKey, card("alice"), nil); err != nil {
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
	first, isNew, err := svc.PublishOffer(ctx, aliceKey, in, "idem-1", "", nil)
	if err != nil || !isNew {
		t.Fatalf("PublishOffer = %v, %v", isNew, err)
	}
	second, isNew, err := svc.PublishOffer(ctx, aliceKey, in, "idem-1", "", nil)
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
		if _, _, err := svc.Register(ctx, id, card(id), nil); err != nil {
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
	}, "", "", nil)
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
		if _, _, err := svc.Register(ctx, id, card(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "3.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PublishOffer(ctx, bobKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "1.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil); err != nil {
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
		if _, _, err := svc.Register(ctx, id, card(id), nil); err != nil {
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
		}, "", "", nil); err != nil {
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
	if _, _, err := svc.Register(ctx, aliceKey, card(aliceKey), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
			Direction:       domain.DirectionAsk,
			Description:     "summarize " + uuid.NewString(),
			PriceAmount:     "1.00",
			PriceMint:       usdc,
			SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		}, "", "", nil); err != nil {
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
	if _, _, err := svc.Register(ctx, aliceKey, card("alice"), nil); err != nil {
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
	svc := registry.New(db, mainnetMints(t), marketSigner(t))
	for _, id := range []string{aliceKey, bobKey} {
		if _, _, err := svc.Register(ctx, id, card(id), nil); err != nil {
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
	if _, _, err := svc.Register(ctx, aliceKey, card("alice"), nil); err != nil {
		t.Fatal(err)
	}
	published, _, err := svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		PriceAmount:     "1.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil)
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
	mints, err := tokens.New(cluster.Localnet, []tokens.Token{{Address: testMint, Symbol: "TEST", Decimals: 2, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	svc := registry.New(db, mints, marketSigner(t))
	withCurrencies := card("alice")
	withCurrencies.Currencies = []string{testMint}
	if _, _, err := svc.Register(ctx, aliceKey, withCurrencies, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.PublishOffer(ctx, aliceKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "more precision than the governed mint supports",
		PriceAmount:     "1.005",
		PriceMint:       testMint,
		SettlementModes: []domain.SettlementMode{domain.SettlementOnchain},
	}, "", "", nil)
	if !errors.Is(err, registry.ErrAmountTooPrecise) {
		t.Fatalf("PublishOffer err = %v, want ErrAmountTooPrecise for the 2-decimal test mint", err)
	}
}

// retirementAgent registers a seller and publishes one open offer, which is the
// state a retirement has to dismantle cleanly.
func retirementAgent(t *testing.T, svc *registry.Service, key string) domain.Offer {
	t.Helper()
	ctx := context.Background()
	if _, _, err := svc.Register(ctx, key, card("seller"), nil); err != nil {
		t.Fatal(err)
	}
	offer, _, err := svc.PublishOffer(ctx, key, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "withdraw me",
		Capabilities:    []string{"summarize"},
		PriceAmount:     "10.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

func TestRetirementClosesTheListingAndIsReversible(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	offer := retirementAgent(t, svc, aliceKey)

	retired, err := svc.Retire(ctx, aliceKey, "withdrawn pending review", "ops@example")
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != domain.AgentRetired {
		t.Fatalf("status = %s, want retired", retired.Status)
	}

	got, err := svc.Offer(ctx, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want closed", got.Status)
	}

	// A retired agent is not in the active directory. Everything that lists
	// sellers to buyers reads that list.
	active, err := svc.Agents(ctx, domain.AgentActive)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == aliceKey {
			t.Error("a retired agent is still listed as active")
		}
	}

	// It is still fetchable by id, because the registration is evidence and the
	// receipts naming it have to stay explainable.
	if _, err := svc.Agent(ctx, aliceKey); err != nil {
		t.Errorf("a retired agent is no longer fetchable: %v", err)
	}

	record, found, err := svc.Retirement(ctx, aliceKey)
	if err != nil || !found {
		t.Fatalf("retirement record found=%v err=%v, want one", found, err)
	}
	if record.Reason != "withdrawn pending review" || record.Actor != "ops@example" {
		t.Errorf("record = %+v, want the reason and actor verbatim", record)
	}
	if record.RestoredAt != nil {
		t.Error("restoredAt set without a restore")
	}

	restored, err := svc.Restore(ctx, aliceKey, "ops@example")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != domain.AgentActive {
		t.Errorf("status = %s, want active", restored.Status)
	}

	// The offers stay closed. Restoring says the agent may trade again; it does
	// not republish on the seller's behalf a listing a buyer already saw close.
	after, err := svc.Offer(ctx, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want still closed after restore", after.Status)
	}

	record, _, err = svc.Retirement(ctx, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if record.RestoredAt == nil {
		t.Error("the audit record lost the reversal")
	}
	if record.RestoredBy != "ops@example" {
		t.Errorf("restoredBy = %q, want the operator who reversed it", record.RestoredBy)
	}
	if record.Actor != "ops@example" {
		t.Errorf("actor = %q, want the operator who withdrew it, not the one who restored it", record.Actor)
	}
}

// Who withdrew a listing and who put it back are usually two different operators,
// and the record has to answer both questions. One column for both would either
// misattribute the withdrawal or lose it.
func TestTheRecordNamesWhomWithdrewTheListingAndWhomRestoredIt(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	retirementAgent(t, svc, aliceKey)

	if _, err := svc.Retire(ctx, aliceKey, "duplicate listing", "ops-a@example"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, aliceKey, "ops-b@example"); err != nil {
		t.Fatal(err)
	}
	record, found, err := svc.Retirement(ctx, aliceKey)
	if err != nil || !found {
		t.Fatalf("record found=%v err=%v", found, err)
	}
	if record.Actor != "ops-a@example" {
		t.Errorf("actor = %q, want the withdrawing operator", record.Actor)
	}
	if record.RestoredBy != "ops-b@example" {
		t.Errorf("restoredBy = %q, want the restoring operator", record.RestoredBy)
	}
}

func TestRetiringTwiceKeepsOneRecord(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	retirementAgent(t, svc, aliceKey)

	if _, err := svc.Retire(ctx, aliceKey, "first", "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, aliceKey, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retire(ctx, aliceKey, "second", "ops"); err != nil {
		t.Fatal(err)
	}

	// An operator who retires, restores and retires again must not have to
	// reconcile three rows to learn what happened: the latest action is the
	// current state of the record.
	record, _, err := svc.Retirement(ctx, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if record.Reason != "second" {
		t.Errorf("reason = %q, want the most recent one", record.Reason)
	}
	if record.RestoredAt != nil {
		t.Error("restoredAt is set although the agent is retired")
	}
}

func TestRetirementNeedsAReasonAndAnUnknownAgentIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	retirementAgent(t, svc, aliceKey)

	if _, err := svc.Retire(ctx, aliceKey, "", "ops"); !errors.Is(err, registry.ErrRetirementReasonRequired) {
		t.Errorf("retire with no reason = %v, want ErrRetirementReasonRequired", err)
	}
	if _, err := svc.Retire(ctx, "4Nd6mBQrHfvcTFY4QY5xL8pQ4CvJKcENDrFbfH9wqLKq", "x", "ops"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("retire an unknown agent = %v, want ErrNotFound", err)
	}
	if _, found, err := svc.Retirement(ctx, aliceKey); err != nil || found {
		t.Errorf("a refused retirement left a record: found=%v err=%v", found, err)
	}
}

func TestAReRegisteredAgentDoesNotReinstateItself(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	retirementAgent(t, svc, aliceKey)
	if _, err := svc.Retire(ctx, aliceKey, "withdrawn", "ops"); err != nil {
		t.Fatal(err)
	}

	// An agent updating its own card must not be a way back from a withdrawal.
	// Otherwise retirement is revocable by the party it excludes, which would
	// make the operator's action advisory rather than effective.
	if _, _, err := svc.Register(ctx, aliceKey, card("alice-again"), nil); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Agent(ctx, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AgentRetired {
		t.Errorf("status = %s after re-registering, want still retired", got.Status)
	}
}

func TestARefusedRetirementIsRecognisableAsTheSameErrorEverywhere(t *testing.T) {
	// Two packages own this refusal and used to name it separately: the store
	// creates it, the registry re-exports a sentinel so a caller can recognise it
	// without importing the store. Each called errors.New with the same sentence,
	// and errors.Is matches on identity rather than on text, so the pair did not
	// match. The retire route was never wrong to an operator - it matches the
	// concrete type with errors.As and writes the 409 itself - but the matching
	// entry in httpapi's status table was unreachable, and any caller reaching for
	// the sentinel the way the table does would have been told the marketplace had
	// broken when an agent had only an unsettled trade.
	err := &store.LiveTradesError{AgentID: aliceKey, TradeIDs: []string{"t1"}}

	if !errors.Is(err, registry.ErrAgentHasLiveTrades) {
		t.Error("the registry sentinel does not recognise the store's refusal, " +
			"so it is a copy of it rather than the same error")
	}
	if !errors.Is(err, store.ErrAgentHasLiveTrades) {
		t.Error("the refusal does not unwrap to the store's own sentinel")
	}
	if errors.Is(err, registry.ErrRetirementReasonRequired) {
		t.Error("the refusal matches an unrelated sentinel")
	}
}

// offerClock is a mutable clock the offer sweep can be driven against, so the
// tests do not have to sleep on wall time.
type offerClock struct{ at time.Time }

func (c *offerClock) now() time.Time          { return c.at }
func (c *offerClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newOfferService(t *testing.T, ttl time.Duration, clock *offerClock) *registry.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "offers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return registry.New(db, mainnetMints(t), marketSigner(t),
		registry.WithOfferTTL(ttl), registry.WithClock(clock.now))
}

func registerAgent(t *testing.T, svc *registry.Service, key string) {
	t.Helper()
	if _, _, err := svc.Register(context.Background(), key, card(key), nil); err != nil {
		t.Fatalf("register %s: %v", key, err)
	}
}

func publishTestOffer(t *testing.T, svc *registry.Service, key, description string) domain.Offer {
	t.Helper()
	offer, _, err := svc.PublishOffer(context.Background(), key, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     description,
		Capabilities:    []string{"summarize"},
		PriceAmount:     "3.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "", "", nil)
	if err != nil {
		t.Fatalf("PublishOffer %s: %v", description, err)
	}
	return offer
}

func TestAPublishedOfferCarriesItsDeadline(t *testing.T) {
	clock := &offerClock{at: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	svc := newOfferService(t, time.Hour, clock)
	registerAgent(t, svc, aliceKey)

	offer := publishTestOffer(t, svc, aliceKey, "summarize a document")
	if want := clock.at.Add(time.Hour); !offer.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want %s: a listing needs a deadline to be swept", offer.ExpiresAt, want)
	}
}

func TestAnExpiredOfferStopsBeingDiscoverable(t *testing.T) {
	clock := &offerClock{at: time.Now().UTC()}
	svc := newOfferService(t, time.Hour, clock)
	registerAgent(t, svc, aliceKey)
	offer := publishTestOffer(t, svc, aliceKey, "summarize a document")

	clock.advance(2 * time.Hour)
	expired, err := svc.ExpireOffers(context.Background(), 100)
	if err != nil || expired != 1 {
		t.Fatalf("ExpireOffers = %d, %v, want 1", expired, err)
	}
	found, err := svc.Search(context.Background(), domain.OfferQuery{Text: "document"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("an expired offer is still discoverable: %d results", len(found))
	}
	got, err := svc.Offer(context.Background(), offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OfferClosed {
		t.Errorf("status = %s, want closed after the sweep", got.Status)
	}
}

func TestTheOfferSweepLeavesAFreshOfferOpen(t *testing.T) {
	clock := &offerClock{at: time.Now().UTC()}
	svc := newOfferService(t, time.Hour, clock)
	registerAgent(t, svc, aliceKey)
	offer := publishTestOffer(t, svc, aliceKey, "summarize a document")

	clock.advance(30 * time.Minute)
	expired, err := svc.ExpireOffers(context.Background(), 100)
	if err != nil || expired != 0 {
		t.Fatalf("ExpireOffers = %d, %v, want 0 before the deadline", expired, err)
	}
	got, err := svc.Offer(context.Background(), offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OfferOpen {
		t.Errorf("status = %s, want open before the deadline", got.Status)
	}
}

func TestTheOfferSweepIsBounded(t *testing.T) {
	clock := &offerClock{at: time.Now().UTC()}
	svc := newOfferService(t, time.Hour, clock)
	registerAgent(t, svc, aliceKey)
	for _, d := range []string{"one", "two", "three"} {
		publishTestOffer(t, svc, aliceKey, "summarize "+d)
	}
	clock.advance(2 * time.Hour)

	first, err := svc.ExpireOffers(context.Background(), 2)
	if err != nil || first != 2 {
		t.Fatalf("first ExpireOffers = %d, %v, want 2", first, err)
	}
	second, err := svc.ExpireOffers(context.Background(), 2)
	if err != nil || second != 1 {
		t.Fatalf("second ExpireOffers = %d, %v, want 1: the batch is a bound, not a target", second, err)
	}
}

func TestAClosedOfferIsNotTouchedByTheSweep(t *testing.T) {
	clock := &offerClock{at: time.Now().UTC()}
	svc := newOfferService(t, time.Hour, clock)
	registerAgent(t, svc, aliceKey)
	offer := publishTestOffer(t, svc, aliceKey, "summarize a document")

	closed, err := svc.CloseOffer(context.Background(), aliceKey, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Hour)
	expired, err := svc.ExpireOffers(context.Background(), 100)
	if err != nil || expired != 0 {
		t.Fatalf("ExpireOffers = %d, %v, want 0 for an already-closed offer", expired, err)
	}
	got, err := svc.Offer(context.Background(), offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(closed.UpdatedAt) {
		t.Errorf("updatedAt = %s, want %s: the sweep overwrote the seller's own close",
			got.UpdatedAt, closed.UpdatedAt)
	}
}

func TestNoOfferDeadlineMeansNoOfferSweep(t *testing.T) {
	clock := &offerClock{at: time.Now().UTC()}
	svc := newOfferService(t, 0, clock)
	registerAgent(t, svc, aliceKey)
	offer := publishTestOffer(t, svc, aliceKey, "summarize a document")

	if !offer.ExpiresAt.IsZero() {
		t.Errorf("expiresAt = %s, want zero when no deadline is configured", offer.ExpiresAt)
	}
	clock.advance(100 * time.Hour)
	expired, err := svc.ExpireOffers(context.Background(), 100)
	if err != nil || expired != 0 {
		t.Fatalf("ExpireOffers = %d, %v, want 0 with no deadline", expired, err)
	}
}

func TestAProbeSignedBeforeRotationStillVerifies(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "rotation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	oldSigner := marketSigner(t)
	newSigner := marketSigner(t)
	svc := registry.New(db, mainnetMints(t), newSigner,
		registry.WithRetiredMarketplaceKeys([]string{oldSigner.PublicKeyBase58()}))

	keys := svc.MarketplaceKeyIDs()
	if len(keys) != 2 || keys[0] != newSigner.PublicKeyBase58() || keys[1] != oldSigner.PublicKeyBase58() {
		t.Fatalf("marketplace keys = %v, want the current key first and the retired key second", keys)
	}

	report := probe.Report{
		AgentID:   aliceKey,
		Target:    "https://agent.example",
		CheckedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		Results:   []probe.Result{{Capability: "summarize", Status: probe.StatusPass}},
	}
	sig, err := oldSigner.SignProbe(probe.Statement(report), report.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	report.Signature = &sig
	if err := svc.VerifyProbe(report); err != nil {
		t.Fatalf("a probe attested before rotation must verify under the retired key: %v", err)
	}

	stranger := marketSigner(t)
	strangerSig, err := stranger.SignProbe(probe.Statement(report), report.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	report.Signature = &strangerSig
	if err := svc.VerifyProbe(report); err == nil {
		t.Error("a probe signed by a key this marketplace never published must be refused")
	}
}
