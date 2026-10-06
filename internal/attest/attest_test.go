package attest_test

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
)

func newKey(t *testing.T) *attest.SigningKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	key, err := attest.NewSigningKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func cardFor(key *attest.SigningKey) attest.Card {
	return attest.Card{
		AgentID:      key.PublicKeyBase58(),
		Name:         "summarizer",
		Description:  "summarizes documents",
		Version:      "1.2.0",
		URL:          "https://agent.example/",
		Capabilities: []string{"summarize:document", "summarize:webpage"},
		Skills: []attest.Skill{
			{ID: "summarize", Name: "Summarize", Tags: []string{"text", "document"},
				Input: []string{"text/plain"}, Output: []string{"text/plain"}},
			{ID: "translate", Name: "Translate", Tags: []string{"language"},
				Input: []string{"text/plain"}, Output: []string{"text/plain"}},
		},
		Currencies:      []string{"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		SettlementModes: []string{"offchain", "onchain"},
	}
}

// signedAt is a fixed instant so a test that rewrites a signature's timestamp is
// unambiguous about what it changed.
var signedAt = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func TestACardSignatureVerifiesUnderTheAgentThatMadeIt(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)

	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCard(card, sig); err != nil {
		t.Fatalf("a freshly signed card does not verify: %v", err)
	}
	if sig.KeyID != card.AgentID {
		t.Errorf("keyId = %s, want the agent ID %s", sig.KeyID, card.AgentID)
	}
	if sig.Alg != attest.Alg {
		t.Errorf("alg = %s, want %s", sig.Alg, attest.Alg)
	}
	if sig.Digest != attest.Digest(attest.CardBytes(card, sig.SignedAt)) {
		t.Error("the digest does not match the card it was computed from")
	}
}

// The point of the whole exercise. An unauthenticated card is just JSON, and
// anybody can write one claiming to be anybody.
func TestACardAlteredAfterSigningDoesNotVerify(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, alter := range []struct {
		name   string
		mutate func(*attest.Card)
	}{
		{"the name", func(c *attest.Card) { c.Name = "somebody-else" }},
		{"the url", func(c *attest.Card) { c.URL = "https://attacker.example/" }},
		{"a capability", func(c *attest.Card) { c.Capabilities = append(c.Capabilities, "withdraw:funds") }},
		{"a currency", func(c *attest.Card) { c.Currencies = []string{"So11111111111111111111111111111111111111112"} }},
		{"the description", func(c *attest.Card) { c.Description = "trust me" }},
		{"the version", func(c *attest.Card) { c.Version = "9.9.9" }},
		{"a skill name", func(c *attest.Card) { c.Skills[0].Name = "Withdraw Funds" }},
		{"a skill tag", func(c *attest.Card) { c.Skills[0].Tags = []string{"withdraw:funds"} }},
		{"a skill's input modes", func(c *attest.Card) { c.Skills[0].Input = nil }},
		{"a skill removed", func(c *attest.Card) { c.Skills = c.Skills[:1] }},
		{"the settlement modes", func(c *attest.Card) { c.SettlementModes = []string{"offchain"} }},
	} {
		tampered := card
		tampered.Capabilities = append([]string(nil), card.Capabilities...)
		tampered.Currencies = append([]string(nil), card.Currencies...)
		tampered.SettlementModes = append([]string(nil), card.SettlementModes...)
		tampered.Skills = append([]attest.Skill(nil), card.Skills...)
		for i := range tampered.Skills {
			tampered.Skills[i].Tags = append([]string(nil), card.Skills[i].Tags...)
			tampered.Skills[i].Input = append([]string(nil), card.Skills[i].Input...)
			tampered.Skills[i].Output = append([]string(nil), card.Skills[i].Output...)
		}
		alter.mutate(&tampered)
		if err := attest.VerifyCard(tampered, sig); err == nil {
			t.Errorf("a signature survived changing %s", alter.name)
		}
	}
}

