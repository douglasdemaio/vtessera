package trade_test

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
)

const (
	buyerKey  = "HofWGrRkGfQy5qnFXkV1MR1PbnixJ4xMPhsmywrpEqYd"
	sellerKey = "4u9MuUGc6GUZMKxguayJXPw8a54quV53DnPQzii2fA3P"
	outsider  = "ApdCWPr4eyvhTRiYh2ndnFXZDkXsUuZq3yhVJK2wR9Tp"
	usdc      = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

type harness struct {
	svc      *trade.Service
	registry *registry.Service
	led      *ledger.Ledger
	db       *store.Store
	offerID  string
}

func card(id string) domain.AgentCard {
	return domain.AgentCard{
		Name:        "agent",
		Description: "trading agent",
		URL:         "https://agent.example.com",
		Version:     "0.1.0",
		PublicKey:   id,
	}
}

func setup(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "trade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	// The governed set matters here even for the off-chain tests: the published
	// offer declares on-chain as an accepted mode, and the registry checks the
	// price's precision against a governed scale when it does.
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatalf("governed mints: %v", err)
	}
	registrySvc := registry.New(db, mints)
	for _, id := range []string{buyerKey, sellerKey, outsider} {
		if _, _, err := registrySvc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	offer, _, err := registrySvc.PublishOffer(ctx, sellerKey, registry.NewOffer{
		Direction:       domain.DirectionAsk,
		Description:     "summarize a document",
		Capabilities:    []string{"summarize:document"},
		PriceAmount:     "12.50",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain, domain.SettlementOnchain},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	led := ledger.New(db, signer)
	return harness{
		svc:      trade.New(db, db, db, led),
		registry: registrySvc,
		led:      led,
		db:       db,
		offerID:  offer.ID,
	}
}

func acceptBoth(t *testing.T, h harness, tradeID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tradeID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, buyerKey, tradeID); err != nil {
		t.Fatal(err)
	}
}

func TestTradeHappyPathIssuesTessera(t *testing.T) {
	ctx := context.Background()
	h := setup(t)

	tr, isNew, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOffchain, "idem-1")
	if err != nil || !isNew {
		t.Fatalf("Create = %v, %v", isNew, err)
	}
	if tr.BuyerAgentID != buyerKey || tr.SellerAgentID != sellerKey {
		t.Errorf("parties = %s/%s, want buyer/seller from the ask direction", tr.BuyerAgentID, tr.SellerAgentID)
	}
	if tr.State != domain.TradeProposed {
		t.Errorf("state = %s, want proposed", tr.State)
	}
	if tr.Amount.String() != "12.50" {
		t.Errorf("amount = %s, want the offer price", tr.Amount)
	}

	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	mid, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.State != domain.TradeNegotiating {
		t.Fatalf("one acceptance advanced the state to %s, want negotiating", mid.State)
	}

	acceptBoth(t, h, tr.ID)
	accepted, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != domain.TradeAccepted {
		t.Fatalf("state = %s after both accepted, want accepted", accepted.State)
	}

	recorded, receipt, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.State != domain.TradeRecorded {
		t.Errorf("state = %s, want recorded", recorded.State)
	}
	if receipt.TradeID != tr.ID {
		t.Errorf("receipt trade = %s, want %s", receipt.TradeID, tr.ID)
	}
	if receipt.JWS == "" {
		t.Error("receipt has no signed tessera")
	}
	claims, err := h.led.Verify(receipt.JWS)
	if err != nil {
		t.Fatalf("tessera does not verify against the marketplace key: %v", err)
	}
	if claims.Trade.TradeID != tr.ID || claims.Trade.State != domain.TradeRecorded {
		t.Errorf("tessera claims = %+v", claims)
	}
	if claims.Trade.Buyer != buyerKey || claims.Trade.Seller != sellerKey {
		t.Errorf("tessera parties = %s/%s", claims.Trade.Buyer, claims.Trade.Seller)
	}
	if claims.Trade.Amount != "12.50" || claims.Trade.Mint != usdc {
		t.Errorf("tessera price = %s %s, want 12.50 %s", claims.Trade.Amount, claims.Trade.Mint, usdc)
	}
	if claims.Trade.Mode != domain.SettlementOffchain {
		t.Errorf("tessera mode = %s, want offchain", claims.Trade.Mode)
	}
	if claims.Settlement.LedgerEntryHash == "" {
		t.Error("tessera is not anchored to a ledger entry")
	}

	full, err := h.svc.Get(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	types := map[domain.TradeEventType]int{}
	for _, e := range full.Events {
		types[e.Type]++
	}
	for _, want := range []domain.TradeEventType{domain.EventProposed, domain.EventNegotiating, domain.EventAccepted, domain.EventRecorded} {
		if types[want] != 1 {
			t.Errorf("event %s recorded %d times, want 1", want, types[want])
		}
	}
}

func TestRecordClosesTheOffer(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	if _, _, err := h.svc.Record(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
	offer, err := h.registry.Offer(ctx, h.offerID)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Status != domain.OfferClosed {
		t.Errorf("offer status = %s, want closed after the trade recorded", offer.Status)
	}
	if _, _, err := h.svc.Create(ctx, outsider, h.offerID, "", ""); !errors.Is(err, trade.ErrOfferUnavailable) {
		t.Errorf("second trade on a closed offer err = %v, want ErrOfferUnavailable", err)
	}
}

func TestRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	_, first, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatalf("second Record failed: %v", err)
	}
	if first.JWS != second.JWS {
		t.Error("re-recording issued a second tessera; the receipt must be stable")
	}
	if _, err := h.svc.Tessera(ctx, buyerKey, tr.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCannotSkipNegotiation(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Record(ctx, buyerKey, tr.ID); !errors.Is(err, trade.ErrIllegalState) {
		t.Fatalf("Record from proposed err = %v, want ErrIllegalState", err)
	}
	if _, err := h.svc.Accept(ctx, sellerKey, tr.ID); !errors.Is(err, trade.ErrIllegalState) {
		t.Fatalf("Accept from proposed err = %v, want ErrIllegalState", err)
	}
}

func TestSelfTradeRejected(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	if _, _, err := h.svc.Create(ctx, sellerKey, h.offerID, "", ""); !errors.Is(err, trade.ErrOfferUnavailable) {
		t.Errorf("own-offer trade err = %v, want ErrOfferUnavailable", err)
	}
}

func TestOutsiderCannotTouchTrade(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.BeginNegotiation(ctx, outsider, tr.ID); !errors.Is(err, trade.ErrNotParty) {
		t.Errorf("outsider err = %v, want ErrNotParty", err)
	}
	if _, err := h.svc.Get(ctx, outsider, tr.ID); !errors.Is(err, trade.ErrNotParty) {
		t.Errorf("outsider read err = %v, want ErrNotParty", err)
	}
	if _, err := h.svc.Tessera(ctx, outsider, tr.ID); !errors.Is(err, trade.ErrNotParty) {
		t.Errorf("outsider tessera err = %v, want ErrNotParty", err)
	}
}

func TestOnchainSettlementRefused(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	_, _, err := h.svc.Create(ctx, buyerKey, h.offerID, domain.SettlementOnchain, "")
	if !errors.Is(err, trade.ErrSettlementUnconfigured) {
		t.Errorf("onchain err = %v, want ErrSettlementUnconfigured", err)
	}
}

func TestCancelAndDispute(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.svc.Cancel(ctx, buyerKey, tr.ID, "changed my mind")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != domain.TradeCancelled {
		t.Errorf("state = %s, want cancelled", cancelled.State)
	}
	if again, err := h.svc.Cancel(ctx, sellerKey, tr.ID, ""); err != nil || again.State != domain.TradeCancelled {
		t.Errorf("repeat cancel = %v, %v", again, err)
	}
	if _, err := h.svc.BeginNegotiation(ctx, buyerKey, tr.ID); !errors.Is(err, trade.ErrIllegalState) {
		t.Errorf("negotiating a cancelled trade err = %v, want ErrIllegalState", err)
	}

	tr2, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr2.ID)
	disputed, err := h.svc.Dispute(ctx, buyerKey, tr2.ID, "work not delivered")
	if err != nil {
		t.Fatal(err)
	}
	if disputed.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed", disputed.State)
	}
	if _, _, err := h.svc.Record(ctx, buyerKey, tr2.ID); !errors.Is(err, trade.ErrIllegalState) {
		t.Errorf("recording a disputed trade err = %v, want ErrIllegalState", err)
	}
}

