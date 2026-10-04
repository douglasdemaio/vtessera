package trade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/google/uuid"
)

var (
	ErrNotParty         = errors.New("agent is not a party to this trade")
	ErrIllegalState     = errors.New("trade state does not allow this transition")
	ErrOfferUnavailable = errors.New("offer cannot be traded")
	// ErrSettlementUnconfigured means this deployment has no cluster and no
	// endpoint, so on-chain trades are refused rather than half-handled. It is
	// a 501: the feature is not switched on here.
	ErrSettlementUnconfigured = errors.New("on-chain settlement is not configured for this deployment")
	// ErrOnchainUnavailable means the chain could not be consulted: an RPC
	// error, a timeout, a rate limit, or a genesis hash that no longer matches
	// the declared cluster. It is a 503 with retry semantics, kept separate from
	// ErrSettlementUnconfigured so a transient node problem is never reported as
	// a missing feature.
	ErrOnchainUnavailable = errors.New("the chain could not be consulted")
	ErrModeNotAccepted    = errors.New("offer does not accept this settlement mode")
	ErrAgentUnavailable   = errors.New("counterparty agent is not available")
	// ErrMintUngoverned means the offer names a governed mint that is not
	// governed on this cluster. It is a 409 rather than a 400: the mint was the
	// seller's choice and the buyer cannot correct it, so it is a conflict with
	// server state and not a malformed request.
	ErrMintUngoverned = errors.New("offer price mint is not governed on this cluster")
	// ErrSpendCapExceeded means the trade would take an agent past a spending cap
	// it has not raised. It is a 409: the cap is server policy the buyer opted
	// into by default and can change only by asking, so it is a conflict with
	// server state rather than a malformed request.
	ErrSpendCapExceeded = errors.New("trade exceeds this agent's spending cap")
	// ErrMintUnpriced means the trade is denominated in a mint this deployment
	// has no USD rate for, so the cap cannot be evaluated against it. It is
	// refused rather than allowed uncapped: an agent that could pick an unpriced
	// currency would have a way around the cap entirely.
	ErrMintUnpriced = errors.New("no USD rate is declared for this mint")
)

type Store interface {
	CreateTrade(ctx context.Context, t domain.Trade, idempotencyKey string) error
	GetTrade(ctx context.Context, id string) (domain.Trade, error)
	GetTradeByIdempotencyKey(ctx context.Context, key string) (domain.Trade, error)
	AddTradeAcceptance(ctx context.Context, tradeID, agentID string, at time.Time) (bool, error)
	SetTradeState(ctx context.Context, tradeID string, from, to domain.TradeState, at time.Time) (bool, error)
	AppendTradeEvent(ctx context.Context, e domain.TradeEvent) (int64, error)
	ListTradeEvents(ctx context.Context, tradeID string) ([]domain.TradeEvent, error)
	UsageMetrics(ctx context.Context) (domain.UsageMetrics, error)
}

type OfferStore interface {
	GetOffer(ctx context.Context, id string) (domain.Offer, error)
	SetOfferStatus(ctx context.Context, id string, status domain.OfferStatus, at time.Time) error
}

type AgentStore interface {
	GetAgent(ctx context.Context, id string) (domain.Agent, error)
}

// LimitStore is the optional dependency spending caps need. It is separate from
// Store because caps are a policy layer over trades rather than part of the
// trade lifecycle: a service without it trades exactly as it did before caps
// existed, which is the same way settlement is optional here.
type LimitStore interface {
	// GetAgentLimits returns ErrNotFound-equivalent for an agent that has not
	// opted in to anything above the deployment default.
	GetAgentLimits(ctx context.Context, agentID string) (domain.AgentLimits, error)
	// SetAgentLimits records an opt-in. Both halves are required: a cap that
	// cannot be read is a cap that cannot be enforced, and a cap that cannot be
	// raised is a cap the operator did not choose.
	SetAgentLimits(ctx context.Context, l domain.AgentLimits) error
	// CommittedSpendSince returns the amounts a buyer has committed since the
	// cutoff, in token terms.
	CommittedSpendSince(ctx context.Context, buyerAgentID string, since time.Time, excludeTradeID string) ([]domain.SpendRow, error)
}

type Ledger interface {
	Record(ctx context.Context, t domain.Trade, solanaSignature, clusterName string) (domain.Receipt, error)
	Receipt(ctx context.Context, tradeID string) (domain.Receipt, error)
}

