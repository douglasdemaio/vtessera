package registry

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/probe"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/google/uuid"
	"strconv"
	"strings"
)

var (
	ErrAgentSuspended      = errors.New("agent is suspended")
	ErrNotOwner            = errors.New("agent does not own this offer")
	ErrOfferClosed         = errors.New("offer is not open")
	ErrCurrencyNotAccepted = errors.New("agent card does not accept this currency")
	ErrAmountTooPrecise    = errors.New("amount has more precision than the settlement mint supports")
	// ErrMintUnpriced means the offer names a currency this deployment has no
	// USD rate for. Publishing it is refused because the spending cap cannot be
	// measured against an unpriced currency, and an offer that cannot be capped
	// is an offer nobody should be able to trade. It belongs here, at the moment
	// a seller chooses a currency, rather than at the moment a buyer tries to
	// spend: the seller is the one who picked it.
	ErrMintUnpriced = errors.New("no USD rate is declared for this currency")

	// ErrAgentHasLiveTrades refuses a retirement while the agent is party to a
	// trade that has not finished. Withdrawing a listing is a statement about
	// future business; a buyer holding an open trade is existing business whose
	// counterparty would disappear mid-negotiation.
	//
	// The refusal itself is a *store.LiveTradesError, which names the blocking
	// trades, and it satisfies errors.Is against this sentinel. This one exists so
	// a caller can recognise the refusal without importing the store.
	//
	// It is an alias of store.ErrAgentHasLiveTrades, not a second errors.New of
	// the same sentence. errors.Is matches on identity, not on text, so two
	// separate sentinels with identical text never match, and these two did not.
	ErrAgentHasLiveTrades = store.ErrAgentHasLiveTrades

	// ErrRetirementReasonRequired refuses a retirement with no stated reason. A
	// privileged action that removes somebody's ability to sell needs to say why
	// at the moment it happens, because the audit record is the only account of it
	// the agent will ever get.
	ErrRetirementReasonRequired = errors.New("a retirement needs a reason")

	// ErrOfferAttestationRequired means the deployment only accepts signed
	// offers and this one had no signature. It is distinct from
	// ErrAttestationRefused, which means a signature was sent and was wrong: one
	// is a missing credential and the other is a bad one, and an operator
	// reading a log needs to tell them apart.
	ErrOfferAttestationRequired = errors.New("this deployment publishes only signed offers")

	// ErrNoProbeTarget means the agent's card does not declare a probe endpoint,
	// so it is never probed. It exists as a refusal rather than a silent skip
	// because an operator asking for a probe and getting an empty report back
	// cannot tell an agent that declined from a runner that did nothing.
	ErrNoProbeTarget = errors.New("agent card does not declare a probe target")

	// ErrAttestationRefused rejects an attestation that does not verify. It is
	// returned rather than silently ignored because an agent that believes it
	// signed something is entitled to be told the service disagrees.
	ErrAttestationRefused = errors.New("attestation does not verify")
)

type Store interface {
	CreateAgent(ctx context.Context, a domain.Agent) error
	GetAgent(ctx context.Context, id string) (domain.Agent, error)
	UpdateAgent(ctx context.Context, a domain.Agent) error
	SaveAgentCard(ctx context.Context, a domain.Agent, agentSig *attest.Signature, marketSig attest.Signature, at time.Time) error
	CardAttestation(ctx context.Context, agentID string) (attest.CardAttestations, bool, error)
	MarketplaceSignatureFor(ctx context.Context, agentID string) (attest.Signature, bool, error)
	OfferAttestation(ctx context.Context, offerID string) (attest.Signature, bool, error)
	SaveProbe(ctx context.Context, report probe.Report, sig attest.Signature, at time.Time) error
	Probe(ctx context.Context, agentID string) (probe.Report, bool, error)
	SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus, at time.Time) error
	ListAgents(ctx context.Context, status domain.AgentStatus) ([]domain.Agent, error)
	ListAgentsByIDs(ctx context.Context, ids []string) (map[string]domain.Agent, error)
	CreateOffer(ctx context.Context, o domain.Offer, idempotencyKey string) error
	CreateOfferWithAttestation(ctx context.Context, o domain.Offer, idempotencyKey string, sig *attest.Signature) error
	GetOffer(ctx context.Context, id string) (domain.Offer, error)
	GetOfferByIdempotencyKey(ctx context.Context, key string) (domain.Offer, error)
	SearchOffers(ctx context.Context, q domain.OfferQuery) ([]domain.Offer, error)
	ListOpenOffers(ctx context.Context) ([]domain.Offer, error)
	OpenOffersBefore(ctx context.Context, cutoff time.Time, limit int) ([]domain.Offer, error)
	SetOfferStatus(ctx context.Context, id string, status domain.OfferStatus, at time.Time) error
	RetireAgent(ctx context.Context, id, reason, actor string, at time.Time) (domain.Agent, error)
	RestoreAgent(ctx context.Context, id, actor string, at time.Time) (domain.Agent, error)
	Retirement(ctx context.Context, id string) (domain.Retirement, bool, error)
	LiveTradesForAgent(ctx context.Context, agentID string) ([]domain.Trade, error)
}

