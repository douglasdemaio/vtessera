package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeTarget is the target a probe is called with: the test server's own address
// plus the path the agent declares. The port is explicit because the target check
// requires one.
func probeTarget(srv *httptest.Server, path string) string {
	return srv.URL + path
}

func testLimits() Limits {
	return Limits{Timeout: 2 * time.Second, MaxResponseBytes: 8 << 10}
}

// lockedRunner is the production constructor. Every refusal below goes through
// it, because a refusal asserted against a relaxed policy would prove nothing.
func lockedRunner(t *testing.T) *Runner {
	t.Helper()
	return New("0.0.0-test", testLimits())
}

// loopbackRunner relaxes only the address check and trusts the test server's own
// certificate, so the exchange can be exercised end to end. Every other control,
// including redirects, the body cap, the challenge echo and the response schema,
// is the production one.
func loopbackRunner(t *testing.T, srvs ...*httptest.Server) *Runner {
	t.Helper()
	return runnerTrusting(t, testLimits(), srvs...)
}

// runnerTrusting builds a runner that trusts the given servers' certificates.
//
// Trusting every server in the test, including one the probe must never reach, is
// deliberate: a certificate error would otherwise be a second, weaker reason for
// a redirect test to pass.
func runnerTrusting(t *testing.T, limits Limits, srvs ...*httptest.Server) *Runner {
	t.Helper()
	pool := x509.NewCertPool()
	for _, srv := range srvs {
		pool.AddCert(srv.Certificate())
	}
	return newRunner("0.0.0-test", limits, func(net.IP) error { return nil },
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
}

// echoAgent answers a probe with the results the test asked it to report.
type echoAgent struct {
	challenge string
	agentID   string
	results   []Result
	status    int
	raw       string
	hits      int
}

func (a *echoAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.hits++
	if a.status != 0 && a.status != http.StatusOK {
		w.WriteHeader(a.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if a.raw != "" {
		fmt.Fprint(w, a.raw)
		return
	}
	challenge := a.challenge
	if challenge == "" {
		var got request
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		challenge = got.Challenge
	}
	out := response{Challenge: challenge, AgentID: a.agentID, Results: a.results}
	_ = json.NewEncoder(w).Encode(out)
}

func claimed() []string { return []string{"summarize:document", "translate:document"} }

func results(pass bool) []Result {
	status := StatusFail
	if pass {
		status = StatusPass
	}
	return []Result{
		{Capability: "summarize:document", Status: status, Detail: "checked"},
		{Capability: "translate:document", Status: status},
	}
}

// A probe that reaches an agent and gets answers back is the whole feature, so
// this is the baseline the refusals below are measured against.
func TestAProbeOfAnAgentThatAnswersReportsItsCapabilities(t *testing.T) {
	agent := &echoAgent{results: results(true)}
	srv := httptest.NewTLSServer(agent)
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %+v, want one per claimed capability", report.Results)
	}
	if !report.Passed() {
		t.Errorf("report = %+v, want every capability passing", report.Results)
	}
	if agent.hits != 1 {
		t.Errorf("the agent was called %d times, want once", agent.hits)
	}
}

// The declared target has to survive the checks that decide whether it is called
// at all, so a refused target is asserted on the reason and not on a status code
// an implementation might change.
func TestTargetsThatAreNotPublicHTTPSAreRefusedWithoutCalling(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"no scheme", "example.com/probe"},
		{"plain http", "http://example.com:443/probe"},
		{"loopback", "https://127.0.0.1:8443/probe"},
		{"loopback by name", "https://localhost:8443/probe"},
		{"private class A", "https://10.0.0.5:8443/probe"},
		{"private class B", "https://172.16.4.5:8443/probe"},
		{"private class C", "https://192.168.1.5:8443/probe"},
		{"carrier NAT", "https://100.64.1.1:8443/probe"},
		// The address a cloud instance's metadata service answers on. It is the one
		// an agent would not think to avoid and the one worth refusing hardest.
		{"link local metadata", "https://169.254.169.254:8443/probe"},
		{"ipv6 loopback", "https://[::1]:8443/probe"},
		{"ipv6 unique local", "https://[fd00::1]:8443/probe"},
		{"ipv6 link local", "https://[fe80::1]:8443/probe"},
		// An IPv4-mapped IPv6 address is the same address, and a check that only
		// looks at the v6 form would call it private by accident or public by luck.
		{"ipv4 mapped ipv6 loopback", "https://[::ffff:127.0.0.1]:8443/probe"},
		{"ipv4 mapped ipv6 private", "https://[::ffff:10.0.0.1]:8443/probe"},
		{"unspecified", "https://0.0.0.0:8443/probe"},
		{"no port", "https://example.com/probe"},
		{"no path", "https://example.com:443"},
		{"credentials", "https://user:pass@example.com:8443/probe"},
		{"query string", "https://example.com:8443/probe?a=1"},
		{"fragment", "https://example.com:8443/probe#a"},
		{"empty", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lockedRunner(t).Run(context.Background(), tc.target, "agent-1", claimed(), "chal-1")
			if err == nil {
				t.Fatalf("probing %q was allowed, want a refusal", tc.target)
			}
			// The reason has to survive the transport wrapping it, or an operator
			// reading the log sees a URL and learns nothing about why it stopped.
			for _, want := range []string{"probe target", "public address", "not a public range", "resolve", "no addresses"} {
				if strings.Contains(err.Error(), want) {
					return
				}
			}
			t.Errorf("error = %v, want it to name the target or its address as the reason", err)
		})
	}
}

