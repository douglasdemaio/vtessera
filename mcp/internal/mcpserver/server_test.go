package mcpserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
)

// These tests run the real MCP server over an in-memory transport against a
// stand-in marketplace, so they check the tool surface and the wire shapes
// without a database or a network.
//
// The stand-in answers with the field names the vtessera service actually emits.
// A test that invented its own response shape would pass while the server
// silently returned empty cards in front of a real marketplace.

// A tool list is the whole interface, so every tool must describe itself and say
// what it refuses rather than only what it does.
func TestEveryToolIsDescribedAndNamed(t *testing.T) {
	s := newTestServer(t, healthOnly())
	tools := s.listTools()
	if len(tools) == 0 {
		t.Fatal("the server advertised no tools")
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not marked read-only, and every tool here is", tool.Name)
		}
		if !strings.HasPrefix(tool.Name, "vtessera_") {
			t.Errorf("tool %s is not namespaced to this marketplace", tool.Name)
		}
		seen[tool.Name] = true
	}
	for _, want := range []string{
		"vtessera_health", "vtessera_search_offers", "vtessera_get_offer",
		"vtessera_get_agent", "vtessera_get_card_attestation",
		"vtessera_get_capability_report", "vtessera_route_intent",
	} {
		if !seen[want] {
			t.Errorf("tool %s is missing from the advertised set", want)
		}
	}
}

// Health is the first thing a client asks, and it is where the verification key
// and the sandbox flag come from. Both have to survive the trip.
func TestHealthReportsTheVerificationKeyAndTheSandboxFlag(t *testing.T) {
	s := newTestServer(t, healthOnly())
	res := s.call("vtessera_health", map[string]any{})

	var health vtessera.Health
	decodeStructured(t, res, &health)
	if health.VerificationKey != "5LRpM9wpvPfRYuQAC7oNdyaQa6sakpMcnZeR9FS5CgjB" {
		t.Errorf("verificationKey = %q, want the marketplace's own", health.VerificationKey)
	}
	if !health.Sandbox {
		t.Error("sandbox = false, want true: a reader must be able to tell that no value can move")
	}
	if health.Cluster != "" {
		t.Errorf("cluster = %q, want it absent in a sandbox with no chain", health.Cluster)
	}
}

// A search with no matches must say so with a count. An empty array alone reads to
// a model as a malformed answer rather than as "nothing matched".
func TestASearchWithNoMatchesReportsAZeroCount(t *testing.T) {
	s := newTestServer(t, offersHandler(`{"offers":[]}`))
	res := s.call("vtessera_search_offers", map[string]any{"capability": "summarize:document"})

	var out struct {
		Count  int              `json:"count"`
		Offers []vtessera.Offer `json:"offers"`
	}
	decodeStructured(t, res, &out)
	if out.Count != 0 || len(out.Offers) != 0 {
		t.Errorf("count = %d with %d offers, want zero of both", out.Count, len(out.Offers))
	}
}

// Prices arrive as decimal strings and the governed mint alongside them. A price
// parsed as a float would lose the precision a spending cap is computed in, so the
// string has to arrive as a string.
func TestAnOfferPriceArrivesAsADecimalStringWithItsMint(t *testing.T) {
	body := `{"offers":[{"id":"o1","agentId":"a1","direction":"ask",
	  "description":"summarize a document","capabilities":["summarize:document"],
	  "priceAmount":"10.00","priceMint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
	  "settlementModes":["offchain"],"status":"open"}]}`
	s := newTestServer(t, offersHandler(body))
	res := s.call("vtessera_search_offers", map[string]any{"capability": "summarize:document"})

	var out struct {
		Count  int              `json:"count"`
		Offers []vtessera.Offer `json:"offers"`
	}
	decodeStructured(t, res, &out)
	if out.Count != 1 || len(out.Offers) != 1 {
		t.Fatalf("count = %d offers = %d, want one of each", out.Count, len(out.Offers))
	}
	got := out.Offers[0]
	if got.PriceAmount != "10.00" {
		t.Errorf("priceAmount = %q, want the string 10.00", got.PriceAmount)
	}
	if got.PriceMint != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" {
		t.Errorf("priceMint = %q, want the governed mint", got.PriceMint)
	}
	if got.Status != "open" {
		t.Errorf("status = %q, want open", got.Status)
	}
}

