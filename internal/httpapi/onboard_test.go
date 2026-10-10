package httpapi_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/mr-tron/base58"
)

// newOnboardableAgent returns an identity with a key and no session: exactly
// the state an agent is in when it has read llms.txt and has not authenticated
// yet.
func newOnboardableAgent(t *testing.T, server *httptest.Server) *agentClient {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &agentClient{
		t:       t,
		base:    server.URL,
		http:    server.Client(),
		id:      base58.Encode(public),
		private: private,
	}
}

func (c *agentClient) issueChallenge() (string, string) {
	c.t.Helper()
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	body := c.do(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": c.id}, false)
	if body == nil {
		c.t.Fatal("challenge failed")
	}
	decodeInto(c.t, body, &issued)
	return issued.ChallengeID, issued.Nonce
}

// onboard presents the challenge, the signature over it, the card and the first
// offer as one request. The offer is a map because the offer's shape is what
// several of these tests vary.
func (c *agentClient) onboard(card domain.AgentCard, cardSig *attest.Signature, offer map[string]any) (int, []byte) {
	c.t.Helper()
	challengeID, nonce := c.issueChallenge()
	signature := ed25519.Sign(c.private, auth.Message(challengeID, c.id, nonce))
	payload := map[string]any{
		"challengeId": challengeID,
		"signature":   base64.StdEncoding.EncodeToString(signature),
		"card":        card,
	}
	if cardSig != nil {
		payload["cardAttestation"] = cardSig
	}
	if offer != nil {
		payload["offer"] = offer
	}
	return c.raw(http.MethodPost, "/v1/auth/onboard", payload, false)
}

func (c *agentClient) signedCard() (domain.AgentCard, *attest.Signature) {
	c.t.Helper()
	sig := c.signCard(c.signingCard())
	return c.signingCardBody(), &sig
}

func (c *agentClient) cardSig() *attest.Signature {
	c.t.Helper()
	_, sig := c.signedCard()
	return sig
}

func (c *agentClient) onboardOffer(offerID, amount, mint string) map[string]any {
	return map[string]any{
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     amount,
		"priceMint":       mint,
		"settlementModes": []string{"offchain", "onchain"},
		"offerId":         offerID,
	}
}

type onboardResponse struct {
	AgentID   string       `json:"agentId"`
	Token     string       `json:"token"`
	TokenType string       `json:"tokenType"`
	ExpiresAt string       `json:"expiresAt"`
	Agent     domain.Agent `json:"agent"`
	Offer     domain.Offer `json:"offer"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

// An agent that follows llms.txt — challenge, then one request carrying the card
// and the first listing — ends up with a session, a published card and a
// buyable offer, without a second authenticated round trip.
func TestOnboardingRegistersTheAgentCardAndFirstOfferInOneRequest(t *testing.T) {
	server, _ := setupServer(t)
	agent := newOnboardableAgent(t, server)

	card, cardSig := agent.signedCard()
	offerID := "11111111-2222-3333-4444-555555555555"
	offer := agent.onboardOffer(offerID, "10.00", usdc)
	offer["attestation"] = agent.signOffer(agent.signingOffer(offerID, "10.00"))

	status, body := agent.onboard(card, cardSig, offer)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/auth/onboard = %d %s, want 201", status, body)
	}
	var res onboardResponse
	decodeInto(t, body, &res)
	if res.AgentID != agent.id || res.Agent.ID != agent.id {
		t.Errorf("onboarded agent = %q/%q, want %q", res.AgentID, res.Agent.ID, agent.id)
	}
	if res.Token == "" || res.TokenType != "Bearer" {
		t.Errorf("session = token %q type %q, want a bearer token", res.Token, res.TokenType)
	}
	if res.Offer.ID != offerID || res.Offer.AgentID != agent.id {
		t.Errorf("offer = %q for %q, want %q for %q", res.Offer.ID, res.Offer.AgentID, offerID, agent.id)
	}
	if res.Offer.Status != domain.OfferOpen {
		t.Errorf("offer status = %q, want %q", res.Offer.Status, domain.OfferOpen)
	}

	// The token is a session, not a receipt: it authenticates like any other.
	agent.token = res.Token
	if agent.do(http.MethodGet, "/v1/agents/"+url.PathEscape(agent.id), nil, true) == nil {
		t.Error("the session returned by onboarding does not authenticate")
	}

	// Both signatures on the card are recorded and verify, so the one-shot path
	// is not a path that quietly skips what the two-call path records.
	verdict := agent.cardVerdict(agent.id)
	if !verdict.Marketplace.Attested || !verdict.Marketplace.Valid {
		t.Errorf("marketplace attestation = attested %v valid %v, want both true",
			verdict.Marketplace.Attested, verdict.Marketplace.Valid)
	}
	if !verdict.Agent.Attested || !verdict.Agent.Valid {
		t.Errorf("agent attestation = attested %v valid %v, want both true",
			verdict.Agent.Attested, verdict.Agent.Valid)
	}
	if offerVerdict := agent.offerVerdict(offerID); !offerVerdict.Signed || !offerVerdict.Valid {
		t.Errorf("offer attestation = signed %v valid %v, want both true",
			offerVerdict.Signed, offerVerdict.Valid)
	}
}

// Onboarding is the fresh path, and the answer for an agent that already exists
// points at the routes that update one rather than replacing its card and terms.
func TestOnboardingRefusesAnAgentThatIsAlreadyRegistered(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	status, body := agent.onboard(agent.signingCardBody(), nil,
		agent.onboardOffer("11111111-2222-3333-4444-555555555555", "10.00", usdc))
	if status != http.StatusConflict {
		t.Fatalf("onboard an already registered agent = %d %s, want 409", status, body)
	}
	var resp errorResponse
	decodeInto(t, body, &resp)
	if resp.Code != "AGENT_ALREADY_REGISTERED" {
		t.Errorf("code = %q, want AGENT_ALREADY_REGISTERED", resp.Code)
	}
	if !strings.Contains(resp.Message, "already registered") {
		t.Errorf("message = %q, want it to name the already registered agent", resp.Message)
	}
}

// A card signature that does not verify is refused before anything is written,
// and the same identity can still onboard afterwards: the refusal left no trace.
func TestOnboardingRefusesACardSignatureThatDoesNotVerifyAndRegistersNothing(t *testing.T) {
	server, _ := setupServer(t)
	agent := newOnboardableAgent(t, server)

	// Signed over a different name than the one being published.
	bad := agent.signingCard()
	bad.Name = "honest agent"
	badSig := agent.signCard(bad)

	card := agent.signingCardBody()
	status, body := agent.onboard(card, &badSig,
		agent.onboardOffer("11111111-2222-3333-4444-555555555555", "10.00", usdc))
	if status != http.StatusBadRequest {
		t.Fatalf("onboard with a bad card signature = %d %s, want 400", status, body)
	}
	var resp errorResponse
	decodeInto(t, body, &resp)
	if resp.Code != "ATTESTATION_REFUSED" {
		t.Errorf("code = %q, want ATTESTATION_REFUSED", resp.Code)
	}

	// The challenge was consumed and nothing was stored, so the same identity
	// onboards cleanly on the next attempt: no half-written agent to clear.
	status, body = agent.onboard(card, agent.cardSig(), agent.onboardOffer(
		"11111111-2222-3333-4444-555555555555", "10.00", usdc))
	if status != http.StatusCreated {
		t.Fatalf("onboard after the refusal = %d %s, want 201", status, body)
	}
}

// The offer is part of the same transaction as the card, so an offer the
// registry refuses writes no agent. The proof is the retry: if the first
// attempt had left an agent behind, it would be refused as already registered.
func TestOnboardingRefusingTheOfferLeavesNoAgentBehind(t *testing.T) {
	server, _ := setupServer(t)
	agent := newOnboardableAgent(t, server)

	card := agent.signingCardBody()
	// eurc is not in the card's currencies, so the listing is refused.
	status, body := agent.onboard(card, nil,
		agent.onboardOffer("11111111-2222-3333-4444-555555555555", "10.00", eurc))
	if status != http.StatusBadRequest {
		t.Fatalf("onboard with a refused offer = %d %s, want 400", status, body)
	}
	var resp errorResponse
	decodeInto(t, body, &resp)
	if resp.Code != "CURRENCY_NOT_ACCEPTED" {
		t.Errorf("code = %q, want CURRENCY_NOT_ACCEPTED", resp.Code)
	}

	status, body = agent.onboard(card, nil,
		agent.onboardOffer("11111111-2222-3333-4444-555555555555", "10.00", usdc))
	if status != http.StatusCreated {
		t.Fatalf("onboard after the refusal = %d %s, want 201: a half-written agent would be refused as already registered",
			status, body)
	}
}

// The challenge has to be redeemed by the key it was issued for, the same way
// /v1/auth/verify demands it: otherwise any key that signs a message the caller
// chose would book an agent under an identity it does not hold.
func TestOnboardingRefusesASignatureFromAnotherKey(t *testing.T) {
	server, _ := setupServer(t)
	agent := newOnboardableAgent(t, server)

	challengeID, nonce := agent.issueChallenge()
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(
		ed25519.Sign(other, auth.Message(challengeID, agent.id, nonce)))

	status, body := agent.raw(http.MethodPost, "/v1/auth/onboard", map[string]any{
		"challengeId": challengeID,
		"signature":   signature,
		"card":        agent.signingCardBody(),
		"offer":       agent.onboardOffer("11111111-2222-3333-4444-555555555555", "10.00", usdc),
	}, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("onboard with another key's signature = %d %s, want 401", status, body)
	}
}