type Service struct {
	store  Store
	mints  tokens.Registry
	priced Pricer
	// requireOfferSig is the operator's declaration that every live offer must be
	// verifiable. It is off by default and never silently degrades: with it off,
	// an unsigned offer is still published and still reports itself unsigned to
	// anyone who asks.
	requireOfferSig bool
	prober          Prober
	now             func() time.Time
	// offerTTL is how long a published offer stays open before the sweep closes
	// it. It is deployment policy, so it is not part of the attested offer.
	offerTTL time.Duration
	// market signs every card this marketplace publishes. It is a required
	// dependency rather than an option, because a card this service accepts is a
	// card it published, and a listing with no marketplace signature would be a
	// listing a directory has no way to attribute to anybody.
	market *attest.SigningKey
}

// Pricer reports whether a mint has a declared USD rate. It is an interface so
// this package does not depend on the cap policy that owns the price table: the
// question being asked here is only whether a currency is priced.
type Pricer interface {
	Priced(mint string) bool
}

// Prober runs a capability probe against an agent's declared endpoint.
type Prober interface {
	Run(ctx context.Context, target, agentID string, capabilities []string, challenge string) (probe.Report, error)
}

// WithProbeRunner supplies the probe executor.
//
// It is a required dependency rather than one built here, because the runner owns
// the outbound connection and its address policy. A registry that constructed its
// own would have a second place where an address check could be relaxed.
func WithProbeRunner(r Prober) Option {
	return func(s *Service) { s.prober = r }
}

// Option configures the service.
type Option func(*Service)

// WithPricer refuses offers priced in a currency with no declared USD rate, so a
// currency the spending cap cannot be measured against cannot be offered at all.
func WithPricer(p Pricer) Option {
	return func(s *Service) { s.priced = p }
}

// WithOfferTTL sets how long a published offer stays open before the sweep
// closes it. A zero TTL disables both setting a deadline and the sweep, which is
// only correct for a deployment that has not configured one; the config layer
// refuses to boot without a positive value, so a zero here means "not wired".
func WithOfferTTL(ttl time.Duration) Option {
	return func(s *Service) { s.offerTTL = ttl }
}

// WithClock overrides the service's clock. It exists so the offer sweep can be
// driven deterministically rather than by sleeping on wall time.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithRequiredOfferAttestation refuses to publish an offer the seller has not
// signed.
//
// It exists as a switch rather than always being on because agents that predate
// attestations publish unsigned offers, and a deployment should get to say its
// agents have been migrated instead of discovering it from a wave of 400s. The
// refusal is a 409 and not a 400: the request is well-formed and the seller can
// fix it by signing, which is a different problem from sending something invalid.
func WithRequiredOfferAttestation(required bool) Option {
	return func(s *Service) { s.requireOfferSig = required }
}