type Service struct {
	store       Store
	offers      OfferStore
	agents      AgentStore
	ledger      Ledger
	settlement  SettlementDeps
	limits      *limits.Policy
	limitsStore LimitStore
	now         func() time.Time

	// reserveMu serialises the read-a-cap-then-reserve-a-cap sequence in
	// Create. Checking a daily cap and then inserting the reservation that
	// discharges it are two statements, and without a lock between them two
	// concurrent creates can both read a budget with room left and both take
	// it. One lock for all buyers rather than one per buyer: SQLite serialises
	// the write that follows anyway, so a second lock would add a map to keep
	// correct in exchange for contention nobody can observe at this scale.
	reserveMu sync.Mutex
}

func New(store Store, offers OfferStore, agents AgentStore, led Ledger) *Service {
	return &Service{store: store, offers: offers, agents: agents, ledger: led, now: func() time.Time { return time.Now().UTC() }}
}

// WithLimits turns spending caps on. Without it a service places no cap on
// anything, which is why it is an explicit call rather than a default: a
// deployment that has not been given a cap policy is making a statement about
// what it will allow, and it should be a deliberate one.
func (s *Service) WithLimits(policy limits.Policy, store LimitStore) *Service {
	if store != nil {
		policy = policy.WithAgentLimits(func(ctx context.Context, agentID string) (money.Amount, money.Amount, bool) {
			stored, err := store.GetAgentLimits(ctx, agentID)
			if err != nil {
				// An unreadable opt-in falls back to the deployment default,
				// which is the tighter of the two numbers in the case that
				// matters: an agent whose raise cannot be read must not be
				// trusted to have one.
				return money.Amount{}, money.Amount{}, false
			}
			return stored.PerTradeUSD, stored.PerDayUSD, true
		})
	}
	s.limits = &policy
	s.limitsStore = store
	return s
}

// Limits returns the active cap policy, or false when caps are not in force.
func (s *Service) Limits() (limits.Policy, bool) {
	if s.limits == nil {
		return limits.Policy{}, false
	}
	return *s.limits, true
}

// EffectiveLimits resolves the caps in force for an agent. It is exported
// through the API so a response can tell an agent what its own limits are
// instead of leaving it to infer them from a refusal.
func (s *Service) EffectiveLimits(ctx context.Context, agentID string) (limits.Limits, error) {
	if s.limits == nil {
		return limits.Limits{}, errors.New("spending caps are not configured on this deployment")
	}
	return s.limits.Effective(ctx, agentID), nil
}

// RaiseLimits records an agent's opt-in to caps above the deployment default.
//
// The ceilings are checked here rather than in the handler, because the decision
// is a policy one and the handler has no way to know the policy. Raising is
// deliberately not lowering: an agent sending a cap below the default gets the
// default back, so this route cannot be used to shrink an agent's own limit into
// a state it then trips over.
func (s *Service) RaiseLimits(ctx context.Context, agentID string, perTrade, perDay money.Amount) (limits.Limits, error) {
	if s.limits == nil || s.limitsStore == nil {
		return limits.Limits{}, errors.New("spending caps are not configured on this deployment")
	}
	policy := *s.limits
	if perTrade.IsZero() || perTrade.Cmp(policy.PerTrade) < 0 {
		perTrade = policy.PerTrade
	}
	if perDay.IsZero() || perDay.Cmp(policy.PerDay) < 0 {
		perDay = policy.PerDay
	}
	if err := policy.Check(perTrade, perDay); err != nil {
		return limits.Limits{}, err
	}
	if err := s.limitsStore.SetAgentLimits(ctx, domain.AgentLimits{
		AgentID:     agentID,
		PerTradeUSD: perTrade,
		PerDayUSD:   perDay,
		RaisedAt:    s.now().UTC(),
	}); err != nil {
		return limits.Limits{}, err
	}
	return policy.Effective(ctx, agentID), nil
}

