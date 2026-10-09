// Package attest signs and verifies detached statements about an agent's own
// content: the card it publishes and the offers it lists.
//
// Everything here is Ed25519 over a canonical byte encoding of the claims. Two
// things it deliberately is not: it is not a KYC system, and it does not make a
// claim true. A signature proves who made a statement, so the remaining question
// is always "did they make it, and has anything changed since". It never answers
// "is this agent good at summarizing documents".
//
// # Why canonical bytes rather than signed JSON
//
// A signature covers bytes, so what gets signed has to be reconstructible by
// anyone verifying it. Verifying signed JSON directly would make every
// insignificant choice load-bearing: field order, whitespace, whether an absent
// optional field was omitted or sent as empty, whether a number arrived as 10 or
// 10.0. Each of those is a way for two honest implementations to disagree about
// whether a signature is valid.
//
// So each statement is encoded into a canonical form first. The encoding is
// domain-separated, so a signature over an offer can never be replayed as a
// signature over a card, and it is length-prefixed, so no field value can be
// crafted to contain the delimiters and shift the boundary between two fields.
package attest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mr-tron/base58"
)

// Alg is the only signature algorithm this package accepts.
//
// It is named in every signature rather than assumed, because a verifier that
// guesses is a verifier that can be talked into a weaker algorithm by whoever
// supplies the signature.
const Alg = "Ed25519"

// Version is the encoding version. It is part of the signed bytes, so a future
// change to the encoding cannot silently re-interpret an old signature.
const Version = 1

// SigningKey is an agent's private half. It never leaves the agent.
type SigningKey struct {
	private ed25519.PrivateKey
}

// NewSigningKey wraps an Ed25519 private key.
func NewSigningKey(private ed25519.PrivateKey) (*SigningKey, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be %d bytes, got %d", ed25519.PrivateKeySize, len(private))
	}
	return &SigningKey{private: private}, nil
}

// PublicKeyBase58 returns the agent ID this key signs under. It is the same
// string the agent authenticates with, so an attestation and a session are
// provably the same identity rather than two keys that ought to match.
func (k *SigningKey) PublicKeyBase58() string {
	return base58.Encode(k.private.Public().(ed25519.PublicKey))
}

// Signature is a detached signature over a statement, with enough metadata for a
// third party to verify it without a prior conversation with the signer.
//
// KeyID is the signer's public key. Digest lets a verifier that already has the
// content confirm cheaply that it is checking the bytes it thinks it is, rather
// than discovering a mismatch after parsing.
//
// SignedAt is part of the signed bytes rather than a note beside them. A
// timestamp a signer did not commit to is a timestamp anybody can rewrite, and a
// reader who takes "signed at" as evidence of when something was published would
// be reading a field nobody vouched for.
type Signature struct {
	Alg      string    `json:"alg"`
	KeyID    string    `json:"keyId"`
	Digest   string    `json:"digest"`
	Value    string    `json:"value"`
	SignedAt time.Time `json:"signedAt"`
}

// Verify reports whether sig was produced over payload by the key in sig.KeyID.
//
// It takes the key from the signature rather than as an argument, and requires
// it to be the expected one, because the interesting failure is not a bad
// signature — it is a good signature from somebody else's key.
func Verify(payload []byte, sig Signature, expectedKeyID string) error {
	if sig.Alg != Alg {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrInvalid, sig.Alg)
	}
	if sig.KeyID != expectedKeyID {
		return fmt.Errorf("%w: signed by %s, expected %s", ErrInvalid, sig.KeyID, expectedKeyID)
	}
	key, err := decodeKey(sig.KeyID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	value, err := base64.StdEncoding.DecodeString(sig.Value)
	if err != nil {
		return fmt.Errorf("%w: signature must be base64", ErrInvalid)
	}
	if len(value) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature is %d bytes, want %d", ErrInvalid, len(value), ed25519.SignatureSize)
	}
	digest := Digest(payload)
	if sig.Digest != digest {
		return fmt.Errorf("%w: signature is over %s, the content hashes to %s", ErrInvalid, sig.Digest, digest)
	}
	if !ed25519.Verify(key, payload, value) {
		return fmt.Errorf("%w: signature does not verify under %s", ErrInvalid, sig.KeyID)
	}
	return nil
}

