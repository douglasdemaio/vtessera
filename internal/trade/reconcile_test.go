package trade_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/gagliardetto/solana-go"
)

// pendingWithSignature is the state a confirmation leaves behind when the
// signature was not yet visible: the trade is settlement_pending and the
// signature is on the request for a later attempt to re-poll.
func pendingWithSignature(t *testing.T, h harness) domain.Trade {
	t.Helper()
	ctx := context.Background()
	accepted := acceptedOnchainTrade(t, h)
	request, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetSettlementSignature(ctx, request.ID, signature, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tr, err := h.svc.Get(ctx, buyerKey, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func reconcilingHarness(t *testing.T, opts ...func(*trade.SettlementDeps)) (harness, *trade.SettlementDeps) {
	t.Helper()
	h, deps := onchainHarness(t, opts...)
	deps.TradeList = h.db
	h.svc.WithSettlement(*deps)
	return h, deps
}

func TestReconcileSettlesAConfirmationThatWasInvisibleAtTheTime(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, _ := reconcilingHarness(t, withChain(chain))
	tr := pendingWithSignature(t, h)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Settled != 1 || stats.Examined != 1 {
		t.Errorf("stats = %+v, want one settled trade examined", stats)
	}
	settled, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != domain.TradeSettled {
		t.Fatalf("state = %s, want settled: the signature landed after all", settled.State)
	}
	receipt, err := h.led.Receipt(ctx, tr.ID)
	if err != nil {
		t.Fatalf("receipt after reconciliation: %v", err)
	}
	if receipt.TradeID != tr.ID {
		t.Errorf("receipt = %+v, want it bound to trade %s", receipt, tr.ID)
	}
	claims, err := h.led.Verify(receipt.JWS)
	if err != nil {
		t.Fatalf("verify reconciled tessera: %v", err)
	}
	if claims.Settlement.Signature != signature {
		t.Errorf("tessera signature = %q, want the signature that actually settled on chain", claims.Settlement.Signature)
	}
	if claims.Trade.State != domain.TradeSettled {
		t.Errorf("tessera state = %s, want settled", claims.Trade.State)
	}
	offer, err := h.registry.Offer(ctx, tr.OfferID)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want closed: a settled trade must not stay open for sale", offer.Status)
	}
	requests, err := h.db.ListSettlementRequests(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests[0].Status != domain.SettlementConfirmed || requests[0].Signature != signature {
		t.Errorf("request = %+v, want the settled request confirmed against its signature", requests[0])
	}
}

func TestReconcileLeavesAnInvisibleSignaturePendingAndNeverDisputes(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{} // every poll reports the signature as not yet on chain
	h, _ := reconcilingHarness(t, withChain(chain))
	tr := pendingWithSignature(t, h)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Settled != 0 || stats.Disputed != 0 {
		t.Errorf("stats = %+v, want one trade still pending", stats)
	}
	if chain.calls == 0 {
		t.Error("chain was never polled, so the signature was never re-checked")
	}
	still, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending: an invisible signature is not a dispute", still.State)
	}
	if _, err := h.led.Receipt(ctx, tr.ID); err == nil {
		t.Error("a pending trade must never carry a tessera")
	}
}

func TestReconcileDisputesWhenTheLandedTransactionDoesNotMatchTheTerms(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(landed().fetched, nil)
	h, _ := reconcilingHarness(t, withChain(chain), withVerifier(&fakeVerifier{err: settlement.ErrMismatch}))
	tr := pendingWithSignature(t, h)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Disputed != 1 {
		t.Errorf("stats = %+v, want one disputed trade", stats)
	}
	disputed, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disputed.State != domain.TradeDisputed {
		t.Fatalf("state = %s, want disputed", disputed.State)
	}
	if _, err := h.led.Receipt(ctx, tr.ID); err == nil {
		t.Error("a disputed trade must never carry a tessera")
	}
}

func TestReconcileExpiresAFailedOnChainTransactionWithoutDisputing(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(settlement.Fetched{Transaction: &solana.Transaction{}, ExecErr: errors.New("insufficient funds")}, nil)
	h, _ := reconcilingHarness(t, withChain(chain))
	tr := pendingWithSignature(t, h)

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Expired != 1 || stats.Disputed != 0 {
		t.Errorf("stats = %+v, want one expired attempt and no dispute", stats)
	}
	after, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending: nobody is at fault when a transaction fails on chain", after.State)
	}
	// The spent blockhash must not keep blocking cancellation or a retry.
	requests, err := h.db.ListSettlementRequests(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests[0].Status != domain.SettlementExpired {
		t.Errorf("request status = %s, want expired", requests[0].Status)
	}
	if requests[0].Live(time.Now().UTC()) {
		t.Error("a spent request must not count as live")
	}
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID); err != nil {
		t.Errorf("rebuild after a failed attempt: %v, want a fresh transaction", err)
	}
}

func TestReconcileKeepsTheTradePendingWhenTheChainIsUnreachable(t *testing.T) {
	ctx := context.Background()
	chain := (&fakeChain{}).add(settlement.Fetched{}, errors.New("rpc unavailable"))
	h, _ := reconcilingHarness(t, withChain(chain))
	tr := pendingWithSignature(t, h)

	if _, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy()); err != nil {
		t.Fatalf("a single unreachable trade must not fail the pass: %v", err)
	}
	after, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending: an RPC outage is not evidence of wrongdoing", after.State)
	}
}

func TestReconcileIsANoOpWithoutSettlement(t *testing.T) {
	ctx := context.Background()
	h := setup(t) // no settlement configured at all
	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatalf("err = %v, want a no-op when settlement is not configured", err)
	}
	if stats != (trade.ReconcileStats{}) {
		t.Errorf("stats = %+v, want nothing to do", stats)
	}
}

func TestReconcileIgnoresATradeWhoseRequestWasNeverSigned(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{}
	h, _ := reconcilingHarness(t, withChain(chain))
	accepted := acceptedOnchainTrade(t, h)
	if _, err := h.svc.BuildSettlement(ctx, buyerKey, accepted.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.DefaultReconcilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Errorf("stats = %+v, want the unsigned request left for the buyer", stats)
	}
	if chain.calls != 0 {
		t.Errorf("chain calls = %d, want none: an unsigned request has no signature to poll", chain.calls)
	}
}

func TestReconcileStopsAtTheBatchLimit(t *testing.T) {
	ctx := context.Background()
	chain := &fakeChain{}
	h, _ := reconcilingHarness(t, withChain(chain))
	for i := 0; i < 3; i++ {
		// Each trade needs its own idempotency key, or Create returns the same
		// trade and the negotiation runs against an already-pending one.
		tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOnchain, fmt.Sprintf("batch-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		acceptBoth(t, h, tr.ID)
		if _, err := h.svc.BuildSettlement(ctx, buyerKey, tr.ID); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := h.svc.ReconcileOutstanding(ctx, trade.ReconcilePolicy{Interval: time.Second, Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Examined != 2 {
		t.Errorf("examined = %d, want the batch limit of 2 so one pass cannot starve the request path", stats.Examined)
	}
}
