package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// A cap that only exists in the trade service is not enforced by anything the
// agent can reach, so the refusal has to come back through the API with a code an
// agent can act on.
func TestATradeOverTheCapIsRefusedThroughTheAPI(t *testing.T) {
	server, _ := setupServerWithCaps(t, limits.DefaultPolicy(limits.DefaultRates()))
	seller := newAgent(t, server)
	buyer := newAgent(t, server)

	offer := seller.publishOffer("25.00", usdc, "summarize:document")
	status, body := buyer.raw(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "over-cap",
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("POST /v1/trades over the cap = %d %s, want 409", status, body)
	}
	got := decode(t, body)
	if got["code"] != "SPEND_CAP_EXCEEDED" {
		t.Errorf("code = %v, want SPEND_CAP_EXCEEDED", got["code"])
	}
}

func TestARaisedCapLetsTheSameTradeThrough(t *testing.T) {
	policy := limits.DefaultPolicy(limits.DefaultRates()).
		WithCeilings(money.MustParse("50.00"), money.MustParse("200.00"))
	server, _ := setupServerWithCaps(t, policy)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)

	offer := seller.publishOffer("25.00", usdc, "summarize:document")
	payload := map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "raised",
	}
	if status, body := buyer.raw(http.MethodPost, "/v1/trades", payload, true); status != http.StatusConflict {
		t.Fatalf("trade over the default cap = %d %s, want 409", status, body)
	}

	buyer.do(http.MethodPut, "/v1/limits", map[string]any{"perTradeUsd": "50.00", "perDayUsd": "200.00"}, true)

	// Same buyer, same offer, after opting in.
	if status, body := buyer.raw(http.MethodPost, "/v1/trades", payload, true); status != http.StatusCreated {
		t.Fatalf("trade after the raise = %d %s, want 201", status, body)
	}

	// A different agent still cannot.
	other := newAgent(t, server)
	if status, body := other.raw(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "other",
	}, true); status != http.StatusConflict {
		t.Fatalf("a second agent's trade = %d %s, want 409: the raise is not shared", status, body)
	}
}

func TestAnOfferInAnUnpricedCurrencyIsRefused(t *testing.T) {
	server, _ := setupServerWithCaps(t, limits.DefaultPolicy(limits.DefaultRates()))

	// The card has to accept the currency first, so the refusal below is about
	// the rate and not about the card.
	const unknownMint = "9n4xM2fPP91Zm3fDc7rT2QkWrB1sRdVvNcR6cMDcyGDQ"
	seller := newAgent(t, server, func(o *clientOptions) {
		o.card.Currencies = []string{usdc, unknownMint}
	})
	status, body := seller.raw(http.MethodPost, "/v1/agents/"+seller.id+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "priced in a currency with no declared rate",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "1.00",
		"priceMint":       unknownMint,
		"settlementModes": []string{"offchain"},
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("offer in an unpriced mint = %d %s, want 409", status, body)
	}
	if code := decode(t, body)["code"]; code != "MINT_UNPRICED" {
		t.Errorf("code = %v, want MINT_UNPRICED", code)
	}
}

func TestHealthzAdvertisesSandbox(t *testing.T) {
	server, _ := setupServer(t)
	// The default harness names a cluster and is not a sandbox.
	got := healthz(t, server)
	if _, present := got["sandbox"]; present {
		t.Errorf("healthz reports sandbox on a normal deployment: %v", got)
	}
	if got["settlementTier"] != "beta" {
		t.Errorf("settlementTier = %v, want beta: on-chain settlement is advertised wherever it is enabled", got["settlementTier"])
	}
}

func TestASandboxDeploymentSaysSo(t *testing.T) {
	server, _ := setupSandboxServer(t)
	got := healthz(t, server)
	if got["sandbox"] != true {
		t.Errorf("sandbox = %v, want true", got["sandbox"])
	}
	// A sandbox is not allowed to settle on chain, so it must not advertise the
	// settlement tier either: an agent reading healthz has to be able to conclude
	// that no real value moves here.
	if _, present := got["settlementTier"]; present {
		t.Errorf("a sandbox deployment advertises a settlement tier: %v", got)
	}
	if _, present := got["cluster"]; present {
		t.Errorf("a sandbox deployment advertises a cluster: %v", got)
	}
}

// The token list is the document an agent reads to decide what it can buy, so the
// maturity label rides there as well as on /healthz. An agent that reads only the
// capabilities must not have to make a test trade to find out that the
// on-chain path here is beta.
func TestTheTokenListCarriesTheSettlementTier(t *testing.T) {
	server, _ := setupServer(t)
	listed := tokensList(t, server)
	if listed["settlementTier"] != "beta" {
		t.Errorf("token list settlementTier = %v, want beta", listed["settlementTier"])
	}
}

// A sandbox governs no mints, so its token list is not configured. That is the
// honest answer: there is nothing here an agent can settle on chain, and a list
// of currencies beside a settlement tier would suggest otherwise.
func TestASandboxHasNoTokensToList(t *testing.T) {
	server, _ := setupSandboxServer(t)
	resp, err := server.Client().Get(server.URL + "/v1/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET /v1/tokens on a sandbox = %d, want 501", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["code"] != "NOT_CONFIGURED" {
		t.Errorf("code = %v, want NOT_CONFIGURED", out["code"])
	}
}

func tokensList(t *testing.T, server *httptest.Server) map[string]any {
	t.Helper()
	resp, err := server.Client().Get(server.URL + "/v1/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/tokens = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode token list: %v", err)
	}
	return out
}

func TestAQuietDeploymentAdvertisesNeitherSettlementNorSandbox(t *testing.T) {
	// The unconfigured harness is what an operator gets with no cluster and no
	// endpoint. It must not claim to settle, and must not claim to be a sandbox:
	// it is simply not configured for on-chain.
	server, _ := setupServerAt(t, "", unconfigured{})
	got := healthz(t, server)
	if _, present := got["settlementTier"]; present {
		t.Errorf("healthz advertises settlement on a deployment with no cluster: %v", got)
	}
	if _, present := got["sandbox"]; present {
		t.Errorf("healthz advertises sandbox on an ordinary deployment: %v", got)
	}
}
func healthz(t *testing.T, server *httptest.Server) map[string]any {
	t.Helper()
	resp, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode healthz %s: %v", body, err)
	}
	return out
}
