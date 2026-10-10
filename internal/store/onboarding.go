package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
)

// OnboardAgent writes a brand-new agent, its card and both card signatures, its
// first offer and that offer's signature in a single transaction.
//
// Onboarding is one fact for the same reason a card and its attestation are: an
// agent that is discoverable but has no offer is not the agent the caller asked
// for, and an offer with no seller is a listing over nothing. Committing them
// separately would leave a window, after a crash or between two statements, in
// which a half-onboarded agent is live — which is exactly the state the one
// request exists to avoid. It reuses the same row writers as the general paths,
// so a schema change cannot land in one and miss the other.
func (s *Store) OnboardAgent(ctx context.Context, agent domain.Agent, agentSig *attest.Signature, marketSig attest.Signature, at time.Time, offer domain.Offer, idempotencyKey string, offerSig *attest.Signature) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := insertAgentCardRows(ctx, tx, agent, agentSig, marketSig, at); err != nil {
			return err
		}
		if err := insertOfferRow(ctx, tx, offer, idempotencyKey); err != nil {
			return err
		}
		if offerSig == nil {
			return nil
		}
		return insertOfferAttestationRow(ctx, tx, offer, offerSig)
	})
}
