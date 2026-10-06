package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

func (s *Store) CreateAgent(ctx context.Context, a domain.Agent) error {
	card, err := json.Marshal(a.Card)
	if err != nil {
		return fmt.Errorf("marshal agent card: %w", err)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agents (id, name, description, version, url, public_key, card, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.Card.Name, a.Card.Description, a.Card.Version, a.Card.URL,
			a.Card.PublicKey, string(card), string(a.Status), nanos(a.CreatedAt), nanos(a.UpdatedAt))
		return mapErr(err)
	})
}

func (s *Store) GetAgent(ctx context.Context, id string) (domain.Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, card, status, created_at, updated_at FROM agents WHERE id = ?`, id)
	return scanAgent(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgent(row rowScanner) (domain.Agent, error) {
	var (
		a    domain.Agent
		card string
		stat string
		crea int64
		upda int64
	)
	if err := row.Scan(&a.ID, &card, &stat, &crea, &upda); err != nil {
		return domain.Agent{}, mapErr(err)
	}
	if err := json.Unmarshal([]byte(card), &a.Card); err != nil {
		return domain.Agent{}, fmt.Errorf("unmarshal agent card: %w", err)
	}
	a.Status = domain.AgentStatus(stat)
	a.CreatedAt = fromNanos(crea)
	a.UpdatedAt = fromNanos(upda)
	return a, nil
}

// ListAgentsByIDs fetches every agent in ids with a single query, keyed by
// agent ID. Missing IDs are simply absent from the result map.
func (s *Store) ListAgentsByIDs(ctx context.Context, ids []string) (map[string]domain.Agent, error) {
	out := make(map[string]domain.Agent, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, card, status, created_at, updated_at FROM agents WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func (s *Store) UpdateAgent(ctx context.Context, a domain.Agent) error {
	card, err := json.Marshal(a.Card)
	if err != nil {
		return fmt.Errorf("marshal agent card: %w", err)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE agents SET name = ?, description = ?, version = ?, url = ?, public_key = ?, card = ?, status = ?, updated_at = ?
			 WHERE id = ?`,
			a.Card.Name, a.Card.Description, a.Card.Version, a.Card.URL, a.Card.PublicKey,
			string(card), string(a.Status), nanos(a.UpdatedAt), a.ID)
		if err != nil {
			return mapErr(err)
		}
		return requireAffected(res)
	})
}