// A public address is what makes a target callable, so the check has to let one
// through or it is refusing everything and passing for a policy.
func TestAPublicAddressIsNotRefusedByTheAddressCheck(t *testing.T) {
	for _, addr := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := checkRoutable(net.ParseIP(addr)); err != nil {
			t.Errorf("%s was refused as a probe target: %v", addr, err)
		}
	}
}

// Every address a name resolves to has to be routable. A name answering with one
// public and one private address is a name that should not be called, because the
// private one is reachable from here and the public one is not.
func TestANameThatReservesToAnyNonPublicAddressIsRefused(t *testing.T) {
	if _, err := publicAddrs(context.Background(), "localhost", checkRoutable); err == nil {
		t.Error("localhost resolved and was allowed, want a refusal")
	}
}

// The response has to echo the challenge that was sent. An answer carrying a
// stale one is a cache, a proxy, or a replay, and reporting its capabilities as
// results would be reporting somebody else's claims as the agent's.
func TestAnAnswerThatDoesNotEchoTheChallengeIsNotAccepted(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{challenge: "a-challenge-from-earlier", results: results(true)})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err == nil {
		t.Fatal("a stale challenge was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "challenge") {
		t.Errorf("error = %v, want it to name the challenge", err)
	}
	if len(report.Results) != 0 {
		t.Errorf("results = %+v, want none: nothing was learned about the agent", report.Results)
	}
}

// An agent answering for somebody else is either confused or is somebody else's
// host, and neither should be recorded as this agent's capabilities.
func TestAnAnswerForAnotherAgentIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{agentID: "somebody-else", results: results(true)})
	defer srv.Close()

	_, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err == nil {
		t.Fatal("an answer for another agent was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "somebody-else") {
		t.Errorf("error = %v, want it to name the agent it answered for", err)
	}
}

// A redirect is a way to reach an address the target check would have refused,
// so the probe stops at the first answer instead of following it.
func TestARedirectIsNotFollowed(t *testing.T) {
	var reached bool
	// A listener that would answer if it were called. It stands in for the
	// internal service a redirect is trying to reach.
	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	report, err := loopbackRunner(t, redirector, internal).Run(context.Background(),
		probeTarget(redirector, "/probe"), "agent-1", claimed(), "chal-1")
	if reached {
		t.Fatal("the probe followed a redirect, want it to stop at the first answer")
	}
	if err != nil {
		t.Fatalf("Run = %v, want the redirect reported rather than an error", err)
	}
	if len(report.Results) == 0 || report.Results[0].Status != StatusFail {
		t.Errorf("results = %+v, want the redirect reported as a failure", report.Results)
	}
}

// A body larger than the cap is refused rather than buffered, so an agent cannot
// hold a connection and the service's memory open by streaming.
func TestAResponseOverTheLimitIsRefusedRatherThanBuffered(t *testing.T) {
	limits := Limits{Timeout: 2 * time.Second, MaxResponseBytes: 512}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"challenge":"x","agentId":"agent-1","results":[{"capability":"summarize:document","status":"pass","detail":"`))
		w.Write([]byte(strings.Repeat("A", 4096)))
		fmt.Fprint(w, `"}]}`)
	}))
	defer srv.Close()

	report, err := runnerTrusting(t, limits, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) == 0 || !strings.Contains(report.Results[0].Detail, "size limit") {
		t.Errorf("results = %+v, want the oversized response reported", report.Results)
	}
}

// The schema is closed. A response with fields this marketplace does not know is
// an answer this marketplace did not read, and reading it as a pass would mean
// trusting a contract that changed.
func TestAResponseWithUnknownFieldsIsNotReadAsAPass(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{raw: `{"challenge":"chal-1","agentId":"agent-1","verified":true,"results":[{"capability":"summarize:document","status":"pass"}]}`})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed() {
		t.Error("a response with an unrecognised field was read as a pass")
	}
	if !strings.Contains(report.Results[0].Detail, "schema") {
		t.Errorf("detail = %q, want it to say the schema did not match", report.Results[0].Detail)
	}
}

