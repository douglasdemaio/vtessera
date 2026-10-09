package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mr-tron/base58"
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

	receipt, err := l.Record(ctx, trade, "", "")
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
	if _, err := l.Record(ctx, trade, "", ""); err == nil {
		t.Fatal("expected error recording a trade that is not recorded")
	}
}

func TestRecordIsIdempotentByTrade(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")
	if _, err := l.Record(ctx, trade, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Record(ctx, trade, "", ""); err == nil {
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
		if _, err := l.Record(ctx, completedTrade(id), "", ""); err != nil {
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
	receipt, err := l.Record(ctx, completedTrade("t1"), "", "")
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
	receipt, err := l.Record(ctx, completedTrade("t1"), "", "")
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
	receipt, err := l.Record(ctx, trade, sig, "mainnet-beta")
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
	if _, err := l.Record(ctx, completedTrade("t1"), "", ""); err != nil {
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

func TestVerificationKeyReturnsSignerPublicKey(t *testing.T) {
	l, _ := newLedger(t)
	if got, want := l.VerificationKey(), l.signer.PublicKeyBase58(); got != want {
		t.Errorf("VerificationKey() = %q, want %q", got, want)
	}
}

func TestNewSignerRejectsWrongKeySize(t *testing.T) {
	if _, err := NewSigner(make([]byte, 10)); err == nil {
		t.Fatal("expected error for an undersized signing key")
	}
}

func TestLoadOrCreateSignerRejectsInvalidBase64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, []byte("not-base64!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSigner(path); err == nil {
		t.Fatal("expected error decoding a non-base64 signing key file")
	}
}

func TestLoadOrCreateSignerRejectsUndersizedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	encoded := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSigner(path); err == nil {
		t.Fatal("expected error loading an undersized signing key")
	}
}

func (l *Ledger) sign(t *testing.T, claims TesseraClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(l.signer.CurrentPrivate())
	if err != nil {
		t.Fatalf("sign test claims: %v", err)
	}
	return signed
}

func TestVerifyRejectsClaimsMissingRequiredFields(t *testing.T) {
	base := func() TesseraClaims {
		return TesseraClaims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: "t1"},
			Trade: TradeRecord{
				TradeID: "t1",
				Buyer:   "buyer-agent",
				Seller:  "seller-agent",
				Mode:    domain.SettlementOffchain,
			},
		}
	}

	cases := map[string]func(TesseraClaims) TesseraClaims{
		"missing trade id": func(c TesseraClaims) TesseraClaims {
			c.Trade.TradeID = ""
			return c
		},
		"missing buyer": func(c TesseraClaims) TesseraClaims {
			c.Trade.Buyer = ""
			return c
		},
		"missing seller": func(c TesseraClaims) TesseraClaims {
			c.Trade.Seller = ""
			return c
		},
		"missing subject": func(c TesseraClaims) TesseraClaims {
			c.Subject = ""
			return c
		},
		"invalid mode": func(c TesseraClaims) TesseraClaims {
			c.Trade.Mode = "bogus"
			return c
		},
	}

	l, _ := newLedger(t)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			jws := l.sign(t, mutate(base()))
			if _, err := l.Verify(jws); err == nil {
				t.Errorf("expected verification failure for %s", name)
			}
		})
	}
}

type fakeLedgerStore struct {
	Store
	headEntry     domain.LedgerEntry
	headFound     bool
	headErr       error
	appendErr     []error
	appendCalls   int
	appendedEntry domain.LedgerEntry
}

func (f *fakeLedgerStore) HeadLedgerEntry(ctx context.Context) (domain.LedgerEntry, bool, error) {
	return f.headEntry, f.headFound, f.headErr
}

func (f *fakeLedgerStore) AppendLedgerEntry(ctx context.Context, a domain.LedgerAppend, hashEntry domain.EntryHasher) (domain.LedgerEntry, error) {
	idx := f.appendCalls
	f.appendCalls++
	if idx < len(f.appendErr) && f.appendErr[idx] != nil {
		return domain.LedgerEntry{}, f.appendErr[idx]
	}
	return f.appendedEntry, nil
}

func TestAppendPropagatesHeadLookupError(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("head lookup failed")
	fs := &fakeLedgerStore{headErr: wantErr}
	l := New(fs, signer)

	if _, err := l.append(context.Background(), domain.LedgerAppend{}); !errors.Is(err, wantErr) {
		t.Errorf("append() error = %v, want %v", err, wantErr)
	}
	if fs.appendCalls != 0 {
		t.Errorf("appendCalls = %d, want 0 (should not attempt write after a lookup failure)", fs.appendCalls)
	}
}