func (s *Store) SetAgentStatus(ctx context.Context, id string, status domain.AgentStatus, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE agents SET status = ?, updated_at = ? WHERE id = ?`, string(status), nanos(at), id)
		if err != nil {
			return mapErr(err)
		}
		return requireAffected(res)
	})
}

func (s *Store) ListAgents(ctx context.Context, status domain.AgentStatus) ([]domain.Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, card, status, created_at, updated_at FROM agents WHERE status = ? ORDER BY created_at, id`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Retirement is an operator withdrawing an agent's listing.
//
// RetireAgent closes the agent's open offers and marks it retired in one
// transaction. The two cannot be allowed to diverge: an agent marked retired
// whose offers are still open is a listing the marketplace has promised to hide
// and has not, and the reverse is a seller whose card vanished while their
// offer kept taking buyers.
//
// Offers in any state other than open are left alone. A closed offer is part of a
// completed trade, and rewriting that history would break the receipts naming it.
//
// Nothing is deleted. Trades, receipts and the registration all stay, so a
// tessera a buyer already holds keeps verifying.
func (s *Store) RetireAgent(ctx context.Context, id, reason, actor string, at time.Time) (domain.Agent, error) {
	err := s.write(ctx, func(tx *sql.Tx) error {
		// Checked here rather than by the caller, in the same transaction as the
		// write. A caller that checked first would have a window: a trade accepted
		// between its check and this statement leaves a buyer holding a trade with
		// a seller that has been withdrawn, which is the exact situation the
		// refusal exists to prevent.
		live, err := s.liveTradeIDs(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(live) > 0 {
			return &LiveTradesError{AgentID: id, TradeIDs: live}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE agents SET status = ?, updated_at = ? WHERE id = ?`,
			string(domain.AgentRetired), nanos(at), id)
		if err != nil {
			return mapErr(err)
		}
		if err := requireAffected(res); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE offers SET status = ?, updated_at = ? WHERE agent_id = ? AND status = ?`,
			string(domain.OfferClosed), nanos(at), id, string(domain.OfferOpen)); err != nil {
			return mapErr(err)
		}
		// The agent_id is the primary key, so retiring an already-retired agent
		// replaces the record rather than accumulating one per attempt. An
		// operator who retires, restores and retires again should not have to
		// reconcile three rows to learn what happened.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_retirements (agent_id, reason, actor, retired_at, restored_at)
			 VALUES (?, ?, ?, ?, NULL)
			 ON CONFLICT (agent_id) DO UPDATE SET
			   reason = excluded.reason, actor = excluded.actor,
			   retired_at = excluded.retired_at, restored_at = NULL`,
			id, reason, actor, nanos(at)); err != nil {
			return mapErr(err)
		}
		return nil
	})
	if err != nil {
		return domain.Agent{}, err
	}
	return s.GetAgent(ctx, id)
}

// RestoreAgent reverses a retirement. It is the reason retirement is a status
// and not a deletion: a listing taken down in error can be put back.
//
// The closed offers are not reopened. Restoring is the operator declaring the
// agent legitimate again, not the agent asking to have its old offers republished
// — an offer may have been withdrawn deliberately, and a buyer who saw it close
// has moved on. The agent republishes if it wants to sell again.
func (s *Store) RestoreAgent(ctx context.Context, id, actor string, at time.Time) (domain.Agent, error) {
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE agents SET status = ?, updated_at = ? WHERE id = ?`,
			string(domain.AgentActive), nanos(at), id)
		if err != nil {
			return mapErr(err)
		}
		if err := requireAffected(res); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_retirements SET restored_at = ?, restored_actor = ?
			 WHERE agent_id = ? AND restored_at IS NULL`,
			nanos(at), actor, id); err != nil {
			return mapErr(err)
		}
		return nil
	})
	if err != nil {
		return domain.Agent{}, err
	}
	return s.GetAgent(ctx, id)
}

// errLiveTrades is the sentinel the live-trade refusal unwraps to. The registry
// re-exports it as ErrAgentHasLiveTrades; the duplication is deliberate, because
// the store cannot import the registry that imports the store.
var errLiveTrades = errors.New("agent has trades that have not reached a terminal state")

// LiveTradesError refuses a retirement while trades of that agent have not
// finished, and names which ones.
//
// It carries the IDs rather than only counting them because the operator holding
// the refusal is the one who can act on it: they have to settle, dispute or let
// expire each trade, and a count tells them how many they are already dealing
// with rather than which.
type LiveTradesError struct {
	AgentID  string
	TradeIDs []string
}

func (e *LiveTradesError) Error() string {
	return fmt.Sprintf("%s has %d live trade(s): %s", e.AgentID, len(e.TradeIDs), strings.Join(e.TradeIDs, ", "))
}

// Unwrap lets errors.Is(err, registry.ErrAgentHasLiveTrades) recognise the
// refusal without the store importing the registry, which would be a cycle.
func (e *LiveTradesError) Unwrap() error {
	return errLiveTrades
}

// Retirement returns the current retirement record for an agent. A retired agent
// always has one, so an operator reading a listing can tell a withdrawal from a
// suspension without inferring it from timestamps.
func (s *Store) Retirement(ctx context.Context, id string) (domain.Retirement, bool, error) {
	var (
		r          domain.Retirement
		reason     string
		actor      string
		retiredAt  int64
		restored   sql.NullInt64
		restoredBy sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id, reason, actor, retired_at, restored_at, restored_actor
		 FROM agent_retirements WHERE agent_id = ?`, id).
		Scan(&r.AgentID, &reason, &actor, &retiredAt, &restored, &restoredBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.Retirement{}, false, nil
	case err != nil:
		return domain.Retirement{}, false, fmt.Errorf("retirement: %w", err)
	}
	r.Reason = reason
	r.Actor = actor
	r.RetiredAt = fromNanos(retiredAt)
	if restored.Valid {
		at := fromNanos(restored.Int64)
		r.RestoredAt = &at
	}
	if restoredBy.Valid {
		r.RestoredBy = restoredBy.String
	}
	return r, true, nil
}
