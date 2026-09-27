package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

func (s *Store) AppendLedgerEntry(ctx context.Context, a domain.LedgerAppend, hashEntry domain.EntryHasher) (domain.LedgerEntry, error) {
	var entry domain.LedgerEntry
	err := s.write(ctx, func(tx *sql.Tx) error {
		head, found, err := s.headLedgerEntry(ctx, tx)
		if err != nil {
			return err
		}
		prevHash := domain.GenesisHash
		var nextSeq int64 = 1
		if found {
			prevHash = head.Hash
			nextSeq = head.Seq + 1
		}
		if prevHash != a.PrevHash {
			return ErrStale
		}
		entry = domain.LedgerEntry{
			Seq:         nextSeq,
			TradeID:     a.TradeID,
			Payload:     a.Payload,
			PayloadHash: a.PayloadHash,
			PrevHash:    prevHash,
			Hash:        hashEntry(nextSeq, prevHash, a.PayloadHash),
			CreatedAt:   a.CreatedAt,
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (seq, trade_id, payload, payload_hash, prev_hash, hash, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			entry.Seq, entry.TradeID, string(entry.Payload), entry.PayloadHash, entry.PrevHash, entry.Hash, nanos(entry.CreatedAt))
		return mapErr(err)
	})
	return entry, err
}

func (s *Store) HeadLedgerEntry(ctx context.Context) (domain.LedgerEntry, bool, error) {
	return s.headLedgerEntry(ctx, nil)
}

func (s *Store) headLedgerEntry(ctx context.Context, tx *sql.Tx) (domain.LedgerEntry, bool, error) {
	query := `SELECT seq, trade_id, payload, payload_hash, prev_hash, hash, created_at FROM ledger_entries ORDER BY seq DESC LIMIT 1`
	var row *sql.Row
	if tx != nil {
		row = tx.QueryRowContext(ctx, query)
	} else {
		row = s.db.QueryRowContext(ctx, query)
	}
	var (
		e       domain.LedgerEntry
		payload string
		crea    int64
	)
	switch err := row.Scan(&e.Seq, &e.TradeID, &payload, &e.PayloadHash, &e.PrevHash, &e.Hash, &crea); {
	case err == sql.ErrNoRows:
		return domain.LedgerEntry{}, false, nil
	case err != nil:
		return domain.LedgerEntry{}, false, err
	}
	e.Payload = []byte(payload)
	e.CreatedAt = fromNanos(crea)
	return e, true, nil
}

func (s *Store) ListLedgerEntries(ctx context.Context) ([]domain.LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, trade_id, payload, payload_hash, prev_hash, hash, created_at FROM ledger_entries ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.LedgerEntry{}
	for rows.Next() {
		var (
			e       domain.LedgerEntry
			payload string
			crea    int64
		)
		if err := rows.Scan(&e.Seq, &e.TradeID, &payload, &e.PayloadHash, &e.PrevHash, &e.Hash, &crea); err != nil {
			return nil, err
		}
		e.Payload = []byte(payload)
		e.CreatedAt = fromNanos(crea)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) SaveReceipt(ctx context.Context, r domain.Receipt) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO receipts (id, trade_id, jws, created_at) VALUES (?, ?, ?, ?)`,
			r.ID, r.TradeID, r.JWS, nanos(r.IssuedAt))
		return mapErr(err)
	})
}

func (s *Store) GetReceiptByTrade(ctx context.Context, tradeID string) (domain.Receipt, error) {
	var (
		r    domain.Receipt
		crea int64
	)
	row := s.db.QueryRowContext(ctx, `SELECT id, trade_id, jws, created_at FROM receipts WHERE trade_id = ?`, tradeID)
	if err := row.Scan(&r.ID, &r.TradeID, &r.JWS, &crea); err != nil {
		return domain.Receipt{}, mapErr(err)
	}
	r.IssuedAt = fromNanos(crea)
	return r, nil
}

func (s *Store) CreateChallenge(ctx context.Context, c domain.Challenge) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO auth_challenges (id, agent_id, nonce, expires_at) VALUES (?, ?, ?, ?)`,
			c.ID, c.AgentID, c.Nonce, nanos(c.ExpiresAt))
		return mapErr(err)
	})
}

func (s *Store) ConsumeChallenge(ctx context.Context, id string, at time.Time) (domain.Challenge, error) {
	var c domain.Challenge
	err := s.write(ctx, func(tx *sql.Tx) error {
		var (
			nonce     string
			expiresAt int64
			consumed  sql.NullInt64
		)
		row := tx.QueryRowContext(ctx,
			`SELECT id, agent_id, nonce, expires_at, consumed_at FROM auth_challenges WHERE id = ?`, id)
		switch err := row.Scan(&c.ID, &c.AgentID, &nonce, &expiresAt, &consumed); {
		case err == sql.ErrNoRows:
			return ErrNotFound
		case err != nil:
			return err
		}
		c.Nonce = nonce
		c.ExpiresAt = fromNanos(expiresAt)
		if consumed.Valid {
			consumedAt := fromNanos(consumed.Int64)
			c.Consumed = &consumedAt
			return ErrStale
		}
		if !c.ExpiresAt.After(at) {
			return ErrStale
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE auth_challenges SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`, nanos(at), id)
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrStale
		}
		consumedAt := at.UTC()
		c.Consumed = &consumedAt
		return nil
	})
	return c, err
}