// VerifyAny accepts a signature made by any key in a trusted set.
//
// It exists for a marketplace that has rotated its signing key: an attestation
// made by a retired key must keep verifying while that key is published as one of
// the marketplace's, and the signature's own KeyID says which one made it. The set
// is the caller's, so trusting a key is always a decision made where the keys are
// known.
func VerifyAny(payload []byte, sig Signature, keyIDs []string) error {
	for _, id := range keyIDs {
		if sig.KeyID == id {
			return Verify(payload, sig, id)
		}
	}
	return fmt.Errorf("%w: signed by %s, which is not among the %d trusted keys",
		ErrInvalid, sig.KeyID, len(keyIDs))
}

// VerifyCardAttestedByAny checks a third-party card attestation against a set of
// trusted attester keys.
func VerifyCardAttestedByAny(card Card, sig Signature, keyIDs []string) error {
	return VerifyAny(CardBytes(card, sig.SignedAt), sig, keyIDs)
}

// VerifyProbeAttestedByAny checks a probe attestation against a set of trusted
// attester keys.
func VerifyProbeAttestedByAny(probe Probe, sig Signature, keyIDs []string) error {
	return VerifyAny(ProbeBytes(probe, sig.SignedAt), sig, keyIDs)
}

// Sign produces a detached signature over payload.
//
// It is a method rather than a free function so a key cannot be passed where a
// signer is expected, and so the algorithm and key ID in the result cannot
// disagree with the key that actually signed.
func (k *SigningKey) Sign(payload []byte, at time.Time) (Signature, error) {
	return Signature{
		Alg:      Alg,
		KeyID:    k.PublicKeyBase58(),
		Digest:   Digest(payload),
		Value:    base64.StdEncoding.EncodeToString(ed25519.Sign(k.private, payload)),
		SignedAt: at.UTC(),
	}, nil
}

// Digest is the hex SHA-256 of the payload, exposed because it is part of the
// wire format and callers need it for the pre-check it enables.
func Digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// SignCard signs an agent card, binding the signature to the agent's own ID.
//
// The agent ID is included in the signed bytes rather than trusted as a header.
// Otherwise a signature produced by one agent could be presented as another
// agent's, which is the same confusion the auth handshake exists to prevent.
func (k *SigningKey) SignCard(card Card, at time.Time) (Signature, error) {
	if card.AgentID != k.PublicKeyBase58() {
		return Signature{}, fmt.Errorf("%w: card claims agent %s but the key is %s",
			ErrInvalid, card.AgentID, k.PublicKeyBase58())
	}
	return k.Sign(CardBytes(card, at), at)
}

// VerifyCard checks a card signature against the agent ID the card claims.
//
// The timestamp is taken from the signature and fed back into the encoding, so
// a changed one produces different bytes and fails rather than being believed.
func VerifyCard(card Card, sig Signature) error {
	return Verify(CardBytes(card, sig.SignedAt), sig, card.AgentID)
}

// AttestCard signs a card on somebody else's behalf.
//
// It is separate from SignCard because the two mean different things and the
// difference has to be visible in the API. SignCard is an agent saying "this is
// my card", and it refuses any card naming another agent, because a self-signing
// call that could name somebody else would defeat the purpose of signing it at
// all. AttestCard is a third party saying "I published this card": the card still
// names its own agent, the signature names the attesting key, and both are in
// the signed bytes. A marketplace attesting a seller's capabilities is not
// claiming to sell anything, and conflating the two would let a reader mistake
// storage for endorsement.
func (k *SigningKey) AttestCard(card Card, at time.Time) (Signature, error) {
	return k.Sign(CardBytes(card, at), at)
}

// VerifyCardAttestedBy checks a third party's attestation over a card, against
// the key expected to have made it.
//
// Both keys are named rather than one being inferred: the card names the agent
// being described and attesterKeyID names who is vouching. A verifier that only
// checked the first would accept the marketplace vouching for an agent's
// capability list, which is exactly the claim it cannot make.
func VerifyCardAttestedBy(card Card, sig Signature, attesterKeyID string) error {
	return Verify(CardBytes(card, sig.SignedAt), sig, attesterKeyID)
}