func TestAppendRetriesOnStaleWriteThenSucceeds(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	want := domain.LedgerEntry{Seq: 2, Hash: "final"}
	fs := &fakeLedgerStore{
		appendErr:     []error{domain.ErrStale},
		appendedEntry: want,
	}
	l := New(fs, signer)

	got, err := l.append(context.Background(), domain.LedgerAppend{})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got.Seq != want.Seq || got.Hash != want.Hash {
		t.Errorf("append() = %+v, want %+v", got, want)
	}
	if fs.appendCalls != 2 {
		t.Errorf("appendCalls = %d, want 2 (one stale retry then success)", fs.appendCalls)
	}
}

func TestAppendGivesUpAfterRepeatedStaleWrites(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeLedgerStore{
		appendErr: []error{domain.ErrStale, domain.ErrStale, domain.ErrStale},
	}
	l := New(fs, signer)

	if _, err := l.append(context.Background(), domain.LedgerAppend{}); !errors.Is(err, domain.ErrStale) {
		t.Errorf("append() error = %v, want wrapped %v", err, domain.ErrStale)
	}
	if fs.appendCalls != appendAttempts {
		t.Errorf("appendCalls = %d, want %d", fs.appendCalls, appendAttempts)
	}
}

// TestOnchainTesseraNamesItsCluster is the reason the claim exists. A devnet
// tessera and a mainnet-beta tessera for the same trade carry the same signature
// field and the same ledger sequence, so the cluster is the only thing that
// tells a verifier which chain actually settled.
func TestOnchainTesseraNamesItsCluster(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	trade := completedTrade("t1")
	trade.SettlementMode = domain.SettlementOnchain
	trade.State = domain.TradeSettled

	receipt, err := l.Record(ctx, trade, "5Ujj8xkPMuQ6K9DkK9K7xkzGmZKrvUzZz2Ff5vJ6n9pLkQeZr4nR8sT7uV6wXyZ", "devnet")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Settlement.Cluster != "devnet" {
		t.Errorf("cluster = %q, want devnet", claims.Settlement.Cluster)
	}
}

