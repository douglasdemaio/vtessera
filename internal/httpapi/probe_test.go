package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/probe"
	"github.com/mr-tron/base58"
)

const probeTarget = "https://agent.example.com:8443/probe"

// stubProber answers a probe without a network.
type stubProber struct {
	report probe.Report
	err    error
	target string
}

func (p *stubProber) Run(_ context.Context, target, agentID string, capabilities []string, challenge string) (probe.Report, error) {
	p.target = target
	out := p.report
	out.AgentID = agentID
	out.Target = target
	return out, p.err
}

// probeServer builds a server with a prober installed and one agent whose card
// declares a probe target.
func probeServer(t *testing.T, prober *stubProber) (*httptest.Server, *agentClient) {
	t.Helper()
	server, _ := buildServer(t, serverBuild{adminToken: adminToken, prober: prober})
	client := newAgent(t, server, func(o *clientOptions) {
		o.card.ProbeTarget = probeTarget
		o.card.Capabilities = []string{"summarize:document"}
	})
	return server, client
}

// The operator can run a probe and read what came back, signed by the
// marketplace.
func TestAnOperatorCanProbeAnAgentAndReadTheSignedResult(t *testing.T) {
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	server, client := probeServer(t, prober)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("probe = %d %s, want 200", status, body)
	}
	payload := decode(t, body)
	if payload["passed"] != true {
		t.Errorf("passed = %v, want true", payload["passed"])
	}
	if payload["valid"] != true {
		t.Errorf("valid = %v, want the result to verify against this marketplace", payload["valid"])
	}
	if payload["target"] != probeTarget {
		t.Errorf("target = %v, want the declared %s", payload["target"], probeTarget)
	}
	if _, ok := payload["signature"]; !ok {
		t.Error("the response carries no signature, so a directory has nothing to check")
	}

	// The result is readable afterwards without the token, because the point of
	// recording it is that a directory can check it.
	read := decode(t, client.do(http.MethodGet, "/v1/agents/"+client.id+"/capabilities", nil, false))
	if read["valid"] != true {
		t.Errorf("the recorded result does not verify on read: %v", read)
	}
}

// A probe that ran and failed is a successful request. It is the most useful
// answer the route can give, and answering 200 is what tells the operator the
// probe happened.
func TestAProbeThatRanAndFailedIsStillReportedAsA200(t *testing.T) {
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results: []probe.Result{
			{Capability: "summarize:document", Status: probe.StatusFail, Detail: "no model loaded"},
		},
	}}
	server, client := probeServer(t, prober)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", adminToken, nil)
	if status != http.StatusOK {
		t.Fatalf("probe = %d %s, want 200: the probe ran and failed", status, body)
	}
	payload := decode(t, body)
	if payload["passed"] != false {
		t.Errorf("passed = %v, want false", payload["passed"])
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v, want the failed capability reported", payload["results"])
	}
}

// An agent that never declared a probe target is refused, and the refusal says
// why. An operator who asked and got silence could not tell a declined agent
// from a runner that did nothing.
func TestProbingAnAgentThatDeclaresNoTargetIsRefusedWithAReason(t *testing.T) {
	prober := &stubProber{}
	server, _ := buildServer(t, serverBuild{adminToken: adminToken, prober: prober})
	client := newAgent(t, server)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", adminToken, nil)
	if status != http.StatusConflict {
		t.Fatalf("probe = %d %s, want 409", status, body)
	}
	if code := decode(t, body)["code"]; code != "NO_PROBE_TARGET" {
		t.Errorf("code = %v, want NO_PROBE_TARGET", code)
	}
}

// Running a probe spends this service's egress, so it takes the admin token and
// not an agent session. An agent that could trigger it could use the marketplace
// as a way to send traffic to hosts it cannot reach itself.
func TestRunningAProbeRequiresTheAdminTokenAndNotAnAgentSession(t *testing.T) {
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	server, client := probeServer(t, prober)

	status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("probe with no token = %d, want 401", status)
	}
	status, _ = adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", "not-the-admin-token", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("probe with a wrong token = %d, want 401", status)
	}
	// The agent's own session is not a substitute for the operator's token.
	if status, body := client.raw(http.MethodPost, "/v1/admin/agents/"+client.id+"/probe", nil, true); status == http.StatusOK {
		t.Errorf("an agent session ran a probe: %s", body)
	}
	if prober.target != "" {
		t.Error("a refused probe request still reached the runner")
	}
}