// New builds the service over a governed mint registry. The registry is a
// required argument rather than a defaulted one: a service that silently assumes
// a token set has no way to be correct about a cluster it was never told, and
// the assumption is invisible from the call site.
//
// Pass nil for a deployment with on-chain settlement unconfigured. Every mint is
// then unknown, which means the service declines to check price precision
// against a scale it does not govern — the honest answer when there is no
// cluster to take a scale from.
func New(store Store, mints tokens.Registry, market *attest.SigningKey, opts ...Option) *Service {
	s := &Service{store: store, mints: mints, market: market, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// mintInfo describes a mint through the governed registry, never through a
// hardcoded table: token identity is by address, and a symbol is decoration.
// A deployment with no governed registry knows no scale, and says so.
func (s *Service) mintInfo(address string) domain.MintInfo {
	if s.mints == nil {
		return domain.MintInfo{Address: address}
	}
	if entry, ok := s.mints.Lookup(address); ok {
		return entry.MintInfo()
	}
	return domain.MintInfo{Address: address}
}

// Register publishes an agent card and signs it twice: once with the agent's own
// key if the agent supplied a signature, and always with the marketplace's.
//
// The two say different things and the distinction is the point. The marketplace
// signature says this listing came from this deployment and has not changed
// since, which is what a directory needs to attribute a card to a marketplace.
// The agent signature says the agent stands behind its own claims, and it is the
// only one that means anything for a capability list: the marketplace can confirm
// it stored a list of capabilities, and only the agent can confirm it intends to
// honour them.
//
// The agent's signature is verified before the card is stored, but its absence is
// not an error. Every agent registered before attestations existed has an unsigned
// card, and refusing those would lock out every existing seller to make a claim
// about a fraction of them. What is refused is a card carrying a signature that
// does not verify, because storing that would mean publishing something the
// service has told a reader is authentic when it is not.
func (s *Service) Register(ctx context.Context, agentID string, card domain.AgentCard, sig *attest.Signature) (domain.Agent, bool, error) {
	if _, err := domain.ParsePublicKey(agentID); err != nil {
		return domain.Agent{}, false, fmt.Errorf("%w: agent id: %w", domain.ErrInvalid, err)
	}
	if card.PublicKey == "" {
		card.PublicKey = agentID
	}
	if card.PublicKey != agentID {
		return domain.Agent{}, false, fmt.Errorf("%w: agent card publicKey %q does not match the authenticated agent %q",
			domain.ErrInvalid, card.PublicKey, agentID)
	}
	if err := card.Validate(); err != nil {
		return domain.Agent{}, false, err
	}
	if sig != nil {
		// Verified against the key in the signature and required to be this
		// agent's, so another agent's signature over this card is refused rather
		// than stored.
		if err := attest.VerifyCard(s.attestCard(card), *sig); err != nil {
			return domain.Agent{}, false, fmt.Errorf("%w: %w", ErrAttestationRefused, err)
		}
	}
	now := s.now().UTC()
	statement := s.attestCard(card)
	// Signed with the same timestamp as the agent's own signature when there is
	// one, so a reader comparing the two is not looking at two cards that were
	// supposedly accepted at different moments.
	marketSig, err := s.market.AttestCard(statement, now)
	if err != nil {
		return domain.Agent{}, false, fmt.Errorf("%w: signing the card for publication: %w", ErrAttestationRefused, err)
	}
	existing, err := s.store.GetAgent(ctx, agentID)
	switch {
	case err == nil:
		existing.Card = card
		existing.UpdatedAt = now
		if err := s.store.SaveAgentCard(ctx, existing, sig, marketSig, now); err != nil {
			return domain.Agent{}, false, err
		}
		return existing, false, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.Agent{}, false, err
	}
	agent := domain.Agent{ID: agentID, Card: card, Status: domain.AgentActive, CreatedAt: now, UpdatedAt: now}
	if err := s.store.SaveAgentCard(ctx, agent, sig, marketSig, now); err != nil {
		return domain.Agent{}, false, err
	}
	return agent, true, nil
}

// attestCard projects a card onto the signed subset, filling the agent ID from
// the authenticated session rather than from the card.
//
// The card's own publicKey is already required to equal the session's, but the
// signed statement names the agent separately, and it comes from the credential
// rather than the payload — otherwise an agent could sign a card claiming to be
// somebody else and have the claim verified against itself.
func (s *Service) attestCard(card domain.AgentCard) attest.Card {
	currencies := card.Currencies
	modes := make([]string, 0, len(card.SettlementModes))
	for _, m := range card.SettlementModes {
		modes = append(modes, string(m))
	}
	skills := make([]attest.Skill, 0, len(card.Skills))
	for _, sk := range card.Skills {
		skills = append(skills, attest.Skill{
			ID:     sk.ID,
			Name:   sk.Name,
			Tags:   sk.Tags,
			Input:  sk.Input,
			Output: sk.Output,
		})
	}
	return attest.Card{
		AgentID:         card.PublicKey,
		Name:            card.Name,
		Description:     card.Description,
		Version:         card.Version,
		URL:             card.URL,
		Capabilities:    card.Capabilities,
		Skills:          skills,
		Currencies:      currencies,
		SettlementModes: modes,
		ProbeTarget:     card.ProbeTarget,
	}
}

// AttestedCard projects a card onto the signed subset, for a caller verifying a
// stored signature.
func (s *Service) AttestedCard(card domain.AgentCard) attest.Card {
	return s.attestCard(card)
}

// MarketplaceKeyID is the key every card this marketplace publishes is signed
// with, so a directory can be told which marketplace to trust for a card rather
// than being handed a signature with no key to check it against.
func (s *Service) MarketplaceKeyID() string {
	if s.market == nil {
		return ""
	}
	return s.market.PublicKeyBase58()
}

// CardAttestation returns an agent's stored card signatures and whether an
// attestation record exists at all.
//
// The three outcomes are kept apart on purpose. A record with a marketplace
// signature and no agent signature is an unsigned card, which every agent
// registered before attestations existed has. No record at all is a card that
// predates attestation. A record with a signature that fails to verify is a
// disagreement between content and signature, which is the only one of the three
// that says something is wrong.
func (s *Service) CardAttestation(ctx context.Context, agentID string) (attest.CardAttestations, bool, error) {
	return s.store.CardAttestation(ctx, agentID)
}

// VerifyOfferAttestation checks a stored offer signature against the offer as it
// stands now.
//
// It returns whether the signature verifies and never an error, because this is
// the check a reader makes: the answer is a verdict, and the common answer is
// "there is nothing to check" rather than something having gone wrong.
func (s *Service) VerifyOfferAttestation(ctx context.Context, offer domain.Offer) (attest.Signature, bool) {
	sig, found, err := s.store.OfferAttestation(ctx, offer.ID)
	if err != nil || !found {
		return attest.Signature{}, false
	}
	return sig, attest.VerifyOffer(s.attestOffer(offer), sig) == nil
}

// attestOffer projects an offer onto the signed subset.
//
// The base-unit figure and the mint scale are what the buyer actually pays, and
// both are derived here from the governed registry rather than taken on trust
// from the caller. A deployment that governs no registry knows no scale, and
// signs the decimal string empty rather than inventing one — which makes the
// signature unverifiable by a reader that knows the scale, rather than
// verifiable against the wrong one.
func (s *Service) attestOffer(offer domain.Offer) attest.Offer {
	modes := make([]string, 0, len(offer.SettlementModes))
	for _, m := range offer.SettlementModes {
		modes = append(modes, string(m))
	}
	scale, units := "", ""
	if mint := s.mintInfo(offer.PriceMint); mint.Known {
		scale = strconv.Itoa(mint.Decimals)
		if base, err := offer.PriceAmount.BaseUnits(mint.Decimals); err == nil {
			units = strconv.FormatUint(base, 10)
		}
	}
	return attest.Offer{
		ID:              offer.ID,
		Seller:          offer.AgentID,
		Description:     offer.Description,
		Direction:       string(offer.Direction),
		Capabilities:    offer.Capabilities,
		Mint:            offer.PriceMint,
		Scale:           scale,
		AmountBaseUnits: units,
		UnitAmount:      offer.PriceAmount.String(),
		SettlementModes: modes,
	}
}

func (s *Service) Agent(ctx context.Context, id string) (domain.Agent, error) {
	return s.store.GetAgent(ctx, id)
}

func (s *Service) Agents(ctx context.Context, status domain.AgentStatus) ([]domain.Agent, error) {
	if status == "" {
		status = domain.AgentActive
	}
	return s.store.ListAgents(ctx, status)
}

type NewOffer struct {
	Direction       domain.OfferDirection
	Description     string
	Capabilities    []string
	PriceAmount     string
	PriceMint       string
	SettlementModes []domain.SettlementMode
}

// PublishOffer lists an offer, optionally with the seller's signature over its
// terms.
//
// in.ID may be supplied by the caller, and doing so is what makes a signed
// publish possible: a signature covers the offer ID, so an agent that cannot
// choose the ID cannot sign the offer before it exists. Letting the seller name
// it also removes the window in which an unsigned offer is live and buyable
// before a signature arrives as a second request.
//
// When both an ID and a signature are supplied they must agree, and the
// signature is verified before anything is stored. Supplying a signature without
// an ID is refused rather than ignored, because the alternative is an agent
// believing it signed an offer that never carried the signature.
func (s *Service) PublishOffer(ctx context.Context, agentID string, in NewOffer, idempotencyKey string, offerID string, sig *attest.Signature) (domain.Offer, bool, error) {
	if sig != nil && offerID == "" {
		return domain.Offer{}, false, fmt.Errorf("%w: an offer signature covers the offer id, so the seller must supply it",
			domain.ErrInvalid)
	}
	// The idempotency lookup comes first so that a client which lost a response
	// can still recover an offer it already created. Turning the requirement on
	// must not strand an offer that exists, and returning it is not the same as
	// allowing a new unsigned listing.
	if idempotencyKey != "" {
		existing, err := s.store.GetOfferByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Offer{}, false, err
		}
	}
	if s.requireOfferSig && sig == nil {
		return domain.Offer{}, false, fmt.Errorf("%w: sign the offer and send it as attestation, or turn --require-offer-attestation off",
			ErrOfferAttestationRequired)
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return domain.Offer{}, false, err
	}
	if agent.Status != domain.AgentActive {
		return domain.Offer{}, false, fmt.Errorf("%w: %s", ErrAgentSuspended, agentID)
	}
	amount, err := money.Parse(in.PriceAmount)
	if err != nil {
		return domain.Offer{}, false, fmt.Errorf("priceAmount: %w", err)
	}
	now := s.now().UTC()
	// Whether the caller chose the ID decides whether a taken one is a collision.
	// A generated ID that is somehow taken is a fault in this service and is
	// reported as one; a caller's ID that is taken is the caller's problem to
	// resolve, and has to be resolved rather than worked around.
	callerSuppliedID := offerID != ""
	if !callerSuppliedID {
		offerID = uuid.NewString()
	}
	offer := domain.Offer{
		ID:              offerID,
		AgentID:         agentID,
		Direction:       in.Direction,
		Description:     in.Description,
		Capabilities:    in.Capabilities,
		PriceAmount:     amount,
		PriceMint:       in.PriceMint,
		SettlementModes: in.SettlementModes,
		Status:          domain.OfferOpen,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if s.offerTTL > 0 {
		offer.ExpiresAt = now.Add(s.offerTTL)
	}
	if err := offer.Validate(); err != nil {
		return domain.Offer{}, false, err
	}
	if err := checkCurrencyAccepted(agent, offer.PriceMint); err != nil {
		return domain.Offer{}, false, err
	}
	if s.priced != nil && !s.priced.Priced(offer.PriceMint) {
		return domain.Offer{}, false, fmt.Errorf("%w: %s", ErrMintUnpriced, offer.PriceMint)
	}
	if offer.AcceptsMode(domain.SettlementOnchain) {
		if err := s.checkSettleable(offer); err != nil {
			return domain.Offer{}, false, err
		}
	}
	statement := s.attestOffer(offer)
	if sig != nil {
		// Verified against the content that is about to be stored, not against a
		// projection of it, so a signature that verified against something else
		// cannot reach the database.
		if err := attest.VerifyOffer(statement, *sig); err != nil {
			return domain.Offer{}, false, fmt.Errorf("%w: %w", ErrAttestationRefused, err)
		}
	}
	if callerSuppliedID {
		existing, err := s.store.GetOffer(ctx, offerID)
		switch {
		case err == nil:
			// A taken ID is a retry of the same listing, and it is one only if
			// the terms are the same terms. Anything else is a collision, and
			// overwriting the incumbent would be a way to replace a seller's
			// terms without them noticing.
			//
			// Returning the incumbent offer instead would be worse than either:
			// the caller would believe it published a listing it did not, and
			// when the ID belongs to another seller it would be handed that
			// seller's terms in the response.
			if !sameListing(statement, s.attestOffer(existing), existing.CreatedAt) {
				return domain.Offer{}, false, fmt.Errorf("%w: offer id %s is already in use",
					domain.ErrConflict, offerID)
			}
			return existing, false, nil
		case errors.Is(err, domain.ErrNotFound):
		default:
			return domain.Offer{}, false, err
		}
	}
	if err := s.store.CreateOfferWithAttestation(ctx, offer, idempotencyKey, sig); err != nil {
		return domain.Offer{}, false, err
	}
	return offer, true, nil
}

// sameListing reports whether two statements are the same set of terms.
//
// It compares canonical digests rather than structs so that a retry which lists
// the same capabilities or settlement modes in a different order is still
// recognised as the same listing, which is what a caller re-sending a request
// means. The timestamp is the incumbent's own, passed to both encodings, since it
// is not part of the terms and two encodings with different timestamps never
// agree.
func sameListing(a, b attest.Offer, at time.Time) bool {
	return attest.Digest(attest.OfferBytes(a, at)) == attest.Digest(attest.OfferBytes(b, at))
}

// ProbeCapabilities runs a capability probe against an agent's declared endpoint
// and records what it found, signed by this marketplace.
//
// The probe is opt-in twice over: the card has to name a target, and that name is
// covered by the agent's own signature and this marketplace's attestation, so an
// agent cannot be redirected to an address it did not declare. There is also no
// schedule behind this. A marketplace that probed agents on a timer would be
// sending traffic nobody asked for, so a probe happens when an operator asks for
// one.
//
// The result is recorded whether or not it passed. A probe that only stored
// passes would leave a directory unable to distinguish an agent that was checked
// and failed from one that was never checked, which is the difference between a
// negative answer and no answer.
func (s *Service) ProbeCapabilities(ctx context.Context, agentID string) (probe.Report, error) {
	if s.prober == nil {
		return probe.Report{}, errors.New("this build has no probe runner configured")
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return probe.Report{}, err
	}
	if agent.Status != domain.AgentActive {
		return probe.Report{}, fmt.Errorf("%w: %s", ErrAgentSuspended, agentID)
	}
	target := strings.TrimSpace(agent.Card.ProbeTarget)
	if target == "" {
		return probe.Report{}, fmt.Errorf("%w: %s", ErrNoProbeTarget, agentID)
	}
	// The capabilities probed are the ones the card claims, taken from the stored
	// card rather than from the request, so what is tested and what is advertised
	// cannot come to differ.
	report, err := s.prober.Run(ctx, target, agentID, agent.Card.Capabilities, s.challenge())
	if err != nil {
		return report, err
	}
	sig, err := s.market.SignProbe(probe.Statement(report), s.now().UTC())
	if err != nil {
		return report, fmt.Errorf("sign probe result: %w", err)
	}
	if err := s.store.SaveProbe(ctx, report, sig, s.now().UTC()); err != nil {
		return report, err
	}
	report.Signature = &sig
	return report, nil
}

// ProbeResult returns an agent's last recorded probe.
func (s *Service) ProbeResult(ctx context.Context, agentID string) (probe.Report, bool, error) {
	return s.store.Probe(ctx, agentID)
}

// VerifyProbe checks a stored probe's attestation against this marketplace's key.
//
// A probe whose signature does not verify is reported as invalid rather than
// dropped, so a reader can tell a corrupt row from an unsigned one.
func (s *Service) VerifyProbe(report probe.Report) error {
	if report.Signature == nil {
		return fmt.Errorf("%w: probe result is unsigned", ErrAttestationRefused)
	}
	return attest.VerifyProbeAttestedBy(probe.Statement(report), *report.Signature,
		s.market.PublicKeyBase58())
}

// challenge is the nonce a probe answer has to echo.
//
// It is random rather than a timestamp because its whole job is to distinguish a
// live agent from a cached or proxied answer, and a timestamp is a value a cache
// would happily replay.
func (s *Service) challenge() string {
	raw := make([]byte, 16)
	// There is no error to handle. crypto/rand.Read never returns one: if the
	// system source fails it crashes the program rather than hand back a short
	// read, so a probe challenge is either random or the process is gone.
	//
	// This previously fell back to a base64 timestamp ending "-not-a-challenge",
	// under a comment saying the fallback kept the failure loud rather than
	// probing with a guessable nonce. It was unreachable, and had it been reached
	// it would have done the thing the comment rules out: it issued a nonce an
	// attacker could compute from the clock. Failing loudly is what rand.Read
	// already does.
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func checkCurrencyAccepted(agent domain.Agent, mint string) error {
	if len(agent.Card.Currencies) == 0 {
		return nil
	}
	for _, accepted := range agent.Card.Currencies {
		if accepted == mint {
			return nil
		}
	}
	return fmt.Errorf("%w: %s accepts %v", ErrCurrencyNotAccepted, agent.ID, agent.Card.Currencies)
}

func (s *Service) checkSettleable(offer domain.Offer) error {
	mint := s.mintInfo(offer.PriceMint)
	if !mint.Known {
		return nil
	}
	if _, err := offer.PriceAmount.BaseUnits(mint.Decimals); err != nil {
		if errors.Is(err, money.ErrTooPrecise) {
			return fmt.Errorf("%w: %s supports %d decimals, %s has more", ErrAmountTooPrecise, mint.Symbol, mint.Decimals, offer.PriceAmount)
		}
		return err
	}
	return nil
}

func (s *Service) CloseOffer(ctx context.Context, agentID, offerID string) (domain.Offer, error) {
	offer, err := s.store.GetOffer(ctx, offerID)
	if err != nil {
		return domain.Offer{}, err
	}
	if offer.AgentID != agentID {
		return domain.Offer{}, fmt.Errorf("%w: offer %s", ErrNotOwner, offerID)
	}
	if offer.Status != domain.OfferOpen {
		return offer, nil
	}
	now := s.now().UTC()
	if err := s.store.SetOfferStatus(ctx, offerID, domain.OfferClosed, now); err != nil {
		return domain.Offer{}, err
	}
	offer.Status = domain.OfferClosed
	offer.UpdatedAt = now
	return offer, nil
}

func (s *Service) Offer(ctx context.Context, id string) (domain.Offer, error) {
	return s.store.GetOffer(ctx, id)
}

func (s *Service) Search(ctx context.Context, q domain.OfferQuery) ([]domain.Offer, error) {
	return s.store.SearchOffers(ctx, q)
}

// ExpireOffers closes up to limit open offers whose deadline has passed. It is
// the sweeper's unit of work and is safe to call by hand.
//
// Each close re-reads the offer before applying: a seller can close a listing
// between the batch being read and the update, and re-closing an already closed
// offer would overwrite the time the seller actually closed it.
func (s *Service) ExpireOffers(ctx context.Context, limit int) (int, error) {
	if s.offerTTL <= 0 {
		return 0, nil
	}
	cutoff := s.now().UTC()
	offers, err := s.store.OpenOffersBefore(ctx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, offer := range offers {
		current, err := s.store.GetOffer(ctx, offer.ID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return expired, err
		}
		if current.Status != domain.OfferOpen || current.ExpiresAt.IsZero() || current.ExpiresAt.After(cutoff) {
			continue
		}
		if err := s.store.SetOfferStatus(ctx, offer.ID, domain.OfferClosed, cutoff); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

// RunOfferExpirySweeper closes expired offers until the context is done. Like the
// trade sweep it is a plain interval loop: a stale listing is not urgent, it is a
// listing a buyer should stop being offered.
func (s *Service) RunOfferExpirySweeper(ctx context.Context, every time.Duration, limit int) {
	if s.offerTTL <= 0 || every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A failed sweep is retried on the next tick; there is nothing to
			// escalate to, and the next tick still delists the stale offer.
			_, _ = s.ExpireOffers(ctx, limit)
		}
	}
}

type AnnouncementSource struct {
	store Store
	mints *Service
	now   func() time.Time
}

func (s *Service) AnnouncementSource() *AnnouncementSource {
	return &AnnouncementSource{store: s.store, mints: s, now: s.now}
}

// Retire withdraws an agent's listing at the operator's request: the
// registration is marked retired and its open offers close. Nothing is deleted,
// so every tessera already issued naming this agent keeps verifying.
//
// The reason is required and recorded against the operator's own actor string.
// This is the only route on the service that removes a principal's ability to
// trade without that principal asking, so it is deliberately the least automatic
// thing in the package: it refuses while a trade is live, and it leaves a row an
// auditor can read.
func (s *Service) Retire(ctx context.Context, agentID, reason, actor string) (domain.Agent, error) {
	if reason == "" {
		return domain.Agent{}, ErrRetirementReasonRequired
	}
	// No check here for live trades. The store makes that call inside the same
	// transaction as the write, so there is no window between the two, and a
	// refusal from it names the blocking trades.
	return s.store.RetireAgent(ctx, agentID, reason, actor, s.now().UTC())
}

// Restore reverses a retirement. It does not reopen the offers the retirement
// closed: restoring says the agent may trade again, not that its old offers are
// republished on the agent's behalf.
func (s *Service) Restore(ctx context.Context, agentID, actor string) (domain.Agent, error) {
	if _, err := s.store.GetAgent(ctx, agentID); err != nil {
		return domain.Agent{}, err
	}
	return s.store.RestoreAgent(ctx, agentID, actor, s.now().UTC())
}

// Retirement reports the recorded withdrawal for an agent, if there is one.
func (s *Service) Retirement(ctx context.Context, agentID string) (domain.Retirement, bool, error) {
	return s.store.Retirement(ctx, agentID)
}