// An agent that has never been probed must fail with NOT_PROBED rather than
// returning an empty report. An empty report with passed: false would be read as a
// check that found nothing working, which is a different and wrong claim.
func TestAnAgentThatHasNeverBeenProbedIsRefusedRatherThanReportedEmpty(t *testing.T) {
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"this agent has never been probed","code":"NOT_PROBED"}`))
			return
		}
		healthResponse(w)
	}))

	res := s.callExpectingError("vtessera_get_capability_report", map[string]any{"agentId": "a1"})
	if !strings.Contains(resultText(res), "NOT_PROBED") {
		t.Errorf("error = %q, want it to carry NOT_PROBED rather than an empty report", resultText(res))
	}
}

// A probe that ran and failed is reported as failed, with the per-capability
// detail and the verification verdict.
func TestAFailedProbeIsReportedAsFailed(t *testing.T) {
	body := `{"agentId":"a1","target":"https://agent.example.com:8443/probe","passed":false,
	  "valid":true,"checkedAt":"2026-10-04T12:00:00Z","attestedBy":"market-key",
	  "results":[{"capability":"summarize:document","status":"fail","detail":"no model loaded"}]}`
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_, _ = w.Write([]byte(body))
			return
		}
		healthResponse(w)
	}))

	res := s.call("vtessera_get_capability_report", map[string]any{"agentId": "a1"})
	var report vtessera.CapabilityReport
	decodeStructured(t, res, &report)
	if report.Passed {
		t.Error("passed = true, want the failure reported as a failure")
	}
	if !report.Valid {
		t.Error("valid = false, want the report's own signature verdict passed through")
	}
	if len(report.Results) != 1 || report.Results[0].Status != vtessera.StatusFail {
		t.Errorf("results = %+v, want the one failing capability", report.Results)
	}
	if report.Results[0].Detail != "no model loaded" {
		t.Errorf("detail = %q, want the agent's own explanation preserved", report.Results[0].Detail)
	}
}

// The card attestation is two-sided and the tool must keep the two apart. A card
// the marketplace published but the agent never signed is the common case for an
// agent predating attestations, and collapsing it into "unverified" would tell a
// directory the marketplace does not vouch for a card it published.
func TestACardAttestationKeepsTheMarketplaceAndAgentSidesApart(t *testing.T) {
	body := `{"agentId":"a1","canonicalForm":"vtessera/attest/v1",
	  "marketplace":{"attested":true,"valid":true,"keyId":"market-key"},
	  "agent":{"attested":false,"valid":false,"keyId":"a1"}}`
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attestation") {
			_, _ = w.Write([]byte(body))
			return
		}
		healthResponse(w)
	}))

	res := s.call("vtessera_get_card_attestation", map[string]any{"agentId": "a1"})
	var out struct {
		Marketplace vtessera.Side `json:"marketplace"`
		Agent       vtessera.Side `json:"agent"`
	}
	decodeStructured(t, res, &out)
	if !out.Marketplace.Attested || !out.Marketplace.Valid {
		t.Errorf("marketplace = %+v, want attested and valid", out.Marketplace)
	}
	if out.Agent.Attested || out.Agent.Valid {
		t.Errorf("agent = %+v, want reported as unsigned rather than folded into the marketplace's answer", out.Agent)
	}
	if out.Marketplace.KeyID != "market-key" {
		t.Errorf("marketplace keyId = %q, want the signing marketplace named", out.Marketplace.KeyID)
	}
}

// The route is the marketplace's choice, not the caller's, so the response is
// passed through as it arrives.
func TestARouteReportsTheChosenAgentAndWhatItCharges(t *testing.T) {
	result := `{"target_capability":"summarize:document","considered":3,
	  "rejected":{"policy":1},"table_fingerprint":"abc123",
	  "route":{"agent_id":"a1","offer_id":"o1","path":"Squad_Marketplace/a1/offer/o1","cost":10,
	    "cost_amount":"10.00","cost_mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
	    "direction":"ask"}}`
	// /agp/route is JSON-RPC: the answer is inside result, and a refused route
	// arrives in error with HTTP 200.
	body := `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agp/route" {
			if r.Method != http.MethodPost {
				t.Errorf("route called with %s, want POST", r.Method)
			}
			_, _ = w.Write([]byte(body))
			return
		}
		healthResponse(w)
	}))

	res := s.call("vtessera_route_intent", map[string]any{
		"targetCapability": "summarize:document",
		"payload":          map[string]any{"document": "hello"},
	})
	var out vtessera.RouteResult
	decodeStructured(t, res, &out)
	if out.Route.AgentID != "a1" || out.Route.OfferID != "o1" {
		t.Errorf("route = %+v, want agent a1 and offer o1", out.Route)
	}
	if out.Route.CostAmount != "10.00" {
		t.Errorf("cost_amount = %q, want the decimal string", out.Route.CostAmount)
	}
	if out.Considered != 3 || out.Rejected["policy"] != 1 {
		t.Errorf("considered = %d rejected = %v, want the marketplace's own accounting", out.Considered, out.Rejected)
	}
}