// TestOffchainTesseraNamesNoCluster keeps the claim honest: an off-chain trade
// settled on no chain, so asserting one would be a false statement in a signed
// artifact.
func TestOffchainTesseraNamesNoCluster(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	receipt, err := l.Record(ctx, completedTrade("t1"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Settlement.Cluster != "" {
		t.Errorf("cluster = %q, want empty for an off-chain tessera", claims.Settlement.Cluster)
	}
}

// A tessera minted by a pre-Phase-3 build has no cluster claim. Its signature
// still proves the trade settled, and the design keeps such a receipt valid
// rather than invalidating receipts an operator already holds. What it cannot do
// is name a chain, and the absence has to stay visible: a verifier that needs to
// know the cluster must refuse the ambiguity, not the tessera.
func TestAPrePhase3OnchainTesseraStaysValidButNamesNoCluster(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	tr := completedTrade("t1")
	tr.SettlementMode = domain.SettlementOnchain
	tr.State = domain.TradeSettled

	// An empty cluster is what a Phase 2 build recorded.
	receipt, err := l.Record(ctx, tr, "5Ujj8xkPMuQ6K9DkK9K7xkzGmZKrvUzZz2Ff5vJ6n9pLkQeZr4nR8sT7uV6wXyZ", "")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatalf("a pre-Phase-3 receipt must stay valid, got %v", err)
	}
	if claims.Settlement.Cluster != "" {
		t.Errorf("cluster = %q, want empty: a receipt that does not name a chain must not appear to", claims.Settlement.Cluster)
	}
}

// A receipt whose claim names a different chain than the one being verified
// against is a different trade, and silently accepting it would let a mainnet
// verifier vouch for a devnet settlement.
func TestATesseraClaimingAnotherClusterIsReportedNotAccepted(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	tr := completedTrade("t2")
	tr.SettlementMode = domain.SettlementOnchain
	tr.State = domain.TradeSettled

	receipt, err := l.Record(ctx, tr, "5Ujj8xkPMuQ6K9DkK9K7xkzGmZKrvUzZz2Ff5vJ6n9pLkQeZr4nR8sT7uV6wXyZ", "devnet")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := l.Verify(receipt.JWS)
	if err != nil {
		t.Fatalf("the signature is valid whatever chain it names: %v", err)
	}
	if claims.Settlement.Cluster != "devnet" {
		t.Fatalf("cluster = %q, want the devnet claim to be readable", claims.Settlement.Cluster)
	}
	// A verifier running on mainnet-beta must be able to notice the disagreement
	// rather than reporting the receipt as settled here.
	if claims.Settlement.Cluster == "mainnet-beta" {
		t.Error("a devnet receipt must not read as a mainnet-beta one")
	}
}

func TestReceiptNamesItsSigningKey(t *testing.T) {
	l, _ := newLedger(t)
	receipt, err := l.Record(context.Background(), completedTrade("t1"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := jwt.NewParser().ParseUnverified(receipt.JWS, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse receipt: %v", err)
	}
	if got := token.Header["kid"]; got != l.VerificationKey() {
		t.Errorf("kid = %v, want the signing key %q", got, l.VerificationKey())
	}
}

func TestRotationKeepsEarlierReceiptsVerifiable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "signing.key")
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "vtessera.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	before, _, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	oldLedger := New(st, before)
	oldReceipt, err := oldLedger.Record(ctx, completedTrade("t1"), "", "")
	if err != nil {
		t.Fatal(err)
	}

	newKey, err := RotateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	after, created, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("rotation must not report the key as newly generated")
	}
	if after.PublicKeyBase58() != newKey {
		t.Fatalf("reloaded key = %q, want the rotated key %q", after.PublicKeyBase58(), newKey)
	}
	if got, want := after.VerificationKeyIDs(), []string{newKey, before.PublicKeyBase58()}; !equalStrings(got, want) {
		t.Fatalf("verification keys = %v, want %v", got, want)
	}

	rotated := New(st, after)
	if _, err := rotated.Verify(oldReceipt.JWS); err != nil {
		t.Fatalf("a receipt issued before rotation must still verify: %v", err)
	}
	if got := rotated.SignerKeyID(oldReceipt.JWS); got != before.PublicKeyBase58() {
		t.Errorf("old receipt names key %q, want the retired key %q", got, before.PublicKeyBase58())
	}

	freshReceipt, err := rotated.Record(ctx, completedTrade("t2"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := rotated.SignerKeyID(freshReceipt.JWS); got != newKey {
		t.Errorf("fresh receipt names key %q, want the current key %q", got, newKey)
	}
	if oldReceipt.JWS == freshReceipt.JWS {
		t.Error("a rotated key must produce a different signature")
	}
}

func TestRotationAccumulatesRetiredKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	first, _, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RotateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	third, err := RotateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{third, second, first.PublicKeyBase58()}
	if got := reloaded.VerificationKeyIDs(); !equalStrings(got, want) {
		t.Errorf("verification keys = %v, want %v", got, want)
	}
}

func TestLegacySignerFileLoadsAsCurrentKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	legacy, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.StdEncoding.EncodeToString(legacy.CurrentPrivate())
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, created, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatalf("a legacy single-key file must load: %v", err)
	}
	if created {
		t.Error("a legacy file must not be reported as generated")
	}
	if loaded.PublicKeyBase58() != legacy.PublicKeyBase58() {
		t.Error("the legacy key must be loaded as the current one")
	}
	if got := loaded.RetiredKeyIDs(); len(got) != 0 {
		t.Errorf("retired keys = %v, want none", got)
	}
}

func TestRotateMigratesLegacyFileToKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	legacy, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	raw := base64.StdEncoding.EncodeToString(legacy.CurrentPrivate())
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateSigner(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if body[0] != '{' {
		t.Errorf("rotated file = %q, want a JSON keyring", body)
	}
	reloaded, _, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.RetiredKeyIDs(); !equalStrings(got, []string{legacy.PublicKeyBase58()}) {
		t.Errorf("retired keys = %v, want the legacy key retired", got)
	}
}

func TestNewSignerWithRetiredRejectsMalformedKeys(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(private)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"not base58":      {"not-base58!!"},
		"wrong length":    {base58.Encode([]byte("short"))},
		"current key":     {signer.PublicKeyBase58()},
		"empty id":        {""},
		"duplicate entry": {signer.PublicKeyBase58()},
	}
	for name, retired := range cases {
		if _, err := NewSignerWithRetired(private, retired); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadOrCreateSignerRejectsCorruptKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, []byte(`{"version":1,"current":"not-base64!!"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSigner(path); err == nil {
		t.Fatal("expected an error loading a keyring with an undecodable current key")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