// checkSpend refuses a trade that would take its buyer past a cap.
//
// It is called at the two points where refusing is still free. The first is
// creation, before either party has committed, and a trade reserves its amount
// against the cap from that moment: a buyer cannot open a fifth negotiation it
// has no budget for and decide later whether to take it. The second is when an
// on-chain settlement is compiled, the last moment before the buyer holds a
// signable transaction, because a trade can sit in accepted across a window
// rolling over.
//
// There is deliberately no check at the off-chain commit. Refusing there would be
// worse than the problem it solves: the trade is already accepted, and an
// accepted trade cannot be cancelled, so the buyer would hold a trade it can
// neither complete nor walk away from.
// excludeTradeID names a trade the caller is accounting for itself, so it is
// not counted twice: see store.CommittedSpendSince.
func (s *Service) checkSpend(ctx context.Context, buyerAgentID string, amount money.Amount, mint string, excludeTradeID string) error {
	if s.limits == nil {
		return nil
	}
	policy := *s.limits
	effective := policy.Effective(ctx, buyerAgentID)

	value, err := policy.USDValue(amount, mint)
	if err != nil {
		if errors.Is(err, limits.ErrNoRate) {
			return fmt.Errorf("%w: %s", ErrMintUnpriced, mint)
		}
		return fmt.Errorf("price this trade: %w", err)
	}

	perTradeCap, err := limits.CapMicro(effective.PerTrade)
	if err != nil {
		return fmt.Errorf("per-trade cap: %w", err)
	}
	if value > perTradeCap {
		return fmt.Errorf("%w: %s is %s and this agent's per-trade cap is %s",
			ErrSpendCapExceeded, amount, limits.FormatUSD(value), effective.PerTrade)
	}

	rows, err := s.committedSpend(ctx, buyerAgentID, policy.Window, excludeTradeID)
	if err != nil {
		return err
	}
	committed := value
	for _, row := range rows {
		rowValue, err := policy.USDValue(row.Amount, row.Mint)
		if err != nil {
			// A committed trade in a mint that has since lost its rate cannot be
			// counted, and counting it as zero would understate the buyer's
			// spend. Refusing is the direction that does not let more through.
			if errors.Is(err, limits.ErrNoRate) {
				return fmt.Errorf("%w: this agent has committed spend in %s, which has no rate",
					ErrMintUnpriced, row.Mint)
			}
			return fmt.Errorf("price this agent's committed spend: %w", err)
		}
		committed += rowValue
		if committed < rowValue {
			// Saturating rather than wrapping: an overflow here would understate
			// the total and let a trade through.
			return fmt.Errorf("%w: this agent's committed spend exceeds the representable range",
				ErrSpendCapExceeded)
		}
	}
	perDayCap, err := limits.CapMicro(effective.PerDay)
	if err != nil {
		return fmt.Errorf("daily cap: %w", err)
	}
	if committed > perDayCap {
		return fmt.Errorf("%w: %s in the last %s against a daily cap of %s",
			ErrSpendCapExceeded, limits.FormatUSD(committed), policy.Window, effective.PerDay)
	}
	return nil
}

func (s *Service) committedSpend(ctx context.Context, buyerAgentID string, window time.Duration, excludeTradeID string) ([]domain.SpendRow, error) {
	if s.limitsStore == nil {
		return nil, nil
	}
	rows, err := s.limitsStore.CommittedSpendSince(ctx, buyerAgentID, s.now().UTC().Add(-window), excludeTradeID)
	if err != nil {
		return nil, fmt.Errorf("read committed spend: %w", err)
	}
	return rows, nil
}

func (s *Service) UsageMetrics(ctx context.Context) (domain.UsageMetrics, error) {
	m, err := s.store.UsageMetrics(ctx)
	if err != nil {
		return domain.UsageMetrics{}, err
	}
	m.GeneratedAt = s.now()
	return m, nil
}

var transitions = map[domain.TradeState]map[domain.TradeState]domain.TradeEventType{
	domain.TradeProposed: {
		domain.TradeNegotiating: domain.EventNegotiating,
		domain.TradeCancelled:   domain.EventCancelled,
	},
	domain.TradeNegotiating: {
		domain.TradeAccepted:  domain.EventAccepted,
		domain.TradeCancelled: domain.EventCancelled,
	},
	domain.TradeAccepted: {
		domain.TradeSettlementPending: domain.EventSettlementPending,
		domain.TradeRecorded:          domain.EventRecorded,
		domain.TradeDisputed:          domain.EventDisputed,
	},
	domain.TradeSettlementPending: {
		domain.TradeSettled:   domain.EventSettled,
		domain.TradeDisputed:  domain.EventDisputed,
		domain.TradeCancelled: domain.EventCancelled,
	},
}

func canTransition(from, to domain.TradeState) (domain.TradeEventType, bool) {
	allowed, ok := transitions[from]
	if !ok {
		return "", false
	}
	event, ok := allowed[to]
	return event, ok
}