// The marketplace refuses a route it cannot satisfy, and it says so inside a
// JSON-RPC error with HTTP 200. A tool that reported that as an empty route would
// be telling a buyer the capability does not exist when it simply is not offered.
func TestARefusedRouteStaysARefusal(t *testing.T) {
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agp/route" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"no agent offers summarize:pdf"}}`))
			return
		}
		healthResponse(w)
	}))

	res := s.callExpectingError("vtessera_route_intent", map[string]any{
		"targetCapability": "summarize:pdf",
		"payload":          map[string]any{},
	})
	if !strings.Contains(resultText(res), "no agent offers summarize:pdf") {
		t.Errorf("the refusal lost the marketplace's reason: %s", resultText(res))
	}
}

// A marketplace that is not answering must not read as an empty result. The
// refusal has to stay a refusal.
func TestAnUnreachableMarketplaceIsAnErrorRatherThanAnEmptyAnswer(t *testing.T) {
	s := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"database is locked","code":"INTERNAL"}`))
	}))
	res := s.callExpectingError("vtessera_search_offers", map[string]any{"capability": "summarize:document"})
	if !strings.Contains(resultText(res), "500") {
		t.Errorf("error = %q, want it to say the marketplace refused", resultText(res))
	}
}

// A misconfigured base URL is refused at startup, not on the first tool call.
func TestABaseURLThatIsNotAnHTTPURLIsRefusedAtStartup(t *testing.T) {
	for _, bad := range []string{"", "   ", "ftp://example.com", "not a url", "http://"} {
		if _, err := vtessera.NewClient(bad, time.Second); err == nil {
			t.Errorf("vtessera.NewClient(%q) was accepted, want a startup refusal", bad)
		}
	}
	if _, err := vtessera.NewClient("http://localhost:8080", 0); err != nil {
		t.Errorf("NewClient with no timeout = %v, want the default applied", err)
	}
}

// The instructions are the only place a client learns that this server cannot
// trade. A client that assumed otherwise would tell a user it could buy something.
func TestTheServerInstructionsSayItCannotTrade(t *testing.T) {
	s := newTestServer(t, healthOnly())
	instructions := strings.ToLower(s.server.Instructions())
	if !strings.Contains(instructions, "read-only") {
		t.Error("the instructions do not say the server is read-only")
	}
	for _, absent := range []string{"register", "publish", "accept", "probe"} {
		if !strings.Contains(instructions, absent) {
			t.Errorf("the instructions do not mention that %s is not available here", absent)
		}
	}
}
