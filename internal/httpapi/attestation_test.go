package httpapi_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
)

// signedCard is the card every attestation test signs. It is a variable so a
// test can sign one card and publish a different one without repeating the
// whole declaration.
func (c *agentClient) signingCard() attest.Card {
	return attest.Card{
		AgentID:      c.id,
		Name:         "signing agent",
		Description:  "signs its own card",
		Version:      "1.0.0",
		URL:          "https://signer.example.com",
		Capabilities: []string{"summarize:document"},
		Skills: []attest.Skill{{
			ID: "summarize", Name: "Summarize",
			Tags: []string{"text"}, Input: []string{"text/plain"}, Output: []string{"text/plain"},
		}},
		Currencies: []string{usdc},
	}
}

// signingCardBody is the card that signingCard describes, as the route takes it.
func (c *agentClient) signingCardBody() domain.AgentCard {
	return domain.AgentCard{
		Name: "signing agent", Description: "signs its own card", Version: "1.0.0",
		URL: "https://signer.example.com", PublicKey: c.id,
		Capabilities: []string{"summarize:document"},
		Skills: []domain.AgentSkill{{
			ID: "summarize", Name: "Summarize",
			Tags: []string{"text"}, Input: []string{"text/plain"}, Output: []string{"text/plain"},
		}},
		Currencies: []string{usdc},
	}
}

func (c *agentClient) signCard(card attest.Card) attest.Signature {
	c.t.Helper()
	key, err := attest.NewSigningKey(c.private)
	if err != nil {
		c.t.Fatal(err)
	}
	sig, err := key.SignCard(card, time.Now().UTC())
	if err != nil {
		c.t.Fatal(err)
	}
	return sig
}

// signingOffer mirrors the projection the service makes of a stored offer, so a
// test that signs an offer signs the same bytes a verifier will reconstruct.
// USDC is 6 decimals, so 10.00 is 10000000 base units.
func (c *agentClient) signingOffer(offerID, amount string) attest.Offer {
	units := amount
	if amount == "10.00" {
		units = strconv.Itoa(10_000_000)
	} else if amount == "1.00" {
		units = strconv.Itoa(1_000_000)
	}
	return attest.Offer{
		ID:              offerID,
		Seller:          c.id,
		Description:     "summarize a document",
		Direction:       "ask",
		Capabilities:    []string{"summarize:document"},
		Mint:            usdc,
		Scale:           "6",
		AmountBaseUnits: units,
		UnitAmount:      amount,
		SettlementModes: []string{"offchain", "onchain"},
	}
}

func (c *agentClient) signOffer(offer attest.Offer) attest.Signature {
	c.t.Helper()
	key, err := attest.NewSigningKey(c.private)
	if err != nil {
		c.t.Fatal(err)
	}
	sig, err := key.SignOffer(offer, time.Now().UTC())
	if err != nil {
		c.t.Fatal(err)
	}
	return sig
}

func (c *agentClient) putCard(card domain.AgentCard, sig *attest.Signature) (int, []byte) {
	c.t.Helper()
	payload := map[string]any{"card": card}
	if sig != nil {
		payload["attestation"] = sig
	}
	return c.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(c.id)+"/card", payload, true)
}

// cardVerdict is the two-sided answer the card attestation route gives. Both
// sides are checked by a test, because the failure mode of keeping only one is
// silent: the route would keep returning 200 with a verdict nobody reads.
type cardVerdict struct {
	Recorded    bool `json:"recorded"`
	Marketplace struct {
		Attested  bool             `json:"attested"`
		Valid     bool             `json:"valid"`
		KeyID     string           `json:"keyId"`
		Signature attest.Signature `json:"signature"`
	} `json:"marketplace"`
	Agent struct {
		Attested  bool             `json:"attested"`
		Valid     bool             `json:"valid"`
		KeyID     string           `json:"keyId"`
		Signature attest.Signature `json:"signature"`
	} `json:"agent"`
}

type offerVerdict struct {
	Signed    bool             `json:"signed"`
	Valid     bool             `json:"valid"`
	Signature attest.Signature `json:"signature"`
}

