package seed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// maxResponseBytes bounds how much of a marketplace answer is read. Every call
// this client makes is small; a response larger than this is a startup page or a
// proxy error, not something worth decoding.
const maxResponseBytes = 1 << 20

// Refused is a refusal the marketplace named, kept with its code so a caller can
// tell an unpriced mint from a missing agent rather than reading "request
// failed" for both.
type Refused struct {
	Status  int
	Code    string
	Message string
}

func (r *Refused) Error() string {
	if r.Code == "" {
		return fmt.Sprintf("marketplace refused with %d: %s", r.Status, r.Message)
	}
	return fmt.Sprintf("marketplace refused (%s): %s", r.Code, r.Message)
}

// Client speaks the subset of the public API a seed run needs. It holds no
// database and no service key: every call it makes is one an agent could make
// with curl and its own Ed25519 identity.
type Client struct {
	base string
	http *http.Client
}

func NewClient(base string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

// Health reports whether the target is up and which identity it is. It is read
// before anything is published so a mistyped URL fails on the first line rather
// than after a partial seeding.
type Health struct {
	VerificationKey string `json:"verificationKey"`
	Sandbox         bool   `json:"sandbox"`
	Cluster         string `json:"cluster"`
	Version         string `json:"version"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var health Health
	err := c.do(ctx, http.MethodGet, "/healthz", "", nil, &health)
	return health, err
}

// Authenticate performs the challenge-response handshake: prove you hold the key
// the agent ID names. The agent ID is inside the signed bytes, so a signature
// made for one seeded agent cannot be presented as another's.
func (c *Client) Authenticate(ctx context.Context, agentID string, key ed25519.PrivateKey) (string, error) {
	var challenge struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/challenge", "", map[string]any{"agentId": agentID}, &challenge); err != nil {
		return "", err
	}
	message := fmt.Sprintf("vtessera/auth/v1\nchallenge:%s\nagent:%s\nnonce:%s", challenge.ChallengeID, agentID, challenge.Nonce)
	var session struct {
		Token string `json:"token"`
	}
	payload := map[string]any{
		"challengeId": challenge.ChallengeID,
		"signature":   base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(message))),
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/verify", "", payload, &session); err != nil {
		return "", err
	}
	if session.Token == "" {
		return "", fmt.Errorf("marketplace issued an empty session token for %s", agentID)
	}
	return session.Token, nil
}

// PutCard registers the agent's card. The row a marketplace trades against is
// created here, so an agent that authenticates and stops can hold a token and
// still be unknown to it.
func (c *Client) PutCard(ctx context.Context, agentID, token string, card domain.AgentCard) error {
	return c.do(ctx, http.MethodPut, "/v1/agents/"+agentID+"/card", token, map[string]any{"card": card}, nil)
}

// OfferRequest is the publish body. It mirrors the service's request without
// importing the http layer: an extracted client that agreed with a stub would
// not prove the real endpoint accepts what the seeder sends.
type OfferRequest struct {
	Direction       domain.OfferDirection   `json:"direction"`
	Description     string                  `json:"description"`
	Capabilities    []string                `json:"capabilities"`
	PriceAmount     string                  `json:"priceAmount"`
	PriceMint       string                  `json:"priceMint"`
	SettlementModes []domain.SettlementMode `json:"settlementModes"`
}

func (c *Client) PublishOffer(ctx context.Context, agentID, token string, req OfferRequest) (domain.Offer, error) {
	var offer domain.Offer
	err := c.do(ctx, http.MethodPost, "/v1/agents/"+agentID+"/offers", token, req, &offer)
	return offer, err
}

func (c *Client) CloseOffer(ctx context.Context, token, offerID string) error {
	return c.do(ctx, http.MethodPost, "/v1/offers/"+offerID+"/close", token, map[string]any{}, nil)
}

func (c *Client) do(ctx context.Context, method, path, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, resp.Request.URL.Path, err)
	}
	if resp.StatusCode >= 400 {
		return refusal(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, resp.Request.URL.Path, err)
		}
	}
	return nil
}

func refusal(status int, body []byte) error {
	var decoded struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &decoded)
	message := decoded.Error
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return &Refused{Status: status, Code: decoded.Code, Message: message}
}
