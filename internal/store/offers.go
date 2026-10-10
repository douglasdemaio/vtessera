package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

const maxOfferLimit = 200

func (s *Store) CreateOffer(ctx context.Context, o domain.Offer, idempotencyKey string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return insertOfferRow(ctx, tx, o, idempotencyKey)
	})
}

// insertOfferRow writes the offer row inside a caller-supplied transaction, so
// an offer can be stored as part of a larger fact (see OnboardAgent) without a
// second copy of this INSERT drifting from the schema. It does not write the
// offer's attestation; that is insertOfferAttestationRow's job, and the two are
// always issued against the same transaction by every caller.
func insertOfferRow(ctx context.Context, tx *sql.Tx, o domain.Offer, idempotencyKey string) error {
	caps, err := json.Marshal(nonNil(o.Capabilities))
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	modes, err := json.Marshal(nonNil(o.SettlementModes))
	if err != nil {
		return fmt.Errorf("marshal settlement modes: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO offers (id, agent_id, direction, description, capabilities, price_amount, price_mint, settlement_modes, status, idempotency_key, created_at, updated_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.AgentID, string(o.Direction), o.Description, string(caps), o.PriceAmount.String(),
		o.PriceMint, string(modes), string(o.Status), nullString(idempotencyKey), nanos(o.CreatedAt), nanos(o.UpdatedAt), expiryColumn(o.ExpiresAt))
	return mapErr(err)
}

// insertOfferAttestationRow writes the seller's signature over an offer's terms
// inside a caller-supplied transaction. A signature stored against an offer that
// does not exist is an attestation over nothing, so every caller writes both in
// the same transaction.
func insertOfferAttestationRow(ctx context.Context, tx *sql.Tx, o domain.Offer, sig *attest.Signature) error {
	encoded, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("marshal offer attestation: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO offer_attestations (offer_id, agent_id, signature, signed_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (offer_id) DO UPDATE SET
		   signature = excluded.signature, signed_at = excluded.signed_at`,
		o.ID, o.AgentID, string(encoded), nanos(sig.SignedAt))
	return mapErr(err)
}

func (s *Store) GetOffer(ctx context.Context, id string) (domain.Offer, error) {
	row := s.db.QueryRowContext(ctx, offerSelect+` WHERE id = ?`, id)
	return scanOffer(row)
}

func (s *Store) SearchOffers(ctx context.Context, q domain.OfferQuery) ([]domain.Offer, error) {
	var (
		where []string
		args  []any
	)
	if q.Status == "" {
		q.Status = domain.OfferOpen
	}
	where = append(where, "status = ?")
	args = append(args, string(q.Status))
	if q.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, q.AgentID)
	}
	if q.ExcludeAgent != "" {
		where = append(where, "agent_id <> ?")
		args = append(args, q.ExcludeAgent)
	}
	if q.Direction != "" {
		where = append(where, "direction = ?")
		args = append(args, string(q.Direction))
	}
	if q.Mint != "" {
		where = append(where, "price_mint = ?")
		args = append(args, q.Mint)
	}
	if q.Capability != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(capabilities) WHERE json_each.value = ?)")
		args = append(args, q.Capability)
	}
	if q.Text != "" {
		where = append(where, "description LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(q.Text)+"%")
	}
	if q.Mode != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(settlement_modes) WHERE json_each.value = ?)")
		args = append(args, string(q.Mode))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 || limit > maxOfferLimit {
		limit = maxOfferLimit
	}
	query := offerSelect + clause + " ORDER BY created_at, id LIMIT ? OFFSET ?"
	args = append(args, limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Offer{}
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) ListOpenOffers(ctx context.Context) ([]domain.Offer, error) {
	return s.SearchOffers(ctx, domain.OfferQuery{Status: domain.OfferOpen})
}

// OpenOffersBefore returns open offers whose deadline has passed, oldest first.
// It is the read side of the offer sweep: the registry closes each one with
// SetOfferStatus after re-checking that it is still open, because a seller can
// close a listing between the batch being read and the close being applied.
func (s *Store) OpenOffersBefore(ctx context.Context, cutoff time.Time, limit int) ([]domain.Offer, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		offerSelect+` WHERE status = ? AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, id LIMIT ?`,
		string(domain.OfferOpen), nanos(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Offer{}
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) SetOfferStatus(ctx context.Context, id string, status domain.OfferStatus, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE offers SET status = ?, updated_at = ? WHERE id = ?`, string(status), nanos(at), id)
		if err != nil {
			return mapErr(err)
		}
		return requireAffected(res)
	})
}

func (s *Store) GetOfferByIdempotencyKey(ctx context.Context, key string) (domain.Offer, error) {
	row := s.db.QueryRowContext(ctx, offerSelect+` WHERE idempotency_key = ?`, key)
	return scanOffer(row)
}

const offerSelect = `SELECT id, agent_id, direction, description, capabilities, price_amount, price_mint, settlement_modes, status, created_at, updated_at, expires_at FROM offers`

func scanOffer(row rowScanner) (domain.Offer, error) {
	var (
		o       domain.Offer
		dir     string
		caps    string
		amount  string
		modes   string
		status  string
		crea    int64
		upda    int64
		expires sql.NullInt64
	)
	if err := row.Scan(&o.ID, &o.AgentID, &dir, &o.Description, &caps, &amount, &o.PriceMint, &modes, &status, &crea, &upda, &expires); err != nil {
		return domain.Offer{}, mapErr(err)
	}
	parsed, err := money.Parse(amount)
	if err != nil {
		return domain.Offer{}, fmt.Errorf("parse stored amount %q: %w", amount, err)
	}
	if err := json.Unmarshal([]byte(caps), &o.Capabilities); err != nil {
		return domain.Offer{}, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	if err := json.Unmarshal([]byte(modes), &o.SettlementModes); err != nil {
		return domain.Offer{}, fmt.Errorf("unmarshal settlement modes: %w", err)
	}
	o.Direction = domain.OfferDirection(dir)
	o.PriceAmount = parsed
	o.Status = domain.OfferStatus(status)
	o.CreatedAt = fromNanos(crea)
	o.UpdatedAt = fromNanos(upda)
	if expires.Valid {
		o.ExpiresAt = fromNanos(expires.Int64)
	}
	return o, nil
}

var likeEscaper = strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")

func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// expiryColumn maps a zero deadline to SQL NULL. An offer with no configured
// deadline must not be stored as the zero time's nanosecond value, which is a
// time in the distant past that a later deployment with a positive TTL would
// sweep on its first tick.
func expiryColumn(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return nanos(t)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
