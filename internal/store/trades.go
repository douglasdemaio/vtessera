package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// TradeAcceptance returns when the trade was accepted, which is the anchor for
// its expiry. A trade that was never accepted has no acceptance and therefore no
// deadline: false, not an error, because "not yet accepted" is a state and not a
// fault.
func (s *Store) TradeAcceptance(ctx context.Context, tradeID string) (time.Time, bool, error) {
	// MIN() over zero matching rows still returns one row, with a NULL value, so
	// sql.ErrNoRows never fires here; a NullInt64 is what tells "no acceptance yet"
	// apart from "accepted at nanosecond zero".
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM trade_acceptances WHERE trade_id = ?`, tradeID).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("trade acceptance: %w", err)
	case !at.Valid:
		return time.Time{}, false, nil
	}
	return fromNanos(at.Int64), true, nil
}

// AcceptedBefore returns accepted trades whose acceptance deadline falls before
// the given instant, oldest first. It is the sweeper's query, so it is bounded:
// a sweep that tried to expire every stale trade in one pass would hold a write
// lock for as long as the backlog and the reservations would not be released
// incrementally for the buyers waiting on them.
func (s *Store) AcceptedBefore(ctx context.Context, deadline time.Time, limit int) ([]domain.Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id FROM trades t
		 WHERE t.state = ?
		   AND EXISTS (SELECT 1 FROM trade_acceptances a WHERE a.trade_id = t.id)
		   AND (SELECT MIN(a.created_at) FROM trade_acceptances a WHERE a.trade_id = t.id) <= ?
		 ORDER BY t.updated_at ASC
		 LIMIT ?`,
		string(domain.TradeAccepted), nanos(deadline), limit)
	if err != nil {
		return nil, fmt.Errorf("expiring accepted trades: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("expiring accepted trades scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("expiring accepted trades rows: %w", err)
	}
	// The state predicate above is the index-friendly filter; the rows are read
	// through GetTrade so a caller sees the same shape it gets everywhere else.
	out := make([]domain.Trade, 0, len(ids))
	for _, id := range ids {
		tr, err := s.GetTrade(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
}

func (s *Store) OpenTradesBefore(ctx context.Context, cutoff time.Time, limit int) ([]domain.Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id FROM trades t
		 WHERE t.state IN (?, ?)
		   AND t.updated_at <= ?
		 ORDER BY t.updated_at ASC
		 LIMIT ?`,
		string(domain.TradeProposed), string(domain.TradeNegotiating), nanos(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("expiring open trades: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("expiring open trades scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("expiring open trades rows: %w", err)
	}
	// The state predicate above is the index-friendly filter; the rows are read
	// through GetTrade so a caller sees the same shape it gets everywhere else.
	out := make([]domain.Trade, 0, len(ids))
	for _, id := range ids {
		tr, err := s.GetTrade(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
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

// liveTradeQueryer is the read side a *sql.DB and a *sql.Tx both satisfy.
type liveTradeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// LiveTradesForAgent returns a party's trades that have not reached a terminal
// state, so a retirement can be refused while one is in flight.
//
// The trade table is not deleted on retirement and its rows keep referencing the
// agent, so this is the same query the cap reservation logic uses. It exists for
// the operator rather than the agent: withdrawing a listing is a statement about
// future business, and a buyer holding an open trade against that seller is
// existing business with a counterparty who will not be there to finish it.
func (s *Store) LiveTradesForAgent(ctx context.Context, agentID string) ([]domain.Trade, error) {
	ids, err := s.liveTradeIDs(ctx, s.db, agentID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Trade, 0, len(ids))
	for _, id := range ids {
		tr, err := s.GetTrade(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
}

// liveTradeIDs lists the trades of an agent that have not finished, against
// either handle. Recorded, settled, disputed and cancelled are finished: a
// dispute is finished with the marketplace's involvement, not pending on it.
//
// It takes a queryer rather than the context alone so the retirement path can ask
// the same question inside its own transaction. That matters: an agent accepted
// a trade a moment before it was retired is a buyer waiting on a seller that can
// no longer trade, and a check that ran before the write cannot see it.
func (s *Store) liveTradeIDs(ctx context.Context, q liveTradeQueryer, agentID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id FROM trades
		 WHERE (buyer_agent_id = ? OR seller_agent_id = ?)
		   AND state NOT IN (?, ?, ?, ?)
		 ORDER BY created_at ASC`,
		agentID, agentID,
		string(domain.TradeRecorded), string(domain.TradeSettled),
		string(domain.TradeDisputed), string(domain.TradeCancelled))
	if err != nil {
		return nil, fmt.Errorf("live trades for %s: %w", agentID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("live trades scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("live trades rows: %w", err)
	}
	return ids, nil
}
