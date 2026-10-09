package store

import (
	"context"
	"database/sql"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// A resolved dispute still counts as disputed. The operator's verdict belongs in
// the trade's event log; the public count is a record that a dispute happened,
// and a review that could lower it would let an operator bury the disputes it
// lost. So "disputed" spans both the open state and the resolved one.
const usageTotalsSQL = `
SELECT
  COUNT(r.trade_id) AS delivered,
  COUNT(CASE WHEN t.state IN (?, ?) THEN 1 END) AS disputed,
  COUNT(CASE WHEN t.state = ? THEN 1 END) AS cancelled,
  COUNT(DISTINCT CASE WHEN r.trade_id IS NOT NULL THEN t.buyer_agent_id END) AS consumers,
  COUNT(DISTINCT CASE WHEN r.trade_id IS NOT NULL THEN t.offer_id END) AS services,
  (SELECT MAX(created_at) FROM receipts) AS as_of
FROM trades t
LEFT JOIN receipts r ON r.trade_id = t.id`

const usageByAgentSQL = `
SELECT
  o.agent_id AS agent_id,
  COUNT(r.trade_id) AS delivered,
  COUNT(CASE WHEN t.state IN (?, ?) THEN 1 END) AS disputed,
  COUNT(CASE WHEN t.state = ? THEN 1 END) AS cancelled
FROM trades t
JOIN offers o ON o.id = t.offer_id
LEFT JOIN receipts r ON r.trade_id = t.id
WHERE r.trade_id IS NOT NULL OR t.state IN (?, ?, ?)
GROUP BY o.agent_id
ORDER BY delivered DESC, o.agent_id ASC`

func (s *Store) UsageMetrics(ctx context.Context) (domain.UsageMetrics, error) {
	m := domain.UsageMetrics{Agents: []domain.AgentUsage{}}

	var asOf sql.NullInt64
	err := s.db.QueryRowContext(ctx, usageTotalsSQL,
		string(domain.TradeDisputed), string(domain.TradeResolved), string(domain.TradeCancelled),
	).Scan(&m.Totals.Delivered, &m.Totals.Disputed, &m.Totals.Cancelled,
		&m.Totals.Consumers, &m.Totals.Services, &asOf)
	if err != nil {
		return domain.UsageMetrics{}, err
	}
	if asOf.Valid {
		at := fromNanos(asOf.Int64)
		m.AsOf = &at
	}

	rows, err := s.db.QueryContext(ctx, usageByAgentSQL,
		string(domain.TradeDisputed), string(domain.TradeResolved), string(domain.TradeCancelled),
		string(domain.TradeDisputed), string(domain.TradeResolved), string(domain.TradeCancelled),
	)
	if err != nil {
		return domain.UsageMetrics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var a domain.AgentUsage
		if err := rows.Scan(&a.AgentID, &a.Delivered, &a.Disputed, &a.Cancelled); err != nil {
			return domain.UsageMetrics{}, err
		}
		m.Agents = append(m.Agents, a)
	}
	return m, rows.Err()
}
