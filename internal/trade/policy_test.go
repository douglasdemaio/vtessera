package trade

import (
	"testing"
	"time"
)

func TestConfirmPolicyAttemptsFallsBackToOneWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero", 0, 1},
		{"negative", -5, 1},
		{"configured", 7, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ConfirmPolicy{Attempts: c.in}
			if got := p.attempts(); got != c.want {
				t.Fatalf("attempts() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestConfirmPolicyDelayForDoublesEachAttemptUntilCapped(t *testing.T) {
	p := ConfirmPolicy{Backoff: 300 * time.Millisecond, MaxDelay: 1000 * time.Millisecond}

	if got, want := p.delayFor(1), 300*time.Millisecond; got != want {
		t.Fatalf("delayFor(1) = %v, want %v", got, want)
	}
	if got, want := p.delayFor(2), 600*time.Millisecond; got != want {
		t.Fatalf("delayFor(2) = %v, want %v", got, want)
	}
	// Uncapped this would be 1200ms; MaxDelay caps it to 1000ms.
	if got, want := p.delayFor(3), 1000*time.Millisecond; got != want {
		t.Fatalf("delayFor(3) = %v, want %v", got, want)
	}
	// Uncapped this would be 2400ms; still capped to 1000ms.
	if got, want := p.delayFor(4), 1000*time.Millisecond; got != want {
		t.Fatalf("delayFor(4) = %v, want %v", got, want)
	}
}

func TestConfirmPolicyDelayForWithoutACapGrowsUnbounded(t *testing.T) {
	p := ConfirmPolicy{Backoff: 300 * time.Millisecond}

	if got, want := p.delayFor(3), 1200*time.Millisecond; got != want {
		t.Fatalf("delayFor(3) = %v, want %v", got, want)
	}
}

func TestConfirmPolicyDelayForWithZeroBackoffStaysZero(t *testing.T) {
	p := ConfirmPolicy{MaxDelay: 1000 * time.Millisecond}

	if got := p.delayFor(5); got != 0 {
		t.Fatalf("delayFor(5) = %v, want 0", got)
	}
}

func TestDefaultConfirmPolicyRetriesBrieflyWithABoundedDelay(t *testing.T) {
	p := DefaultConfirmPolicy()

	if p.attempts() != 3 {
		t.Fatalf("attempts() = %d, want 3", p.attempts())
	}
	if got, want := p.delayFor(1), 500*time.Millisecond; got != want {
		t.Fatalf("delayFor(1) = %v, want %v", got, want)
	}
	if got, want := p.delayFor(3), 2000*time.Millisecond; got != want {
		t.Fatalf("delayFor(3) = %v, want %v", got, want)
	}
}

func TestReconcilePolicyIntervalFallsBackToTheDefaultWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ReconcilePolicy{Interval: c.in}
			if got, want := p.interval(), DefaultReconcilePolicy().Interval; got != want {
				t.Fatalf("interval() = %v, want %v", got, want)
			}
		})
	}
}

func TestReconcilePolicyIntervalKeepsAConfiguredValue(t *testing.T) {
	p := ReconcilePolicy{Interval: 5 * time.Millisecond}
	if got, want := p.interval(), 5*time.Millisecond; got != want {
		t.Fatalf("interval() = %v, want %v", got, want)
	}
}

func TestReconcilePolicyBatchFallsBackToOneWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero", 0, 1},
		{"negative", -3, 1},
		{"configured", 10, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ReconcilePolicy{Batch: c.in}
			if got := p.batch(); got != c.want {
				t.Fatalf("batch() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDefaultReconcilePolicyPollsEveryFifteenSecondsInBatchesOfFifty(t *testing.T) {
	p := DefaultReconcilePolicy()

	if got, want := p.interval(), 15*time.Second; got != want {
		t.Fatalf("interval() = %v, want %v", got, want)
	}
	if got, want := p.batch(), 50; got != want {
		t.Fatalf("batch() = %d, want %d", got, want)
	}
}
