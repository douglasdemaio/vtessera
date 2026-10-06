package store

import (
	"context"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/probe"
)

func TestAProbeResultRoundTripsWithItsAttestation(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	agentID := seedAttestAgent(t, db, attestSigner(t)).ID

	checked := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	report := probe.Report{
		AgentID: agentID, Target: "https://agent.example.com:8443/probe",
		CheckedAt: checked,
		Results: []probe.Result{
			{Capability: "summarize:document", Status: probe.StatusPass},
			{Capability: "translate:text", Status: probe.StatusFail, Detail: "unsupported language"},
		},
	}
	key := attestSigner(t)
	sig, err := key.SignProbe(probe.Statement(report), checked)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveProbe(ctx, report, sig, checked); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.Probe(ctx, agentID)
	if err != nil || !found {
		t.Fatalf("Probe = %v, %v", found, err)
	}
	if err := attest.VerifyProbeAttestedBy(probe.Statement(got), *got.Signature, key.PublicKeyBase58()); err != nil {
		t.Errorf("the round-tripped result does not verify: %v", err)
	}
	if got.Passed() {
		t.Error("a report with a failing capability reports as passing")
	}
	if got.Target != report.Target {
		t.Errorf("target = %q, want %q", got.Target, report.Target)
	}
	if !got.CheckedAt.Equal(checked) {
		t.Errorf("checkedAt = %s, want %s", got.CheckedAt, checked)
	}
}

// An agent that has never been probed is not the same as an agent that failed its
// probe, and a reader has to be able to tell them apart.
func TestAnAgentThatHasNeverBeenProbedHasNoProbeRecord(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	agentID := seedAttestAgent(t, db, attestSigner(t)).ID

	if _, found, err := db.Probe(ctx, agentID); err != nil || found {
		t.Errorf("Probe = %v, %v, want no record and no error", found, err)
	}
}

// Probing an agent again replaces the record rather than adding a second one, so
// a directory cannot read a stale answer as a current one.
func TestASecondProbeReplacesTheStoredResult(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	key := attestSigner(t)
	agentID := seedAttestAgent(t, db, key).ID

	first := probe.Report{
		AgentID: agentID, Target: "https://agent.example.com:8443/probe",
		CheckedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Results:   []probe.Result{{Capability: "summarize:document", Status: probe.StatusPass}},
	}
	firstSig, err := key.SignProbe(probe.Statement(first), first.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveProbe(ctx, first, firstSig, first.CheckedAt); err != nil {
		t.Fatal(err)
	}

	second := first
	second.CheckedAt = first.CheckedAt.Add(time.Hour)
	second.Results = []probe.Result{{Capability: "summarize:document", Status: probe.StatusFail}}
	secondSig, err := key.SignProbe(probe.Statement(second), second.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveProbe(ctx, second, secondSig, second.CheckedAt); err != nil {
		t.Fatal(err)
	}

	got, _, err := db.Probe(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed() {
		t.Error("the earlier passing result is still what the store returns")
	}
	if err := attest.VerifyProbeAttestedBy(probe.Statement(got), *got.Signature, key.PublicKeyBase58()); err != nil {
		t.Errorf("the replacement does not verify: %v", err)
	}
}