func (c *agentClient) cardVerdict(agentID string) cardVerdict {
	c.t.Helper()
	var out cardVerdict
	decodeInto(c.t, c.do(http.MethodGet, "/v1/agents/"+url.PathEscape(agentID)+"/attestation", nil, false), &out)
	return out
}

func (c *agentClient) offerVerdict(offerID string) offerVerdict {
	c.t.Helper()
	var out offerVerdict
	decodeInto(c.t, c.do(http.MethodGet, "/v1/offers/"+offerID+"/attestation", nil, false), &out)
	return out
}

// A card an agent signed is stored with the signature and reports as valid. This
// is the whole feature: a reader can tell that a card came from the agent rather
// than from whoever wrote it into the marketplace.
func TestACardAnAgentSignsIsStoredAndReadsBackAsValid(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	card := agent.signingCardBody()
	sig := agent.signCard(agent.signingCard())

	status, body := agent.putCard(card, &sig)
	if status != http.StatusOK {
		t.Fatalf("PUT signed card = %d %s, want 200", status, body)
	}

	verdict := agent.cardVerdict(agent.id)
	if !verdict.Agent.Attested || !verdict.Agent.Valid {
		t.Errorf("agent side reports attested=%v valid=%v, want both true", verdict.Agent.Attested, verdict.Agent.Valid)
	}
	if verdict.Agent.KeyID != agent.id {
		t.Errorf("agent keyId = %s, want the agent's own key %s", verdict.Agent.KeyID, agent.id)
	}
	// The marketplace signs every card it publishes, whether or not the agent
	// signed its own, so a directory can attribute the card to a marketplace
	// without depending on the agent cooperating.
	if !verdict.Marketplace.Attested || !verdict.Marketplace.Valid {
		t.Errorf("marketplace side reports attested=%v valid=%v, want both true", verdict.Marketplace.Attested, verdict.Marketplace.Valid)
	}
	if verdict.Marketplace.KeyID == "" || verdict.Marketplace.KeyID == agent.id {
		t.Errorf("marketplace keyId = %s, want this marketplace's own key and not the agent's", verdict.Marketplace.KeyID)
	}
}

// An unsigned card stays acceptable, because every agent registered before
// attestations existed has one and refusing would lock them all out. The answer
// is reported rather than left to be inferred from silence.
func TestAnUnsignedCardIsStoredAndReportsItselfUnsigned(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	status, body := agent.putCard(domain.AgentCard{
		Name: "unsigned", URL: "https://unsigned.example.com", PublicKey: agent.id,
		Currencies: []string{usdc},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT unsigned card = %d %s, want 200: refusing would lock out every agent registered before this existed", status, body)
	}

	verdict := agent.cardVerdict(agent.id)
	if verdict.Agent.Attested {
		t.Error("an unsigned card reports an agent signature")
	}
	// Refusing the card is not the alternative here: the marketplace still
	// published this listing, and it says so whether or not the agent signed.
	if !verdict.Marketplace.Attested || !verdict.Marketplace.Valid {
		t.Errorf("marketplace side reports attested=%v valid=%v, want both true even for an unsigned card",
			verdict.Marketplace.Attested, verdict.Marketplace.Valid)
	}
}

// A signature that does not verify is refused rather than stored. Storing it
// would mean publishing content the service has called authentic when it is not.
func TestACardWhoseSignatureDoesNotVerifyIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	// Signed honestly, over a different name than the one being published.
	sig := agent.signCard(func() attest.Card {
		card := agent.signingCard()
		card.Name = "honest agent"
		return card
	}())
	card := domain.AgentCard{
		Name: "something else entirely", URL: "https://liar.example.com",
		PublicKey: agent.id, Currencies: []string{usdc},
	}

	status, body := agent.putCard(card, &sig)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT card whose signature does not verify = %d %s, want 400", status, body)
	}
	var failure struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &failure)
	if failure.Code != "ATTESTATION_REFUSED" {
		t.Errorf("code = %s, want ATTESTATION_REFUSED", failure.Code)
	}
}

