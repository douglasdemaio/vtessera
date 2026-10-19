package seed

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/mr-tron/base58"
)

// fakeMarketplace is a stand-in for the public API the seeder talks to. It
// verifies the challenge signature rather than accepting any bytes, so a seeder
// that signed the wrong message would fail here the way it would in production.
type fakeMarketplace struct {
	mu       sync.Mutex
	ttl      time.Duration
	nonces   map[string]string
	cards    map[string]domain.AgentCard
	offers   map[string]offerRecord
	nextID   int
	server   *httptest.Server
	verKey   string
	sandbox  bool
	cluster  string
	healthOK bool
}

type offerRecord struct {
	agentID string
	offer   domain.Offer
	closed  bool
}

func newFakeMarketplace(t *testing.T) *fakeMarketplace {
	t.Helper()
	f := &fakeMarketplace{
		ttl:      24 * time.Hour,
		nonces:   map[string]string{},
		cards:    map[string]domain.AgentCard{},
		offers:   map[string]offerRecord{},
		verKey:   "Fake111111111111111111111111111111111111111",
		sandbox:  true,
		healthOK: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", f.health)
	mux.HandleFunc("POST /v1/auth/challenge", f.challenge)
	mux.HandleFunc("POST /v1/auth/verify", f.verify)
	mux.HandleFunc("PUT /v1/agents/{id}/card", f.putCard)
	mux.HandleFunc("POST /v1/agents/{id}/offers", f.publishOffer)
	mux.HandleFunc("POST /v1/offers/{id}/close", f.closeOffer)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (f *fakeMarketplace) health(w http.ResponseWriter, _ *http.Request) {
	if !f.healthOK {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "down", "code": "INTERNAL"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"verificationKey": f.verKey,
		"sandbox":         f.sandbox,
		"cluster":         f.cluster,
	})
}

func (f *fakeMarketplace) challenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agentId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if _, err := domain.ParsePublicKey(req.AgentID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "INVALID_REQUEST"})
		return
	}
	f.mu.Lock()
	id := fmt.Sprintf("challenge-%d", len(f.nonces)+1)
	f.nonces[id] = req.AgentID
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"challengeId": id, "agentId": req.AgentID, "nonce": "nonce-" + id})
}

func (f *fakeMarketplace) verify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challengeId"`
		Signature   string `json:"signature"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	agentID, ok := f.nonces[req.ChallengeID]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unknown challenge", "code": "UNAUTHORIZED"})
		return
	}
	public, err := domain.ParsePublicKey(agentID)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error(), "code": "UNAUTHORIZED"})
		return
	}
	raw, err := base58.Decode(public)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad public key", "code": "UNAUTHORIZED"})
		return
	}
	signature, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad signature encoding", "code": "UNAUTHORIZED"})
		return
	}
	message := fmt.Sprintf("vtessera/auth/v1\nchallenge:%s\nagent:%s\nnonce:%s", req.ChallengeID, agentID, "nonce-"+req.ChallengeID)
	if !ed25519.Verify(ed25519.PublicKey(raw), []byte(message), signature) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "signature does not verify", "code": "UNAUTHORIZED"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agentId": agentID, "token": "session-" + agentID, "tokenType": "Bearer"})
}

func (f *fakeMarketplace) putCard(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	var req struct {
		Card domain.AgentCard `json:"card"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Card.PublicKey != agentID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "card publicKey does not match path", "code": "INVALID_REQUEST"})
		return
	}
	if err := req.Card.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "INVALID_REQUEST"})
		return
	}
	f.mu.Lock()
	f.cards[agentID] = req.Card
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, req.Card)
}

