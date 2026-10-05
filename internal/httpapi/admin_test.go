package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

const adminToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

// adminServer is the harness with an operator token, plus a helper for calling
// the admin routes. Every test in this file differs only by how it is
// authenticated or what state the marketplace is in, so the setup is shared.
func adminServer(t *testing.T) (*httptest.Server, *agentClient) {
	t.Helper()
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	return server, newAgent(t, server)
}

// adminDo issues an admin request with an arbitrary bearer token. It does not go
// through agentClient on purpose: an agent client signs a challenge and gets a
// session, and the claim under test is that an agent session is not accepted here.
func adminDo(t *testing.T, server *httptest.Server, method, path, token string, payload map[string]any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestAnOperatorCanRetireAListingAndTheRecordSaysWhy(t *testing.T) {
	server, seller := adminServer(t)
	seller.publishOffer("10.00", usdc)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken,
		map[string]any{"reason": "probe left behind by an acceptance test"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s, want 200", status, body)
	}
	var agent domain.Agent
	decodeInto(t, body, &agent)
	if agent.Status != domain.AgentRetired {
		t.Errorf("status = %s, want retired", agent.Status)
	}

	// The reason is not decoration. It is the only account of the withdrawal the
	// operator and the agent will ever have, so it has to be retrievable.
	status, body = adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("read retirement = %d %s, want 200", status, body)
	}
	var retirement domain.Retirement
	decodeInto(t, body, &retirement)
	if retirement.Reason != "probe left behind by an acceptance test" {
		t.Errorf("reason = %q, want it recorded verbatim", retirement.Reason)
	}
	if retirement.Actor == "" {
		t.Error("actor is empty: a privileged action with no accountable name is not auditable")
	}
	if retirement.RestoredAt != nil {
		t.Error("restoredAt is set on a retirement nobody restored")
	}
}

func TestRetiringClosesTheOffersSoNothingKeepsTakingBuyers(t *testing.T) {
	server, seller := adminServer(t)
	offer := seller.publishOffer("10.00", usdc)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{"reason": "withdrawn"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s", status, body)
	}

	// An agent marked retired whose offers are still open is a listing the
	// marketplace promised to hide and did not.
	var got domain.Offer
	decodeInto(t, seller.do(http.MethodGet, "/v1/offers/"+offer.ID, nil, false), &got)
	if got.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want closed", got.Status)
	}

	// And it must be gone from discovery, or a buyer would still be routed to a
	// seller the operator withdrew.
	search := decode(t, seller.do(http.MethodGet, "/v1/offers", nil, false))
	if entries, ok := search["offers"].([]any); ok {
		for _, e := range entries {
			if m, ok := e.(map[string]any); ok && m["id"] == offer.ID {
				t.Error("a closed offer is still returned by discovery")
			}
		}
	}
}

func TestRetiringDoesNotDeleteWhatABuyerAlreadyHolds(t *testing.T) {
	server, seller := adminServer(t)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	var created domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId": offer.ID, "settlementMode": "offchain", "idempotencyKey": "retire-1",
	}, true), &created)
	buyer.do(http.MethodPost, "/v1/trades/"+created.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+created.ID+"/accept", map[string]any{}, true)
	buyer.do(http.MethodPost, "/v1/trades/"+created.ID+"/accept", map[string]any{}, true)
	var recorded struct {
		Tessera struct {
			JWS             string `json:"jws"`
			VerificationKey string `json:"verificationKey"`
		} `json:"tessera"`
	}
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades/"+created.ID+"/record", map[string]any{}, true), &recorded)
	if recorded.Tessera.JWS == "" {
		t.Fatal("no tessera issued")
	}

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{"reason": "seller left the market"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s", status, body)
	}

	// The tessera a buyer already holds has to keep verifying. Retiring is a
	// status change; if it invalidated issued receipts then withdrawing a seller
	// would be destroying evidence a buyer needs.
	// The payload carries the JWS and the verified claims rather than a receipt
	// struct, so the assertion is on the claim naming the trade. If verification
	// stopped working the claims block would be missing entirely, which is why
	// this reads the claims rather than a status code alone.
	var payload struct {
		JWS             string         `json:"jws"`
		VerificationKey string         `json:"verificationKey"`
		Claims          map[string]any `json:"claims"`
	}
	decodeInto(t, buyer.do(http.MethodGet, "/v1/tesseras/"+created.ID, nil, true), &payload)
	if payload.JWS == "" {
		t.Fatal("no tessera returned for a recorded trade against a retired seller")
	}
	if payload.VerificationKey == "" {
		t.Error("no verification key: the buyer cannot check the tessera")
	}
	if got, _ := payload.Claims["trade"].(map[string]any)["tradeId"].(string); got != created.ID {
		t.Errorf("claims name trade %q, want %s", got, created.ID)
	}
}

