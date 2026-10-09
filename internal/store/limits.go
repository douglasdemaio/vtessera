package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// GetAgentLimits returns the caps an agent has explicitly opted in to. A missing
// row is not an error: it means the agent is on the deployment defaults, which
// every agent is until it asks for more.
func (s *Store) GetAgentLimits(ctx context.Context, agentID string) (domain.AgentLimits, error) {
	var out domain.AgentLimits
	var perTrade, perDay string
	var raisedAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id, per_trade_usd, per_day_usd, raised_at FROM agent_limits WHERE agent_id = ?`,
		agentID).Scan(&out.AgentID, &perTrade, &perDay, &raisedAt)
	if err == sql.ErrNoRows {
		return domain.AgentLimits{}, ErrNotFound
	}
	if err != nil {
		return domain.AgentLimits{}, fmt.Errorf("get agent limits: %w", err)
	}
	out.PerTradeUSD, err = money.Parse(perTrade)
	if err != nil {
		return domain.AgentLimits{}, fmt.Errorf("stored per-trade cap %q: %w", perTrade, err)
	}
	out.PerDayUSD, err = money.Parse(perDay)
	if err != nil {
		return domain.AgentLimits{}, fmt.Errorf("stored daily cap %q: %w", perDay, err)
	}
	out.RaisedAt = fromNanos(raisedAt)
	return out, nil
}

// SetAgentLimits records an agent's opt-in, replacing any previous one. The
// ceilings are the policy's business and are checked before this is called, so
// this is the write half of a decision that has already been allowed.
func (s *Store) SetAgentLimits(ctx context.Context, l domain.AgentLimits) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agent_limits (agent_id, per_trade_usd, per_day_usd, raised_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(agent_id) DO UPDATE SET
				per_trade_usd = excluded.per_trade_usd,
				per_day_usd   = excluded.per_day_usd,
				raised_at     = excluded.raised_at`,
			l.AgentID, l.PerTradeUSD.String(), l.PerDayUSD.String(), nanos(l.RaisedAt))
		return mapErr(err)
	})
}

// releasedStates end an engagement without money having moved. A cancelled trade
// releases its reservation, because neither party is left owing anything, and so
// does a resolved dispute: the operator has ended the trade, and the cap bounds
// live commitments rather than recording a verdict, so holding the reservation
// after the review is done would charge the buyer for a trade nobody is in.
var releasedStates = []string{"cancelled", "resolved"}

// committedStates are the trade states that make an engagement irreversible, and
// the transitions into them. They matter for *when* a trade is charged rather than
// whether: a trade that reaches one of these has moved value, or holds something
// the buyer can move value with, and its exposure starts from that moment.
//
// settlement_pending and disputed count even though they are not settled, because
// in both cases value may already have moved: settlement_pending holds a
// transaction the buyer can sign and broadcast at any moment, and a disputed
// on-chain trade's transfer is not reversed.
var committedStates = []string{
	"settlement_pending",
	"recorded",
	"settled",
	"disputed",
}

// CommittedSpendSince returns the amounts this buyer is engaged for since the
// cutoff.
//
// Every trade the buyer still holds a position in is charged, not only the settled
// ones. A proposed or negotiating trade reserves its amount, because the point of
// a daily cap is to bound exposure: a buyer that can open a fifth negotiation
// against a budget it has already spent has no cap at all. Cancelling releases
// the reservation.
//
// The window is anchored on the later of two moments. Before any money can move,
// that is when the trade was opened, so the reservation expires with the
// negotiation. After it, that is when the trade first entered a committed state,
// so a trade left open for a week and settled on Sunday is charged on Sunday and
// not on the day it was proposed. Taking the later of the two means the exposure
// survives until a day after the money actually moved.
//
// Taking MIN over the commit transitions also means a trade is not charged twice
// for one commitment: a trade settled yesterday and disputed today has two commit
// transitions but one exposure.
//
// The trades_buyer_idx index covers the buyer and state columns, which is the
// only access pattern here; the timestamps are not in the index, so a buyer with a
// very long history pays for a scan of their own rows. That is bounded by the
// deployment's own volume rather than by an attacker's, since the buyer is an
// authenticated agent.
// excludeTradeID names one trade to leave out, for a caller that is about to
// account for that trade itself. A settlement re-check passes the trade it is
// about to compile; without this the buyer is charged for it twice and a trade
// that was inside its cap at creation is refused after the seller accepted it.
// No trade has an empty ID, so "" excludes nothing.
func (s *Store) CommittedSpendSince(ctx context.Context, buyerAgentID string, since time.Time, excludeTradeID string) ([]domain.SpendRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT amount, mint FROM (
		     SELECT t.amount AS amount, t.mint AS mint, t.state AS state,
		            MAX(t.created_at, COALESCE(MIN(e.created_at), 0)) AS engaged_at
		     FROM trades t
		     LEFT JOIN trade_events e
		            ON e.trade_id = t.id AND e.to_state IN (?, ?, ?, ?)
		     WHERE t.buyer_agent_id = ? AND t.id != ?
		     GROUP BY t.id
		 )
		 WHERE engaged_at >= ? AND state NOT IN (?, ?)`,
		committedStates[0], committedStates[1], committedStates[2], committedStates[3],
		buyerAgentID,
		excludeTradeID,
		nanos(since),
		releasedStates[0], releasedStates[1])
	if err != nil {
		return nil, fmt.Errorf("committed spend: %w", err)
	}
	defer rows.Close()
	var out []domain.SpendRow
	for rows.Next() {
		var amount, mint string
		if err := rows.Scan(&amount, &mint); err != nil {
			return nil, fmt.Errorf("committed spend scan: %w", err)
		}
		parsed, err := money.Parse(amount)
		if err != nil {
			// A stored amount that will not parse is a data fault, not an
			// agent's. Surfacing it means the cap fails loudly instead of
			// silently counting nothing for this buyer.
			return nil, fmt.Errorf("stored trade amount %q: %w", amount, err)
		}
		out = append(out, domain.SpendRow{Mint: mint, Amount: parsed})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("committed spend rows: %w", err)
	}
	return out, nil
}