func (s *Service) Create(ctx context.Context, actorID, offerID string, mode domain.SettlementMode, idempotencyKey string) (domain.Trade, bool, error) {
	if mode == "" {
		mode = domain.SettlementOffchain
	}
	if !mode.Valid() {
		return domain.Trade{}, false, fmt.Errorf("%w: %q", ErrModeNotAccepted, mode)
	}
	if mode == domain.SettlementOnchain {
		if err := s.requireSettlement(ctx); err != nil {
			return domain.Trade{}, false, err
		}
	}
	if idempotencyKey != "" {
		existing, err := s.store.GetTradeByIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Trade{}, false, err
		}
	}
	offer, err := s.offers.GetOffer(ctx, offerID)
	if err != nil {
		return domain.Trade{}, false, err
	}
	if offer.Status != domain.OfferOpen {
		return domain.Trade{}, false, fmt.Errorf("%w: %s is %s", ErrOfferUnavailable, offerID, offer.Status)
	}
	if !offer.AcceptsMode(mode) {
		return domain.Trade{}, false, fmt.Errorf("%w: %s offers %v", ErrModeNotAccepted, offerID, offer.SettlementModes)
	}
	if offer.AgentID == actorID {
		return domain.Trade{}, false, fmt.Errorf("%w: an agent cannot trade with its own offer", ErrOfferUnavailable)
	}
	buyer, seller := actorID, offer.AgentID
	if offer.Direction == domain.DirectionBid {
		buyer, seller = offer.AgentID, actorID
	}
	for _, party := range []string{buyer, seller} {
		agent, err := s.agents.GetAgent(ctx, party)
		if err != nil {
			return domain.Trade{}, false, err
		}
		if agent.Status != domain.AgentActive {
			return domain.Trade{}, false, fmt.Errorf("%w: %s is %s", ErrAgentUnavailable, party, agent.Status)
		}
	}
	if mode == domain.SettlementOnchain {
		if err := s.checkSettleable(buyer, seller, offer.PriceMint); err != nil {
			return domain.Trade{}, false, err
		}
	}
	// Held from the cap read to the reservation write, and released once the
	// reservation is visible to the next reader.
	if s.limits != nil {
		s.reserveMu.Lock()
		defer s.reserveMu.Unlock()
		if err := s.checkSpend(ctx, buyer, offer.PriceAmount, offer.PriceMint, ""); err != nil {
			return domain.Trade{}, false, err
		}
	}
	now := s.now().UTC()
	tr := domain.Trade{
		ID:             uuid.NewString(),
		OfferID:        offer.ID,
		BuyerAgentID:   buyer,
		SellerAgentID:  seller,
		Description:    offer.Description,
		Amount:         offer.PriceAmount,
		Mint:           offer.PriceMint,
		SettlementMode: mode,
		State:          domain.TradeProposed,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateTrade(ctx, tr, idempotencyKey); err != nil {
		return domain.Trade{}, false, err
	}
	if err := s.appendEvent(ctx, tr.ID, actorID, domain.EventProposed, "", domain.TradeProposed, nil); err != nil {
		return domain.Trade{}, false, err
	}
	tr.Acceptances = []string{}
	return tr, true, nil
}

func (s *Service) BeginNegotiation(ctx context.Context, actorID, tradeID string) (domain.Trade, error) {
	return s.transition(ctx, actorID, tradeID, domain.TradeProposed, domain.TradeNegotiating, nil)
}

func (s *Service) Cancel(ctx context.Context, actorID, tradeID, reason string) (domain.Trade, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if tr.State == domain.TradeCancelled {
		return tr, nil
	}
	// A live (unexpired) settlement transaction may already be in flight, so the
	// trade is not cancellable until the blockhash lapses (design spec §5).
	if _, live, err := s.liveSettlement(ctx, tradeID); err != nil {
		return domain.Trade{}, err
	} else if live {
		return domain.Trade{}, ErrSettlementLive
	}
	detail := reasonDetail(reason)
	return s.apply(ctx, tr, actorID, domain.TradeCancelled, detail)
}

func (s *Service) Dispute(ctx context.Context, actorID, tradeID, reason string) (domain.Trade, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if tr.State == domain.TradeDisputed {
		return tr, nil
	}
	return s.apply(ctx, tr, actorID, domain.TradeDisputed, reasonDetail(reason))
}