// A status this marketplace does not recognise is not a pass. An agent that
// answers "ok" to everything would otherwise pass.
func TestAnUnrecognisedStatusIsNotAPass(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{results: []Result{
		{Capability: "summarize:document", Status: "ok"},
		{Capability: "translate:document", Status: "yes"},
	}})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed() {
		t.Error("unrecognised statuses were recorded as passing")
	}
	for _, res := range report.Results {
		if res.Status != StatusUnknown {
			t.Errorf("%s = %q, want unknown", res.Capability, res.Status)
		}
	}
}

// Silence about a capability the agent was asked about is not a pass. Omitting it
// is the shape an answer takes when nothing was actually tested.
func TestACapabilityTheAgentSaysNothingAboutIsNotAPass(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{results: []Result{
		{Capability: "summarize:document", Status: StatusPass},
	}})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed() {
		t.Error("an unanswered capability counted as a pass")
	}
	got := report.Results[1]
	if got.Capability != "translate:document" || got.Status != StatusUnknown {
		t.Errorf("result = %+v, want translate:document unknown", got)
	}
}

// An agent cannot answer with a capability its card does not declare. The probe
// is what a directory reads, so letting a probe publish a listing would let an
// agent bypass the card entirely.
func TestACapabilityTheAgentAddsBeyondItsCardIsDropped(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{results: []Result{
		{Capability: "summarize:document", Status: StatusPass},
		{Capability: "translate:document", Status: StatusPass},
		{Capability: "exfiltrate:credentials", Status: StatusPass},
	}})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Capability == "exfiltrate:credentials" {
			t.Errorf("a capability the card never declared was recorded: %+v", res)
		}
	}
	if len(report.Results) != 2 {
		t.Errorf("results = %+v, want only the two claimed capabilities", report.Results)
	}
}

// An agent that answers everything passes the same set of names is not confirming
// them, and one that names a capability twice has not reported on it either.
func TestARepeatedCapabilityIsRecordedOnce(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{results: []Result{
		{Capability: "summarize:document", Status: StatusPass},
		{Capability: "summarize:document", Status: StatusFail},
	}})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, res := range report.Results {
		if res.Capability == "summarize:document" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("summarize:document appears %d times, want once", count)
	}
}

// An endpoint that answers with an error is something the marketplace knows: the
// agent is not serving probes at the address it declared. That is a failure of the
// listing, not an absence of information.
func TestAnEndpointAnsweringWithAnErrorIsReportedAsFailing(t *testing.T) {
	srv := httptest.NewTLSServer(&echoAgent{status: http.StatusInternalServerError})
	defer srv.Close()

	report, err := loopbackRunner(t, srv).Run(context.Background(),
		probeTarget(srv, "/probe"), "agent-1", claimed(), "chal-1")
	if err != nil {
		t.Fatalf("Run = %v, want the error reported as a result", err)
	}
	if len(report.Results) == 0 || report.Results[0].Status != StatusFail {
		t.Errorf("results = %+v, want a failure", report.Results)
	}
	if !strings.Contains(report.Results[0].Detail, "500") {
		t.Errorf("detail = %q, want it to carry the status the agent answered with", report.Results[0].Detail)
	}
}

// The probe is bounded in time, and the bound is one the caller declares so an
// operator can see what it is.
func TestAProbeStopsAtItsTimeout(t *testing.T) {
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()

	runner := runnerTrusting(t, Limits{Timeout: 150 * time.Millisecond, MaxResponseBytes: 1 << 10}, slow)
	start := time.Now()
	report, err := runner.Run(context.Background(), probeTarget(slow, "/probe"), "agent-1", claimed(), "chal-1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a probe that outlasted its timeout returned %+v, want a refusal", report)
	}
	if elapsed > 2*time.Second {
		t.Errorf("probe took %s, want it bounded near its 150ms timeout", elapsed)
	}
}

// The runner cannot reach a loopback server, which is why the cases above cannot
// use httptest end to end. This asserts the same refusal at the check the dial
// path actually calls, so the two stay connected.
func TestTheAddressCheckTheDialPathUsesRefusesLoopback(t *testing.T) {
	if _, err := publicAddrs(context.Background(), "127.0.0.1", checkRoutable); err == nil {
		t.Error("the loopback address passed the check the dial path uses")
	}
	if _, err := publicAddrs(context.Background(), "::1", checkRoutable); err == nil {
		t.Error("the ipv6 loopback address passed the check the dial path uses")
	}
}

// A report with no results has passed nothing. An agent declaring no
// capabilities cannot produce a passing probe, and treating that as a pass would
// be a listing that passes by claiming nothing.
func TestAReportWithNoResultsHasNotPassed(t *testing.T) {
	if (Report{}).Passed() {
		t.Error("an empty report reports as passing")
	}
}
