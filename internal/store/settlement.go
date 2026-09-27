package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// CreateSettlementRequest persists a freshly issued unsigned transaction and
// supersedes any previous live request for the trade in the same transaction,
// so a trade can never hold two live requests at once. A replacement is how a
// buyer recovers from an expired blockhash (design spec §6.4).
func (s *Store) CreateSettlementRequest(ctx context.Context, r domain.SettlementRequest) (domain.SettlementRequest, error) {
	if r.ID == "" {
		return domain.SettlementRequest{}, errors.New("settlement request id is required")
	}
	if r.Status == "" {
		r.Status = domain.SettlementIssued
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE settlement_requests SET status = ?, updated_at = ?
			 WHERE trade_id = ? AND status = ?`,
			string(domain.SettlementExpired), nanos(r.CreatedAt), r.TradeID, string(domain.SettlementIssued)); err != nil {
			return mapErr(err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO settlement_requests (id, trade_id, unsigned_tx, blockhash, last_valid, buyer_ata, seller_ata, created_ata, fee_lamports, fee_wallet, status, signature, created_at, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.TradeID, r.UnsignedTx, r.Blockhash, r.LastValid, r.BuyerATA, r.SellerATA,
			boolInt(r.CreatedATA), r.FeeLamports, r.FeeWallet, string(r.Status), nullString(r.Signature),
			nanos(r.CreatedAt), nanos(r.UpdatedAt), nanos(r.ExpiresAt))
		return mapErr(err)
	})
	return r, err
}

// GetSettlementRequest returns the live request for a trade, falling back to the
// most recently issued one. It returns ErrNotFound when the trade has never had
// a request.
func (s *Store) GetSettlementRequest(ctx context.Context, tradeID string) (domain.SettlementRequest, error) {
	row := s.db.QueryRowContext(ctx, settlementSelect+`
		WHERE trade_id = ?
		ORDER BY (status = ?) DESC, created_at DESC, id DESC
		LIMIT 1`, tradeID, string(domain.SettlementIssued))
	return scanSettlementRequest(row)
}

// LiveSettlementRequest returns the trade's unexpired request, or ErrNotFound
// when the blockhash has lapsed and the trade may be cancelled or reissued.
func (s *Store) LiveSettlementRequest(ctx context.Context, tradeID string, now time.Time) (domain.SettlementRequest, error) {
	row := s.db.QueryRowContext(ctx, settlementSelect+`
		WHERE trade_id = ? AND status = ? AND expires_at > ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, tradeID, string(domain.SettlementIssued), nanos(now))
	return scanSettlementRequest(row)
}

// ListSettlementRequests returns every request issued for a trade, oldest
// first, so an operator auditing a dispute can see every transaction offered.
func (s *Store) ListSettlementRequests(ctx context.Context, tradeID string) ([]domain.SettlementRequest, error) {
	rows, err := s.db.QueryContext(ctx, settlementSelect+` WHERE trade_id = ? ORDER BY created_at, id`, tradeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SettlementRequest{}
	for rows.Next() {
		request, err := scanSettlementRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, request)
	}
	return out, rows.Err()
}

// SetSettlementSignature records the signature a client reported submitting, so
// reconciliation can re-poll it even if the buyer never comes back (spec §6.4).
func (s *Store) SetSettlementSignature(ctx context.Context, id, signature string, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE settlement_requests SET signature = ?, updated_at = ?
			 WHERE id = ? AND status = ? AND (signature IS NULL OR signature = ?)`,
			signature, nanos(at), id, string(domain.SettlementIssued), signature)
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Either the request is gone or a different signature was already
			// recorded; refuse to overwrite the audit trail.
			return fmt.Errorf("%w: settlement request %s is not an open request for signature %s",
				ErrStale, id, signature)
		}
		return nil
	})
}

// ConfirmSettlementRequest marks the request that settled the trade, naming the
// signature that proved it.
func (s *Store) ConfirmSettlementRequest(ctx context.Context, id, signature string, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE settlement_requests SET status = ?, signature = ?, updated_at = ?
			 WHERE id = ? AND status = ?`,
			string(domain.SettlementConfirmed), signature, nanos(at), id, string(domain.SettlementIssued))
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: settlement request %s is not issued", ErrStale, id)
		}
		return nil
	})
}

// ExpireSettlementRequests retires every live request for a trade, for example
// after a failed on-chain execution that spent the blockhash.
func (s *Store) ExpireSettlementRequests(ctx context.Context, tradeID string, at time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE settlement_requests SET status = ?, updated_at = ?
			 WHERE trade_id = ? AND status = ?`,
			string(domain.SettlementExpired), nanos(at), tradeID, string(domain.SettlementIssued))
		return mapErr(err)
	})
}

// TradesInState returns every trade in a state, used by the reconciliation
// worker to re-poll signatures for trades stuck in settlement_pending.
func (s *Store) TradesInState(ctx context.Context, state domain.TradeState) ([]domain.Trade, error) {
	rows, err := s.db.QueryContext(ctx, tradeSelect+` WHERE state = ? ORDER BY updated_at, id`, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Trade{}
	for rows.Next() {
		tr, err := scanTrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

const settlementSelect = `SELECT id, trade_id, unsigned_tx, blockhash, last_valid, buyer_ata, seller_ata, created_ata, fee_lamports, fee_wallet, status, signature, created_at, updated_at, expires_at FROM settlement_requests`

func scanSettlementRequest(row rowScanner) (domain.SettlementRequest, error) {
	var (
		r          domain.SettlementRequest
		status     string
		signature  sql.NullString
		created    int64
		updated    int64
		expires    int64
		createdAta int
	)
	if err := row.Scan(&r.ID, &r.TradeID, &r.UnsignedTx, &r.Blockhash, &r.LastValid, &r.BuyerATA,
		&r.SellerATA, &createdAta, &r.FeeLamports, &r.FeeWallet, &status, &signature,
		&created, &updated, &expires); err != nil {
		return domain.SettlementRequest{}, mapErr(err)
	}
	r.Status = domain.SettlementStatus(status)
	r.Signature = signature.String
	r.CreatedATA = createdAta != 0
	r.CreatedAt = fromNanos(created)
	r.UpdatedAt = fromNanos(updated)
	r.ExpiresAt = fromNanos(expires)
	return r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