// SignOffer signs an offer, binding the signature to the seller.
//
// The offer ID is included because an offer's terms are what the buyer is
// agreeing to, and two offers that differ only in ID are still different
// promises. The amount is rendered at the mint's own scale by the caller rather
// than from a float, so the signed bytes carry the integer the buyer will pay.
func (k *SigningKey) SignOffer(offer Offer, at time.Time) (Signature, error) {
	if offer.Seller != k.PublicKeyBase58() {
		return Signature{}, fmt.Errorf("%w: offer claims seller %s but the key is %s",
			ErrInvalid, offer.Seller, k.PublicKeyBase58())
	}
	if _, err := base58.Decode(offer.Mint); err != nil {
		return Signature{}, fmt.Errorf("%w: offer mint is not base58: %w", ErrInvalid, err)
	}
	return k.Sign(OfferBytes(offer, at), at)
}

// VerifyOffer checks an offer signature against the seller the offer names.
func VerifyOffer(offer Offer, sig Signature) error {
	return Verify(OfferBytes(offer, sig.SignedAt), sig, offer.Seller)
}

// Probe is the signed form of a capability probe result.
//
// A probe is the only thing this marketplace does that observes an agent rather
// than taking its word, so the result is signed to say which marketplace watched.
// CheckedAt is included: an observation is only worth what its age makes it worth,
// and a report whose time is not covered is a report that can be presented as
// current long after it stopped being true.
type Probe struct {
	AgentID   string
	Target    string
	Results   []ProbeResult
	CheckedAt time.Time
}

// ProbeResult is one capability's outcome within a probe.
type ProbeResult struct {
	Capability string
	Status     string
	Detail     string
}

// ProbeBytes is the canonical encoding of a probe result.
func ProbeBytes(probe Probe, at time.Time) []byte {
	enc := newEncoder("probe", at)
	enc.field("agent", probe.AgentID)
	enc.field("target", probe.Target)
	enc.field("checkedAt", probe.CheckedAt.UTC().Format(time.RFC3339Nano))
	enc.probeResults("results", probe.Results)
	return enc.bytes()
}

// SignProbe signs a probe result on the marketplace's behalf.
//
// It is a third-party attestation rather than a self-signature, the same
// distinction as AttestCard: the marketplace watched the agent, so the agent
// cannot produce this and neither can any other marketplace.
func (k *SigningKey) SignProbe(probe Probe, at time.Time) (Signature, error) {
	return k.Sign(ProbeBytes(probe, at), at)
}

// VerifyProbeAttestedBy checks a marketplace's attestation over a probe result.
func VerifyProbeAttestedBy(probe Probe, sig Signature, attesterKeyID string) error {
	return Verify(ProbeBytes(probe, sig.SignedAt), sig, attesterKeyID)
}

// Card is the subset of an agent card that is signed.
//
// Version is the card's own version string, deliberately included: an agent that
// ships a new version of its card is making a different claim, and a signature
// that survives the change would say the new version was the one attested.
//
// Capabilities and currencies are sorted before encoding. Two cards that declare
// the same set in a different order are the same claim, and making the signature
// depend on the order would push that distinction onto every agent for no gain.
type Card struct {
	AgentID      string
	Name         string
	Description  string
	Version      string
	URL          string
	Capabilities []string
	Skills       []Skill
	Currencies   []string
	// SettlementModes is signed for the same reason a currency is: it is part of
	// how an agent has told a buyer what it will accept, and an attestation that
	// did not cover it would attest to a listing that could gain a mode after
	// the fact.
	SettlementModes []string
	// ProbeTarget is part of the signed card because it is an address this
	// marketplace will send traffic to. A card whose probe endpoint was not
	// covered would let anybody who can write the card choose where the request
	// goes, which is the thing the address checks exist to prevent.
	ProbeTarget string
}

