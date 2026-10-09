package trade_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/trade"
)

// disputeAnAcceptedTrade walks a fresh trade to accepted and then disputes it,
// which is the only state a resolution can act on.
func disputeAnAcceptedTrade(t *testing.T, h harness, offerID string) string {
	t.Helper()
	ctx := context.Background()
	tr, _, err := h.svc.Create(ctx, buyerKey, offerID, domain.SettlementOffchain, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Dispute(ctx, buyerKey, tr.ID, "seller delivered nothing"); err != nil {
		t.Fatal(err)
	}
	return tr.ID
}

func TestResolvingADisputedTradeEndsItAndRecordsTheVerdict(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	id := disputeAnAcceptedTrade(t, h, h.offerID)

	resolved, err := h.svc.Resolve(ctx, "operator-7", id, domain.ResolutionReleased, "buyer was right")
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if resolved.State != domain.TradeResolved {
		t.Fatalf("state = %s, want resolved", resolved.State)
	}

	// The verdict is on the event, not just in the state: released and upheld end
	// the trade the same way, and the difference is what an auditor reads.
	withEvents, err := h.svc.Get(ctx, buyerKey, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvents.Events) == 0 {
		t.Fatal("no events on the resolved trade")
	}
	last := withEvents.Events[len(withEvents.Events)-1]
	if last.Type != domain.EventResolved {
		t.Fatalf("last event = %s, want resolved", last.Type)
	}
	if last.FromState != domain.TradeDisputed || last.ToState != domain.TradeResolved {
		t.Errorf("resolved event moved %s -> %s, want disputed -> resolved", last.FromState, last.ToState)
	}
	if last.ActorAgentID != "operator-7" {
		t.Errorf("actor = %s, want the operator label", last.ActorAgentID)
	}
	var detail struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(last.Detail, &detail); err != nil {
		t.Fatalf("resolve detail = %s: %v", last.Detail, err)
	}
	if detail.Outcome != string(domain.ResolutionReleased) || detail.Reason != "buyer was right" {
		t.Errorf("detail = %+v, want the released outcome and the reason", detail)
	}

	// A retried operator request is a no-op rather than a conflict, so the
	// settlement of one dispute does not depend on the caller's retry policy.
	again, err := h.svc.Resolve(ctx, "operator-7", id, domain.ResolutionUpheld, "second look")
	if err != nil {
		t.Fatalf("second Resolve = %v", err)
	}
	if again.State != domain.TradeResolved {
		t.Errorf("state after retry = %s, want still resolved", again.State)
	}
}

func TestResolvingATradeThatIsNotDisputedIsRefused(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOffchain, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.Resolve(ctx, "operator-7", tr.ID, domain.ResolutionReleased, ""); !errors.Is(err, trade.ErrIllegalState) {
		t.Fatalf("Resolve on a proposed trade = %v, want ErrIllegalState", err)
	}
}

func TestResolvingWithAnUnknownOutcomeIsRefused(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	id := disputeAnAcceptedTrade(t, h, h.offerID)

	if _, err := h.svc.Resolve(ctx, "operator-7", id, domain.Resolution("perhaps"), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Resolve with an unknown outcome = %v, want ErrInvalid", err)
	}
	// The trade is untouched by the refused call.
	after, err := h.svc.Get(ctx, buyerKey, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeDisputed {
		t.Errorf("state = %s after a refused resolve, want still disputed", after.State)
	}
}

func TestResolvingADisputeReleasesTheBuyersReservation(t *testing.T) {
	ctx := context.Background()
	h := capSetup(t, nil)
	offer := offerFor(t, h, "5.00")
	id := disputeAnAcceptedTrade(t, h, offer)

	since := time.Now().Add(-time.Hour)
	before, err := h.db.CommittedSpendSince(ctx, buyerKey, since, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("committed spend before resolve = %d rows, want the disputed trade charged", len(before))
	}

	if _, err := h.svc.Resolve(ctx, "operator-7", id, domain.ResolutionReleased, "voided"); err != nil {
		t.Fatalf("Resolve = %v", err)
	}

	after, err := h.db.CommittedSpendSince(ctx, buyerKey, since, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("committed spend after resolve = %d rows, want the reservation released", len(after))
	}
}
