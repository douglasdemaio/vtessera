package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/google/uuid"
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
	ErrAgentHasLiveTrades = errors.New("agent has trades that have not reached a terminal state")

	// ErrRetirementReasonRequired refuses a retirement with no stated reason. A
	// privileged action that removes somebody's ability to sell needs to say why
	// at the moment it happens, because the audit record is the only account of it
	// the agent will ever get.
	ErrRetirementReasonRequired = errors.New("a retirement needs a reason")
)

type Store interface {
	CreateAgent(ctx context.Context, a domain.Agent) error
	GetAgent(ctx context.Context, id string) (domain.Agent, error)
	UpdateAgent(ctx context.Context, a domain.Agent) error
	SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus, at time.Time) error
	ListAgents(ctx context.Context, status domain.AgentStatus) ([]domain.Agent, error)
	ListAgentsByIDs(ctx context.Context, ids []string) (map[string]domain.Agent, error)
	CreateOffer(ctx context.Context, o domain.Offer, idempotencyKey string) error
	GetOffer(ctx context.Context, id string) (domain.Offer, error)
	GetOfferByIdempotencyKey(ctx context.Context, key string) (domain.Offer, error)
	SearchOffers(ctx context.Context, q domain.OfferQuery) ([]domain.Offer, error)
	ListOpenOffers(ctx context.Context) ([]domain.Offer, error)
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
	now    func() time.Time
}

// Pricer reports whether a mint has a declared USD rate. It is an interface so
// this package does not depend on the cap policy that owns the price table: the
// question being asked here is only whether a currency is priced.
type Pricer interface {
	Priced(mint string) bool
}

// Option configures the service.
type Option func(*Service)

// WithPricer refuses offers priced in a currency with no declared USD rate, so a
// currency the spending cap cannot be measured against cannot be offered at all.
func WithPricer(p Pricer) Option {
	return func(s *Service) { s.priced = p }
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
func New(store Store, mints tokens.Registry, opts ...Option) *Service {
	s := &Service{store: store, mints: mints, now: func() time.Time { return time.Now().UTC() }}
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

func (s *Service) Register(ctx context.Context, agentID string, card domain.AgentCard) (domain.Agent, bool, error) {
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
	now := s.now().UTC()
	existing, err := s.store.GetAgent(ctx, agentID)
	switch {
	case err == nil:
		existing.Card = card
		existing.UpdatedAt = now
		if err := s.store.UpdateAgent(ctx, existing); err != nil {
			return domain.Agent{}, false, err
		}
		return existing, false, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.Agent{}, false, err
	}
	agent := domain.Agent{ID: agentID, Card: card, Status: domain.AgentActive, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return domain.Agent{}, false, err
	}
	return agent, true, nil
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

func (s *Service) PublishOffer(ctx context.Context, agentID string, in NewOffer, idempotencyKey string) (domain.Offer, bool, error) {
	if idempotencyKey != "" {
		existing, err := s.store.GetOfferByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Offer{}, false, err
		}
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
	offer := domain.Offer{
		ID:              uuid.NewString(),
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
	if err := s.store.CreateOffer(ctx, offer, idempotencyKey); err != nil {
		return domain.Offer{}, false, err
	}
	return offer, true, nil
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
	live, err := s.store.LiveTradesForAgent(ctx, agentID)
	if err != nil {
		return domain.Agent{}, err
	}
	if len(live) > 0 {
		return domain.Agent{}, fmt.Errorf("%w: %s still has %d (%s)",
			ErrAgentHasLiveTrades, agentID, len(live), describeTradeStates(live))
	}
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

func describeTradeStates(trades []domain.Trade) string {
	counts := map[domain.TradeState]int{}
	var order []domain.TradeState
	for _, tr := range trades {
		if _, seen := counts[tr.State]; !seen {
			order = append(order, tr.State)
		}
		counts[tr.State]++
	}
	parts := make([]string, 0, len(order))
	for _, state := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
	}
	return strings.Join(parts, ", ")
}
