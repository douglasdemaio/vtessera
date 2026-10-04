package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// mustGet fetches a public read and returns the status and body, since a refused
// read is itself the thing several of these assertions are about.
func mustGet(t *testing.T, server *httptest.Server, path string) []byte {
	t.Helper()
	status, body := mustDo(t, server, http.MethodGet, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, status, body)
	}
	return body
}

func mustGet2(t *testing.T, server *httptest.Server, path string) (int, []byte) {
	t.Helper()
	return mustDo(t, server, http.MethodGet, path)
}

func mustDo(t *testing.T, server *httptest.Server, method, path string) (int, []byte) {
	t.Helper()
	resp, err := server.Client().Do(mustRequest(t, server, method, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func mustRequest(t *testing.T, server *httptest.Server, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// An agent ID is an Ed25519 public key, and a session proves control of it, so an
// agent cannot authenticate as somebody else. It could still name somebody else
// in the path. These are the two routes where it could, and each one lets an
// authenticated agent write to another agent's record: the card it is listed
// under, and the offers it sells.
//
// Both refusals are 403. A mismatch is not a malformed request, which is 400, and
// it is not absent authentication, which is 401: the caller is well identified
// and is asking to write to somebody else's record.

func TestAnAgentCannotRewriteAnotherAgentsCard(t *testing.T) {
	server, _ := setupServer(t)
	victim := newAgent(t, server)
	attacker := newAgent(t, server)

	// The victim is registered with a card that says one thing.
	var before domain.Agent
	decodeInto(t, mustGet(t, server, "/v1/agents/"+url.PathEscape(victim.id)), &before)

	// The attacker names the victim in the path and supplies a card claiming to
	// be the victim. The session is the attacker's own.
	status, body := attacker.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(victim.id)+"/card", map[string]any{
		"name":        "the victim's new name",
		"description": "this text was written by somebody else",
		"url":         "https://attacker.example.com",
		"version":     "9.9.9",
		"skills":      []map[string]string{{"id": "anything", "name": "Anything"}},
		"publicKey":   victim.id,
		"currencies":  []string{usdc},
	}, true)
	if status != http.StatusForbidden {
		t.Fatalf("PUT another agent's card = %d %s, want 403", status, body)
	}
	if got := decode(t, body); got["code"] != "FORBIDDEN" {
		t.Errorf("code = %v, want FORBIDDEN", got["code"])
	}

	var after domain.Agent
	decodeInto(t, mustGet(t, server, "/v1/agents/"+url.PathEscape(victim.id)), &after)
	if after.Card.Name != before.Card.Name {
		t.Errorf("the victim's card name is now %q, was %q", after.Card.Name, before.Card.Name)
	}
	if after.Card.URL != before.Card.URL {
		t.Errorf("the victim's card URL is now %q, was %q", after.Card.URL, before.Card.URL)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("the victim's card was touched: updatedAt moved from %s to %s", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestAnAgentCannotPublishOffersUnderAnotherAgentsID(t *testing.T) {
	server, _ := setupServer(t)
	victim := newAgent(t, server)
	attacker := newAgent(t, server)

	status, body := attacker.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(victim.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "cheap summarising, posted under somebody else's name",
		"capabilities":    []string{"summarize"},
		"priceAmount":     "0.01",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain"},
	}, true)
	if status != http.StatusForbidden {
		t.Fatalf("POST another agent's offers = %d %s, want 403", status, body)
	}

	// Nothing was listed under the victim.
	status, body = mustDo(t, server, http.MethodGet, "/v1/agents/"+url.PathEscape(victim.id)+"/offers")
	if status != http.StatusOK {
		t.Fatalf("GET the victim's offers = %d %s", status, body)
	}
	if listed := decode(t, body); len(listed["offers"].([]any)) != 0 {
		t.Errorf("the victim has %v offers, want none", listed["offers"])
	}
}

// Closing an offer is the third ownership check, and it lives in the service
// rather than the handler because the offer names its owner in a column. It is
// here so the three are known to agree: a route that checks ownership in one
// place and not another is how the two above were missed.
func TestAnAgentCannotCloseAnotherAgentsOffer(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	attacker := newAgent(t, server)
	offer := seller.publishOffer("1.00", usdc, "summarize")

	status, body := attacker.raw(http.MethodPost, "/v1/offers/"+url.PathEscape(offer.ID)+"/close", map[string]any{}, true)
	if status != http.StatusForbidden {
		t.Fatalf("POST another agent's offer close = %d %s, want 403", status, body)
	}

	// Still open, and still listed under its owner.
	status, body = mustGet2(t, server, "/v1/offers/"+url.PathEscape(offer.ID))
	if status != http.StatusOK {
		t.Fatalf("GET the offer = %d %s", status, body)
	}
	var after domain.Offer
	decodeInto(t, body, &after)
	if after.Status != domain.OfferOpen {
		t.Errorf("the offer is %s, want still open", after.Status)
	}
	if after.AgentID != seller.id {
		t.Errorf("the offer belongs to %s, want %s", after.AgentID, seller.id)
	}
}

// An agent still writes to its own record. A check that refuses mismatches is
// only safe if the matching case keeps working, and the whole marketplace is the
// matching case.
func TestAnAgentCanStillWriteToItsOwnCardAndOffers(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	status, body := agent.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agent.id)+"/card", map[string]any{
		"name":       "renamed by its owner",
		"url":        "https://trader.example.com",
		"version":    "0.2.0",
		"publicKey":  agent.id,
		"currencies": []string{usdc},
	}, true)
	if status != http.StatusOK {
		t.Fatalf("PUT own card = %d %s, want 200", status, body)
	}
	status, body = agent.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(agent.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "summarise a document",
		"capabilities":    []string{"summarize"},
		"priceAmount":     "1.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain"},
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("POST own offers = %d %s, want 201", status, body)
	}
}

// The check has to be on the identity in the path, not on the request body, so a
// card body claiming somebody else's key is refused as well. Register refuses it
// on the card's own public key; this is the route in front of that.
func TestACardClaimingAnotherKeyIsRefusedEvenOnTheOwnersOwnRoute(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	other := newAgent(t, server)

	status, body := agent.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agent.id)+"/card", map[string]any{
		"name":       "not me",
		"url":        "https://trader.example.com",
		"version":    "0.1.0",
		"publicKey":  other.id,
		"currencies": []string{usdc},
	}, true)
	if status == http.StatusOK {
		t.Fatalf("PUT a card claiming another agent's key = 200 %s, want a refusal", body)
	}
	if !strings.Contains(strings.ToUpper(string(body)), "PUBLICKEY") {
		t.Errorf("body = %s, want it to name the mismatched public key", body)
	}
}
