package trade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/google/uuid"
)

var (
	ErrNotParty           = errors.New("agent is not a party to this trade")
	ErrIllegalState       = errors.New("trade state does not allow this transition")
	ErrOfferUnavailable   = errors.New("offer cannot be traded")
	ErrOnchainUnavailable = errors.New("on-chain settlement is not available yet")
	ErrModeNotAccepted    = errors.New("offer does not accept this settlement mode")
	ErrAgentUnavailable   = errors.New("counterparty agent is not available")
)

type Store interface {
	CreateTrade(ctx context.Context, t domain.Trade, idempotencyKey string) error
	GetTrade(ctx context.Context, id string) (domain.Trade, error)
	GetTradeByIdempotencyKey(ctx context.Context, key string) (domain.Trade, error)
	AddTradeAcceptance(ctx context.Context, tradeID, agentID string, at time.Time) (bool, error)
	SetTradeState(ctx context.Context, tradeID string, from, to domain.TradeState, at time.Time) (bool, error)
	AppendTradeEvent(ctx context.Context, e domain.TradeEvent) (int64, error)
	ListTradeEvents(ctx context.Context, tradeID string) ([]domain.TradeEvent, error)
}

type OfferStore interface {
	GetOffer(ctx context.Context, id string) (domain.Offer, error)
	SetOfferStatus(ctx context.Context, id string, status domain.OfferStatus, at time.Time) error
}

type AgentStore interface {
	GetAgent(ctx context.Context, id string) (domain.Agent, error)
}

type Ledger interface {
	Record(ctx context.Context, t domain.Trade, solanaSignature string) (domain.Receipt, error)
	Receipt(ctx context.Context, tradeID string) (domain.Receipt, error)
}

type Service struct {
	store      Store
	offers     OfferStore
	agents     AgentStore
	ledger     Ledger
	settlement SettlementDeps
	now        func() time.Time
}

func New(store Store, offers OfferStore, agents AgentStore, led Ledger) *Service {
	return &Service{store: store, offers: offers, agents: agents, ledger: led, now: func() time.Time { return time.Now().UTC() }}
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
		if err := s.requireSettlement(); err != nil {
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
	receipt, err := s.ledger.Record(ctx, recorded, "")
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
