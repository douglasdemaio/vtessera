package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// capsServer is the standard harness with spending caps wired and the ceilings
// set high enough that a raise is permitted, so a test that wants a refusal can
// get one by asking for too much rather than by the deployment forbidding raises.
func capsServer(t *testing.T, adjust func(limits.Policy) limits.Policy) (*httptest.Server, *agentClient) {
	t.Helper()
	policy := limits.DefaultPolicy(limits.DefaultRates()).
		WithCeilings(money.MustParse("50.00"), money.MustParse("200.00"))
	if adjust != nil {
		policy = adjust(policy)
	}
	server, _ := setupServerWithCaps(t, policy)
	return server, newAgent(t, server)
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestAnAgentCanReadItsOwnCaps(t *testing.T) {
	_, agent := capsServer(t, nil)

	body := agent.do(http.MethodGet, "/v1/limits", nil, true)
	got := decode(t, body)
	if got["perTradeUsd"] != "10.00" {
		t.Errorf("perTradeUsd = %v, want 10.00", got["perTradeUsd"])
	}
	if got["perDayUsd"] != "10.00" {
		t.Errorf("perDayUsd = %v, want 10.00", got["perDayUsd"])
	}
	if got["currency"] != "USD" {
		t.Errorf("currency = %v, want USD: an agent has to know what the caps are denominated in", got["currency"])
	}
	if got["raised"] != false {
		t.Errorf("raised = %v, want false for an agent that has not opted in", got["raised"])
	}
	// The ceilings are reported, so an agent can find out what it could ask for
	// instead of discovering the limit by being refused.
	if got["maxPerTradeUsd"] != "50.00" {
		t.Errorf("maxPerTradeUsd = %v, want the configured ceiling 50.00", got["maxPerTradeUsd"])
	}
}

func TestReadingCapsRequiresASession(t *testing.T) {
	_, agent := capsServer(t, nil)

	// The caps belong to an agent, so there is nothing to report to an anonymous
	// caller rather than a default set that might be mistaken for a fact.
	status, body := agent.raw(http.MethodGet, "/v1/limits", nil, false)
	if status == http.StatusOK {
		t.Fatalf("GET /v1/limits without a session = %d %s", status, body)
	}
}

func TestRaisingCapsAppliesToTheCallerOnly(t *testing.T) {
	server, agent := capsServer(t, nil)

	body := agent.do(http.MethodPut, "/v1/limits", map[string]any{
		"perTradeUsd": "25.00",
		"perDayUsd":   "100.00",
	}, true)
	got := decode(t, body)
	if got["perTradeUsd"] != "25.00" || got["perDayUsd"] != "100.00" {
		t.Fatalf("PUT /v1/limits returned %v/%v, want the values asked for", got["perTradeUsd"], got["perDayUsd"])
	}
	if got["raised"] != true {
		t.Errorf("raised = %v, want true after opting in", got["raised"])
	}

	// And it persists: a second read reports the raise, because an agent that
	// asked for a limit and got a success has to be able to rely on it.
	got = decode(t, agent.do(http.MethodGet, "/v1/limits", nil, true))
	if got["perTradeUsd"] != "25.00" {
		t.Errorf("perTradeUsd after the raise = %v, want 25.00", got["perTradeUsd"])
	}

	// Somebody else's agent is unaffected.
	other := newAgent(t, server)
	got = decode(t, other.do(http.MethodGet, "/v1/limits", nil, true))
	if got["perTradeUsd"] != "10.00" || got["raised"] != false {
		t.Errorf("a second agent reads %v raised=%v, want the deployment default", got["perTradeUsd"], got["raised"])
	}
}

func TestRaisingAboveTheCeilingIsRefusedWithTheCeilingNamed(t *testing.T) {
	_, agent := capsServer(t, nil)

	status, body := agent.raw(http.MethodPut, "/v1/limits", map[string]any{
		"perTradeUsd": "500.00",
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("PUT /v1/limits above the ceiling = %d %s, want 409", status, body)
	}
	got := decode(t, body)
	if got["code"] != "CAP_ABOVE_CEILING" {
		t.Errorf("code = %v, want CAP_ABOVE_CEILING", got["code"])
	}
	message, _ := got["error"].(string)
	if !strings.Contains(message, "50.00") {
		t.Errorf("error %q does not name the ceiling the agent hit", message)
	}

	// Refused, not clamped: the agent's caps are still the defaults.
	got = decode(t, agent.do(http.MethodGet, "/v1/limits", nil, true))
	if got["perTradeUsd"] != "10.00" {
		t.Errorf("perTradeUsd after a refused raise = %v, want the unchanged 10.00", got["perTradeUsd"])
	}
}

func TestAMalformedRaiseIsRefused(t *testing.T) {
	_, agent := capsServer(t, nil)

	for _, payload := range []map[string]any{
		{"perTradeUsd": "lots"},
		{"perTradeUsd": "-5.00"},
		{"perTradeUsd": 25},
		{"perDayUsd": ""},
		{"somethingElse": "1.00"},
	} {
		status, body := agent.raw(http.MethodPut, "/v1/limits", payload, true)
		if status != http.StatusBadRequest {
			t.Errorf("PUT /v1/limits %v = %d %s, want 400", payload, status, body)
		}
	}
}

// A misspelled field is the failure mode this route cannot afford to be lenient
// about: the request would otherwise succeed and change nothing, and the agent
// would go on trading under a cap it did not choose.
func TestARaiseWithAMisspelledFieldIsRefusedRatherThanIgnored(t *testing.T) {
	_, agent := capsServer(t, nil)
	for _, payload := range []string{
		`{"perTradeAmount":"25.00"}`,
		`{"perTradeUsd":"25.00","perDay":"200.00"}`,
	} {
		status, body := agent.rawBody(http.MethodPut, "/v1/limits", payload, true)
		if status != http.StatusBadRequest {
			t.Errorf("PUT /v1/limits %s = %d %s, want 400", payload, status, body)
		}
	}
	// Nothing changed: the caps still in force are the deployment defaults.
	status, body := agent.raw(http.MethodGet, "/v1/limits", nil, true)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/limits = %d %s", status, body)
	}
	if after := decode(t, body); after["perTradeUsd"] != "10.00" {
		t.Errorf("perTradeUsd = %v after a refused raise, want the 10.00 default", after["perTradeUsd"])
	}
}

func TestTwoJSONDocumentsInOneRaiseAreRefused(t *testing.T) {
	_, agent := capsServer(t, nil)
	status, body := agent.rawBody(http.MethodPut, "/v1/limits",
		`{"perTradeUsd":"25.00"}{"perTradeUsd":"0.01"}`, true)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT /v1/limits with two documents = %d %s, want 400", status, body)
	}
	status, body = agent.raw(http.MethodGet, "/v1/limits", nil, true)
	if after := decode(t, body); after["perTradeUsd"] != "10.00" {
		t.Errorf("perTradeUsd = %v after a two-document raise, want the 10.00 default", after["perTradeUsd"])
	}
}

func TestCapsEndpointsReportWhenCapsAreNotConfigured(t *testing.T) {
	// A deployment built without caps must say so rather than reporting zeros,
	// which an agent could read as "nothing is allowed".
	base, _ := setupServerAt(t, "")
	agent := newAgent(t, base)
	status, body := agent.raw(http.MethodGet, "/v1/limits", nil, true)
	if status != http.StatusNotImplemented {
		t.Fatalf("GET /v1/limits with no policy = %d %s, want 501", status, body)
	}
	got := decode(t, body)
	if got["code"] != "LIMITS_UNCONFIGURED" {
		t.Errorf("code = %v, want LIMITS_UNCONFIGURED", got["code"])
	}
	status, _ = agent.raw(http.MethodPut, "/v1/limits", map[string]any{"perTradeUsd": "50.00"}, true)
	if status != http.StatusNotImplemented {
		t.Errorf("PUT /v1/limits with no policy = %d, want 501", status)
	}
}