// Skill is one declared capability of a card, in the form the card itself
// carries it.
//
// It is a separate type rather than reusing domain.AgentSkill so the signed
// bytes stay a specification this package owns. A signature over a struct is only
// reproducible if the struct's shape cannot change underneath the encoding, and
// that is a property of a wire format rather than of whatever an internal domain
// type happens to be today.
type Skill struct {
	ID     string
	Name   string
	Tags   []string
	Input  []string
	Output []string
}

// Offer is the subset of an offer that is signed: the terms a buyer agrees to.
//
// It carries the capability list rather than a single capability because an
// offer may declare several, and a signature that covered one of them would be
// a signature on a different, smaller promise than the one published.
type Offer struct {
	ID           string
	Seller       string
	Description  string
	Direction    string
	Capabilities []string
	Mint         string
	// Scale is the mint's decimal count. It is signed because the base-unit
	// figure below is derived from it: a signature over an amount without the
	// scale that produced it would verify the same digits as 1.00 USDC and
	// 1.00 of a zero-decimal token.
	Scale string
	// AmountBaseUnits is the price at the mint's own scale, as an integer
	// string. A float would make the signed bytes depend on how the value
	// round-tripped, which is not a property of the offer.
	AmountBaseUnits string
	UnitAmount      string
	SettlementModes []string
}

// CardBytes is the canonical encoding of a card, and is exported because a
// verifier in another language needs to reproduce it exactly, not trust this
// implementation's copy.
//
// The timestamp is part of the encoding, so the signed bytes say when the claim
// was made as well as what it was. A verifier reads it out of the signature and
// passes it here, which is why a signature cannot be made to claim a different
// date than the one it was made.
func CardBytes(card Card, at time.Time) []byte {
	enc := newEncoder("agent-card", at)
	enc.field("agent", card.AgentID)
	enc.field("name", card.Name)
	enc.field("description", card.Description)
	enc.field("version", card.Version)
	enc.field("url", card.URL)
	enc.list("capabilities", sortedUnique(card.Capabilities))
	enc.skills("skills", card.Skills)
	enc.field("probeTarget", card.ProbeTarget)
	enc.list("currencies", sortedUnique(card.Currencies))
	enc.list("settlementModes", sortedUnique(card.SettlementModes))
	return enc.bytes()
}

// OfferBytes is the canonical encoding of an offer's terms.
func OfferBytes(offer Offer, at time.Time) []byte {
	enc := newEncoder("offer", at)
	enc.field("offer", offer.ID)
	enc.field("seller", offer.Seller)
	enc.field("description", offer.Description)
	enc.field("direction", offer.Direction)
	enc.list("capabilities", sortedUnique(offer.Capabilities))
	enc.field("mint", offer.Mint)
	enc.field("scale", offer.Scale)
	enc.field("amountBaseUnits", offer.AmountBaseUnits)
	enc.field("unitAmount", offer.UnitAmount)
	enc.list("settlementModes", sortedUnique(offer.SettlementModes))
	return enc.bytes()
}

// encoder builds the canonical form.
//
// The layout is line-based with a length prefix on every value:
//
//	vtessera/attest/v1
//	kind:agent-card
//	field:5:hello
//
// The length is what makes this unambiguous. Without it, a description of
// "alice\nkind:offer" would let a card smuggle a field boundary into its own
// text and change what a verifier believes it signed.
type encoder struct {
	b strings.Builder
}

func newEncoder(kind string, at time.Time) *encoder {
	e := &encoder{}
	e.b.WriteString(CanonicalForm)
	e.b.WriteByte('\n')
	e.b.WriteString("kind:")
	e.b.WriteString(kind)
	e.b.WriteByte('\n')
	// UTC and RFC 3339 so two implementations cannot disagree about what
	// "12:00" meant or how a fractional second is written.
	e.field("signedAt", at.UTC().Format(time.RFC3339Nano))
	return e
}

func (e *encoder) field(name, value string) {
	e.b.WriteString(name)
	e.b.WriteByte(':')
	e.b.WriteString(strconv.Itoa(len(value)))
	e.b.WriteByte(':')
	e.b.WriteString(value)
	e.b.WriteByte('\n')
}

