package agp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const eurc = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc"

// usdcMint and eurcMint stand in for the governed token registry: the routing
// table only needs identity and decimals, never a global mint table.
func usdcMint() domain.MintInfo {
	return domain.MintInfo{Address: usdc, Symbol: "USDC", Decimals: 6, Known: true}
}

func eurcMint() domain.MintInfo {
	return domain.MintInfo{Address: eurc, Symbol: "EURC", Decimals: 6, Known: true}
}

type source []CapabilityAnnouncement

func (s source) Announcements(context.Context) ([]CapabilityAnnouncement, error) {
	return []CapabilityAnnouncement(s), nil
}

func offer(id, agent, amount, mint string, modes ...domain.SettlementMode) domain.Offer {
	return domain.Offer{
		ID:              id,
		AgentID:         agent,
		Direction:       domain.DirectionAsk,
		Description:     "sentiment analysis over financial news",
		Capabilities:    []string{"nlp"},
		PriceAmount:     money.MustParse(amount),
		PriceMint:       mint,
		SettlementModes: modes,
		Status:          domain.OfferOpen,
	}
}

func agent(id string) domain.Agent {
	return domain.Agent{ID: id, Card: domain.AgentCard{Name: "agent", PublicKey: id, Version: "2.0"}}
}

func TestCapabilitiesForOfferUsesDeclaredAndDerived(t *testing.T) {
	o := offer("o1", "alice", "25", usdc, domain.SettlementOffchain)
	capabilities := CapabilitiesForOffer(o)
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %v, want 1", capabilities)
	}
	if capabilities[0][:4] != "nlp:" {
		t.Errorf("capability %q should start with the declared domain", capabilities[0])
	}

	o.Capabilities = []string{"Finance:Quarterly", "nlp:sentiment"}
	got := CapabilitiesForOffer(o)
	if len(got) != 2 {
		t.Errorf("capabilities = %v, want the two declared domain:action values", got)
	}
	if got[0] != "finance:quarterly" {
		t.Errorf("capability = %q, want normalized finance:quarterly", got[0])
	}

	o.Capabilities = nil
	got = CapabilitiesForOffer(o)
	if len(got) != 1 || got[0][:len(defaultCapabilityDomain)+1] != defaultCapabilityDomain+CapabilitySeparator {
		t.Errorf("capabilities = %v, want a general: fallback", got)
	}
}

func TestRoutePrefersLowestCost(t *testing.T) {
	now := time.Now()
	announcements := []CapabilityAnnouncement{
		AnnouncementsForOffer(offer("cheap", "alice", "10", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0],
		AnnouncementsForOffer(offer("pricey", "bob", "250", usdc, domain.SettlementOffchain), agent("bob"), usdcMint(), now)[0],
	}
	table, err := NewRouting().BuildTable(context.Background(), source(announcements))
	if err != nil {
		t.Fatal(err)
	}
	result, err := table.Route(Intent{TargetCapability: announcements[0].Capability, Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.OfferID != "cheap" {
		t.Errorf("route = %s, want the cheapest offer", result.Route.OfferID)
	}
	if result.Considered != 2 {
		t.Errorf("considered = %d, want 2", result.Considered)
	}
	if result.Route.Cost != 10 {
		t.Errorf("cost = %v, want 10", result.Route.Cost)
	}
}

func TestRouteIsPolicyFirstThenCost(t *testing.T) {
	now := time.Now()
	cheap := AnnouncementsForOffer(
		offer("cheap", "alice", "10", usdc, domain.SettlementOffchain, domain.SettlementOnchain),
		agent("alice"), usdcMint(), now)[0]
	pricey := AnnouncementsForOffer(
		offer("pricey", "bob", "250", eurc, domain.SettlementOffchain),
		agent("bob"), eurcMint(), now)[0]
	capability := cheap.Capability
	if pricey.Capability != capability {
		t.Fatalf("test premise: offers should share a capability, got %q and %q", cheap.Capability, pricey.Capability)
	}
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{cheap, pricey}))
	if err != nil {
		t.Fatal(err)
	}

	constraints := map[string]any{
		PolicySettlementModes: []any{string(domain.SettlementOnchain)},
	}
	result, err := table.Route(Intent{TargetCapability: capability, Payload: map[string]any{}, PolicyConstraints: constraints})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.OfferID != "cheap" {
		t.Errorf("route = %s, want the only on-chain capable offer", result.Route.OfferID)
	}
	if result.Rejected["pricey"] != 1 {
		t.Errorf("rejected = %v, want the off-chain-only offer reported as rejected", result.Rejected)
	}

	constraints = map[string]any{
		PolicyCurrencies: []any{eurc},
	}
	result, err = table.Route(Intent{TargetCapability: capability, Payload: map[string]any{}, PolicyConstraints: constraints})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.OfferID != "pricey" {
		t.Errorf("route = %s, want the EURC offer: policy must beat cost", result.Route.OfferID)
	}
	if result.Rejected["cheap"] != 1 {
		t.Errorf("rejected = %v, want the USDC offer reported as rejected", result.Rejected)
	}

	constraints = map[string]any{
		PolicyCurrencies:      []any{eurc},
		PolicySettlementModes: []any{string(domain.SettlementOnchain)},
	}
	if _, err := table.Route(Intent{TargetCapability: capability, Payload: map[string]any{}, PolicyConstraints: constraints}); !IsPolicyViolation(err) {
		t.Errorf("unsatisfiable constraints = %v, want AGP_POLICY_VIOLATION", err)
	}
}

func TestSettlementModeConstraintIsAnyOf(t *testing.T) {
	now := time.Now()
	announcement := AnnouncementsForOffer(
		offer("o1", "alice", "25", usdc, domain.SettlementOffchain, domain.SettlementOnchain),
		agent("alice"), usdcMint(), now)[0]
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{announcement}))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []domain.SettlementMode{domain.SettlementOffchain, domain.SettlementOnchain} {
		result, err := table.Route(Intent{
			TargetCapability:  announcement.Capability,
			Payload:           map[string]any{},
			PolicyConstraints: map[string]any{PolicySettlementModes: []any{string(mode)}},
		})
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		if result.Route.OfferID != "o1" {
			t.Errorf("mode %s routed to %s, want o1", mode, result.Route.OfferID)
		}
	}
}

