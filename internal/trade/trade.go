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
	// ErrNoCardPublished means the caller authenticated, which proves it holds
	// the key behind its identity, and then tried to trade without ever publishing
	// a card. Sessions are issued before an agent exists, so this is the ordinary
	// state of any identity that has not run PUT /v1/agents/{id}/card yet.
	//
	// It is a 409 with its own code rather than the 404 the caller used to get.
	// A 404 says the thing asked for does not exist, and that is false in a way
	// that costs an integrator a day: the offer exists, the offer ID is right, and
	// the caller is looking at a refusal that reads as "bad identifier" for a
	// request that had nothing wrong with it.
	ErrNoCardPublished = errors.New("this identity has authenticated but published no agent card")
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
	// ErrNotExpiredYet means a party tried to cancel an accepted trade before its
	// deadline. Acceptance is a commitment, so it holds until the deadline rather
	// than until one side changes its mind: an accepted trade that could be walked
	// away from at will would leave the counterparty with no way to plan.
	ErrNotExpiredYet = errors.New("this accepted trade has not expired yet")
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
	// TradeAcceptance returns when the trade was accepted. False means it has not
	// been, which is a state rather than a fault.
	TradeAcceptance(ctx context.Context, tradeID string) (time.Time, bool, error)
	// AcceptedBefore returns accepted trades whose deadline has passed, bounded.
	AcceptedBefore(ctx context.Context, deadline time.Time, limit int) ([]domain.Trade, error)
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

	// acceptTTL is how long an accepted trade may sit uncommitted before it can be
	// cancelled. It exists so that refusing a commit is never a dead end: see
	// Record. Zero means no deadline, which is the safe direction for an operator
	// who has not chosen one.
	acceptTTL time.Duration

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

// WithClock replaces the service's clock. It exists because a deadline cannot be
// tested any other way: waiting out a real hour proves nothing that a fixed clock
// does not prove instantly and repeatably.
func (s *Service) WithClock(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

// WithAcceptanceTTL bounds how long an accepted trade may wait to be committed.
// Without it an accepted trade cannot be cancelled at all, which is what forces
// Record to go unchecked rather than enforcing the cap.
func (s *Service) WithAcceptanceTTL(ttl time.Duration) *Service {
	s.acceptTTL = ttl
	return s
}

// AcceptanceTTL reports the configured deadline length, zero when there is none.
func (s *Service) AcceptanceTTL() time.Duration { return s.acceptTTL }

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

// acceptanceDeadline is when an accepted trade stops being a commitment. It is
// anchored on the acceptance rather than on creation, because what the deadline
// bounds is how long after both parties said yes the buyer has to move.
//
// A trade that was never accepted has no deadline, and a service with no
// configured TTL has no deadline either. Both answer zero, which every caller
// reads as "there is nothing to wait for".
func (s *Service) acceptanceDeadline(ctx context.Context, tr domain.Trade) (time.Time, error) {
	if s.acceptTTL <= 0 || tr.State != domain.TradeAccepted {
		return time.Time{}, nil
	}
	acceptedAt, found, err := s.store.TradeAcceptance(ctx, tr.ID)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, nil
	}
	return acceptedAt.UTC().Add(s.acceptTTL), nil
}

// Expired reports whether an accepted trade is past its deadline and can therefore
// be cancelled. A trade in any other state is never expired: it has its own
// transitions, and reporting otherwise would let a recorded trade be cancelled.
func (s *Service) Expired(ctx context.Context, tr domain.Trade) (bool, error) {
	deadline, err := s.acceptanceDeadline(ctx, tr)
	if err != nil {
		return false, err
	}
	if deadline.IsZero() {
		return false, nil
	}
	return !s.now().UTC().Before(deadline), nil
}