// Another agent's faithful signature over this card. This is the case that would
// let one agent's attestation become another's.
func TestACardSignedByAnotherAgentIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	victim := newAgent(t, server)
	attacker := newAgent(t, server)

	attackerKey, err := attest.NewSigningKey(attacker.private)
	if err != nil {
		t.Fatal(err)
	}
	// The attacker signs the victim's card bytes with their own key, naming the
	// victim inside. The package's own SignCard refuses that, so the attacker
	// reaches past it to the raw signer, which is exactly what an attacker would
	// do and what the service therefore has to catch.
	sig, err := attackerKey.Sign(attest.CardBytes(victim.signingCard(), time.Now().UTC()), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	status, body := victim.putCard(victim.signingCardBody(), &sig)
	if status != http.StatusBadRequest {
		t.Fatalf("another agent's signature was accepted: %d %s", status, body)
	}
}

// Replacing a signed card with an unsigned one must clear the old signature, or
// the stored pair would disagree and a verifier would be checking content that
// no longer exists.
func TestReplacingASignedCardWithAnUnsignedOneClearsTheSignature(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	card := agent.signingCardBody()
	sig := agent.signCard(agent.signingCard())
	if status, body := agent.putCard(card, &sig); status != http.StatusOK {
		t.Fatalf("PUT signed card = %d %s", status, body)
	}
	if status, body := agent.putCard(card, nil); status != http.StatusOK {
		t.Fatalf("PUT unsigned replacement = %d %s", status, body)
	}
	verdict := agent.cardVerdict(agent.id)
	if verdict.Agent.Attested {
		t.Error("the previous agent signature survived an unsigned replacement, so it now describes a card that is not stored")
	}
	// The marketplace re-signed the replacement, so its signature is verifiable
	// against what is actually stored. A stale one here is the failure this
	// transaction exists to prevent.
	if !verdict.Marketplace.Valid {
		t.Error("the marketplace signature does not verify against the card that replaced the one it signed")
	}
}

// An offer's terms are what a buyer agrees to, so the seller signs them too.
func TestAOfferAnAgentSignsIsStoredAndReadsBackAsValid(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)

	offerID := "11111111-2222-3333-4444-555555555555"
	sig := seller.signOffer(seller.signingOffer(offerID, "10.00"))

	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"offerId":         offerID,
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "10.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
		"attestation":     sig,
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("publish signed offer = %d %s, want 201", status, body)
	}

	verdict := seller.offerVerdict(offerID)
	if !verdict.Signed || !verdict.Valid {
		t.Errorf("stored offer reports signed=%v valid=%v, want both true", verdict.Signed, verdict.Valid)
	}
	if verdict.Signature.KeyID != seller.id {
		t.Errorf("signature keyId = %s, want the seller's own key", verdict.Signature.KeyID)
	}
}

// A signature covers the offer id, so an agent that cannot choose the id cannot
// sign the offer before it exists. Publishing a signature with no id would mean
// storing an attestation over something that does not exist.
func TestAnOfferSignatureWithNoSellerChosenIDIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)

	sig := seller.signOffer(seller.signingOffer("11111111-2222-3333-4444-555555555555", "10.00"))
	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "10.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
		"attestation":     sig,
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("signed publish with no offerId = %d %s, want 400", status, body)
	}
}

// The price is the point of the signature. A seller must not be able to publish
// at one price and have the signature stand for different terms.
func TestAnOfferWhoseSignatureCoversDifferentTermsIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)

	offerID := "11111111-2222-3333-4444-555555555555"
	// Signed at 10.00, published at 1.00.
	sig := seller.signOffer(seller.signingOffer(offerID, "10.00"))
	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"offerId":         offerID,
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "1.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
		"attestation":     sig,
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("published price differing from the signed price = %d %s, want 400", status, body)
	}
	var failure struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &failure)
	if failure.Code != "ATTESTATION_REFUSED" {
		t.Errorf("code = %s, want ATTESTATION_REFUSED", failure.Code)
	}
}

func TestAnUnsignedOfferReportsItselfUnsigned(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	verdict := seller.offerVerdict(offer.ID)
	if verdict.Signed || verdict.Valid {
		t.Errorf("an unsigned offer reports signed=%v valid=%v, want both false", verdict.Signed, verdict.Valid)
	}
}

