package main

// The hosted routing rules are tested here because they are easy to get wrong in a
// way no unit test below would notice: the discovery document, the health probe and
// the MCP transport all live under /mcp, and a pattern that shadowed another would
// leave a directory probing a dead endpoint or a client reading the probe line
// where it expected JSON.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/mcp/internal/mcpserver"
	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
)

func testMux(t *testing.T) http.Handler {
	t.Helper()
	client, err := vtessera.NewClient("http://127.0.0.1:0", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return newMux(mcpserver.New(client, "https://vtessera.fly.dev"))
}

// A discovering client reads the card as JSON, and it is told to connect back to
// the host it fetched it from.
func TestTheServerCardIsServedAsJSONAtTheTransportPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://vtessera-mcp.fly.dev/mcp/server-card", nil)
	req.Host = "vtessera-mcp.fly.dev"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	testMux(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q, want JSON", ct)
	}
	var card mcpserver.ServerCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("the card is not JSON: %v", err)
	}
	if card.Name != mcpserver.CardName {
		t.Errorf("card names %q, want %q", card.Name, mcpserver.CardName)
	}
	if len(card.Remotes) != 1 || card.Remotes[0].URL != "https://vtessera-mcp.fly.dev/mcp" {
		t.Errorf("card remotes = %+v, want the request's own host", card.Remotes)
	}
}

// The MCP transport is POST-only, but a directory probes it with GET, so GET must
// answer a status line rather than the handler's rejection.
func TestTheMCPPathAnswersAProbeWithGET(t *testing.T) {
	rec := httptest.NewRecorder()
	testMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://vtessera-mcp.fly.dev/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "POST /mcp") {
		t.Errorf("body %q, want the probe status line", rec.Body.String())
	}
}

// A POST to the transport is the MCP handler's, not the probe's: it must not come
// back with the plain-text status line a directory would read as success.
func TestAPostToTheMCPPathIsNotTheProbe(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://vtessera-mcp.fly.dev/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testMux(t).ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "vtessera-mcp is serving") {
		t.Errorf("a POST to the transport got the probe line instead of the MCP handler: %q", rec.Body.String())
	}
}

// Every other path is a probe, so a directory that does not know the transport URL
// still learns the process is up; a non-GET there is a miss, not a probe.
func TestUnknownPathsProbeOnGETAndMissOtherwise(t *testing.T) {
	rec := httptest.NewRecorder()
	testMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://vtessera-mcp.fly.dev/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz gave %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	testMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "https://vtessera-mcp.fly.dev/healthz", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /healthz gave %d, want 404", rec.Code)
	}
}
