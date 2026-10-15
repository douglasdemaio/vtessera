// Temporary standing seller for one session. It holds an Ed25519 identity,
// publishes an offer on the marketplace, and exposes an HTTPS-able endpoint (via
// an ssh tunnel) that accepts a trade when a buyer hands it the trade id. Delete
// after the session.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mr-tron/base58"
)

//go:embed buyer.py
var buyerScript []byte

//go:embed buyer.mjs
var buyerModule []byte

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

type seller struct {
	base       string
	public     string
	id         string
	key        ed25519.PrivateKey
	token      string
	client     *http.Client
	capability string
	price      string
	keyFile    string
	authMu     sync.Mutex
}

func main() {
	base := flag.String("base", envOr("VTESSERA_BASE_URL", "https://vtessera.fly.dev"), "marketplace")
	public := flag.String("public-url", os.Getenv("SELLER_PUBLIC_URL"), "the https URL this seller is reachable at (the tunnel)")
	addr := flag.String("addr", ":8099", "local listen address")
	capability := flag.String("capability", "code:explain", "capability to sell")
	price := flag.String("price", "0.10", "price in USDC")
	keyFile := flag.String("key", envOr("SELLER_KEY_FILE", "/tmp/seller-key.json"), "identity file, created on first use")
	republish := flag.Duration("republish", 10*time.Minute, "how often to check for a live listing and restock")
	flag.Parse()

	if *public == "" {
		log.Fatalf("FAILED: --public-url (SELLER_PUBLIC_URL) is required so the card can name a reachable endpoint")
	}
	s := &seller{
		base:       strings.TrimRight(*base, "/"),
		public:     strings.TrimRight(*public, "/"),
		client:     &http.Client{Timeout: 15 * time.Second},
		capability: *capability,
		price:      *price,
		keyFile:    *keyFile,
	}
	if err := s.loadOrCreateKey(); err != nil {
		log.Fatalf("FAILED: key: %v", err)
	}
	health, err := s.call("/healthz", nil, "GET", "")
	if err != nil {
		log.Fatalf("FAILED: health: %v", err)
	}
	log.Printf("marketplace %s cluster=%v sandbox=%v verificationKey=%v",
		s.base, health["cluster"], health["sandbox"], health["verificationKey"])
	if err := s.authenticate(); err != nil {
		log.Fatalf("FAILED: authenticate: %v", err)
	}
	if err := s.publishCard(); err != nil {
		log.Fatalf("FAILED: publish card: %v", err)
	}
	if err := s.closeStaleOffers(); err != nil {
		log.Fatalf("FAILED: close stale offers: %v", err)
	}
	if err := s.publishOffer(); err != nil {
		log.Fatalf("FAILED: publish offer: %v", err)
	}
	go s.keepStocked(*republish)
	log.Printf("SELLER LIVE: agent %s  capability %s at %s USDC", s.id, s.capability, s.price)
	log.Printf("listening on %s, advertised at %s, restocking every %s", *addr, s.public, *republish)
	log.Fatal(http.ListenAndServe(*addr, s.mux()))
}

func (s *seller) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agentId": s.id})
	})
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"name": "temp-seller", "version": "0.1.0", "url": s.public,
			"capabilities": []string{s.capability},
		})
	})
	mux.HandleFunc("GET /buyer.py", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/x-python; charset=utf-8")
		_, _ = w.Write(buyerScript)
	})
	mux.HandleFunc("GET /buyer.mjs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/javascript; charset=utf-8")
		_, _ = w.Write(buyerModule)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"agentId":       s.id,
			"capability":    s.capability,
			"priceUsdc":     s.price,
			"marketplace":   s.base,
			"offerBoard":    s.base + "/v1/offers",
			"buyPythonCurl": "curl -s " + s.public + "/buyer.py | python3 -",
			"buyNodeCurl":   "curl -s " + s.public + "/buyer.mjs | node --input-type=module",
		})
	})
	mux.HandleFunc("POST /task", s.handleTask)
	mux.HandleFunc("POST /", s.handleTask)
	return mux
}

func (s *seller) handleTask(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	tradeID := extractTradeID(body)
	if tradeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no tradeId in body (top-level or params.message.payload.tradeId)"})
		return
	}
	if err := s.accept(tradeID); err != nil {
		log.Printf("accept %s failed: %v", tradeID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("accepted trade %s", tradeID)
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
		"tradeId":  tradeID,
		"agentId":  s.id,
		"result":   "explained: this is where the seller's work product would go",
	})
}

func extractTradeID(body map[string]any) string {
	if v, _ := body["tradeId"].(string); v != "" {
		return v
	}
	params, _ := body["params"].(map[string]any)
	message, _ := params["message"].(map[string]any)
	payload, _ := message["payload"].(map[string]any)
	if v, _ := payload["tradeId"].(string); v != "" {
		return v
	}
	return ""
}

func (s *seller) accept(tradeID string) error {
	_, err := s.callAuthed("/v1/trades/"+tradeID+"/accept", map[string]any{}, "POST")
	return err
}

func (s *seller) publishCard() error {
	_, err := s.callAuthed("/v1/agents/"+s.id+"/card", map[string]any{"card": map[string]any{
		"name":            "standing seller",
		"description":     "Explains code. A standing seller for the marketplace.",
		"version":         "0.1.0",
		"url":             s.public,
		"publicKey":       s.id,
		"capabilities":    []string{s.capability},
		"currencies":      []string{usdc},
		"settlementModes": []string{"offchain", "onchain"},
	}}, "PUT")
	return err
}