// With no admin token configured the probe route is not registered at all, so a
// deployment that did not opt in does not advertise the capability.
func TestTheProbeRouteDoesNotExistWithoutAnAdminToken(t *testing.T) {
	server, _ := buildServer(t, serverBuild{prober: &stubProber{}})
	client := newAgent(t, server)

	status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", "", nil)
	if status != http.StatusNotFound {
		t.Errorf("probe with no admin token configured = %d, want 404", status)
	}
}

// An agent that has never been probed is not a failure. It is a different answer
// from one that failed, and the two are distinguishable.
func TestAnAgentThatHasNeverBeenProbedIsNotReportedAsFailing(t *testing.T) {
	server, _ := buildServer(t, serverBuild{})
	client := newAgent(t, server)

	status, body := client.raw(http.MethodGet, "/v1/agents/"+client.id+"/capabilities", nil, false)
	if status != http.StatusNotFound {
		t.Fatalf("read capabilities = %d %s, want 404", status, body)
	}
	if code := decode(t, body)["code"]; code != "NOT_PROBED" {
		t.Errorf("code = %v, want NOT_PROBED rather than a failure code", code)
	}
}

// An agent that does not exist is a 404 with a different code from an unprobed
// agent, so a caller can tell a bad ID from a missing record.
func TestReadingCapabilitiesOfAnUnknownAgentIsRefused(t *testing.T) {
	server, _ := buildServer(t, serverBuild{})
	client := newAgent(t, server)
	_, unknown, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	status, body := client.raw(http.MethodGet,
		"/v1/agents/"+base58.Encode(unknown)+"/capabilities", nil, false)
	if status != http.StatusNotFound {
		t.Fatalf("read capabilities = %d %s, want 404", status, body)
	}
	if code := decode(t, body)["code"]; code == "NOT_PROBED" {
		t.Error("an unknown agent is reported as unprobed")
	}
}

// A retired agent is not probed: a probe would tell a buyer that an agent
// withdrawn from the marketplace still works.
func TestARetiredAgentCannotBeProbed(t *testing.T) {
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	server, client := probeServer(t, prober)

	if status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/retire", adminToken,
		map[string]any{"reason": "withdrawn"}); status != http.StatusOK {
		t.Fatalf("retire = %d %s", status, body)
	}
	status, _ := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+client.id+"/probe", adminToken, nil)
	if status == http.StatusOK {
		t.Error("a retired agent was probed")
	}
	if prober.target != "" {
		t.Error("the runner was called for a retired agent")
	}
}

// The card that declares the target is signed by the agent and attested by the
// marketplace, so an agent cannot be redirected to an address it did not
// declare, and the declaration is auditable afterwards.
func TestTheProbeTargetIsCoveredByTheAgentsSignatureAndTheMarketplaceAttestation(t *testing.T) {
	_, client := probeServer(t, &stubProber{})
	// The card is republished with a signature, because an unsigned card has
	// nothing on the agent side to check and the claim under test is that the
	// declared target is inside what the agent signed.
	statement := client.signingCard()
	statement.ProbeTarget = probeTarget
	body := client.signingCardBody()
	body.ProbeTarget = probeTarget
	sig := client.signCard(statement)
	if status, errBody := client.putCard(body, &sig); status != http.StatusOK {
		t.Fatalf("publish a signed card = %d %s", status, errBody)
	}

	payload := decode(t, client.do(http.MethodGet, "/v1/agents/"+client.id+"/attestation", nil, false))
	market, _ := payload["marketplace"].(map[string]any)
	agentSide, _ := payload["agent"].(map[string]any)
	if market["attested"] != true || market["valid"] != true {
		t.Errorf("marketplace attestation = %v, want attested and valid", market)
	}
	if agentSide["attested"] != true || agentSide["valid"] != true {
		t.Errorf("agent signature = %v, want attested and valid", agentSide)
	}
}

// A card whose probe target is not a permitted endpoint is refused at
// registration, before any of it is signed or stored.
func TestACardDeclaringAnImpermissibleProbeTargetIsRefused(t *testing.T) {
	server, _ := buildServer(t, serverBuild{adminToken: adminToken, prober: &stubProber{}})
	client := newAgent(t, server)
	c := domain.AgentCard{
		Name: "loopback probe", URL: "https://trader.example.com", PublicKey: client.id,
		Currencies: []string{usdc}, Capabilities: []string{"summarize:document"},
		ProbeTarget: "http://127.0.0.1:8080/probe",
	}

	status, body := client.putCard(c, nil)
	if status == http.StatusOK {
		t.Fatalf("a card declaring a loopback plaintext probe target was accepted: %s", body)
	}
}