// An agent's card is its public identity. A valid signature from a different key
// must never be accepted for it, because that is how one agent's attestation
// becomes another's.
func TestACardSignedByAnotherAgentIsRefused(t *testing.T) {
	victim := newKey(t)
	attacker := newKey(t)

	card := cardFor(victim)
	// The attacker signs the victim's card faithfully, with their own key.
	sig, err := attacker.Sign(attest.CardBytes(card, signedAt), signedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCard(card, sig); err == nil {
		t.Fatal("another agent's signature was accepted for this agent's card")
	}
}

// SignCard refuses before signing, so an agent cannot produce a well-formed
// signature over a card that names somebody else.
func TestAnAgentCannotSignACardClaimingToBeSomebodyElse(t *testing.T) {
	key := newKey(t)
	other := newKey(t)

	// The card names one agent and the key belongs to another.
	card := cardFor(other)
	if _, err := key.SignCard(card, time.Now()); err == nil {
		t.Fatal("an agent signed a card naming another agent")
	}
}

// The whole point of the length prefix. Without it, a field value can contain the
// delimiter and a verifier can be made to read a different set of fields than
// the signer meant.
func TestAFieldValueCannotForgeAFieldBoundary(t *testing.T) {
	key := newKey(t)
	base := cardFor(key)
	base.Description = "innocent"
	baseSig, err := key.SignCard(base, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// Same signature, but the description now contains what looks like a
	// different signed statement.
	forged := base
	forged.Description = "innocent\nkind:offer\nseller:someone"
	if err := attest.VerifyCard(forged, baseSig); err == nil {
		t.Fatal("a description containing field delimiters was not detected as a change")
	}

	// And a URL carrying the same trick.
	smuggled := base
	smuggled.URL = "https://agent.example/\ncapabilities.0:withdraw:funds\ncapabilities.count:1"
	if err := attest.VerifyCard(smuggled, baseSig); err == nil {
		t.Fatal("a URL containing field delimiters was not detected as a change")
	}
}

// A signature is over a kind of statement. Without domain separation, a valid
// offer signature would be a valid card signature and vice versa.
func TestAnOfferSignatureCannotBeReplayedAsACard(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	offer := attest.Offer{
		ID: "offer-1", Seller: key.PublicKeyBase58(), Description: "summarize",
		Direction:    "ask",
		Capabilities: []string{"summarize:document"}, Scale: "6",
		Mint:            "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		AmountBaseUnits: "1000000", UnitAmount: "1.00",
		SettlementModes: []string{"offchain"},
	}
	sig, err := key.SignOffer(offer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyOffer(offer, sig); err != nil {
		t.Fatalf("a freshly signed offer does not verify: %v", err)
	}

	if err := attest.Verify(attest.CardBytes(card, signedAt), sig, key.PublicKeyBase58()); err == nil {
		t.Error("an offer signature verified as a card signature")
	}
	// A card renamed to look like an offer is the other direction of the same
	// confusion, and is the one a rewrite of the stored bytes would take.
	disguised := attest.Offer{
		ID: card.Name, Seller: card.AgentID, Description: card.Description,
		Direction:    "ask",
		Capabilities: []string{"summarize:document"}, Scale: "6",
		Mint:       "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		UnitAmount: "1.00", SettlementModes: []string{"offchain"},
	}
	if err := attest.Verify(attest.OfferBytes(disguised, signedAt), sig, key.PublicKeyBase58()); err == nil {
		t.Error("an offer signature verified over content it was not signed for")
	}
}

// The buyer's agreement is to specific terms. A changed price is not the same
// offer.
func TestAnOfferWithChangedTermsDoesNotVerify(t *testing.T) {
	key := newKey(t)
	offer := attest.Offer{
		ID: "offer-1", Seller: key.PublicKeyBase58(), Description: "summarize",
		Direction:    "ask",
		Capabilities: []string{"summarize:document"}, Scale: "6",
		Mint:            "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		AmountBaseUnits: "1000000", UnitAmount: "1.00",
		SettlementModes: []string{"offchain"},
	}
	sig, err := key.SignOffer(offer, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, alter := range []struct {
		name   string
		mutate func(*attest.Offer)
	}{
		{"the price", func(o *attest.Offer) { o.AmountBaseUnits = "1" }},
		{"the unit amount", func(o *attest.Offer) { o.UnitAmount = "0.01" }},
		{"a capability", func(o *attest.Offer) { o.Capabilities = []string{"withdraw:funds"} }},
		{"the scale", func(o *attest.Offer) { o.Scale = "0" }},
		{"the mint", func(o *attest.Offer) { o.Mint = "So11111111111111111111111111111111111111112" }},
		{"the direction", func(o *attest.Offer) { o.Direction = "bid" }},
		{"the description", func(o *attest.Offer) { o.Description = "something else entirely" }},
		{"the settlement modes", func(o *attest.Offer) { o.SettlementModes = []string{"onchain"} }},
		{"the offer id", func(o *attest.Offer) { o.ID = "offer-2" }},
	} {
		tampered := offer
		tampered.SettlementModes = append([]string(nil), offer.SettlementModes...)
		tampered.Capabilities = append([]string(nil), offer.Capabilities...)
		alter.mutate(&tampered)
		if err := attest.VerifyOffer(tampered, sig); err == nil {
			t.Errorf("a signature survived changing %s", alter.name)
		}
	}
}

// Ordering and duplication are not part of what a card claims. Two agents
// listing the same set in a different order make the same claim, and making them
// produce different signatures pushes a distinction onto every implementer for
// no gain.
func TestSetOrderAndDuplicationAreNotPartOfTheClaim(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	card.Capabilities = []string{"b:two", "a:one"}
	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	reordered := card
	reordered.Capabilities = []string{"a:one", "b:two"}
	if err := attest.VerifyCard(reordered, sig); err != nil {
		t.Errorf("reordering a set broke the signature: %v", err)
	}

	duplicated := card
	duplicated.Capabilities = []string{"b:two", "a:one", "a:one"}
	if err := attest.VerifyCard(duplicated, sig); err != nil {
		t.Errorf("a repeated entry broke the signature: %v", err)
	}
}

// A verifier must not be talked into a weaker algorithm, so the algorithm is
// named in the signature and anything but Ed25519 is refused rather than
// defaulted.
func TestAnUnknownAlgorithmIsRefusedRatherThanAssumed(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, alg := range []string{"", "none", "HS256", "Ed25519ph", "ed25519"} {
		weakened := sig
		weakened.Alg = alg
		if err := attest.VerifyCard(card, weakened); err == nil {
			t.Errorf("algorithm %q was accepted", alg)
		}
	}
}

// A signature detached from its content is only meaningful if it still names the
// content it was made over. The digest check catches a mismatch cheaply and says
// so, rather than failing as an opaque bad signature.
func TestASignatureCarryingTheWrongDigestIsRefused(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mismatched := sig
	mismatched.Digest = attest.Digest([]byte("something else"))
	err = attest.VerifyCard(card, mismatched)
	if err == nil {
		t.Fatal("a signature with a mismatched digest was accepted")
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("error = %v, want it to report the digest mismatch", err)
	}
}

func TestMalformedKeysAndSignaturesAreRefusedNotPanicked(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "not base58 !!", "abc"} {
		broken := sig
		broken.KeyID = bad
		if err := attest.VerifyCard(card, broken); err == nil {
			t.Errorf("keyId %q was accepted", bad)
		}
	}
	for _, bad := range []string{"", "not base64 !!", "AAAA"} {
		broken := sig
		broken.Value = bad
		if err := attest.VerifyCard(card, broken); err == nil {
			t.Errorf("signature value %q was accepted", bad)
		}
	}

	// A well-formed signature from a different key, presented over this card.
	other := newKey(t)
	foreign, err := other.SignCard(cardFor(other), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCard(card, foreign); err == nil {
		t.Error("a signature from another agent was accepted for this card")
	}
}

// The encoding is the contract with anyone verifying in another language, so its
// shape is pinned rather than left to drift.
func TestTheCanonicalEncodingIsStable(t *testing.T) {
	card := attest.Card{
		AgentID: "KEY", Name: "n", Description: "", Version: "",
		URL: "https://x/", Capabilities: []string{"a", "b"},
		Currencies: nil, SettlementModes: []string{"offchain"},
	}
	got := string(attest.CardBytes(card, signedAt))
	want := "vtessera/attest/v1\n" +
		"kind:agent-card\n" +
		"signedAt:20:2026-10-04T12:00:00Z\n" +
		"agent:3:KEY\n" +
		"name:1:n\n" +
		"description:0:\n" +
		"version:0:\n" +
		"url:10:https://x/\n" +
		"capabilities.count:2\n" +
		"capabilities.0:1:a\n" +
		"capabilities.1:1:b\n" +
		"skills.count:0\n" +
		"probeTarget:0:\n" +
		"currencies.count:0\n" +
		"settlementModes.count:1\n" +
		"settlementModes.0:8:offchain\n"
	if got != want {
		t.Errorf("canonical encoding drifted:\n got %q\nwant %q", got, want)
	}
}

// Signing is deterministic in Ed25519, so the same card signed twice produces the
// same signature bytes. That is what lets an agent re-sign on every publication
// without the signature looking different each time.
func TestSigningTheSameCardTwiceProducesTheSameSignature(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	first, err := key.SignCard(card, at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := key.SignCard(card, at)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value != second.Value {
		t.Error("signing the same card twice produced different signatures")
	}
	if !first.SignedAt.Equal(at) {
		t.Errorf("signedAt = %s, want %s", first.SignedAt, at)
	}
}

// A timestamp beside a signature is a timestamp nobody vouched for. A verifier
// that reads "signedAt" as evidence of when the claim was made has to be able to
// depend on it, so it is in the signed bytes and changing it fails.
func TestRewritingTheSignatureTimestampDoesNotVerify(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, signedAt)
	if err != nil {
		t.Fatal(err)
	}

	for _, moved := range []time.Time{
		signedAt.Add(-time.Hour),
		signedAt.Add(time.Hour),
		signedAt.Add(time.Nanosecond),
	} {
		rewritten := sig
		rewritten.SignedAt = moved
		if err := attest.VerifyCard(card, rewritten); err == nil {
			t.Errorf("a signature whose timestamp was rewritten to %s still verified", moved)
		}
	}

	// And the same for an offer, where a rewritten timestamp would say the terms
	// were promised before they were.
	offer := attest.Offer{
		ID: "offer-1", Seller: key.PublicKeyBase58(), Direction: "ask",
		Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Scale: "6",
		AmountBaseUnits: "1000000", UnitAmount: "1.00",
		SettlementModes: []string{"offchain"},
	}
	offerSig, err := key.SignOffer(offer, signedAt)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := offerSig
	rewritten.SignedAt = offerSig.SignedAt.Add(-72 * time.Hour)
	if err := attest.VerifyOffer(offer, rewritten); err == nil {
		t.Error("an offer signature with a rewritten timestamp still verified")
	}
}

// Skill order is not a claim, the same way capability order is not. An agent that
// reorders its own JSON between runs must still be able to verify what it signed.
func TestSkillAndTagOrderAreNotPartOfTheClaim(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	sig, err := key.SignCard(card, signedAt)
	if err != nil {
		t.Fatal(err)
	}

	reordered := card
	reordered.Skills = []attest.Skill{card.Skills[1], card.Skills[0]}
	reordered.Skills[0].Tags = []string{"language"}
	reordered.Skills[1].Tags = []string{"document", "text"}
	if err := attest.VerifyCard(reordered, sig); err != nil {
		t.Errorf("reordering skills and tags broke the signature: %v", err)
	}
}

// Two implementations have to produce the same bytes for the same card, so the
// skill encoding is pinned rather than left to drift.
func TestTheSkillEncodingIsStable(t *testing.T) {
	card := attest.Card{
		AgentID: "KEY", Name: "n", URL: "https://x/",
		Skills: []attest.Skill{{ID: "summarize", Name: "Summarize",
			Tags: []string{"text"}, Input: []string{"text/plain"}, Output: []string{"text/plain"}}},
	}
	want := "vtessera/attest/v1\n" +
		"kind:agent-card\n" +
		"signedAt:20:2026-10-04T12:00:00Z\n" +
		"agent:3:KEY\n" +
		"name:1:n\n" +
		"description:0:\n" +
		"version:0:\n" +
		"url:10:https://x/\n" +
		"capabilities.count:0\n" +
		"skills.count:1\n" +
		"skills.0.id:9:summarize\n" +
		"skills.0.name:9:Summarize\n" +
		"skills.0.tags.count:1\n" +
		"skills.0.tags.0:4:text\n" +
		"skills.0.inputModes.count:1\n" +
		"skills.0.inputModes.0:10:text/plain\n" +
		"skills.0.outputModes.count:1\n" +
		"skills.0.outputModes.0:10:text/plain\n" +
		"probeTarget:0:\n" +
		"currencies.count:0\n" +
		"settlementModes.count:0\n"
	if got := string(attest.CardBytes(card, signedAt)); got != want {
		t.Errorf("canonical skill encoding drifted:\n got %q\nwant %q", got, want)
	}
}

// The timestamp is written in one form. A signer in another timezone and a
// verifier in UTC must agree, or a perfectly good signature would fail on a
// machine set to Auckland.
func TestTheSignedTimestampIsWrittenInUTCRegardlessOfTheSignersZone(t *testing.T) {
	key := newKey(t)
	card := cardFor(key)
	zone := time.FixedZone("test", 12*60*60)
	if sig, err := key.SignCard(card, signedAt.In(zone)); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(attest.CardBytes(card, sig.SignedAt)), "signedAt:20:2026-10-04T12:00:00Z") {
		t.Errorf("encoding = %q, want the timestamp in UTC", attest.CardBytes(card, sig.SignedAt))
	}
}

// A third party's attestation and an agent's own signature are different claims,
// and the API says so rather than offering one function that means both. A
// marketplace that publishes a seller's capability list is attesting that it
// stored the list; it is not promising to honour it.
func TestAMarketplaceAttestationIsNotAnAgentsOwnSignature(t *testing.T) {
	agent := newKey(t)
	market := newKey(t)
	card := cardFor(agent)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	// The marketplace may attest the card.
	marketSig, err := market.AttestCard(card, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCardAttestedBy(card, marketSig, market.PublicKeyBase58()); err != nil {
		t.Fatalf("the marketplace's own attestation does not verify under its key: %v", err)
	}
	// It is still not the agent's signature, and must not read as one.
	if err := attest.VerifyCard(card, marketSig); err == nil {
		t.Error("a marketplace attestation was accepted as the agent's own signature")
	}

	// And the marketplace cannot sign an agent's card the way the agent does,
	// which is what keeps a reader from mistaking one for the other.
	if _, err := market.SignCard(card, at); err == nil {
		t.Error("a third party produced a self-signature over an agent's card")
	}
}

// The key a verifier is told to check against has to be the key that actually
// signed, or the attestation says nothing about who made it.
func TestAnAttestationFromAnUnexpectedKeyIsRefused(t *testing.T) {
	agent := newKey(t)
	market := newKey(t)
	impostor := newKey(t)
	card := cardFor(agent)

	sig, err := market.AttestCard(card, signedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCardAttestedBy(card, sig, impostor.PublicKeyBase58()); err == nil {
		t.Fatal("an attestation was accepted against a key that did not sign it")
	}
	// The card's own agent is not the attestor either.
	if err := attest.VerifyCardAttestedBy(card, sig, card.AgentID); err == nil {
		t.Fatal("a marketplace attestation was accepted as the agent's")
	}
}

// Attestation is not a comment on content: it changes nothing about what the
// card says, only about who vouches for it.
func TestAMarketplaceAttestationDoesNotAlterTheStatement(t *testing.T) {
	agent := newKey(t)
	market := newKey(t)
	card := cardFor(agent)

	signed, err := market.AttestCard(card, signedAt)
	if err != nil {
		t.Fatal(err)
	}
	before := string(attest.CardBytes(card, signed.SignedAt))
	altered := card
	altered.Capabilities = append(altered.Capabilities, "withdraw:funds")
	if err := attest.VerifyCardAttestedBy(altered, signed, market.PublicKeyBase58()); err == nil {
		t.Error("attested content changed without invalidating the attestation")
	}
	if got := string(attest.CardBytes(card, signed.SignedAt)); got != before {
		t.Error("verifying an attestation changed the bytes it signs")
	}
}

// The probe target is part of what an agent signs. A declaration the signature
// did not cover could be changed by anyone holding the row, which would make the
// opt-in meaningless.
func TestTheProbeTargetIsCoveredByTheCardSignature(t *testing.T) {
	key := newKey(t)
	card := attest.Card{
		AgentID: key.PublicKeyBase58(), Name: "probeable",
		URL:         "https://agent.example.com",
		ProbeTarget: "https://agent.example.com:8443/probe",
	}
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sig, err := key.SignCard(card, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyCard(card, sig); err != nil {
		t.Fatalf("a card declaring a probe target does not verify: %v", err)
	}
	redirected := card
	redirected.ProbeTarget = "https://elsewhere.example.com:8443/probe"
	if err := attest.VerifyCard(redirected, sig); err == nil {
		t.Error("a signature carried over to a card declaring a different probe target")
	}
}

// Results are sorted into the statement, so two runs that found the same thing in
// a different order produce the same bytes and the same digest. Otherwise an
// agent that answers in a variable order would produce a different attestation
// each time for no reason.
func TestProbeResultsAreSortedIntoTheStatement(t *testing.T) {
	key := newKey(t)
	first := attest.Probe{
		AgentID: key.PublicKeyBase58(), Target: "https://agent.example.com:8443/probe",
		CheckedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Results: []attest.ProbeResult{
			{Capability: "translate:text", Status: "pass"},
			{Capability: "summarize:document", Status: "fail"},
		},
	}
	second := first
	second.Results = []attest.ProbeResult{
		{Capability: "summarize:document", Status: "fail"},
		{Capability: "translate:text", Status: "pass"},
	}
	at := time.Date(2026, 10, 4, 12, 5, 0, 0, time.UTC)
	if string(attest.ProbeBytes(first, at)) != string(attest.ProbeBytes(second, at)) {
		t.Errorf("the same results in a different order produce different statements:\n%s\n%s",
			attest.ProbeBytes(first, at), attest.ProbeBytes(second, at))
	}
	// And the sort must not be a way to smuggle an extra capability past the
	// signature: a third result is still covered.
	third := first
	third.Results = append(first.Results, attest.ProbeResult{Capability: "acquire:company", Status: "pass"})
	sig, err := key.SignProbe(first, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyProbeAttestedBy(third, sig, key.PublicKeyBase58()); err == nil {
		t.Error("a result added after signing still verified")
	}
}

// A probe statement names the agent and the target it ran against, so a result
// cannot be presented as somebody else's or as coming from somewhere else.
func TestAProbeStatementIsBoundToItsAgentAndTarget(t *testing.T) {
	key, other := newKey(t), newKey(t)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	statement := attest.Probe{
		AgentID: key.PublicKeyBase58(), Target: "https://agent.example.com:8443/probe",
		CheckedAt: at,
		Results:   []attest.ProbeResult{{Capability: "summarize:document", Status: "pass"}},
	}
	sig, err := key.SignProbe(statement, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := attest.VerifyProbeAttestedBy(statement, sig, key.PublicKeyBase58()); err != nil {
		t.Fatalf("a signed probe does not verify: %v", err)
	}
	moved := statement
	moved.AgentID = other.PublicKeyBase58()
	if err := attest.VerifyProbeAttestedBy(moved, sig, key.PublicKeyBase58()); err == nil {
		t.Error("a result naming a different agent verified")
	}
	elsewhere := statement
	elsewhere.Target = "https://elsewhere.example.com:8443/probe"
	if err := attest.VerifyProbeAttestedBy(elsewhere, sig, key.PublicKeyBase58()); err == nil {
		t.Error("a result naming a different target verified")
	}
	// And a different marketplace's key does not vouch for it.
	if err := attest.VerifyProbeAttestedBy(statement, sig, other.PublicKeyBase58()); err == nil {
		t.Error("a result verified against a key that did not sign it")
	}
}