func (s *seller) publishOffer() error {
	offer, err := s.callAuthed("/v1/agents/"+s.id+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "Explain a function or file in plain language.",
		"capabilities":    []string{s.capability},
		"priceAmount":     s.price,
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
	}, "POST")
	if err != nil {
		return err
	}
	fmt.Printf("OFFER %v live at %s USDC, endpoint %s\n", offer["id"], s.price, s.public)
	return nil
}

// openOffers returns the ids of the seller's listings a buyer can still take.
func (s *seller) openOffers() ([]string, error) {
	listed, err := s.call("/v1/agents/"+s.id+"/offers", nil, "GET", "")
	if err != nil {
		return nil, err
	}
	var open []string
	offers, _ := listed["offers"].([]any)
	for _, raw := range offers {
		offer, _ := raw.(map[string]any)
		if status, _ := offer["status"].(string); status == "open" {
			open = append(open, offer["id"].(string))
		}
	}
	return open, nil
}

// closeStaleOffers closes every open listing before boot publishes a new one. A
// redeploy mints a new offer id, and without this the previous boot's listing
// stays open until its TTL — one seller quietly holding several listings.
func (s *seller) closeStaleOffers() error {
	open, err := s.openOffers()
	if err != nil {
		return err
	}
	for _, id := range open {
		if _, err := s.call("/v1/offers/"+id+"/close", map[string]any{}, "POST", s.token); err != nil {
			return err
		}
		fmt.Printf("CLOSED stale offer %s\n", id)
	}
	return nil
}

// keepStocked keeps one live listing on the board: if the seller has none open
// — because the last one sold or lapsed — it publishes a replacement, so an
// agent that comes looking always finds a seller that can answer.
func (s *seller) keepStocked(every time.Duration) {
	for range time.Tick(every) {
		open, err := s.openOffers()
		if err != nil {
			log.Printf("restock check: %v", err)
			continue
		}
		if len(open) > 0 {
			continue
		}
		if err := s.publishOffer(); err != nil {
			log.Printf("restock publish: %v", err)
		}
	}
}

func (s *seller) loadOrCreateKey() error {
	if raw, err := os.ReadFile(s.keyFile); err == nil {
		var stored struct {
			Seed string `json:"seed"`
		}
		if json.Unmarshal(raw, &stored) == nil {
			seed, err := base64.StdEncoding.DecodeString(stored.Seed)
			if err == nil && len(seed) == ed25519.SeedSize {
				s.key = ed25519.NewKeyFromSeed(seed)
				s.id = base58.Encode(s.key.Public().(ed25519.PublicKey))
				fmt.Printf("IDENTITY reused %s…  (%s)\n", s.id[:8], s.keyFile)
				return nil
			}
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	s.key = ed25519.NewKeyFromSeed(seed)
	s.id = base58.Encode(s.key.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]string{"seed": base64.StdEncoding.EncodeToString(seed)})
	if err := os.WriteFile(s.keyFile, body, 0o600); err != nil {
		return err
	}
	fmt.Printf("IDENTITY created %s…  (%s)\n", s.id[:8], s.keyFile)
	return nil
}

func (s *seller) authenticate() error {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.authenticateLocked()
}

func (s *seller) authenticateLocked() error {
	issued, err := s.call("/v1/auth/challenge", map[string]any{"agentId": s.id}, "POST", "")
	if err != nil {
		return err
	}
	message := fmt.Sprintf("vtessera/auth/v1\nchallenge:%v\nagent:%s\nnonce:%v",
		issued["challengeId"], s.id, issued["nonce"])
	session, err := s.call("/v1/auth/verify", map[string]any{
		"challengeId": issued["challengeId"],
		"signature":   base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(message))),
	}, "POST", "")
	if err != nil {
		return err
	}
	s.token, _ = session["token"].(string)
	return nil
}

func (s *seller) call(path string, body any, method, token string) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.base+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	if token != "" {
		request.Header.Set("authorization", "Bearer "+token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("%s %s answered %d: %s", method, path, response.StatusCode, raw)
		}
	}
	if response.StatusCode >= 400 {
		return nil, &statusError{status: response.StatusCode, err: fmt.Errorf("%s %s refused %d: %v", method, path, response.StatusCode, decoded)}
	}
	return decoded, nil
}

// statusError keeps the HTTP status so a caller can tell an expired session from
// any other refusal.
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

// callAuthed performs an authenticated request and re-authenticates once if the
// session has lapsed. A session lasts an hour (auth.WithSessionTTL) while the
// seller runs for days, so without this its accept and restock calls start
// returning 401 and the listing quietly drops off the board.
func (s *seller) callAuthed(path string, body any, method string) (map[string]any, error) {
	s.authMu.Lock()
	token := s.token
	s.authMu.Unlock()

	out, err := s.call(path, body, method, token)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusUnauthorized {
		log.Printf("session lapsed on %s %s; re-authenticating", method, path)
		if aerr := s.authenticate(); aerr != nil {
			return nil, fmt.Errorf("re-authenticate after 401: %w", aerr)
		}
		s.authMu.Lock()
		token = s.token
		s.authMu.Unlock()
		return s.call(path, body, method, token)
	}
	return out, err
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
