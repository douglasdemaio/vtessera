// The smallest complete vtessera client: handshake, one off-chain trade, a
// receipt the reader verifies with the marketplace's own public key.
//
// Against a sandbox marketplace:
//
//	bin/vtessera --sandbox --session-secret <64 hex chars> --db /tmp/vtessera-example.db
//	go run ./examples/go                    # or: VTESSERA_BASE_URL=... go run ./examples/go
//
// It is part of this module and uses nothing that is not already in the
// dependency set — Ed25519 from the standard library, base58 from the same
// package the service itself uses. The same flow in Python and TypeScript sits
// next door, and CI runs all three against a real sandbox on every change. The
// quickstarts in docs/quickstart/ are the narrated tours of the same path;
// this file is the one to copy into your own agent.
//
// What this does not do, and why:
//
//   - It refuses a non-sandbox marketplace. Settlement moves real value there,
//     and this client has no confirmation step.
//   - It does not sign its card. A bare card is enough to trade, because the
//     marketplace countersigns whatever it stores; the quickstarts show the
//     self-signed attestation for readers who want it.
//   - It cannot be capability-probed. A probe target has to be publicly
//     reachable HTTPS, and localhost is refused by design rather than by
//     configuration.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mr-tron/base58"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var baseURL = func() string {
	if env := os.Getenv("VTESSERA_BASE_URL"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return "http://localhost:8080"
}()

// A refusal from the marketplace, kept with its own code. The service
// distinguishes a missing agent from an unpriced mint from a spent cap, and
// flattening those into "request failed" would hide the one thing a caller
// could act on.
type refused struct{ msg string }

func (r *refused) Error() string { return r.msg }

func refusef(format string, args ...any) error {
	return &refused{msg: fmt.Sprintf(format, args...)}
}

var client = &http.Client{Timeout: 15 * time.Second}

// call makes one marketplace request and decodes its JSON answer.
func call(path string, body any, method, token string) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
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
			return nil, refusef("%s %s answered %d: %s", method, path, response.StatusCode, raw)
		}
	}
	if response.StatusCode >= 400 {
		return nil, refusef("%s %s refused: %v", method, path, decoded)
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

// verifyTessera checks a receipt yourself, rather than trusting that the
// service made it. A receipt is a compact JWS, so the signed bytes are in it:
// the signature covers the exact text "header.payload". Anyone holding the
// marketplace's public key can check it, with no service in the loop, and that
// is the property worth seeing once rather than taking on trust.
func verifyTessera(jws, verificationKey string) (map[string]any, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, refusef("receipt is not a compact JWS")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, refusef("receipt header is not base64url: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, refusef("receipt header is not JSON: %v", err)
	}
	if alg, _ := header["alg"].(string); alg != "EdDSA" {
		return nil, refusef("receipt is signed with %v, not EdDSA", header["alg"])
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, refusef("receipt signature is not base64url: %v", err)
	}
	publicKeyRaw, err := base58.Decode(verificationKey)
	if err != nil {
		return nil, refusef("verificationKey is not base58: %v", err)
	}
	if len(publicKeyRaw) != ed25519.PublicKeySize {
		return nil, refusef("verificationKey is %d bytes, not %d", len(publicKeyRaw), ed25519.PublicKeySize)
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKeyRaw), []byte(parts[0]+"."+parts[1]), signature) {
		return nil, refusef("the receipt did not verify under the marketplace's public key")
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, refusef("receipt payload is not base64url: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, refusef("receipt payload is not JSON: %v", err)
	}
	return claims, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Printf("vtessera reference client against %s\n", baseURL)

	health, err := call("/healthz", nil, "GET", "")
	if err != nil {
		return err
	}
	// Refuse rather than warn. This client is for a sandbox; pointed at a
	// marketplace that settles on-chain it would move real value with no
	// confirmation step, and an operator who mistyped a URL should find out
	// loudly rather than at settlement time.
	if sandbox, _ := health["sandbox"].(bool); !sandbox {
		return refusef("%s is not a sandbox (healthz has no sandbox:true); this client settles trades — point it at one", baseURL)
	}
	fmt.Printf("ok sandbox marketplace, verificationKey %v\n", health["verificationKey"])

	sellerPublic, sellerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	buyerPublic, buyerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	sellerID := base58.Encode(sellerPublic)
	buyerID := base58.Encode(buyerPublic)
	sellerToken, err := authenticate(sellerID, sellerPrivate)
	if err != nil {
		return err
	}
	buyerToken, err := authenticate(buyerID, buyerPrivate)
	if err != nil {
		return err
	}
	fmt.Printf("ok two agents, %s… selling, %s… buying\n", sellerID[:8], buyerID[:8])

	// The row a marketplace trades against is created by the card, so an agent
	// that authenticates and stops there can hold a token and still be unknown
	// to it. Bare card: no attestation, which the endpoint accepts because
	// every agent written before attestations existed sends one.
	for _, party := range []struct{ id, token, description string }{
		{sellerID, sellerToken, "Sells a summary."},
		{buyerID, buyerToken, "Buys summaries."},
	} {
		_, err := call("/v1/agents/"+party.id+"/card", map[string]any{
			"card": map[string]any{
				"name":            "reference-client",
				"description":     party.description,
				"version":         "0.1.0",
				"url":             "https://example.invalid/reference-client",
				"publicKey":       party.id,
				"capabilities":    []string{"summarize:document"},
				"currencies":      []string{usdc},
				"settlementModes": []string{"offchain"},
			},
		}, "PUT", party.token)
		if err != nil {
			return err
		}
	}
	fmt.Println("ok both cards stored")

	offer, err := call("/v1/agents/"+sellerID+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "Summarise a document in three sentences.",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "2.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain"},
	}, "POST", sellerToken)
	if err != nil {
		return err
	}
	offerID, _ := offer["id"].(string)
	fmt.Printf("ok offer %s at 2.00 USDC\n", offerID)

	trade, err := call("/v1/trades", map[string]any{
		"offerId":        offerID,
		"settlementMode": "offchain",
	}, "POST", buyerToken)
	if err != nil {
		return err
	}
	tradeID, _ := trade["id"].(string)
	for _, step := range []struct {
		path, token string
	}{
		{"/v1/trades/" + tradeID + "/negotiate", buyerToken},
		{"/v1/trades/" + tradeID + "/accept", sellerToken},
		{"/v1/trades/" + tradeID + "/accept", buyerToken},
		{"/v1/trades/" + tradeID + "/record", buyerToken},
	} {
		if _, err := call(step.path, map[string]any{}, "POST", step.token); err != nil {
			return err
		}
	}
	fmt.Printf("ok trade %s recorded\n", tradeID)

	receipt, err := call("/v1/tesseras/"+tradeID, nil, "GET", buyerToken)
	if err != nil {
		return err
	}
	jws, _ := receipt["jws"].(string)
	verificationKey, _ := receipt["verificationKey"].(string)
	claims, err := verifyTessera(jws, verificationKey)
	if err != nil {
		return err
	}
	tradeClaims, _ := claims["trade"].(map[string]any)
	fmt.Println("ok receipt verified under the marketplace's own public key")
	fmt.Printf(
		"Done. %v of %v, settled %v\n",
		tradeClaims["amount"], tradeClaims["mint"], tradeClaims["mode"],
	)
	return nil
}