// list writes a count and then each element.
//
// The count goes through a plain counted field of its own rather than through
// field, because a count is a bare number rather than a value with a length
// prefix. Routing it through field would encode it as "count:<len>:<digits>",
// which is well-formed and unambiguous but means every reader has to know that
// one field is spelled differently from the rest.
func (e *encoder) list(name string, values []string) {
	e.b.WriteString(name)
	e.b.WriteString(".count:")
	e.b.WriteString(strconv.Itoa(len(values)))
	e.b.WriteByte('\n')
	for i, v := range values {
		e.field(name+"."+strconv.Itoa(i), v)
	}
}

// probeResults writes a probe's results, ordered by capability.
//
// Sorted for the same reason skills are: the same capabilities probed in another
// order is the same probe, and a verifier should not have to reproduce the order
// an agent happened to answer in. Detail is included because why a capability
// failed is usually the part that tells a buyer whether to wait.
func (e *encoder) probeResults(name string, results []ProbeResult) {
	ordered := append([]ProbeResult(nil), results...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Capability < ordered[j].Capability
	})
	e.b.WriteString(name)
	e.b.WriteString(".count:")
	e.b.WriteString(strconv.Itoa(len(ordered)))
	e.b.WriteByte('\n')
	for i, r := range ordered {
		prefix := name + "." + strconv.Itoa(i)
		e.field(prefix+".capability", r.Capability)
		e.field(prefix+".status", r.Status)
		e.field(prefix+".detail", r.Detail)
	}
}

// skills writes a card's skill list.
//
// Skills are sorted by ID and each field inside a skill is a sorted set, on the
// same reasoning as capabilities: a card that lists the same skills in another
// order is making the same claim. An agent gets one canonical form to reproduce,
// and an agent cannot dodge a changed signature by reordering its own JSON.
func (e *encoder) skills(name string, skills []Skill) {
	ordered := append([]Skill(nil), skills...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return skillKey(ordered[i]) < skillKey(ordered[j])
	})
	e.b.WriteString(name)
	e.b.WriteString(".count:")
	e.b.WriteString(strconv.Itoa(len(ordered)))
	e.b.WriteByte('\n')
	for i, skill := range ordered {
		prefix := name + "." + strconv.Itoa(i)
		e.field(prefix+".id", skill.ID)
		e.field(prefix+".name", skill.Name)
		e.list(prefix+".tags", sortedUnique(skill.Tags))
		e.list(prefix+".inputModes", sortedUnique(skill.Input))
		e.list(prefix+".outputModes", sortedUnique(skill.Output))
	}
}

// skillKey is the sort key for a skill. ID alone is not enough: two skills can
// share an ID and disagree on everything else, and sorting on ID alone would
// leave their relative order down to the input, which is exactly the kind of
// ambiguity the canonical form exists to remove.
func skillKey(s Skill) string {
	return strings.Join([]string{s.ID, s.Name, strings.Join(sortedUnique(s.Tags), "\x00"),
		strings.Join(sortedUnique(s.Input), "\x00"), strings.Join(sortedUnique(s.Output), "\x00")}, "\x00")
}

func (e *encoder) bytes() []byte { return []byte(e.b.String()) }

func sortedUnique(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func decodeKey(keyID string) (ed25519.PublicKey, error) {
	raw, err := base58.Decode(keyID)
	if err != nil {
		return nil, fmt.Errorf("key id is not base58: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key id decodes to %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// CardAttestations is what a marketplace knows about a card's provenance.
//
// The two signatures are kept apart rather than merged into one, because they
// answer different questions and a verifier has to be able to ask them
// separately. Market says this marketplace published this card and it has not
// changed since. Agent says the agent itself stands behind the claims, which is
// the one a capability list actually depends on: the marketplace can confirm it
// stored a list of capabilities, and only the agent can confirm it intends to
// honour them.
type CardAttestations struct {
	Market Signature
	Agent  *Signature
}

// CanonicalForm names the byte encoding this package signs and verifies, so a
// reader holding a signature and some content can tell which encoding produced it
// rather than assuming the one its own library happens to implement.
//
// It is deliberately a string in the payload and a constant in the source: a
// future change to the encoding takes a new version rather than a flag, so an old
// signature cannot be re-read under new rules and reported as valid.
const CanonicalForm = "vtessera/attest/v1"