func TestAttestationForSomethingThatDoesNotExistIsNotFound(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	for _, path := range []string{
		"/v1/agents/3n6FHPdCyPzGxUQtUXHLY5ZGhJtsPkfvFTUJpmAsDkFRDU/attestation",
		"/v1/offers/11111111-2222-3333-4444-555555555555/attestation",
	} {
		if status, body := agent.raw(http.MethodGet, path, nil, false); status != http.StatusNotFound {
			t.Errorf("GET %s = %d %s, want 404", path, status, body)
		}
	}
}

// A directory has to be able to tell a marketplace's attestation from an agent's
// own, because they answer different questions and only one of them means the
// agent intends to honour its capability list.
func TestTheTwoSignaturesOnACardAreReportedSeparately(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	card := agent.signingCardBody()
	sig := agent.signCard(agent.signingCard())
	if status, body := agent.putCard(card, &sig); status != http.StatusOK {
		t.Fatalf("PUT signed card = %d %s", status, body)
	}
	verdict := agent.cardVerdict(agent.id)

	if verdict.Agent.KeyID != agent.id {
		t.Errorf("agent side keyId = %s, want %s", verdict.Agent.KeyID, agent.id)
	}
	if verdict.Marketplace.KeyID == agent.id {
		t.Error("the marketplace side reports the agent's key, so a reader could not tell the two apart")
	}
	// The two signatures are distinct values over the same content, not one value
	// reported twice under two names.
	if verdict.Agent.Signature.Value == verdict.Marketplace.Signature.Value {
		t.Error("the agent and marketplace signatures are the same value")
	}
	// The agent's key is its own ID, so a reader can verify it with nothing but
	// the card. The marketplace's is not, which is why the route reports it rather
	// than leaving a reader to guess which key to fetch.
	if verdict.Agent.Signature.KeyID != verdict.Agent.KeyID {
		t.Error("the agent side reports a key ID that differs from the signature's own")
	}
}

// The healthz verification key is the key a reader needs for a marketplace
// attestation, so it has to be the same one the route reports. If they drift, a
// directory following the documented key checks nothing.
func TestTheMarketplaceAttestationKeyIsThePublishedVerificationKey(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	if status, body := agent.putCard(agent.signingCardBody(), nil); status != http.StatusOK {
		t.Fatalf("PUT card = %d %s", status, body)
	}

	var health struct {
		VerificationKey string `json:"verificationKey"`
	}
	resp, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decodeInto(t, mustRead(t, resp.Body), &health)

	if got := agent.cardVerdict(agent.id).Marketplace.KeyID; got != health.VerificationKey {
		t.Errorf("card attestation key = %s, healthz verificationKey = %s; a reader following one would not verify the other",
			got, health.VerificationKey)
	}
}

// Every agent written before attestations existed sends a bare card. Requiring
// the wrapper would answer all of them with a 400 about a missing field they were
// never told about, so the bare body has to keep working.
func TestABareCardIsStillAccepted(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	status, body := agent.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agent.id)+"/card", domain.AgentCard{
		Name: "legacy agent", URL: "https://legacy.example.com", PublicKey: agent.id,
		Currencies: []string{usdc},
	}, true)
	if status != http.StatusOK {
		t.Fatalf("PUT bare card = %d %s, want 200: this is the shape every agent already sends", status, body)
	}
	var stored domain.Agent
	decodeInto(t, body, &stored)
	if stored.Card.Name != "legacy agent" {
		t.Errorf("stored name = %q, want legacy agent", stored.Card.Name)
	}
}

// The legacy body carries no signature, and the marketplace still attests what it
// published. Reporting the agent side as unsigned is what tells a reader the
// capability list is unvouched-for.
func TestABareCardIsUnsignedButMarketplaceAttested(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	if status, body := agent.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agent.id)+"/card", domain.AgentCard{
		Name: "legacy agent", URL: "https://legacy.example.com", PublicKey: agent.id,
		Currencies: []string{usdc},
	}, true); status != http.StatusOK {
		t.Fatalf("PUT bare card = %d %s", status, body)
	}
	verdict := agent.cardVerdict(agent.id)
	if verdict.Agent.Attested {
		t.Error("a bare card reports an agent signature it never sent")
	}
	if !verdict.Marketplace.Attested || !verdict.Marketplace.Valid {
		t.Error("the marketplace did not attest a card it published")
	}
}

