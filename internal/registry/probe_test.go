package registry_test

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/probe"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/store"
)

// stubProber answers without a network, so a test is asserting what the service
// does with a probe result rather than whether a socket worked.
type stubProber struct {
	report  probe.Report
	err     error
	targets []string
	caps    []string
}

func (p *stubProber) Run(_ context.Context, target, agentID string, capabilities []string, challenge string) (probe.Report, error) {
	p.targets = append(p.targets, target)
	p.caps = append(p.caps, capabilities...)
	out := p.report
	out.AgentID = agentID
	out.Target = target
	return out, p.err
}

const probePath = "https://agent.example.com:8443/probe"

// probeService returns a service holding one active agent whose card declares a
// probe target and one capability.
func probeService(t *testing.T, prober *stubProber, target string) (*registry.Service, *attest.SigningKey, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	market := marketSigner(t)
	svc := registry.New(db, mainnetMints(t), market, registry.WithProbeRunner(prober))
	agent := registeredAgent(t, svc, "probeable", target)
	return svc, market, agent.ID
}

// registeredAgent registers an agent with a signed card declaring the given probe
// target, and returns it.
func registeredAgent(t *testing.T, svc *registry.Service, name, target string) domain.Agent {
	t.Helper()
	ctx := context.Background()
	key := agentSigner(t)
	c := card(name)
	c.PublicKey = key.PublicKeyBase58()
	c.Capabilities = []string{"summarize:document"}
	c.Currencies = []string{usdc}
	c.ProbeTarget = target
	at := time.Now().UTC()
	sig, err := key.SignCard(svc.AttestedCard(c), at)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := svc.Register(ctx, key.PublicKeyBase58(), c, &sig)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// agentSigner is an agent's own key, distinct from the marketplace's.
func agentSigner(t *testing.T) *attest.SigningKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := attest.NewSigningKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A probe that ran is recorded and signed, so the result can be read back and
// tied to this marketplace.
func TestAProbeResultIsRecordedAndAttestedByThisMarketplace(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	svc, market, agentID := probeService(t, prober, probePath)

	report, err := svc.ProbeCapabilities(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed() {
		t.Errorf("report = %+v, want it passing", report.Results)
	}
	if report.Signature == nil {
		t.Fatal("the result carries no signature")
	}
	if report.Signature.KeyID != market.PublicKeyBase58() {
		t.Errorf("signed by %s, want this marketplace %s", report.Signature.KeyID, market.PublicKeyBase58())
	}
	if err := svc.VerifyProbe(report); err != nil {
		t.Errorf("the recorded result does not verify: %v", err)
	}
	read, found, err := svc.ProbeResult(ctx, agentID)
	if err != nil || !found {
		t.Fatalf("ProbeResult = %v, %v", found, err)
	}
	if err := svc.VerifyProbe(read); err != nil {
		t.Errorf("the result read back does not verify: %v", err)
	}
}

// A probe that ran and failed is recorded too. Storing only passes would leave a
// directory unable to tell an agent that was checked and failed from one that was
// never checked.
func TestAFailedProbeIsRecordedRatherThanDiscarded(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results: []probe.Result{
			{Capability: "summarize:document", Status: probe.StatusFail, Detail: "no model loaded"},
		},
	}}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	read, found, err := svc.ProbeResult(ctx, agentID)
	if err != nil || !found {
		t.Fatalf("a failed probe was not recorded: %v, %v", found, err)
	}
	if read.Passed() {
		t.Error("a recorded failure reports as passing")
	}
	if err := svc.VerifyProbe(read); err != nil {
		t.Errorf("the recorded failure does not verify: %v", err)
	}
}

// The target and the capabilities come from the stored card, not from the
// request, so what was tested and what is advertised cannot come to differ.
func TestAProbeUsesTheStoredCardsTargetAndCapabilities(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	if len(prober.targets) != 1 || prober.targets[0] != probePath {
		t.Errorf("probed %v, want the card's declared target", prober.targets)
	}
	if len(prober.caps) != 1 || prober.caps[0] != "summarize:document" {
		t.Errorf("probed %v, want the card's declared capabilities", prober.caps)
	}
}

// A card that declares no probe target is never probed. This is the opt-in: an
// agent must ask to be probed, and publishing a capability list is not asking.
func TestAnAgentThatDeclaresNoProbeTargetIsNeverProbed(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{}
	svc, _, agentID := probeService(t, prober, "")

	_, err := svc.ProbeCapabilities(ctx, agentID)
	if err == nil {
		t.Fatal("an agent with no declared probe target was probed")
	}
	if !strings.Contains(err.Error(), "probe target") {
		t.Errorf("error = %v, want it to name the missing declaration", err)
	}
	if len(prober.targets) != 0 {
		t.Errorf("the runner was called with %v, want it never called", prober.targets)
	}
}

// A retired agent is not probed. A probe would be this service telling a buyer
// that an agent withdrawn from the marketplace still works.
func TestARetiredAgentIsNotProbed(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.Retire(ctx, agentID, "no longer selling", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProbeCapabilities(ctx, agentID); err == nil {
		t.Fatal("a retired agent was probed")
	}
	if len(prober.targets) != 0 {
		t.Errorf("the runner was called for a retired agent with %v", prober.targets)
	}
}

// A probe that could not run is not recorded as a result. Nothing was learned
// about the agent, and a row claiming otherwise is an observation that never
// happened.
func TestAProbeThatCouldNotRunIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{err: probe.ErrRefused}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err == nil {
		t.Fatal("a refused probe was reported as a result")
	}
	if _, found, err := svc.ProbeResult(ctx, agentID); err != nil || found {
		t.Errorf("a probe that never ran left a record: %v, %v", found, err)
	}
}

// A later probe replaces the earlier one rather than accumulating. A directory
// reading this is asking whether the agent still does what it says, and an old
// answer presented as current is worse than no answer.
func TestASecondProbeReplacesTheFirst(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	prober.report = probe.Report{
		CheckedAt: time.Now().UTC().Add(time.Hour),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusFail}},
	}
	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	read, _, err := svc.ProbeResult(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Passed() {
		t.Error("the earlier passing probe survived the later failing one")
	}
	if err := svc.VerifyProbe(read); err != nil {
		t.Errorf("the replacement does not verify: %v", err)
	}
}

// A result whose signature covers different results must not verify, or the
// record is worth nothing to a directory checking it.
func TestAProbeResultEditedAfterProbingDoesNotVerify(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	read, _, err := svc.ProbeResult(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	read.Results[0].Status = probe.StatusFail
	if err := svc.VerifyProbe(read); err == nil {
		t.Error("a result edited from pass to fail still verified")
	}
}

// An unsigned result is reported as invalid rather than valid. An unsigned
// observation is an assertion by whoever put it there.
func TestAnUnsignedProbeResultDoesNotVerify(t *testing.T) {
	ctx := context.Background()
	prober := &stubProber{report: probe.Report{
		CheckedAt: time.Now().UTC(),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}}
	svc, _, agentID := probeService(t, prober, probePath)

	if _, err := svc.ProbeCapabilities(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	read, _, err := svc.ProbeResult(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	read.Signature = nil
	if err := svc.VerifyProbe(read); err == nil {
		t.Error("an unsigned result verified")
	}
}
