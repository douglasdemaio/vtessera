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
	{
		version: 3,
		name:    "agent_retirements",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// An operator withdrawing an agent's listing is the one action on this
			// marketplace that a principal did not ask for, so it is recorded
			// rather than inferred from the agent row's status. A row here is the
			// answer to "who took this seller's listing and why", which the agents
			// table alone cannot give: status says that it happened, not who or
			// on what grounds.
			// restored_actor is recorded separately from actor because the two are
			// different people on the common case: the operator who withdrew a
			// listing and the operator who put it back. One column for both would
			// mean either that a restore left the retirement looking retired by the
			// person who undid it, or that restoring overwrote who withdrew it.
			_, err := tx.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS agent_retirements (
					agent_id       TEXT PRIMARY KEY REFERENCES agents (id),
					reason         TEXT NOT NULL,
					actor          TEXT NOT NULL,
					retired_at     INTEGER NOT NULL,
					restored_at    INTEGER,
					restored_actor TEXT
				)`)
			return err
		},
	},
	{
		version: 4,
		name:    "agent_attestations",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// An agent's card and its offers are self-describing: until now
			// nothing proved who was behind a listing. This table holds detached
			// Ed25519 signatures over a canonical encoding of the content itself,
			// which a third party can verify with no call to this service at all.
			//
			// There are two signatures and they answer different questions.
			// market_signature is the marketplace's own, over the card it just
			// accepted: it says this listing came from this deployment and has not
			// changed since, which is what a directory needs in order to say which
			// marketplace vouched for a card. card_signature is the agent's,
			// saying the agent itself stands behind its own claims — a capability
			// list is a promise only the agent can make, and the marketplace
			// signing it would say nothing except that the marketplace stored it.
			//
			// card_signature is nullable and is absent for every agent registered
			// before this migration. An unsigned card is not an invalid card, so
			// nothing here refuses to start: the column answers whether the agent's
			// own attestation exists, and a verifier decides what an absent one
			// means. market_signature is NOT nullable, because a card on this
			// service that the marketplace did not attest is not a card this
			// marketplace published.
			//
			// The signed bytes are a canonical encoding, not the JSON stored in
			// card, so these columns cannot be a substitute for the content and
			// rewriting the JSON without re-signing leaves the pair disagreeing
			// visibly rather than silently.
			_, err := tx.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS agent_attestations (
					agent_id         TEXT PRIMARY KEY REFERENCES agents (id),
					card_signature   TEXT,
					card_signed_at   INTEGER,
					market_signature TEXT NOT NULL,
					updated_at       INTEGER NOT NULL
				)`)
			if err != nil {
				return err
			}
			// An offer's terms are a promise to a buyer, so the seller signs them
			// and the signature travels with the offer row. One agent has many
			// offers, so this is keyed per offer and the agent is a lookup column.
			// There is no marketplace column here: the service does not speak for
			// what a seller charges.
			//
			// ON DELETE CASCADE cannot fire on this table as written: nothing
			// deletes an offer. It is here because the foreign key is the honest
			// description of the relationship, and SQLite only enforces it when
			// declared.
			_, err = tx.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS offer_attestations (
					offer_id       TEXT PRIMARY KEY REFERENCES offers (id),
					agent_id       TEXT NOT NULL REFERENCES agents (id),
					signature      TEXT NOT NULL,
					signed_at      INTEGER NOT NULL
				)`)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`CREATE INDEX IF NOT EXISTS offer_attestations_agent
				 ON offer_attestations (agent_id)`)
			return err
		},
	},
	{
		version: 5,
		name:    "capability_probes",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// One row per agent per probe, holding the last observation rather than
			// the history. A probe answers "does this agent still do what it says",
			// and a directory that can be shown any older answer is being offered a
			// claim about when the agent was last checked, which is not the question.
			// The signed_at column is when the marketplace attested this result, so
			// a stale row is visible as a stale row.
			_, err := tx.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS capability_probes (
					agent_id        TEXT PRIMARY KEY REFERENCES agents (id),
					target          TEXT NOT NULL,
					results         TEXT NOT NULL,
					passed          INTEGER NOT NULL,
					signature       TEXT NOT NULL,
					signed_at       INTEGER NOT NULL,
					checked_at      INTEGER NOT NULL,
					updated_at      INTEGER NOT NULL
				)`)
			if err != nil {
				return err
			}
			return nil
		},
	},
	{
		version: 6,
		name:    "offer_expiry",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// A listing with no deadline is a listing a silent seller leaves on
			// the board forever. The column is the deadline the sweeper enforces;
			// it is NOT part of the attested offer bytes, because it is the
			// marketplace's policy rather than one of the seller's terms, and a
			// signature over it would make a policy change invalidate listings
			// that never changed.
			//
			// Existing rows are backfilled from their creation time with the
			// historical default of 24h, the same default the flag ships. That is
			// deliberate and not silently extended: a listing older than a day at
			// the moment this migration runs is exactly the stale listing this
			// exists to remove, and it expires on the next sweep. An operator who
			// wants a longer grace period for legacy rows should export and
			// republish them.
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE offers ADD COLUMN expires_at INTEGER`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE offers SET expires_at = created_at + ? WHERE expires_at IS NULL`,
				int64(24*time.Hour)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`CREATE INDEX IF NOT EXISTS offers_expiry_idx ON offers (status, expires_at)`)
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
