package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// disputedTradeOverHTTP walks a fresh trade to disputed through the public
// routes, which is the only state the operator resolve route can act on.
func disputedTradeOverHTTP(t *testing.T, server *httptest.Server, seller, buyer *agentClient) domain.Trade {
	t.Helper()
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	var trade domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
	}, true), &trade)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/dispute", map[string]any{
		"reason": "seller delivered nothing",
	}, true)
	return trade
}

func TestAnOperatorCanResolveADisputedTrade(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	trade := disputedTradeOverHTTP(t, server, seller, buyer)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/"+trade.ID+"/resolve", adminToken,
		map[string]any{"outcome": "released", "reason": "seller never delivered"})
	if status != http.StatusOK {
		t.Fatalf("resolve = %d %s, want 200", status, body)
	}
	var resolved domain.Trade
	decodeInto(t, body, &resolved)
	if resolved.State != domain.TradeResolved {
		t.Fatalf("state = %s, want resolved", resolved.State)
	}

	// The dispute is closed, but it is not erased: the public count is a record
	// that a dispute happened, so a review cannot bury the ones it lost.
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	var m domain.UsageMetrics
	decodeInto(t, anon.do(http.MethodGet, "/v1/metrics", nil, false), &m)
	if m.Totals.Disputed != 1 {
		t.Errorf("disputed = %d after resolution, want it still counted", m.Totals.Disputed)
	}
}

func TestResolvingATradeRequiresTheOperatorToken(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	trade := disputedTradeOverHTTP(t, server, seller, buyer)

	if status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/"+trade.ID+"/resolve", "",
		map[string]any{"outcome": "released"}); status != http.StatusUnauthorized {
		t.Errorf("resolve with no token = %d, want 401", status)
	}
	if status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/"+trade.ID+"/resolve", "not-the-admin-token",
		map[string]any{"outcome": "released"}); status != http.StatusUnauthorized {
		t.Errorf("resolve with a wrong token = %d, want 401", status)
	}
	// A party's own session is not a substitute: an agent that could resolve its
	// own dispute could undo the one filed against it.
	if status, body := buyer.raw(http.MethodPost, "/v1/admin/trades/"+trade.ID+"/resolve",
		map[string]any{"outcome": "released"}, true); status == http.StatusOK {
		t.Errorf("an agent session resolved a dispute: %s", body)
	}
}

func TestTheResolveRouteDoesNotExistWithoutAnAdminToken(t *testing.T) {
	server, _ := buildServer(t, serverBuild{})

	status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/some-trade/resolve", adminToken,
		map[string]any{"outcome": "released"})
	if status != http.StatusNotFound {
		t.Errorf("resolve with no admin token configured = %d, want 404", status)
	}
}

func TestResolvingWithAnUnknownOutcomeIsRefused(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	trade := disputedTradeOverHTTP(t, server, seller, buyer)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/"+trade.ID+"/resolve", adminToken,
		map[string]any{"reason": "no outcome given"})
	if status != http.StatusBadRequest {
		t.Fatalf("resolve with no outcome = %d %s, want 400", status, body)
	}
	if code := decode(t, body)["code"]; code != "INVALID_REQUEST" {
		t.Errorf("code = %v, want INVALID_REQUEST", code)
	}
}

func TestResolvingATradeThatIsNotDisputedIsRefused(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	var trade domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
	}, true), &trade)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/trades/"+trade.ID+"/resolve", adminToken,
		map[string]any{"outcome": "released"})
	if status != http.StatusConflict {
		t.Fatalf("resolve on a proposed trade = %d %s, want 409", status, body)
	}
	if code := decode(t, body)["code"]; code != "ILLEGAL_STATE" {
		t.Errorf("code = %v, want ILLEGAL_STATE", code)
	}
}
