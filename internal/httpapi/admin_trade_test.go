package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

func TestAnOperatorCanReadADisputedTrade(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	trade := disputedTradeOverHTTP(t, server, seller, buyer)

	status, body := adminDo(t, server, http.MethodGet, "/v1/admin/trades/"+trade.ID, adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("read = %d %s, want 200", status, body)
	}
	var got domain.Trade
	decodeInto(t, body, &got)
	if got.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed", got.State)
	}

	// The reason is the point of the view: it lives on the dispute event, which
	// before this route an operator could only reach by opening SQLite directly.
	var reason string
	for _, e := range got.Events {
		if e.Type == domain.EventDisputed {
			reason = string(e.Detail)
		}
	}
	if !strings.Contains(reason, "seller delivered nothing") {
		t.Errorf("dispute event detail = %q, want the buyer's reason", reason)
	}
}

func TestReadingATradeRequiresTheOperatorToken(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	trade := disputedTradeOverHTTP(t, server, seller, buyer)

	if status, _ := adminDo(t, server, http.MethodGet, "/v1/admin/trades/"+trade.ID, "", nil); status != http.StatusUnauthorized {
		t.Errorf("read with no token = %d, want 401", status)
	}
	if status, _ := adminDo(t, server, http.MethodGet, "/v1/admin/trades/"+trade.ID, "not-the-admin-token", nil); status != http.StatusUnauthorized {
		t.Errorf("read with a wrong token = %d, want 401", status)
	}
	// A party's own session is not a substitute. The operator is the one actor
	// who is neither party, which is what lets them review a dispute the two
	// parties cannot agree on.
	if status, body := buyer.raw(http.MethodGet, "/v1/admin/trades/"+trade.ID, nil, true); status == http.StatusOK {
		t.Errorf("an agent session read the operator view: %s", body)
	}
}

func TestTheAdminTradeRouteDoesNotExistWithoutAnAdminToken(t *testing.T) {
	server, _ := buildServer(t, serverBuild{})

	status, _ := adminDo(t, server, http.MethodGet, "/v1/admin/trades/some-trade", adminToken, nil)
	if status != http.StatusNotFound {
		t.Errorf("read with no admin token configured = %d, want 404", status)
	}
}

func TestReadingAnUnknownTradeIsRefused(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken})

	status, body := adminDo(t, server, http.MethodGet, "/v1/admin/trades/no-such-trade", adminToken, nil)
	if status != http.StatusNotFound {
		t.Fatalf("read an unknown trade = %d %s, want 404", status, body)
	}
}