// An empty "card" key is a malformed request, not a bare card. Falling back to
// reading the wrapper as a bare card would report a confusing missing-name error
// for something that is really a broken envelope.
func TestAWrappedRequestWithNoCardIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)

	status, body := agent.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agent.id)+"/card",
		map[string]any{"attestation": map[string]any{"alg": "Ed25519"}}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("wrapped body with no card = %d %s, want 400", status, body)
	}
	if !strings.Contains(string(body), "name is required") {
		t.Errorf("body = %s, want it to name the missing card", body)
	}
}

// fetchOffer reads a stored offer back through the API.
func fetchOffer(t *testing.T, c *agentClient, id string) domain.Offer {
	t.Helper()
	var offer domain.Offer
	status, body := c.raw(http.MethodGet, "/v1/offers/"+id, nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/offers/%s = %d %s", id, status, body)
	}
	decodeInto(t, body, &offer)
	return offer
}

// A retry that re-sends the same terms under the same ID is the same listing, and
// returns the offer already there rather than failing. Capabilities in a different
// order are the same terms: an offer is a set of what it can do, not a sequence.
func TestARetryWithTheSameTermsReturnsTheExistingOffer(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document", "translate:document")

	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"offerId":         offer.ID,
		"direction":       "ask",
		"description":     offer.Description,
		"capabilities":    []string{"translate:document", "summarize:document"},
		"priceAmount":     "10.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
	}, true)
	if status != http.StatusOK {
		t.Fatalf("re-sending the same terms = %d %s, want 200 returning the existing offer", status, body)
	}
	var got domain.Offer
	decodeInto(t, body, &got)
	if got.ID != offer.ID {
		t.Errorf("returned offer id = %s, want the one that was published", got.ID)
	}
}

// A caller-chosen ID that is taken with different terms is a collision. Silently
// keeping the incumbent would leave the seller believing it republished a listing
// it never changed, which on a price is the difference between two offers and one.
func TestACallerSuppliedOfferIDWithDifferentTermsIsRefused(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"offerId":         offer.ID,
		"direction":       "ask",
		"description":     "a completely different offer",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "1.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("republishing a taken offerId with new terms = %d %s, want 409", status, body)
	}
	// The incumbent has to survive the attempt untouched.
	stored := fetchOffer(t, seller, offer.ID)
	if stored.PriceAmount.String() != "10.00" || stored.Description != offer.Description {
		t.Errorf("the live offer was modified by a refused publish: %s at %s", stored.Description, stored.PriceAmount)
	}
}

// Another seller's ID is not a retry, and answering with their offer would hand a
// caller somebody else's terms and tell it it published them.
func TestAnotherAgentsOfferIDIsRefusedWithoutDisclosingTheirOffer(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	other := newAgent(t, server)
	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	status, body := other.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(other.id)+"/offers", map[string]any{
		"offerId":         offer.ID,
		"direction":       "ask",
		"description":     offer.Description,
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "10.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain", "onchain"},
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("publishing under another agent's offerId = %d %s, want 409", status, body)
	}
	if strings.Contains(string(body), offer.Description) {
		t.Errorf("the refusal echoed the incumbent offer's description: %s", body)
	}
}

// publishSignedOffer publishes a signed offer with a seller-chosen ID, which is
// the only shape a signature can cover.
func (c *agentClient) publishSignedOffer(amount, mint string, capabilities ...string) domain.Offer {
	c.t.Helper()
	offerID := uuid.NewString()
	sig := c.signOffer(c.signingOffer(offerID, amount))
	status, body := c.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(c.id)+"/offers", map[string]any{
		"offerId": offerID, "direction": "ask", "description": "summarize a document",
		"capabilities": capabilities, "priceAmount": amount, "priceMint": mint,
		"settlementModes": []string{"offchain", "onchain"}, "attestation": sig,
	}, true)
	if status != http.StatusCreated {
		c.t.Fatalf("publish signed offer = %d %s", status, body)
	}
	var offer domain.Offer
	decodeInto(c.t, body, &offer)
	return offer
}

