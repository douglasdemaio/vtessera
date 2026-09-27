package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

func (s *Store) CreateTrade(ctx context.Context, t domain.Trade, idempotencyKey string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO trades (id, offer_id, buyer_agent_id, seller_agent_id, description, amount, mint, settlement_mode, state, idempotency_key, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.OfferID, t.BuyerAgentID, t.SellerAgentID, t.Description, t.Amount.String(),
			t.Mint, string(t.SettlementMode), string(t.State), nullString(idempotencyKey), nanos(t.CreatedAt), nanos(t.UpdatedAt))
		return mapErr(err)
	})
}

func (s *Store) GetTrade(ctx context.Context, id string) (domain.Trade, error) {
	row := s.db.QueryRowContext(ctx, tradeSelect+` WHERE id = ?`, id)
	t, err := scanTrade(row)
	if err != nil {
		return domain.Trade{}, err
	}
	acceptances, err := s.tradeAcceptances(ctx, id)
	if err != nil {
		return domain.Trade{}, err
	}
	t.Acceptances = acceptances
	return t, nil
}

func (s *Store) GetTradeByIdempotencyKey(ctx context.Context, key string) (domain.Trade, error) {
	row := s.db.QueryRowContext(ctx, tradeSelect+` WHERE idempotency_key = ?`, key)
	t, err := scanTrade(row)
	if err != nil {
		return domain.Trade{}, err
	}
	acceptances, err := s.tradeAcceptances(ctx, t.ID)
	if err != nil {
		return domain.Trade{}, err
	}
	t.Acceptances = acceptances
	return t, nil
}

func (s *Store) tradeAcceptances(ctx context.Context, tradeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_id FROM trade_acceptances WHERE trade_id = ? ORDER BY created_at, agent_id`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return nil, err
		}
		out = append(out, agentID)
	}
	return out, rows.Err()
}

func (s *Store) AddTradeAcceptance(ctx context.Context, tradeID, agentID string, at time.Time) (bool, error) {
	inserted := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO trade_acceptances (trade_id, agent_id, created_at) VALUES (?, ?, ?)`,
			tradeID, agentID, nanos(at))
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		return nil
	})
	return inserted, err
}

func (s *Store) SetTradeState(ctx context.Context, tradeID string, from, to domain.TradeState, at time.Time) (bool, error) {
	changed := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE trades SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			string(to), nanos(at), tradeID, string(from))
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		return nil
	})
	return changed, err
}

func (s *Store) AppendTradeEvent(ctx context.Context, e domain.TradeEvent) (int64, error) {
	var seq int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO trade_events (trade_id, actor_agent_id, type, from_state, to_state, detail, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.TradeID, e.ActorAgentID, string(e.Type), string(e.FromState), string(e.ToState),
			string(nonNilBytes(e.Detail)), nanos(e.CreatedAt))
		if err != nil {
			return mapErr(err)
		}
		seq, err = res.LastInsertId()
		return err
	})
	return seq, err
}

func (s *Store) ListTradeEvents(ctx context.Context, tradeID string) ([]domain.TradeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, trade_id, actor_agent_id, type, from_state, to_state, detail, created_at
		 FROM trade_events WHERE trade_id = ? ORDER BY seq`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TradeEvent{}
	for rows.Next() {
		var (
			e        domain.TradeEvent
			typ      string
			from, to string
			detail   string
			crea     int64
		)
		if err := rows.Scan(&e.Seq, &e.TradeID, &e.ActorAgentID, &typ, &from, &to, &detail, &crea); err != nil {
			return nil, err
		}
		e.Type = domain.TradeEventType(typ)
		e.FromState = domain.TradeState(from)
		e.ToState = domain.TradeState(to)
		e.Detail = json.RawMessage(detail)
		e.CreatedAt = fromNanos(crea)
		out = append(out, e)
	}
	return out, rows.Err()
}

const tradeSelect = `SELECT id, offer_id, buyer_agent_id, seller_agent_id, description, amount, mint, settlement_mode, state, created_at, updated_at FROM trades`

func scanTrade(row rowScanner) (domain.Trade, error) {
	var (
		t      domain.Trade
		amount string
		mode   string
		state  string
		crea   int64
		upda   int64
	)
	if err := row.Scan(&t.ID, &t.OfferID, &t.BuyerAgentID, &t.SellerAgentID, &t.Description,
		&amount, &t.Mint, &mode, &state, &crea, &upda); err != nil {
		return domain.Trade{}, mapErr(err)
	}
	parsed, err := money.Parse(amount)
	if err != nil {
		return domain.Trade{}, fmt.Errorf("parse stored amount %q: %w", amount, err)
	}
	t.Amount = parsed
	t.SettlementMode = domain.SettlementMode(mode)
	t.State = domain.TradeState(state)
	t.CreatedAt = fromNanos(crea)
	t.UpdatedAt = fromNanos(upda)
	return t, nil
}

func nonNilBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