// ExpireAccepted cancels accepted trades whose deadline has passed, so that a
// trade neither party acts on stops reserving a buyer's budget and stops being
// listed as live. It is safe to run repeatedly and safe to run concurrently with
// agents: the state change is conditional on the trade still being accepted, so a
// buyer who commits a trade at the same moment loses the race in the direction
// that keeps their money.
//
// The cap on the batch is what makes this a background job rather than a startup
// cost. A large backlog is swept over several ticks, and each trade releases its
// reservation as it is cancelled.
func (s *Service) ExpireAccepted(ctx context.Context, limit int) (int, error) {
	if s.acceptTTL <= 0 {
		return 0, nil
	}
	deadline := s.now().UTC()
	trades, err := s.store.AcceptedBefore(ctx, deadline, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, tr := range trades {
		// Re-checked per trade rather than trusted from the query: the deadline and
		// the state can both have moved while the batch was being read, and
		// cancelling a trade a buyer just committed is not recoverable from here.
		past, err := s.Expired(ctx, tr)
		if err != nil {
			return expired, err
		}
		if !past || tr.State != domain.TradeAccepted {
			continue
		}
		detail := reasonDetail("the acceptance deadline passed before this trade was committed")
		if _, err := s.applyAs(ctx, tr, "", domain.TradeCancelled, detail, domain.EventExpired); err != nil {
			if errors.Is(err, ErrIllegalState) {
				continue
			}
			return expired, err
		}
		expired++
	}
	return expired, nil
}

// RunExpirySweeper cancels expired accepted trades until the context is done. It
// is deliberately a plain loop on an interval: an expired trade is not urgent, it
// is a budget that ought to come back.
func (s *Service) RunExpirySweeper(ctx context.Context, every time.Duration, limit int) {
	if s.acceptTTL <= 0 || every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A failed sweep is retried on the next tick rather than propagated:
			// there is nothing to escalate to and the next tick still frees the
			// budget. Silent by design, not by oversight; the deadline bounds how
			// long a stuck sweep can delay anything.
			_, _ = s.ExpireAccepted(ctx, limit)
		}
	}
}

// checkSpend refuses a trade that would take its buyer past a cap.
//
// It is called at every point where money can actually move. The first is
// creation, before either party has committed, and a trade reserves its amount
// against the cap from that moment: a buyer cannot open a fifth negotiation it
// has no budget for and decide later whether to take it. The second is when an
// on-chain settlement is compiled, the last moment before the buyer holds a
// signable transaction. The third is the off-chain commit, which is only safe
// because an accepted trade expires: a refused commit leaves a trade the buyer
// can cancel rather than one they are stuck holding.
//
// All three are needed rather than one, because a trade can sit in accepted
// across a window rolling over. Whichever check it passes, the others may be the
// ones that saw the budget spent.
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

