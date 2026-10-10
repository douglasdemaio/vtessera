package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/probe"
)

// CardAttestation returns an agent's stored card signatures.
//
// The boolean is false for an agent registered before attestations existed, which
// is the only case with no row: a card an agent did not sign still has a
// marketplace attestation, and is still returned. An absent row therefore means
// "this card predates attestation", which is different from "this card is
// unsigned" and would be wrongly reported as either if the two shared a shape.
func (s *Store) CardAttestation(ctx context.Context, agentID string) (attest.CardAttestations, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT card_signature, market_signature FROM agent_attestations WHERE agent_id = ?`, agentID)
	var agentSig, marketSig sql.NullString
	switch err := row.Scan(&agentSig, &marketSig); {
	case errors.Is(err, sql.ErrNoRows):
		return attest.CardAttestations{}, false, nil
	case err != nil:
		return attest.CardAttestations{}, false, fmt.Errorf("card attestation: %w", err)
	}
	out := attest.CardAttestations{}
	if marketSig.Valid {
		sig, err := decodeSignature(marketSig.String)
		if err != nil {
			return attest.CardAttestations{}, false, fmt.Errorf("marketplace card attestation for %s is corrupt: %w", agentID, err)
		}
		out.Market = sig
	}
	if agentSig.Valid {
		sig, err := decodeSignature(agentSig.String)
		if err != nil {
			return attest.CardAttestations{}, false, fmt.Errorf("card attestation for %s is corrupt: %w", agentID, err)
		}
		out.Agent = &sig
	}
	return out, true, nil
}

// decodeSignature turns a stored signature back into a value.
//
// A row that will not decode is reported rather than skipped, because a signature
// that cannot be parsed is not an absent signature: reporting it as absent would
// turn a corrupted row into an unsigned card, which is the one answer a reader
// would act on.
// signedAtOf is the timestamp column for an optional signature: null when there
// is no signature, because a timestamp with no signature beside it describes
// nothing.
func signedAtOf(sig *attest.Signature) sql.NullInt64 {
	if sig == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: nanos(sig.SignedAt), Valid: true}
}

func decodeSignature(encoded string) (attest.Signature, error) {
	var sig attest.Signature
	if err := json.Unmarshal([]byte(encoded), &sig); err != nil {
		return attest.Signature{}, err
	}
	return sig, nil
}

// OfferAttestation returns a stored offer signature, and whether one exists.
func (s *Store) OfferAttestation(ctx context.Context, offerID string) (attest.Signature, bool, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT signature FROM offer_attestations WHERE offer_id = ?`, offerID).Scan(&encoded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return attest.Signature{}, false, nil
	case err != nil:
		return attest.Signature{}, false, fmt.Errorf("offer attestation: %w", err)
	}
	var sig attest.Signature
	if err := json.Unmarshal([]byte(encoded), &sig); err != nil {
		return attest.Signature{}, false, fmt.Errorf("offer attestation for %s is corrupt: %w", offerID, err)
	}
	return sig, true, nil
}

// SaveAgentCard stores an agent's card together with both of its attestations in
// a single transaction.
//
// The card and its signatures are one fact. Written separately, a crash between
// them leaves a card whose signature is missing or, worse, a signature
// describing a card that has been replaced. The agent's absent signature has to
// travel in the same transaction for the same reason: clearing a stale one after
// a card change is what stops a verifier being invited to check content that no
// longer exists.
//
// A nil agentSig means the agent did not sign its own card, which is the state
// of every agent registered before attestations existed. The marketplace
// signature is required, because a card this service accepted is a card it
// published.
func (s *Store) SaveAgentCard(ctx context.Context, agent domain.Agent, agentSig *attest.Signature, marketSig attest.Signature, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return insertAgentCardRows(ctx, tx, agent, agentSig, marketSig, at)
	})
}

