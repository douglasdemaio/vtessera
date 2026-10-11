package agp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const eurc = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"

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

func TestRouteTaskRendersAnAddressedEnvelope(t *testing.T) {
	now := time.Now()
	announcement := AnnouncementsForOffer(
		offer("o1", "alice", "25", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0]
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{announcement}))
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"text": "a long document to summarise"}
	intent := Intent{TargetCapability: announcement.Capability, Payload: payload}
	result, err := table.RouteTask(intent)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task == nil {
		t.Fatal("route_task result carries no task envelope")
	}
	task := *result.Task
	if task.JSONRPC != "2.0" || task.Method != "message" {
		t.Errorf("envelope = %s/%s, want jsonrpc 2.0 / method message", task.JSONRPC, task.Method)
	}
	if task.Params.Message.Role != "agent" || task.Params.Message.Kind != "task" {
		t.Errorf("message = %s/%s, want an agent task message", task.Params.Message.Role, task.Params.Message.Kind)
	}
	if task.ID == "" || task.Params.Message.TaskID == "" || task.Params.Message.ContextID == "" {
		t.Error("envelope, task and context ids must all be present")
	}
	if task.Params.Message.TargetCapability != announcement.Capability {
		t.Errorf("targetCapability = %q, want %q", task.Params.Message.TargetCapability, announcement.Capability)
	}
	if task.Params.Message.Payload["text"] != "a long document to summarise" {
		t.Errorf("payload = %v, want the caller's intent content", task.Params.Message.Payload)
	}
	fresh, err := table.RouteTask(intent)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Task.ID == task.ID {
		t.Error("two route_task calls mint the same envelope id")
	}

	plain, err := table.Route(intent)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Task != nil {
		t.Error("Route must not attach a task envelope")
	}
}

func TestRouteTaskSharesRoutesFailureModes(t *testing.T) {
	now := time.Now()
	announcement := AnnouncementsForOffer(
		offer("o1", "alice", "25", usdc, domain.SettlementOffchain), agent("alice"), usdcMint(), now)[0]
	table, err := NewRouting().BuildTable(context.Background(), source([]CapabilityAnnouncement{announcement}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.RouteTask(Intent{TargetCapability: "nope:nope", Payload: map[string]any{}}); !IsRouteNotFound(err) {
		t.Errorf("unknown capability = %v, want AGP_ROUTE_NOT_FOUND", err)
	}
	if _, err := table.RouteTask(Intent{TargetCapability: announcement.Capability, Payload: map[string]any{}, PolicyConstraints: map[string]any{PolicyCurrencies: []any{eurc}}}); !IsPolicyViolation(err) {
		t.Errorf("unsatisfied constraint = %v, want AGP_POLICY_VIOLATION", err)
	}
}

func TestForwardingMethods(t *testing.T) {
	if len(ForwardingMethods) != 2 || ForwardingMethods[0] != "agp/route_intent" || ForwardingMethods[1] != "agp/route_task" {
		t.Errorf("forwarding methods = %v, want [agp/route_intent agp/route_task]", ForwardingMethods)
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

func TestToBool(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    bool
		wantErr bool
	}{
		{name: "bool true", in: true, want: true},
		{name: "bool false", in: false, want: false},
		{name: "string true", in: "true", want: true},
		{name: "string false", in: "false", want: false},
		{name: "string not a bool", in: "maybe", wantErr: true},
		{name: "unsupported type", in: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toBool(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("toBool(%v) = %v, nil, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toBool(%v) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("toBool(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestToNumber(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    float64
		wantErr bool
	}{
		{name: "float64", in: float64(3.5), want: 3.5},
		{name: "float32", in: float32(2.5), want: 2.5},
		{name: "int", in: 4, want: 4},
		{name: "int64", in: int64(5), want: 5},
		{name: "json.Number", in: json.Number("6.25"), want: 6.25},
		{name: "json.Number malformed", in: json.Number("not-a-number"), wantErr: true},
		{name: "string", in: "7.5", want: 7.5},
		{name: "string not a number", in: "nope", wantErr: true},
		{name: "unsupported type", in: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toNumber(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("toNumber(%v) = %v, nil, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toNumber(%v) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("toNumber(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestToStringList(t *testing.T) {
	got, err := toStringList([]string{"a", "b"})
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("toStringList([]string) = %v, %v, want [a b], nil", got, err)
	}

	got, err = toStringList([]any{"a", json.Number("2")})
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "2" {
		t.Errorf("toStringList([]any) = %v, %v, want [a 2], nil", got, err)
	}

	got, err = toStringList("solo")
	if err != nil || len(got) != 1 || got[0] != "solo" {
		t.Errorf("toStringList(string) = %v, %v, want [solo], nil", got, err)
	}

	if _, err := toStringList(42); err == nil {
		t.Error("toStringList(42) = nil error, want an error for an unsupported type")
	}
}

func TestNormalizeValue(t *testing.T) {
	if got := normalizeValue(json.Number("1.5")); got != 1.5 {
		t.Errorf("normalizeValue(json.Number(1.5)) = %v, want 1.5", got)
	}
	if got := normalizeValue(json.Number("not-a-number")); got != "not-a-number" {
		t.Errorf("normalizeValue(json.Number(not-a-number)) = %v, want the raw string", got)
	}
	if got := normalizeValue("plain"); got != "plain" {
		t.Errorf("normalizeValue(plain) = %v, want it unchanged", got)
	}
}

func TestMatchPolicyDefaultCaseComparesNormalizedValues(t *testing.T) {
	if err := matchPolicy(PolicyDirection, "ask", "ask"); err != nil {
		t.Errorf("matching direction values should not error: %v", err)
	}
	if err := matchPolicy(PolicyDirection, "ask", "bid"); err == nil {
		t.Error("mismatched direction values should error")
	}
	if err := matchPolicy(PolicyDirection, json.Number("1"), float64(1)); err != nil {
		t.Errorf("a json.Number and its float64 equivalent should match: %v", err)
	}
}

func TestMatchAnyOfPropagatesConversionErrors(t *testing.T) {
	if err := matchAnyOf(PolicyCurrencies, 42, []any{"usdc"}); err == nil {
		t.Error("an unconvertible want value should error")
	}
	if err := matchAnyOf(PolicyCurrencies, []any{"usdc"}, 42); err == nil {
		t.Error("an unconvertible have value should error")
	}
}