// searchOffers lists what the marketplace will show, so a test can assert that a
// refused publish left nothing behind rather than only that it returned an error.
func searchOffers(t *testing.T, c *agentClient, query string) []domain.Offer {
	t.Helper()
	var payload struct {
		Offers []domain.Offer `json:"offers"`
	}
	decodeInto(t, c.do(http.MethodGet, "/v1/offers"+query, nil, false), &payload)
	return payload.Offers
}

// An operator who declares the requirement gets a refusal, not an unsigned
// listing. The offer is well-formed, so this is a 409: the seller can fix it by
// signing.
func TestAnUnsignedOfferIsRefusedWhenTheDeploymentRequiresSignatures(t *testing.T) {
	server, _ := buildServer(t, serverBuild{requireOfferSig: true})
	seller := newAgent(t, server)

	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     "10.00",
		"priceMint":       usdc,
		"settlementModes": []string{"offchain"},
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("unsigned publish where signatures are required = %d %s, want 409", status, body)
	}
	if !strings.Contains(string(body), "OFFER_ATTESTATION_REQUIRED") {
		t.Errorf("body = %s, want it to name the requirement", body)
	}
	if !strings.Contains(string(body), "--require-offer-attestation") {
		t.Errorf("body = %s, want it to say how the operator turned this off", body)
	}
}

// Nothing is published by the refused attempt. A 409 that still left a listing
// behind would be the worst outcome: the offer is invisible as unsigned and
// listable as signed.
func TestARefusedUnsignedOfferPublishesNothing(t *testing.T) {
	server, _ := buildServer(t, serverBuild{requireOfferSig: true})
	seller := newAgent(t, server)

	status, _ := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"direction": "ask", "description": "summarize a document",
		"capabilities": []string{"summarize:document"},
		"priceAmount":  "10.00", "priceMint": usdc,
		"settlementModes": []string{"offchain"},
	}, true)
	if status != http.StatusConflict {
		t.Fatalf("unsigned publish = %d, want 409", status)
	}
	offers := searchOffers(t, seller, "")
	for _, o := range offers {
		if o.Description == "summarize a document" {
			t.Errorf("a refused offer was listed anyway: %+v", o)
		}
	}
}

// A signed offer is published normally when the requirement is on, so the switch
// is a filter on unsigned offers and not a refusal of attestation itself.
func TestASignedOfferIsPublishedWhenTheDeploymentRequiresSignatures(t *testing.T) {
	server, _ := buildServer(t, serverBuild{requireOfferSig: true})
	seller := newAgent(t, server)

	offer := seller.publishSignedOffer("10.00", usdc, "summarize:document")
	verdict := seller.offerVerdict(offer.ID)
	if !verdict.Signed || !verdict.Valid {
		t.Errorf("a signed offer published under the requirement reports signed=%v valid=%v", verdict.Signed, verdict.Valid)
	}
}

// A missing credential and a wrong one are different problems for whoever has to
// fix them, and an operator reading a log needs to tell them apart.
func TestAMissingSignatureIsDistinguishedFromAWrongOne(t *testing.T) {
	server, _ := buildServer(t, serverBuild{requireOfferSig: true})
	seller := newAgent(t, server)

	status, body := seller.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"offerId": "226c069c-c769-4657-b2a2-ffb5bbbcc6f7", "direction": "ask",
		"description": "summarize a document", "capabilities": []string{"summarize:document"},
		"priceAmount": "10.00", "priceMint": usdc, "settlementModes": []string{"offchain"},
		"attestation": map[string]any{"alg": "Ed25519", "keyId": "not-a-real-key", "value": "AAAA"},
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("a wrong signature = %d %s, want 400 ATTESTATION_REFUSED rather than the missing-credential code", status, body)
	}
	if !strings.Contains(string(body), "ATTESTATION_REFUSED") {
		t.Errorf("body = %s, want ATTESTATION_REFUSED", body)
	}
}

// Turning the requirement on must not strand an offer that already exists. A
// client that lost a response still gets its offer back, because recovering a
// created offer is not the same as allowing a new unsigned listing.
func TestAnOfferCreatedBeforeTheRequirementCanStillBeRecovered(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)

	offer := seller.publishOffer("10.00", usdc, "summarize:document")

	if got := fetchOffer(t, seller, offer.ID); got.ID != offer.ID {
		t.Errorf("the offer that already existed cannot be read back: %+v", got)
	}
}
