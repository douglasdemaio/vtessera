package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = domain.ErrNotFound
	ErrConflict = domain.ErrConflict
	ErrStale    = domain.ErrStale
)

//go:embed schema.sql
var schemaSQL string

// migration is one ordered, versioned schema change. schema.sql stays the
// idempotent base schema and every migration runs in its own transaction and
// records its version, so a deployment that already has the base schema still
// gets the columns added to it. Re-opening is a no-op.
type migration struct {
	version int
	name    string
	apply   func(ctx context.Context, tx *sql.Tx) error
}

// migrations is append-only. Renumbering an applied migration would re-run it.
var migrations = []migration{
	{
		version: 1,
		name:    "settlement_requests_cluster",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// Records the cluster whose mint set compiled the request's frozen
			// terms — the chain the request is valid for, not where it was
			// observed to land, which was never captured.
			//
			// The backfill is 'pre-phase-3' and not 'mainnet-beta' on purpose.
			// Labelling legacy rows mainnet would make the mismatch guard
			// silent exactly where it matters most, and would assert an
			// execution context that was never observed: those terms were
			// compiled with a flat registry naming mainnet's USDC regardless of
			// the chain the request was built against. The sentinel says what
			// is actually known, which is nothing, and it is not a declarable
			// Cluster, so a pre-Phase-3 request can never be confirmed by
			// accident.
			_, err := tx.ExecContext(ctx,
				`ALTER TABLE settlement_requests ADD COLUMN cluster TEXT NOT NULL DEFAULT 'pre-phase-3'`)
			return err
		},
	},
	{
		version: 2,
		name:    "agent_limits",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// An agent's own opt-in to a higher spending cap. The defaults live
			// in configuration, so an agent with no row here is on the
			// deployment's default caps and this table stays empty until somebody
			// asks for more.
			//
			// Amounts are TEXT decimal strings for the same reason trades.amount
			// is: a cap is an exact figure an operator will read, and storing it
			// as a float would make the stored value differ from the configured
			// one.
			_, err := tx.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS agent_limits (
					agent_id        TEXT PRIMARY KEY REFERENCES agents (id),
					per_trade_usd   TEXT NOT NULL,
					per_day_usd     TEXT NOT NULL,
					raised_at       INTEGER NOT NULL
				)`)
			return err
		},
	},
}

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		dsn = "file::memory:"
	}
	memory := strings.HasPrefix(dsn, "file::memory:") || dsn == ":memory:"

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pragmas := []string{"foreign_keys(1)", "busy_timeout(5000)"}
	if !memory {
		pragmas = append(pragmas, "journal_mode(WAL)", "synchronous(NORMAL)")
	}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}

	db, err := sql.Open("sqlite", dsn+sep+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if memory {
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range migrations {
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check migration %d: %w", m.version, err)
	}
	if exists > 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		// Re-check inside the transaction: two processes opening the same volume
		// at once can both pass the check above, and the loser must not apply
		// the same ALTER TABLE twice.
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return nil
		}
		if err := m.apply(ctx, tx); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func nanos(t time.Time) int64 {
	return t.UTC().UnixNano()
}

func fromNanos(n int64) time.Time {
	return time.Unix(0, n).UTC()
}

func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