func (s *Service) Accept(ctx context.Context, actorID, tradeID string) (domain.Trade, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if tr.State == domain.TradeAccepted || tr.State == domain.TradeRecorded {
		return tr, nil
	}
	if tr.State != domain.TradeNegotiating {
		return domain.Trade{}, fmt.Errorf("%w: cannot accept in state %s", ErrIllegalState, tr.State)
	}
	now := s.now().UTC()
	inserted, err := s.store.AddTradeAcceptance(ctx, tradeID, actorID, now)
	if err != nil {
		return domain.Trade{}, err
	}
	if !inserted {
		return s.store.GetTrade(ctx, tradeID)
	}
	updated, err := s.store.GetTrade(ctx, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if len(updated.Acceptances) < 2 {
		return updated, nil
	}
	return s.apply(ctx, updated, actorID, domain.TradeAccepted, acceptanceDetail(updated))
}

func (s *Service) Record(ctx context.Context, actorID, tradeID string) (domain.Trade, domain.Receipt, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if tr.State == domain.TradeRecorded {
		receipt, err := s.ledger.Receipt(ctx, tradeID)
		return tr, receipt, err
	}
	recorded, err := s.apply(ctx, tr, actorID, domain.TradeRecorded, nil)
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	// An off-chain trade settles on no chain, so the tessera names none. Passing
	// the configured cluster here would assert a fact about a settlement that
	// never touched a block.
	receipt, err := s.ledger.Record(ctx, recorded, "", "")
	if err != nil {
		return domain.Trade{}, domain.Receipt{}, err
	}
	if err := s.offers.SetOfferStatus(ctx, recorded.OfferID, domain.OfferClosed, s.now().UTC()); err != nil {
		return recorded, receipt, err
	}
	return recorded, receipt, nil
}

func (s *Service) Get(ctx context.Context, actorID, tradeID string) (domain.Trade, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	events, err := s.store.ListTradeEvents(ctx, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	tr.Events = events
	return tr, nil
}

func (s *Service) Tessera(ctx context.Context, actorID, tradeID string) (domain.Receipt, error) {
	if _, err := s.partyTrade(ctx, actorID, tradeID); err != nil {
		return domain.Receipt{}, err
	}
	return s.ledger.Receipt(ctx, tradeID)
}

func (s *Service) transition(ctx context.Context, actorID, tradeID string, from, to domain.TradeState, detail json.RawMessage) (domain.Trade, error) {
	tr, err := s.partyTrade(ctx, actorID, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if tr.State == to {
		return tr, nil
	}
	if tr.State != from {
		return domain.Trade{}, fmt.Errorf("%w: %s cannot move to %s from %s", ErrIllegalState, tradeID, to, tr.State)
	}
	return s.apply(ctx, tr, actorID, to, detail)
}

func (s *Service) apply(ctx context.Context, tr domain.Trade, actorID string, to domain.TradeState, detail json.RawMessage) (domain.Trade, error) {
	event, ok := canTransition(tr.State, to)
	if !ok {
		return domain.Trade{}, fmt.Errorf("%w: %s cannot move from %s to %s", ErrIllegalState, tr.ID, tr.State, to)
	}
	now := s.now().UTC()
	changed, err := s.store.SetTradeState(ctx, tr.ID, tr.State, to, now)
	if err != nil {
		return domain.Trade{}, err
	}
	if !changed {
		return s.store.GetTrade(ctx, tr.ID)
	}
	if err := s.appendEvent(ctx, tr.ID, actorID, event, tr.State, to, detail); err != nil {
		return domain.Trade{}, err
	}
	return s.store.GetTrade(ctx, tr.ID)
}

func (s *Service) partyTrade(ctx context.Context, actorID, tradeID string) (domain.Trade, error) {
	tr, err := s.store.GetTrade(ctx, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if !tr.Party(actorID) {
		return domain.Trade{}, fmt.Errorf("%w: trade %s", ErrNotParty, tradeID)
	}
	return tr, nil
}

func (s *Service) appendEvent(ctx context.Context, tradeID, actorID string, eventType domain.TradeEventType, from, to domain.TradeState, detail json.RawMessage) error {
	_, err := s.store.AppendTradeEvent(ctx, domain.TradeEvent{
		TradeID:      tradeID,
		ActorAgentID: actorID,
		Type:         eventType,
		FromState:    from,
		ToState:      to,
		Detail:       detail,
		CreatedAt:    s.now().UTC(),
	})
	return err
}

func reasonDetail(reason string) json.RawMessage {
	if reason == "" {
		return nil
	}
	detail, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return nil
	}
	return detail
}

func acceptanceDetail(tr domain.Trade) json.RawMessage {
	detail, err := json.Marshal(map[string]any{"acceptedBy": tr.Acceptances})
	if err != nil {
		return nil
	}
	return detail
}
