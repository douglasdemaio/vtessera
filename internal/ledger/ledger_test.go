package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/store"
)

const mint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

func newLedger(t *testing.T) (*Ledger, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "vtessera.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	l := New(st, signer)
	l.now = func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	return l, st
}

func completedTrade(id string) domain.Trade {
	return domain.Trade{
		ID:             id,
		OfferID:        "offer-1",
		BuyerAgentID:   "buyer-agent",
		SellerAgentID:  "seller-agent",
		Description:    "summarization job",
		Amount:         money.MustParse("25"),
		Mint:           mint,
		SettlementMode: domain.SettlementOffchain,
		State:          domain.TradeRecorded,
	}
}

func TestRecordIssuesVerifiableTessera(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")

	receipt, err := l.Record(ctx, trade, "")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if receipt.TradeID != "t1" || receipt.ID == "" {
		t.Errorf("receipt = %+v, want trade t1 with an id", receipt)
	}

	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "t1" {
		t.Errorf("sub = %q, want t1", claims.Subject)
	}
	if claims.Trade.Amount != "25" || claims.Trade.Mint != mint {
		t.Errorf("trade claims = %+v, want amount 25 mint %s", claims.Trade, mint)
	}
	if claims.Trade.Mode != domain.SettlementOffchain {
		t.Errorf("mode = %s, want offchain", claims.Trade.Mode)
	}
	if claims.Settlement.LedgerSequence != 1 {
		t.Errorf("ledger sequence = %d, want 1", claims.Settlement.LedgerSequence)
	}
	if claims.Settlement.LedgerEntryHash == "" {
		t.Error("tessera must reference its ledger entry hash")
	}
	if claims.Issuer != "vtessera" {
		t.Errorf("iss = %q, want vtessera", claims.Issuer)
	}
}

func TestRecordRejectsNonCompletableState(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")
	trade.State = domain.TradeNegotiating
	if _, err := l.Record(ctx, trade, ""); err == nil {
		t.Fatal("expected error recording a trade that is not recorded")
	}
}

func TestRecordIsIdempotentByTrade(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")
	if _, err := l.Record(ctx, trade, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Record(ctx, trade, ""); err == nil {
		t.Fatal("expected second receipt issuance to be refused")
	}
	entries, err := l.Entries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger entries = %d, want 1 (no duplicate append)", len(entries))
	}
}

func TestLedgerChainLinksEntries(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, err := l.Record(ctx, completedTrade(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := l.Entries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].PrevHash != domain.GenesisHash {
		t.Errorf("first prevHash = %s, want genesis", entries[0].PrevHash)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].PrevHash != entries[i-1].Hash {
			t.Errorf("entry %d prevHash = %s, want %s", i, entries[i].PrevHash, entries[i-1].Hash)
		}
		if entries[i].Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, entries[i].Seq, i+1)
		}
		if entries[i].Hash != EntryHash(entries[i].Seq, entries[i].PrevHash, entries[i].PayloadHash) {
			t.Errorf("entry %d hash does not recompute", i)
		}
		if entries[i].PayloadHash != PayloadHash(entries[i].Payload) {
			t.Errorf("entry %d payloadHash does not match payload", i)
		}
	}
}

func TestVerifyRejectsTamperedTessera(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	receipt, err := l.Record(ctx, completedTrade("t1"), "")
	if err != nil {
		t.Fatal(err)
	}
	tampered := receipt.JWS[:len(receipt.JWS)-4] + "AAAA"
	if _, err := l.Verify(tampered); err == nil {
		t.Fatal("expected verification failure for tampered signature")
	}
	if _, err := l.Verify("not.a.jws"); err == nil {
		t.Fatal("expected verification failure for garbage input")
	}
}

func TestVerifyRejectsForeignSigner(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	other, _ := newLedger(t)
	receipt, err := l.Record(ctx, completedTrade("t1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(receipt.JWS); err == nil {
		t.Fatal("a tessera from a different service key must not verify")
	}
}

func TestOnchainTesseraCarriesSignature(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")
	trade.SettlementMode = domain.SettlementOnchain
	trade.State = domain.TradeSettled
	const sig = "5Ujj8xkPMuQ6K9DkK9K7xkzGmZKrvUzZz2Ff5vJ6n9pLkQeZr4nR8sT7uV6wXyZ"
	receipt, err := l.Record(ctx, trade, sig)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Settlement.Signature != sig {
		t.Errorf("signature = %q, want %q", claims.Settlement.Signature, sig)
	}
	if claims.Trade.Mode != domain.SettlementOnchain {
		t.Errorf("mode = %s, want onchain", claims.Trade.Mode)
	}
}

func TestLoadOrCreateSignerPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "signing.key")
	first, created, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected a newly created key on first call")
	}
	second, created, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("expected existing key on second call")
	}
	if first.PublicKeyBase58() != second.PublicKeyBase58() {
		t.Error("reloaded signer must publish the same verification key")
	}
	if len(first.PublicKeyBase58()) < 32 {
		t.Errorf("verification key %q looks too short", first.PublicKeyBase58())
	}
}

func TestReceiptFetch(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	if _, err := l.Record(ctx, completedTrade("t1"), ""); err != nil {
		t.Fatal(err)
	}
	got, err := l.Receipt(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verify(got.JWS); err != nil {
		t.Errorf("stored receipt must verify: %v", err)
	}
	if _, err := l.Receipt(ctx, "missing"); err == nil {
		t.Error("expected error for trade without a receipt")
	}
}