func TestCreateIdempotency(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	first, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "trade-idem")
	if err != nil {
		t.Fatal(err)
	}
	second, isNew, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "trade-idem")
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Error("replay reported a new trade")
	}
	if first.ID != second.ID {
		t.Errorf("replay id = %s, want %s", second.ID, first.ID)
	}
}

func TestBidSwapsParties(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "bid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatalf("governed mints: %v", err)
	}
	registrySvc := registry.New(db, mints)
	for _, id := range []string{buyerKey, sellerKey} {
		if _, _, err := registrySvc.Register(ctx, id, card(id)); err != nil {
			t.Fatal(err)
		}
	}
	bid, _, err := registrySvc.PublishOffer(ctx, sellerKey, registry.NewOffer{
		Direction:       domain.DirectionBid,
		Description:     "will summarize your document",
		PriceAmount:     "5.00",
		PriceMint:       usdc,
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := trade.New(db, db, db, ledger.New(db, signer))
	tr, _, err := svc.Create(ctx, buyerKey, bid.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.BuyerAgentID != sellerKey || tr.SellerAgentID != buyerKey {
		t.Errorf("parties = %s/%s, want the bidder to be the seller", tr.BuyerAgentID, tr.SellerAgentID)
	}
}

func TestTesseraDoesNotVerifyWithWrongKey(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	_, receipt, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherSigner, err := ledger.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if h.led.VerificationKey() == otherSigner.PublicKeyBase58() {
		t.Fatal("test premise: signers must differ")
	}
	stranger := ledger.New(h.db, otherSigner)
	if _, err := stranger.Verify(receipt.JWS); err == nil {
		t.Error("tessera verified under a foreign verification key")
	}
}

func TestTesseraRejectsTamperedClaims(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	tr, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	acceptBoth(t, h, tr.ID)
	_, receipt, err := h.svc.Record(ctx, buyerKey, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(receipt.JWS, ".")
	if len(parts) != 3 {
		t.Fatalf("tessera is not a compact JWS: %q", receipt.JWS)
	}
	tampered, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(tampered), `"amount":"12.50"`, `"amount":"1.25"`, 1)
	if forged == string(tampered) {
		t.Fatal("test premise: amount claim not found in payload")
	}
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged))
	if _, err := h.led.Verify(strings.Join(parts, ".")); err == nil {
		t.Error("tessera verified after the amount claim was altered")
	}
}

func TestSuspendedAgentCannotTrade(t *testing.T) {
	ctx := context.Background()
	h := setup(t)
	if err := h.db.SetAgentStatus(ctx, sellerKey, domain.AgentSuspended, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Create(ctx, buyerKey, h.offerID, "", ""); !errors.Is(err, trade.ErrAgentUnavailable) {
		t.Errorf("err = %v, want ErrAgentUnavailable", err)
	}
}