// CommittedSpendUSD returns the value, in micro-USD, of every engagement this
// buyer holds inside the rolling window — the same figure checkSpend refuses
// against. It is a read: it reserves nothing and changes nothing. It exists so
// an agent can see how much of its day a hanging or disputed trade is holding
// rather than discovering it only by being refused.
//
// A committed mint that no longer has a rate is an error rather than a zero,
// for the same reason checkSpend refuses one: counting it as nothing would
// understate the buyer's spend.
func (s *Service) CommittedSpendUSD(ctx context.Context, buyerAgentID string) (uint64, error) {
	if s.limits == nil {
		return 0, nil
	}
	policy := *s.limits
	rows, err := s.committedSpend(ctx, buyerAgentID, policy.Window, "")
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, row := range rows {
		value, err := policy.USDValue(row.Amount, row.Mint)
		if err != nil {
			if errors.Is(err, limits.ErrNoRate) {
				return 0, fmt.Errorf("%w: this agent has committed spend in %s, which has no rate", ErrMintUnpriced, row.Mint)
			}
			return 0, fmt.Errorf("price this agent's committed spend: %w", err)
		}
		total += value
		if total < value {
			return 0, fmt.Errorf("%w: this agent's committed spend exceeds the representable range", ErrSpendCapExceeded)
		}
	}
	return total, nil
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
	// An accepted trade can be cancelled, but only once its deadline has passed.
	// The map says the transition is legal and Service.Cancel says when: a legal
	// transition that depends on a clock cannot be expressed as a constant.
	domain.TradeAccepted: {
		domain.TradeSettlementPending: domain.EventSettlementPending,
		domain.TradeRecorded:          domain.EventRecorded,
		domain.TradeDisputed:          domain.EventDisputed,
		domain.TradeCancelled:         domain.EventCancelled,
	},
	domain.TradeSettlementPending: {
		domain.TradeSettled:   domain.EventSettled,
		domain.TradeDisputed:  domain.EventDisputed,
		domain.TradeCancelled: domain.EventCancelled,
	},
	// disputed is terminal for the parties and has exactly one way out, and it is
	// the operator's. It does not go to cancelled directly: a resolution records a
	// verdict, and an event log that read the same for a dispute the operator
	// voided and one they refused to void would be worth less than the token it
	// took to file it.
	domain.TradeDisputed: {
		domain.TradeResolved: domain.EventResolved,
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
			// A missing row means one of two different things, and answering both
			// with 404 was wrong for one of them. For the counterparty it is close
			// enough: something the offer names is not there. For the caller it is
			// not — the session already proved the identity exists, so there is no
			// agent row only because no card has been published, and 404 NOT_FOUND
			// on an offer that is sitting right there reads as a bad offer ID.
			if errors.Is(err, domain.ErrNotFound) && party == actorID {
				return domain.Trade{}, false, fmt.Errorf(
					"%w: PUT /v1/agents/%s/card first", ErrNoCardPublished, actorID)
			}
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
	// Acceptance is a commitment and it holds until the deadline rather than until
	// one side changes its mind. After the deadline the trade is dead and either
	// party may walk away, which is what makes it safe for Record to refuse a
	// commit: there is somewhere to go.
	if tr.State == domain.TradeAccepted {
		deadline, err := s.acceptanceDeadline(ctx, tr)
		if err != nil {
			return domain.Trade{}, err
		}
		// With no deadline there is no point at which the commitment lapses, so
		// the transition stays closed. Refusing is the direction that does not
		// quietly widen who may cancel, and configuration refuses to boot in this
		// state anyway.
		if deadline.IsZero() {
			return domain.Trade{}, fmt.Errorf("%w: %s is accepted with no deadline to cancel it after",
				ErrIllegalState, tr.ID)
		}
		if s.now().UTC().Before(deadline) {
			return domain.Trade{}, fmt.Errorf("%w: %s is accepted until %s",
				ErrNotExpiredYet, tr.ID, deadline.Format(time.RFC3339))
		}
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

// Resolve closes a disputed trade on the operator's authority and records the
// verdict. It is deliberately not party-gated: the whole point of a dispute is
// that the two parties disagree, so the one actor who can end it is the one who
// is neither of them. The route that calls it is behind the operator token.
//
// Resolving an already-resolved trade is a no-op rather than a conflict, the
// same way Dispute is, so a retried operator request cannot fail on its own
// success. Any other state is refused: the state machine, not the caller, owns
// which trades are open to review.
func (s *Service) Resolve(ctx context.Context, operatorID, tradeID string, outcome domain.Resolution, reason string) (domain.Trade, error) {
	if !outcome.Valid() {
		return domain.Trade{}, fmt.Errorf("%w: unknown resolution %q", domain.ErrInvalid, outcome)
	}
	tr, err := s.store.GetTrade(ctx, tradeID)
	if err != nil {
		return domain.Trade{}, err
	}
	if tr.State == domain.TradeResolved {
		return tr, nil
	}
	if tr.State != domain.TradeDisputed {
		return domain.Trade{}, fmt.Errorf("%w: trade %s is %s, not disputed", ErrIllegalState, tradeID, tr.State)
	}
	return s.applyAs(ctx, tr, operatorID, domain.TradeResolved, resolutionDetail(outcome, reason), domain.EventResolved)
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
	// Checked here as well as at creation, because this is the only place an
	// off-chain trade can be refused and the check is cheap. It used to be absent
	// for a reason that is no longer true: refusing at the commit used to strand
	// the buyer in accepted, which has no route out. With a deadline there is a
	// route out, so the cap can be enforced at the moment the commitment is made
	// rather than a window earlier.
	//
	// The trade being committed is excluded from its own total for the same
	// reason the on-chain build excludes it: the reservation query already counts
	// it, and counting it twice would refuse a trade inside its cap.
	if err := s.checkSpend(ctx, tr.BuyerAgentID, tr.Amount, tr.Mint, tr.ID); err != nil {
		return domain.Trade{}, domain.Receipt{}, err
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
	return s.applyAs(ctx, tr, actorID, to, detail, "")
}

// applyAs is apply with the option to record a different event than the
// transition implies. The expiry sweep cancels an accepted trade through the
// cancelled transition but is not a cancellation anybody asked for, and the event
// log is what an auditor reads to tell those two apart.
func (s *Service) applyAs(ctx context.Context, tr domain.Trade, actorID string, to domain.TradeState, detail json.RawMessage, event domain.TradeEventType) (domain.Trade, error) {
	kind, ok := canTransition(tr.State, to)
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
	if event != "" {
		kind = event
	}
	if err := s.appendEvent(ctx, tr.ID, actorID, kind, tr.State, to, detail); err != nil {
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

func resolutionDetail(outcome domain.Resolution, reason string) json.RawMessage {
	detail, err := json.Marshal(map[string]string{
		"outcome": string(outcome),
		"reason":  reason,
	})
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
