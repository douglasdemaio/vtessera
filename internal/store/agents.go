package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