func (f *fakeMarketplace) publishOffer(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	var req OfferRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.PriceAmount == "" || req.PriceAmount == "0" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "offer price amount must be greater than zero", "code": "INVALID_REQUEST"})
		return
	}
	f.mu.Lock()
	f.nextID++
	now := time.Now().UTC()
	offer := domain.Offer{
		ID:              fmt.Sprintf("offer-%d", f.nextID),
		AgentID:         agentID,
		Direction:       req.Direction,
		Description:     req.Description,
		Capabilities:    req.Capabilities,
		PriceAmount:     money.MustParse(req.PriceAmount),
		PriceMint:       req.PriceMint,
		SettlementModes: req.SettlementModes,
		Status:          domain.OfferOpen,
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now.Add(f.ttl),
	}
	f.offers[offer.ID] = offerRecord{agentID: agentID, offer: offer}
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, offer)
}

func (f *fakeMarketplace) closeOffer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	record, ok := f.offers[id]
	if ok {
		record.closed = true
		record.offer.Status = domain.OfferClosed
		f.offers[id] = record
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such offer", "code": "NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusOK, record.offer)
}

func (f *fakeMarketplace) openOffers() []domain.Offer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Offer
	for _, record := range f.offers {
		if !record.closed {
			out = append(out, record.offer)
		}
	}
	return out
}

func (f *fakeMarketplace) closedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, record := range f.offers {
		if record.closed {
			count++
		}
	}
	return count
}

func (f *fakeMarketplace) agentIDs() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := map[string]bool{}
	for id := range f.cards {
		ids[id] = true
	}
	return ids
}

func testCatalog() []Listing {
	return []Listing{
		{Name: "seeded-summarize", Description: "Summarizes.", URL: "https://seed.test/summarize", Capabilities: []string{"summarize:document"}, Offer: "Summarize a document.", PriceAmount: "0.25"},
		{Name: "seeded-explain", Description: "Explains.", URL: "https://seed.test/explain", Capabilities: []string{"code:explain"}, Offer: "Explain some code.", PriceAmount: "0.10"},
	}
}

