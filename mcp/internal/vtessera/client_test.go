package vtessera

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	market := httptest.NewServer(handler)
	t.Cleanup(market.Close)
	client, err := NewClient(market.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, market
}

// capture records what the client sent and answers with a fixed body.
func capture(t *testing.T, body string) (http.Handler, *string) {
	t.Helper()
	var sent string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			encoded = append(encoded, buf[:n]...)
			if err != nil {
				break
			}
		}
		sent = string(encoded)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(body))
	}), &sent
}

func TestRoutingSendsAJSONRPCEnvelopeAndUnwrapsTheResult(t *testing.T) {
	handler, sent := capture(t, `{"jsonrpc":"2.0","id":1,"result":{
	  "target_capability":"summarize:document",
	  "route":{"path":"Squad_Marketplace/text-analyzer/offer/o1","cost":0.02,"cost_amount":"20000","cost_mint":"EURC","currency":"EURC"},
	  "considered":3,"rejected":{},"table_fingerprint":"fp","table_as_of":"2026-10-05T00:00:00Z"}}`)
	client, _ := newTestClient(t, handler)

	got, err := client.Route(context.Background(), RouteIntent{
		TargetCapability: "summarize:document",
		Payload:          map[string]any{"document": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Route.CostAmount != "20000" || got.Considered != 3 {
		t.Fatalf("routing returned %+v, want the result the marketplace sent", got)
	}

	var call struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		ID      int    `json:"id"`
		Params  struct {
			TargetCapability string         `json:"target_capability"`
			Payload          map[string]any `json:"payload"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(*sent), &call); err != nil {
		t.Fatalf("the client sent %q: %v", *sent, err)
	}
	if call.JSONRPC != "2.0" || call.Method != "agp/route" || call.ID == 0 {
		t.Errorf("the call was not a JSON-RPC request: %+v", call)
	}
	if call.Params.TargetCapability != "summarize:document" {
		t.Errorf("the intent was not inside params: %+v", call)
	}
	if call.Params.Payload["document"] != "hello" {
		t.Errorf("the payload was lost: %+v", call.Params.Payload)
	}
}

// A capability that takes no arguments still needs an object, because the
// marketplace refuses a missing payload outright.
func TestRoutingSendsAnEmptyPayloadWhenTheCallerHasNone(t *testing.T) {
	handler, sent := capture(t, `{"jsonrpc":"2.0","id":1,"result":{"target_capability":"translate","considered":1}}`)
	client, _ := newTestClient(t, handler)

	if _, err := client.Route(context.Background(), RouteIntent{TargetCapability: "translate"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*sent, `"payload":{}`) {
		t.Errorf("the call omitted payload, which the marketplace refuses: %s", *sent)
	}
}

// The service answers a refused route with HTTP 200 and an error object, so a
// client that only checks the status would report an empty route.
func TestARefusedRouteIsReportedRatherThanAnEmptyRoute(t *testing.T) {
	handler, _ := capture(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"no agent offers summarize:pdf"}}`)
	client, _ := newTestClient(t, handler)

	got, err := client.Route(context.Background(), RouteIntent{TargetCapability: "summarize:pdf"})
	if err == nil {
		t.Fatalf("routing returned %+v, want the marketplace's refusal", got)
	}
	if !strings.Contains(err.Error(), "no agent offers summarize:pdf") {
		t.Errorf("the refusal lost the marketplace's reason: %v", err)
	}
}

// A refusal keeps its own status code, because the service distinguishes a missing
// agent from a spent cap and a caller needs to tell them apart.
func TestANotFoundKeepsItsStatus(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"agent not found","code":"NOT_FOUND"}`))
	}))

	_, err := client.Agent(context.Background(), "nobody")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("looking up the agent returned %v, want an APIError", err)
	}
	if apiErr.Status != http.StatusNotFound {
		t.Errorf("status was %d, want 404", apiErr.Status)
	}
	if apiErr.Code() != "NOT_FOUND" {
		t.Errorf("code was %q, want NOT_FOUND", apiErr.Code())
	}
}

// A body larger than the cap is refused rather than arriving truncated, because a
// limit whose failure depends on where it lands is not a limit.
func TestAnOversizedBodyIsRefused(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"offers":[` + strings.Repeat(`{"id":"a"},`, (maxBody/9)+10) + `{"id":"b"}]}`))
	}))

	if _, err := client.SearchOffers(context.Background(), OfferQuery{}); err == nil {
		t.Fatal("an oversized body was accepted")
	}
}

func TestTheClientRefusesABaseURLThatIsNotHTTP(t *testing.T) {
	if _, err := NewClient("ftp://example.com", time.Second); err == nil {
		t.Fatal("a non-HTTP base URL was accepted")
	}
}