func TestRetirementIsRefusedWhileATradeIsLive(t *testing.T) {
	server, seller := adminServer(t)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	var created domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId": offer.ID, "settlementMode": "offchain", "idempotencyKey": "live-1",
	}, true), &created)
	buyer.do(http.MethodPost, "/v1/trades/"+created.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+created.ID+"/accept", map[string]any{}, true)

	// Withdrawing a listing is a statement about future business. A buyer holding
	// an open trade is existing business, and its counterparty is about to stop
	// being reachable.
	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{"reason": "withdrawn"})
	if status != http.StatusConflict {
		t.Fatalf("retire with a live trade = %d %s, want 409", status, body)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &refusal)
	if refusal.Code != "AGENT_HAS_LIVE_TRADES" {
		t.Errorf("code = %s, want AGENT_HAS_LIVE_TRADES", refusal.Code)
	}

	// No record was written either, so the audit log does not claim a withdrawal
	// that did not happen.
	status, body = adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", adminToken, nil)
	if status == http.StatusOK {
		t.Errorf("a refused retirement left a record: %s", body)
	}
	var got domain.Agent
	decodeInto(t, seller.do(http.MethodGet, "/v1/agents/"+seller.id, nil, false), &got)
	if got.Status == domain.AgentRetired {
		t.Error("the agent was retired despite the refusal")
	}
}

func TestARetirementNeedsAReason(t *testing.T) {
	server, seller := adminServer(t)

	// A privileged action that removes somebody's ability to sell, taken without
	// a stated reason, is the version of this that is hard to defend afterwards.
	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("retire with no reason = %d %s, want 400", status, body)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &refusal)
	if refusal.Code != "REASON_REQUIRED" {
		t.Errorf("code = %s, want REASON_REQUIRED", refusal.Code)
	}
}

func TestAnAgentSessionCannotRetireAnotherAgent(t *testing.T) {
	server, seller := adminServer(t)

	// The whole point of a separate credential. If an agent's own session worked
	// here, every agent on the marketplace could withdraw every other one.
	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", "not-the-operator-token",
		map[string]any{"reason": "hostile"})
	if status != http.StatusUnauthorized {
		t.Errorf("retire with a wrong token = %d %s, want 401", status, body)
	}
	status, _ = adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", "", map[string]any{"reason": "hostile"})
	if status != http.StatusUnauthorized {
		t.Errorf("retire with no token = %d, want 401", status)
	}

	var got domain.Agent
	decodeInto(t, seller.do(http.MethodGet, "/v1/agents/"+seller.id, nil, false), &got)
	if got.Status == domain.AgentRetired {
		t.Error("the agent was retired by an unauthenticated request")
	}
}

func TestRetiringIsReversible(t *testing.T) {
	server, seller := adminServer(t)
	seller.publishOffer("10.00", usdc)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{"reason": "mistake"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s", status, body)
	}
	status, body = adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/restore", adminToken, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("restore = %d %s", status, body)
	}
	var agent domain.Agent
	decodeInto(t, body, &agent)
	if agent.Status != domain.AgentActive {
		t.Errorf("status = %s, want active", agent.Status)
	}

	// The record of the withdrawal stays, with the reversal on it. An audit trail
	// that deletes itself when the action is undone is not a trail.
	var retirement domain.Retirement
	decodeInto(t, mustGetRetirement(t, server, seller.id), &retirement)
	if retirement.RestoredAt == nil {
		t.Error("restoredAt is nil after a restore: the audit record lost the reversal")
	}
	if retirement.Reason != "mistake" {
		t.Errorf("reason = %q, want the original reason to survive the restore", retirement.Reason)
	}
}

func TestTheAdminRoutesAreAbsentWithoutAToken(t *testing.T) {
	_, seller := adminServer(t)
	serverNoAdmin, _ := buildServer(t, serverBuild{})

	// Not 401 and not 403: a deployment that has not configured an operator token
	// has no retirement capability, and a capability it does not have should not
	// be discoverable from the outside.
	status, _ := adminDo(t, serverNoAdmin, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", adminToken, map[string]any{"reason": "x"})
	if status != http.StatusNotFound {
		t.Errorf("retire on a deployment with no admin token = %d, want 404", status)
	}
}

func mustGetRetirement(t *testing.T, server *httptest.Server, id string) []byte {
	t.Helper()
	status, body := adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+id+"/retirement", adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("read retirement = %d %s", status, body)
	}
	return body
}
