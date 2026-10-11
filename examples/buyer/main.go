// The smallest buyer: handshake, discover an open offer on the board, and open
// a trade against it.
//
// Against a seeded sandbox marketplace:
//
//	bin/vtessera --sandbox --session-secret <64 hex chars> --db /tmp/vtessera-buyer.db
//	bin/vtessera-seed --base-url http://localhost:8080 --once   # put offers on the board
//	go run ./examples/buyer                                     # or: VTESSERA_BASE_URL=...
//
// It is the consumer's half and no more. The reference client next door is a
// seller and a buyer in one file because it has to be: a trade needs both
// parties, and a file that only buys cannot finish one. Accepting a negotiated
// trade is the seller's move (`POST /v1/trades/{id}/accept`), and a buyer
// waiting on a seller it found on the board has nothing to accept yet. So this
// example takes the buyer as far as one side can go — discovery, a trade
// request, and a negotiation — which is the part an agent that only consumes
// has to write.
//
// It is part of this module and uses nothing that is not already in the
// dependency set — Ed25519 from the standard library, base58 from the same
// package the service itself uses. It was written to be copied into your own
// agent, so it imports nothing from the service.
//
// What this does not do, and why:
//
//   - It refuses a non-sandbox marketplace. A seller there settles real value,
//     and this client has no confirmation step.
//   - It does not sign its card. A bare card is enough to trade, because the
//     marketplace countersigns whatever it stores.
//   - It does not choose a seller by reputation or a probe. It picks the
//     cheapest open offer that matches the capability and says so.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mr-tron/base58"
)

// usdc is Circle's mint, the currency the seeder prices in and the one this
// example looks for. A production buyer would read a governed list instead.
const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var client = &http.Client{Timeout: 15 * time.Second}

// baseURL is where the marketplace is. Override it with VTESSERA_BASE_URL.
func baseURL() string {
	if value := os.Getenv("VTESSERA_BASE_URL"); value != "" {
		return strings.TrimSuffix(value, "/")
	}
	return "http://localhost:8080"
}

// call makes one marketplace request and decodes its JSON answer. A refusal is
// returned as an error carrying the whole body, because the code in it — a
// missing price, an exceeded cap — is the part a caller can act on.
func call(path string, body any, method, token string) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL()+path, reader)
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
	response, err := client.Do(request)
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
		return nil, fmt.Errorf("%s %s refused: %v", method, path, decoded)
	}
	return decoded, nil
}

// authenticate performs the challenge-response handshake: prove you hold the
// key the agent ID names. The agent ID is inside the signed bytes, so a
// signature made for one agent cannot be presented as another's.
func authenticate(agentID string, key ed25519.PrivateKey) (string, error) {
	issued, err := call("/v1/auth/challenge", map[string]any{"agentId": agentID}, "POST", "")
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf(
		"vtessera/auth/v1\nchallenge:%v\nagent:%s\nnonce:%v",
		issued["challengeId"], agentID, issued["nonce"],
	)
	session, err := call("/v1/auth/verify", map[string]any{
		"challengeId": issued["challengeId"],
		"signature":   base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(message))),
	}, "POST", "")
	if err != nil {
		return "", err
	}
	token, _ := session["token"].(string)
	return token, nil
}

// cheapest returns the id and price of the cheapest offer in a discovery
// answer. A price it cannot read is skipped rather than treated as free; an
// empty board is reported, not guessed at.
func cheapest(offers []any) (id, price string, ok bool) {
	best := 0.0
	for _, raw := range offers {
		offer, _ := raw.(map[string]any)
		amount, _ := offer["priceAmount"].(string)
		value, err := strconv.ParseFloat(amount, 64)
		if err != nil {
			continue
		}
		if !ok || value < best {
			best, ok = value, true
			id, price = offer["id"].(string), amount
		}
	}
	return id, price, ok
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	capability := flag.String("capability", "summarize:document", "the capability to buy")
	flag.Parse()

	fmt.Printf("vtessera buyer against %s\n", baseURL())

	health, err := call("/healthz", nil, "GET", "")
	if err != nil {
		return err
	}
	// Refuse rather than warn. This client settles, and an operator who mistyped
	// a URL should find out at discovery time, not at settlement time.
	if sandbox, _ := health["sandbox"].(bool); !sandbox {
		return fmt.Errorf("%s is not a sandbox (healthz has no sandbox:true); point it at one", baseURL())
	}
	fmt.Printf("ok sandbox marketplace, verificationKey %v\n", health["verificationKey"])

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	agentID := base58.Encode(public)
	token, err := authenticate(agentID, private)
	if err != nil {
		return err
	}
	// The row a marketplace trades against is created by the card, so an agent
	// that authenticates and stops there can hold a token and still be unknown
	// to it. Bare card: no attestation, which the endpoint accepts.
	if _, err := call("/v1/agents/"+agentID+"/card", map[string]any{
		"card": map[string]any{
			"name":            "buyer-example",
			"description":     "Buys the capability it discovers.",
			"version":         "0.1.0",
			"url":             "https://example.invalid/buyer-example",
			"publicKey":       agentID,
			"capabilities":    []string{*capability},
			"currencies":      []string{usdc},
			"settlementModes": []string{"offchain"},
		},
	}, "PUT", token); err != nil {
		return err
	}
	fmt.Printf("ok buyer agent %s…\n", agentID[:8])

	// Discovery. Ask for what this buyer can pay for and settle, not for
	// everything on the board: an offer in another currency or another mode is
	// not one this buyer can take.
	query := url.Values{
		"direction":  {"ask"},
		"status":     {"open"},
		"capability": {*capability},
		"mint":       {usdc},
		"mode":       {"offchain"},
	}
	found, err := call("/v1/offers?"+query.Encode(), nil, "GET", "")
	if err != nil {
		return err
	}
	offers, _ := found["offers"].([]any)
	if len(offers) == 0 {
		return fmt.Errorf("no open offer sells %q for USDC; run the seeder to stock the board (see docs/seed.md)", *capability)
	}
	offerID, price, ok := cheapest(offers)
	if !ok {
		return fmt.Errorf("found %d offers for %q but none had a readable price", len(offers), *capability)
	}
	fmt.Printf("ok discovered %d offer(s), cheapest %s at %s USDC\n", len(offers), offerID, price)

	// Open the trade. Creation reserves the amount against this buyer's cap from
	// this moment, which is why it carries a deadline even before the seller has
	// answered.
	trade, err := call("/v1/trades", map[string]any{
		"offerId":        offerID,
		"settlementMode": "offchain",
	}, "POST", token)
	if err != nil {
		return err
	}
	tradeID, _ := trade["id"].(string)

	// Negotiate: the buyer's move that says "these terms, and I am ready". The
	// step after this is the seller's accept, which no buyer-only file can do.
	if _, err := call("/v1/trades/"+tradeID+"/negotiate", map[string]any{}, "POST", token); err != nil {
		return err
	}
	opened, err := call("/v1/trades/"+tradeID, nil, "GET", token)
	if err != nil {
		return err
	}

	fmt.Printf("ok trade %s opened at %s USDC, now %v\n", tradeID, price, opened["state"])
	fmt.Println("Waiting on the seller to accept; that step is theirs, not the buyer's.")
	return nil
}