func newSeeder(t *testing.T, f *fakeMarketplace, refresh time.Duration) *Seeder {
	t.Helper()
	seeder, err := New(Config{
		BaseURL:     f.server.URL,
		KeyringPath: t.TempDir() + "/seed-keyring.json",
		Refresh:     refresh,
		Timeout:     5 * time.Second,
		Catalog:     testCatalog(),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return seeder
}

func TestSeedPublishesAnOfferForEveryListing(t *testing.T) {
	f := newFakeMarketplace(t)
	seeder := newSeeder(t, f, 12*time.Hour)
	if err := seeder.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	open := f.openOffers()
	if len(open) != len(testCatalog()) {
		t.Fatalf("open offers = %d, want %d", len(open), len(testCatalog()))
	}
	if len(f.agentIDs()) != len(testCatalog()) {
		t.Fatalf("registered agents = %d, want %d", len(f.agentIDs()), len(testCatalog()))
	}
}

func TestSeedRefreshReplacesTheOfferItPublishedBefore(t *testing.T) {
	f := newFakeMarketplace(t)
	seeder := newSeeder(t, f, 12*time.Hour)
	ctx := context.Background()
	if err := seeder.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := seeder.Pass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := len(f.openOffers()); got != len(testCatalog()) {
		t.Fatalf("open offers after refresh = %d, want %d (a listing should be replaced, not duplicated)", got, len(testCatalog()))
	}
	if got := f.closedCount(); got != len(testCatalog()) {
		t.Fatalf("closed offers = %d, want %d (each refresh should close the one it supersedes)", got, len(testCatalog()))
	}
}

func TestSeedKeepsAgentIdentityAcrossRestart(t *testing.T) {
	f := newFakeMarketplace(t)
	keyringPath := t.TempDir() + "/seed-keyring.json"
	cfg := Config{
		BaseURL:     f.server.URL,
		KeyringPath: keyringPath,
		Refresh:     12 * time.Hour,
		Timeout:     5 * time.Second,
		Catalog:     testCatalog(),
		Logger:      discardLogger(),
	}
	first, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.Pass(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	before := f.agentIDs()

	second, err := New(cfg)
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	if err := second.Pass(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	after := f.agentIDs()
	if len(after) != len(before) {
		t.Fatalf("registered agents changed from %d to %d; a restart must reuse the keyring, not mint new identities", len(before), len(after))
	}
	for id := range before {
		if !after[id] {
			t.Fatalf("agent %s vanished after restart", id)
		}
	}
}

func TestSeedRefusesARefreshThatOutrunsTheOfferTTL(t *testing.T) {
	f := newFakeMarketplace(t)
	f.ttl = time.Hour
	seeder := newSeeder(t, f, 2*time.Hour)
	err := seeder.Pass(context.Background())
	if err == nil {
		t.Fatal("Pass succeeded, want a refusal: a refresh of 2h against a 1h TTL would let offers lapse")
	}
	if !strings.Contains(err.Error(), "TTL") {
		t.Fatalf("error %q does not name the TTL", err)
	}
}

func TestSeedClosesItsOffersWhenAskedOnExit(t *testing.T) {
	f := newFakeMarketplace(t)
	seeder, err := New(Config{
		BaseURL:     f.server.URL,
		KeyringPath: t.TempDir() + "/seed-keyring.json",
		Refresh:     50 * time.Millisecond,
		Timeout:     5 * time.Second,
		Catalog:     testCatalog(),
		CloseOnExit: true,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- seeder.Run(ctx) }()
	waitForPublishedOffers(t, seeder, testCatalog())
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(f.openOffers()); got != 0 {
		t.Fatalf("open offers after a close-on-exit shutdown = %d, want 0", got)
	}
}

// waitForPublishedOffers blocks until the seeder has recorded a live offer for
// every listing. It reads the seeder's own bookkeeping rather than the fake
// marketplace's, because the seeder records a listing only once that listing's
// publish request has returned. A test that cancelled as soon as the marketplace
// had the offers would abort the very pass it means to observe: the second publish
// would be cut off mid-flight, Run would report the cancellation as a failed pass,
// and close-on-exit would have nothing recorded to close.
func waitForPublishedOffers(t *testing.T, s *Seeder, listings []Listing) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if recordedOffers(s, listings) == len(listings) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the seeder recorded an offer for %d of %d listings before the deadline", recordedOffers(s, listings), len(listings))
}

// recordedOffers counts the listings the seeder has published and not yet
// replaced, read from the seeder rather than the marketplace.
func recordedOffers(s *Seeder, listings []Listing) int {
	published := 0
	for _, listing := range listings {
		if id, ok := s.openOffer(listing.Name); ok && id != "" {
			published++
		}
	}
	return published
}

func TestOpenKeyringRefusesAnUnknownVersion(t *testing.T) {
	path := t.TempDir() + "/keyring.json"
	writeFile(t, path, `{"version":99,"keys":{}}`)
	if _, err := OpenKeyring(path); err == nil {
		t.Fatal("expected an error for a keyring with an unknown version")
	} else if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error = %v, want it to name the version", err)
	}
}

func TestParseCatalogRefusesAnEmptyList(t *testing.T) {
	path := t.TempDir() + "/catalog.json"
	writeFile(t, path, `[]`)
	if _, err := LoadCatalog(path); err == nil {
		t.Fatal("expected an error for a catalog that lists no agents")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSeedRefusesAnUnacknowledgedMainnetTarget(t *testing.T) {
	f := newFakeMarketplace(t)
	f.sandbox = false
	f.cluster = "mainnet-beta"
	seeder := newSeeder(t, f, 12*time.Hour)
	if err := seeder.Once(context.Background()); err == nil {
		t.Fatal("expected a refusal when seeding mainnet without an acknowledgement")
	} else if !strings.Contains(err.Error(), "allow-mainnet") {
		t.Fatalf("error = %v, want it to name --allow-mainnet", err)
	}

	seeder.cfg.AllowMainnet = true
	if err := seeder.Once(context.Background()); err != nil {
		t.Fatalf("Once with the acknowledgement: %v", err)
	}
}
