package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/google/uuid"
)

// Onboard registers a brand-new agent, its card and its first offer in one
// request, for the one-shot path a fresh agent follows from llms.txt: take a
// challenge, then present the session, the card and the first listing together.
//
// It is deliberately the fresh path only. An agent that already has a card is
// refused with a conflict that points at the general routes rather than having
// its card and terms replaced, because those routes are the update path and a
// one-shot that overwrote an incumbent would be a way for a fresh session on a
// re-used key to displace a live listing without the seller noticing. That also
// answers every resend: a retry of a successful onboard finds the agent and is
// told it is registered, so onboarding needs no idempotency-recovery branch
// distinct from that refusal.
//
// The writes are one transaction (see store.OnboardAgent), because a discoverable
// agent with no offer or an offer with no seller is not a half-configured agent
// but a marketplace inconsistency, and the request exists precisely so that
// cannot be observed.
func (s *Service) Onboard(ctx context.Context, agentID string, card domain.AgentCard, cardSig *attest.Signature, in NewOffer, idempotencyKey string, offerID string, offerSig *attest.Signature) (domain.Agent, domain.Offer, error) {
	if _, err := s.store.GetAgent(ctx, agentID); err == nil {
		return domain.Agent{}, domain.Offer{}, fmt.Errorf("%w: agent %s is already registered; use PUT /v1/agents/{id}/card and POST /v1/agents/{id}/offers",
			ErrAgentAlreadyRegistered, agentID)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Agent{}, domain.Offer{}, err
	}
	if s.requireOfferSig && offerSig == nil {
		return domain.Agent{}, domain.Offer{}, fmt.Errorf("%w: sign the offer and send it as attestation, or turn --require-offer-attestation off",
			ErrOfferAttestationRequired)
	}
	card, err := s.prepareCard(agentID, card, cardSig)
	if err != nil {
		return domain.Agent{}, domain.Offer{}, err
	}
	now := s.now().UTC()
	cardStatement := s.attestCard(card)
	marketSig, err := s.market.AttestCard(cardStatement, now)
	if err != nil {
		return domain.Agent{}, domain.Offer{}, fmt.Errorf("%w: signing the card for publication: %w", ErrAttestationRefused, err)
	}
	agent := domain.Agent{ID: agentID, Card: card, Status: domain.AgentActive, CreatedAt: now, UpdatedAt: now}

	callerSuppliedID := offerID != ""
	if !callerSuppliedID {
		offerID = uuid.NewString()
	}
	offer, offerStatement, err := s.buildOffer(now, agent, in, offerID)
	if err != nil {
		return domain.Agent{}, domain.Offer{}, err
	}
	if offerSig != nil {
		if err := attest.VerifyOffer(offerStatement, *offerSig); err != nil {
			return domain.Agent{}, domain.Offer{}, fmt.Errorf("%w: %w", ErrAttestationRefused, err)
		}
	}
	if callerSuppliedID {
		// The agent is new, so any offer already under this ID belongs to somebody
		// else and is a collision rather than a retry; the recovery retry is
		// refused earlier, at the agent check.
		if _, err := s.store.GetOffer(ctx, offerID); err == nil {
			return domain.Agent{}, domain.Offer{}, fmt.Errorf("%w: offer id %s is already in use",
				domain.ErrConflict, offerID)
		} else if !errors.Is(err, domain.ErrNotFound) {
			return domain.Agent{}, domain.Offer{}, err
		}
	}
	if err := s.store.OnboardAgent(ctx, agent, cardSig, marketSig, now, offer, idempotencyKey, offerSig); err != nil {
		return domain.Agent{}, domain.Offer{}, err
	}
	return agent, offer, nil
}
