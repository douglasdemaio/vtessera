package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

func seedOnchainTrade(t *testing.T, s *Store, tradeID string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{
		"4Tidoi6HsEK5Q3AUyLH2ZpPZ4eM3gGdrFyNsrrkT2eNe",
		"8BzQfjYwmhZ2uXuXmocRQmkZesfRkJWasEDS3ipoXfcrW",
	} {
		if err := s.CreateAgent(ctx, testAgent(id)); err != nil {
			t.Fatal(err)
		}
	}
	offer := testOffer("offer-"+tradeID, "8BzQfjYwmhZ2uXuXmocRQmkZesfRkJWasEDS3ipoXfcrW")
	offer.SettlementModes = []domain.SettlementMode{domain.SettlementOnchain}
	if err := s.CreateOffer(ctx, offer, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateTrade(ctx, domain.Trade{
		ID:             tradeID,
		OfferID:        offer.ID,
		BuyerAgentID:   "4Tidoi6HsEK5Q3AUyLH2ZpPZ4eM3gGdrFyNsrrkT2eNe",
		SellerAgentID:  offer.AgentID,
		Description:    "sentiment dataset",
		Amount:         money.MustParse("25"),
		Mint:           validMint,
		SettlementMode: domain.SettlementOnchain,
		State:          domain.TradeSettlementPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, ""); err != nil {
		t.Fatal(err)
	}
}

func testRequest(tradeID, id string, at time.Time, ttl time.Duration) domain.SettlementRequest {
	return domain.SettlementRequest{
		ID:          id,
		TradeID:     tradeID,
		UnsignedTx:  "AQAB",
		Blockhash:   "9zjePBqC5DBShJ7wzSjPQFPQjk6jJKAJgYbn3yiv1sPk",
		LastValid:   1000,
		BuyerATA:    "buyer-ata",
		SellerATA:   "seller-ata",
		CreatedATA:  true,
		FeeLamports: 500_000,
		FeeWallet:   "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh",
		Status:      domain.SettlementIssued,
		CreatedAt:   at,
		UpdatedAt:   at,
		ExpiresAt:   at.Add(ttl),
	}
}

func TestSettlementRequestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := testRequest("t1", "req-1", now, 90*time.Second)
	stored, err := s.CreateSettlementRequest(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != "req-1" {
		t.Fatalf("stored id = %q", stored.ID)
	}
	got, err := s.GetSettlementRequest(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UnsignedTx != want.UnsignedTx || got.Blockhash != want.Blockhash || got.LastValid != want.LastValid {
		t.Errorf("round trip lost the transaction: %+v", got)
	}
	if !got.CreatedATA || got.FeeLamports != 500_000 || got.Status != domain.SettlementIssued {
		t.Errorf("got = %+v, want the issued request with its fee and ATA flag", got)
	}
	if got.BuyerATA != "buyer-ata" || got.SellerATA != "seller-ata" || got.FeeWallet == "" {
		t.Errorf("ATAs or fee wallet lost in round trip: %+v", got)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("expiresAt = %s, want %s", got.ExpiresAt, want.ExpiresAt)
	}
	if !got.Live(now.Add(30 * time.Second)) {
		t.Error("request should be live before its expiry")
	}
	if got.Live(now.Add(2 * time.Minute)) {
		t.Error("request should not be live after its expiry")
	}
}

func TestCreateSettlementRequestSupersedesTheLiveOne(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC()
	first, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-1", now, 90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// A re-request after an expired blockhash is how a buyer recovers, and the
	// trade must never hold two live requests.
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-2", now.Add(time.Minute), 90*time.Second)); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveSettlementRequest(ctx, "t1", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != "req-2" {
		t.Errorf("live request = %s, want req-2", live.ID)
	}
	all, err := s.ListSettlementRequests(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("requests = %d, want 2", len(all))
	}
	if all[0].Status != domain.SettlementExpired {
		t.Errorf("first request status = %s, want %s", all[0].Status, domain.SettlementExpired)
	}
	if all[0].ID != first.ID {
		t.Errorf("first request = %s, want %s", all[0].ID, first.ID)
	}
}

func TestConfirmSettlementRequest(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC()
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-1", now, 90*time.Second)); err != nil {
		t.Fatal(err)
	}
	sig := "3n1kM1LPRPRTzXcQKCSGyHZUnQVXPPiQjRHDXpNzWJmEfjJcEbLKLzsjpmpfd1GJPPfM6P2KSPfrRtGYEPrTQ3mB"
	if err := s.SetSettlementSignature(ctx, "req-1", sig, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmSettlementRequest(ctx, "req-1", sig, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettlementRequest(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.SettlementConfirmed || got.Signature != sig {
		t.Errorf("got = %+v, want confirmed with signature %s", got, sig)
	}
	// Confirming twice is a stale write, not a second settlement.
	if err := s.ConfirmSettlementRequest(ctx, "req-1", sig, now); !errors.Is(err, ErrStale) {
		t.Errorf("second confirm err = %v, want ErrStale", err)
	}
}

func TestSetSettlementSignatureRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC()
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-1", now, 90*time.Second)); err != nil {
		t.Fatal(err)
	}
	first := "3n1kM1LPRPRTzXcQKCSGyHZUnQVXPPiQjRHDXpNzWJmEfjJcEbLKLzsjpmpfd1GJPPfM6P2KSPfrRtGYEPrTQ3mB"
	if err := s.SetSettlementSignature(ctx, "req-1", first, now); err != nil {
		t.Fatal(err)
	}
	other := "4n1kM1LPRPRTzXcQKCSGyHZUnQVXPPiQjRHDXpNzWJmEfjJcEbLKLzsjpmpfd1GJPPfM6P2KSPfrRtGYEPrTQ3mB"
	if err := s.SetSettlementSignature(ctx, "req-1", other, now); !errors.Is(err, ErrStale) {
		t.Errorf("err = %v, want ErrStale so a conflicting signature cannot be recorded", err)
	}
	got, err := s.GetSettlementRequest(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Signature != first {
		t.Errorf("signature = %q, want the first recorded %q", got.Signature, first)
	}
}

func TestLiveSettlementRequestIgnoresLapsedBlockhashes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC()
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-1", now, 90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LiveSettlementRequest(ctx, "t1", now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound once the blockhash has lapsed", err)
	}
}

func TestExpireSettlementRequests(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	now := time.Now().UTC()
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-1", now, 90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireSettlementRequests(ctx, "t1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LiveSettlementRequest(ctx, "t1", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after expiry", err)
	}
	// A fresh request is allowed again once the old one is retired.
	if _, err := s.CreateSettlementRequest(ctx, testRequest("t1", "req-2", now, 90*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestTradesInStateFindsPendingSettlements(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedOnchainTrade(t, s, "t1")
	pending, err := s.TradesInState(ctx, domain.TradeSettlementPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "t1" {
		t.Fatalf("pending = %+v, want t1", pending)
	}
	settled, err := s.TradesInState(ctx, domain.TradeSettled)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 0 {
		t.Errorf("settled = %+v, want none", settled)
	}
}