// insertAgentCardRows writes the agent row and both of its card signatures inside
// a caller-supplied transaction, so a card and its attestations are one fact and
// can also be part of a larger fact (see OnboardAgent) without a second copy of
// these INSERTs drifting from the schema.
func insertAgentCardRows(ctx context.Context, tx *sql.Tx, agent domain.Agent, agentSig *attest.Signature, marketSig attest.Signature, at time.Time) error {
	// The signature must be the agent's own. The registry has already checked
	// that it covers these exact terms, but this is the one property that must
	// hold in the database no matter who calls: a row whose agent signature is
	// some other key's is a signature that can never verify, and a reader
	// cannot tell that from one that was tampered with later.
	if agentSig != nil && agentSig.KeyID != agent.ID {
		return fmt.Errorf("%w: card attestation for %s was signed by %s",
			attest.ErrInvalid, agent.ID, agentSig.KeyID)
	}
	card, err := json.Marshal(agent.Card)
	if err != nil {
		return fmt.Errorf("marshal agent card: %w", err)
	}
	marketEncoded, err := json.Marshal(marketSig)
	if err != nil {
		return fmt.Errorf("marshal marketplace card attestation: %w", err)
	}
	var agentEncoded sql.NullString
	if agentSig != nil {
		raw, err := json.Marshal(agentSig)
		if err != nil {
			return fmt.Errorf("marshal card attestation: %w", err)
		}
		agentEncoded = sql.NullString{String: string(raw), Valid: true}
	}
	// An upsert rather than an insert-or-update pair, because the caller
	// cannot know which one applies without a race against a concurrent
	// registration, and the row plus its attestation have to agree on which.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO agents (id, name, description, version, url, public_key, card, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   name = excluded.name, description = excluded.description,
		   version = excluded.version, url = excluded.url,
		   public_key = excluded.public_key, card = excluded.card,
		   status = excluded.status, updated_at = excluded.updated_at`,
		agent.ID, agent.Card.Name, agent.Card.Description, agent.Card.Version, agent.Card.URL,
		agent.Card.PublicKey, string(card), string(agent.Status),
		nanos(agent.CreatedAt), nanos(at))
	if err != nil {
		return mapErr(err)
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	// The agent signature is written whole or cleared whole. A partial write
	// here would leave a row claiming a signature over content a reader
	// cannot reproduce, which is worse than no row at all.
	res, err = tx.ExecContext(ctx,
		`INSERT INTO agent_attestations (agent_id, card_signature, card_signed_at, market_signature, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (agent_id) DO UPDATE SET
		   card_signature   = excluded.card_signature,
		   card_signed_at   = excluded.card_signed_at,
		   market_signature = excluded.market_signature,
		   updated_at       = excluded.updated_at`,
		agent.ID, agentEncoded, signedAtOf(agentSig), string(marketEncoded), nanos(at))
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// CreateOfferWithAttestation stores a new offer and the seller's signature over
// its terms in a single transaction.
//
// An unsigned offer has to be as atomic as a signed one, because the window
// matters in both directions: an offer live without the signature the seller
// believes covers it is an unsigned promise, and a signature stored against an
// offer that does not exist is an attestation over nothing.
func (s *Store) CreateOfferWithAttestation(ctx context.Context, o domain.Offer, idempotencyKey string, sig *attest.Signature) error {
	if sig == nil {
		return s.CreateOffer(ctx, o, idempotencyKey)
	}
	// The signature must be the seller's, for the same reason a card's must be the
	// agent's. Whether it covers these exact terms is the registry's business,
	// since only the registry knows the mint's scale; the identity binding is
	// checkable here and has to hold in the row either way.
	if sig.KeyID != o.AgentID {
		return fmt.Errorf("%w: offer attestation for %s was signed by %s, not the seller",
			attest.ErrInvalid, o.ID, sig.KeyID)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := insertOfferRow(ctx, tx, o, idempotencyKey); err != nil {
			return err
		}
		return insertOfferAttestationRow(ctx, tx, o, sig)
	})
}

// MarketplaceSignatureFor returns the marketplace's attestation on its own, for
// the paths that do not need the agent's. It exists so the registry can re-sign
// an existing card without also rewriting the agent's signature it did not read.
func (s *Store) MarketplaceSignatureFor(ctx context.Context, agentID string) (attest.Signature, bool, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT market_signature FROM agent_attestations WHERE agent_id = ?`, agentID).Scan(&encoded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return attest.Signature{}, false, nil
	case err != nil:
		return attest.Signature{}, false, fmt.Errorf("marketplace card attestation: %w", err)
	}
	sig, err := decodeSignature(encoded)
	if err != nil {
		return attest.Signature{}, false, fmt.Errorf("marketplace card attestation for %s is corrupt: %w", agentID, err)
	}
	return sig, true, nil
}

// SaveProbe stores a capability probe result and the marketplace's attestation on
// it, in one transaction.
//
// The result and its signature are one fact for the same reason a card and its
// signature are: a probe that a reader cannot tie to a marketplace key is an
// observation with no provenance, and an observation with no provenance is an
// assertion by whoever put it there.
func (s *Store) SaveProbe(ctx context.Context, report probe.Report, sig attest.Signature, at time.Time) error {
	results, err := json.Marshal(report.Results)
	if err != nil {
		return fmt.Errorf("marshal probe results: %w", err)
	}
	encoded, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("marshal probe attestation: %w", err)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO capability_probes
			   (agent_id, target, results, passed, signature, signed_at, checked_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (agent_id) DO UPDATE SET
			   target     = excluded.target,
			   results    = excluded.results,
			   passed     = excluded.passed,
			   signature  = excluded.signature,
			   signed_at  = excluded.signed_at,
			   checked_at = excluded.checked_at,
			   updated_at = excluded.updated_at`,
			report.AgentID, report.Target, string(results), report.Passed(),
			string(encoded), nanos(sig.SignedAt), nanos(report.CheckedAt), nanos(at))
		return mapErr(err)
	})
}

// Probe returns an agent's last recorded probe, and whether one exists.
//
// A missing row means the agent has never been probed, which is different from a
// row reporting a failure, and a reader has to be able to tell them apart: one
// agent has not been checked and another has been checked and did not pass.
func (s *Store) Probe(ctx context.Context, agentID string) (probe.Report, bool, error) {
	var (
		target, results, encoded string
		passed                   bool
		checkedAt                int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT target, results, passed, signature, checked_at FROM capability_probes WHERE agent_id = ?`,
		agentID).Scan(&target, &results, &passed, &encoded, &checkedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return probe.Report{}, false, nil
	case err != nil:
		return probe.Report{}, false, fmt.Errorf("capability probe for %s: %w", agentID, err)
	}
	report := probe.Report{AgentID: agentID, Target: target, CheckedAt: fromNanos(checkedAt).UTC()}
	if err := json.Unmarshal([]byte(results), &report.Results); err != nil {
		return probe.Report{}, false, fmt.Errorf("probe results for %s are corrupt: %w", agentID, err)
	}
	// The passed column is what a directory filters on and the results are what a
	// human reads, so a disagreement between them is reported rather than resolved
	// in favour of one. A row that says it passed while its own results say
	// otherwise is corrupt, and silently trusting either would hide that.
	if passed != report.Passed() {
		return probe.Report{}, false, fmt.Errorf(
			"probe for %s records a pass its own results do not support", agentID)
	}
	sig, err := decodeSignature(encoded)
	if err != nil {
		return probe.Report{}, false, fmt.Errorf("probe attestation for %s is corrupt: %w", agentID, err)
	}
	report.Signature = &sig
	return report, true, nil
}
