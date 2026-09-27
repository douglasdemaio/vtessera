CREATE TABLE IF NOT EXISTS agents (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  description  TEXT NOT NULL DEFAULT '',
  version      TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',
  public_key   TEXT NOT NULL,
  card         TEXT NOT NULL,
  status       TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS agents_status_idx ON agents (status);

CREATE TABLE IF NOT EXISTS offers (
  id               TEXT PRIMARY KEY,
  agent_id         TEXT NOT NULL REFERENCES agents (id),
  direction        TEXT NOT NULL,
  description      TEXT NOT NULL,
  capabilities     TEXT NOT NULL DEFAULT '[]',
  price_amount     TEXT NOT NULL,
  price_mint       TEXT NOT NULL,
  settlement_modes TEXT NOT NULL,
  status           TEXT NOT NULL,
  idempotency_key  TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS offers_idempotency_idx ON offers (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS offers_agent_idx ON offers (agent_id, status);
CREATE INDEX IF NOT EXISTS offers_discovery_idx ON offers (direction, status, price_mint);

CREATE TABLE IF NOT EXISTS trades (
  id               TEXT PRIMARY KEY,
  offer_id         TEXT NOT NULL REFERENCES offers (id),
  buyer_agent_id   TEXT NOT NULL REFERENCES agents (id),
  seller_agent_id  TEXT NOT NULL REFERENCES agents (id),
  description      TEXT NOT NULL,
  amount           TEXT NOT NULL,
  mint             TEXT NOT NULL,
  settlement_mode  TEXT NOT NULL,
  state            TEXT NOT NULL,
  idempotency_key  TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS trades_idempotency_idx ON trades (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS trades_buyer_idx ON trades (buyer_agent_id, state);
CREATE INDEX IF NOT EXISTS trades_seller_idx ON trades (seller_agent_id, state);
CREATE INDEX IF NOT EXISTS trades_offer_idx ON trades (offer_id);

CREATE TABLE IF NOT EXISTS trade_acceptances (
  trade_id   TEXT NOT NULL REFERENCES trades (id),
  agent_id   TEXT NOT NULL REFERENCES agents (id),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (trade_id, agent_id)
);

CREATE TABLE IF NOT EXISTS trade_events (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  trade_id      TEXT NOT NULL REFERENCES trades (id),
  actor_agent_id TEXT NOT NULL,
  type          TEXT NOT NULL,
  from_state    TEXT NOT NULL,
  to_state      TEXT NOT NULL,
  detail        TEXT NOT NULL DEFAULT '{}',
  created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS trade_events_trade_idx ON trade_events (trade_id, seq);

CREATE TABLE IF NOT EXISTS settlement_requests (
  id            TEXT PRIMARY KEY,
  trade_id      TEXT NOT NULL REFERENCES trades (id),
  unsigned_tx   TEXT NOT NULL,
  blockhash     TEXT NOT NULL,
  last_valid    INTEGER NOT NULL,
  buyer_ata     TEXT NOT NULL,
  seller_ata    TEXT NOT NULL,
  created_ata   INTEGER NOT NULL DEFAULT 0,
  fee_lamports  INTEGER NOT NULL,
  fee_wallet    TEXT NOT NULL,
  status        TEXT NOT NULL,
  signature     TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS settlement_requests_live_idx ON settlement_requests (trade_id) WHERE status = 'issued';
CREATE INDEX IF NOT EXISTS settlement_requests_trade_idx ON settlement_requests (trade_id, created_at);
CREATE INDEX IF NOT EXISTS settlement_requests_signature_idx ON settlement_requests (signature) WHERE signature IS NOT NULL;

CREATE TABLE IF NOT EXISTS ledger_entries (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,
  trade_id     TEXT NOT NULL,
  payload      TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  prev_hash    TEXT NOT NULL,
  hash         TEXT NOT NULL,
  created_at   INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS ledger_trade_idx ON ledger_entries (trade_id);

CREATE TABLE IF NOT EXISTS receipts (
  id         TEXT PRIMARY KEY,
  trade_id   TEXT NOT NULL UNIQUE,
  jws        TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_challenges (
  id         TEXT PRIMARY KEY,
  agent_id   TEXT NOT NULL,
  nonce      TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  consumed_at INTEGER
);