func TestRouteErrors(t *testing.T) {
	now := time.Now()
	announcement := AnnouncementsForOffer(
		offer("o1", "alice", "25", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0]
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{announcement}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = table.Route(Intent{TargetCapability: "unknown:capability", Payload: map[string]any{}})
	if !IsRouteNotFound(err) {
		t.Errorf("unknown capability = %v, want AGP_ROUTE_NOT_FOUND", err)
	}
	if code, _ := CodeOf(err); code != CodeRouteNotFound {
		t.Errorf("code = %d, want %d", code, CodeRouteNotFound)
	}

	_, err = table.Route(Intent{
		TargetCapability:  announcement.Capability,
		Payload:           map[string]any{},
		PolicyConstraints: map[string]any{PolicyCurrencies: []any{eurc}},
	})
	if !IsPolicyViolation(err) {
		t.Errorf("currency mismatch = %v, want AGP_POLICY_VIOLATION", err)
	}

	_, err = table.Route(Intent{
		TargetCapability:  announcement.Capability,
		Payload:           map[string]any{},
		PolicyConstraints: map[string]any{PolicyRequiresPII: true},
	})
	if !IsPolicyViolation(err) {
		t.Errorf("unannounced policy = %v, want AGP_POLICY_VIOLATION", err)
	}

	if _, err := table.Route(Intent{TargetCapability: announcement.Capability}); !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("missing payload = %v, want ErrInvalidIntent", err)
	}
	if _, err := table.Route(Intent{TargetCapability: "malformed", Payload: map[string]any{}}); !errors.Is(err, ErrInvalidIntent) {
		t.Errorf("malformed capability = %v, want ErrInvalidIntent", err)
	}
}

func TestSecurityLevelThreshold(t *testing.T) {
	announced := map[string]any{PolicySecurityLevel: float64(2)}
	if err := CheckPolicy(announced, map[string]any{PolicySecurityLevel: float64(3)}); err == nil {
		t.Error("security_level 2 must not satisfy a constraint of 3")
	}
	if err := CheckPolicy(announced, map[string]any{PolicySecurityLevel: float64(2)}); err != nil {
		t.Errorf("security_level 2 must satisfy 2: %v", err)
	}
	if err := CheckPolicy(announced, map[string]any{PolicySecurityLevel: float64(1)}); err != nil {
		t.Errorf("security_level 2 must satisfy 1: %v", err)
	}
}

func TestCostComparisonIsExact(t *testing.T) {
	now := time.Now()
	cheaper := AnnouncementsForOffer(offer("a", "alice", "10000000000.000000001", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0]
	pricier := AnnouncementsForOffer(offer("b", "bob", "10000000000.000000002", usdc, domain.SettlementOffchain), agent("bob"), usdcMint(), now)[0]
	if cheaper.Cost != pricier.Cost {
		t.Skipf("float64 no longer collides for these amounts (%v vs %v); exactness still holds but the premise is void", cheaper.Cost, pricier.Cost)
	}
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{cheaper, pricier}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := table.Route(Intent{TargetCapability: cheaper.Capability, Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Route.OfferID != "a" {
		t.Errorf("route = %s, want the exact-lower amount even though the float costs are equal", result.Route.OfferID)
	}
}

func TestTableFingerprintChangesWithTable(t *testing.T) {
	now := time.Now()
	first, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{
		AnnouncementsForOffer(offer("a", "alice", "25", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0],
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{
		AnnouncementsForOffer(offer("a", "alice", "30", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0],
	}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() == second.Fingerprint() {
		t.Error("a price change must change the table fingerprint")
	}
}

func TestBuildTableRejectsBadAnnouncement(t *testing.T) {
	if _, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{{
		Capability: "nlp:sentiment",
		Version:    "1.0",
		SquadPath:  "Squad_Marketplace/alice/offer/o1",
		CostAmount: "not-a-number",
	}})); err == nil {
		t.Error("expected rejection of a malformed costAmount")
	}
}

func TestExtensionDeclaration(t *testing.T) {
	if ExtensionURI == "" || Version != "1.0" {
		t.Fatalf("extension constants = %q %q", ExtensionURI, Version)
	}
}
