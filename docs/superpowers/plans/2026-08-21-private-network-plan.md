# Implementation Plan: Private/Enterprise Network Mode

> **Spec:** `docs/superpowers/specs/2026-08-21-private-network-design.md`
> **Issue:** [#52](https://github.com/douglasdemaio/vtessera/issues/52)
> **Date:** 2026-08-21

## Overview

Implement private/enterprise network mode for vtesserad, including CIDR
validation, key registry, marketplace target routing, a reference server,
and an interactive config wizard. All new code; no breaking changes to
existing public-mode behavior.

## Phase 1: Core daemon changes (vtesserad)

### 1.1 CIDR parser (`crates/vtesserad/src/cidr.rs`) — NEW FILE

**Goal:** Hand-rolled CIDR parser, no new deps.

- `IpNet` struct: `{ addr: Ipv4Addr, prefix_len: u8 }`
- `parse_cidr(s: &str) -> Result<IpNet, CidrError>` — parse `"x.x.x.x/N"`
- `ip_in_cidrs(ip: Ipv4Addr, cidrs: &[IpNet]) -> bool` — mask + compare
- `parse_cidr_list(ss: &[String]) -> Result<Vec<IpNet>, CidrError>` — batch parse
- Unit tests: valid CIDRs, invalid octets, prefix > 32, empty list, matching

**Estimate:** ~80 lines of code + ~60 lines of tests.

### 1.2 Key registry (`crates/vtesserad/src/key_registry.rs`) — NEW FILE

**Goal:** Load and query a TOML file of allowed Ed25519 public keys.

- `KeyRegistry` struct with `keys: Vec<AllowedKey>`
- `AllowedKey { name: String, pubkey: Pubkey }`
- `KeyRegistry::load(path: &str) -> Result<Self, ConfigError>`
- `KeyRegistry::contains(&self, pubkey: &Pubkey) -> bool`
- Unit tests: load valid, load missing file, load invalid TOML, contains hit/miss

**Estimate:** ~60 lines of code + ~50 lines of tests.

### 1.3 Config additions (`crates/vtesserad/src/config.rs`)

**Goal:** Add `NetworkConfig`, `MarketplaceConfig`, extend `Config::validate()`.

- Add structs:
  ```rust
  #[derive(Debug, Deserialize, Default)]
  pub struct NetworkConfig {
      pub mode: String,                    // "public" | "private"
      pub allowed_cidrs: Vec<String>,      // CIDR strings
      pub require_internal_ca: bool,
      pub key_registry_path: Option<String>,
  }

  #[derive(Debug, Deserialize, Default)]
  pub struct MarketplaceConfig {
      pub target: String,                  // "public" | "internal" | "none"
      pub endpoint: Option<String>,
  }
  ```
- Add to `Config`:
  ```rust
  #[serde(default)]
  pub network: NetworkConfig,
  #[serde(default)]
  pub marketplace: MarketplaceConfig,
  ```
- Extend `validate()`:
  - If `network.mode == "private"`, validate marketplace section is present.
  - If `marketplace.target == "internal"`, validate `endpoint` is set.
  - If `network.require_internal_ca`, validate `key_registry_path` is set and file exists.
  - Parse all `network.allowed_cidrs` entries; reject invalid CIDRs.
  - Validate `marketplace.target` is one of `"public"`, `"internal"`, `"none"`.
- Backward compat: `submit_endpoint` still works when `marketplace.target = "public"`.

**Estimate:** ~80 lines of new code + ~40 lines of new tests.

### 1.4 Submission routing (`crates/vtesserad/src/main.rs`)

**Goal:** Route receipt submission based on `marketplace.target`.

- Add `route_submission(signed: &SignedReceipt, config: &Config, key_registry: Option<&KeyRegistry>)` function.
- Match on `config.marketplace.target`:
  - `"public"` → existing `submit_receipt()` path (use `marketplace.endpoint` or `submit_endpoint`).
  - `"internal"` → check key registry if `require_internal_ca`, then `submit_to_internal()` (same signature as `submit_receipt()`).
  - `"none"` → log debug, return.
- Add key registry loading at startup: if `require_internal_ca`, load from `key_registry_path`.
- Add CIDR warning at startup when `mode = "private"` and endpoint is outside allowed CIDRs.
- Wire into the sampling loop where `submit_receipt` is currently called.

**Estimate:** ~50 lines of new code.

### 1.5 `submit_to_internal()` (`crates/vtesserad/src/submit.rs`)

**Goal:** Same as `submit_receipt()` but named for clarity; identical logic.

- `pub fn submit_to_internal(endpoint: &str, sr: &SignedReceipt) -> Result<(), SubmitError>`
- Body is identical to `submit_receipt()` — the format is the same.
- Alternatively, just make `submit_receipt()` public and reuse it. The distinction is in the routing, not the function.

**Decision:** Reuse `submit_receipt()` directly. No new function needed; the routing logic in `main.rs` selects the endpoint URL. This is simpler and avoids code duplication.

**Estimate:** 0 lines (no change needed).

---

## Phase 2: Reference marketplace server

### 2.1 New crate scaffolding (`crates/marketplace-server/`)

**Goal:** Axum-based HTTP server that receives and stores signed receipts.

- `Cargo.toml`: axum, tokio (full), serde, serde_json, ed25519-dalek, sha2, uuid, toml
- `src/main.rs`: server entry point, config loading, router setup
- `src/receipt_store.rs`: JSON lines file storage
- `src/config.rs`: server config struct

### 2.2 Receipt store (`crates/marketplace-server/src/receipt_store.rs`)

**Goal:** Append-only JSON lines file storage.

- `ReceiptStore` struct with `path: PathBuf`
- `ReceiptStore::new(path: &str) -> Self`
- `ReceiptStore::store(&self, sr: &SignedReceipt) -> Result<String, StoreError>`
  - Validate signature against key registry.
  - Check for duplicate (same `node_id` + `window_start`).
  - Append JSON line to file.
  - Return UUID.
- `ReceiptStore::list(&self, node_id: Option<&str>, since: Option<u64>) -> Vec<SignedReceipt>`
- Unit tests: store, list, duplicate detection, signature validation.

### 2.3 HTTP handlers (`crates/marketplace-server/src/main.rs`)

**Goal:** REST API for receipt submission and listing.

- `POST /api/v1/receipts` — deserialize `SignedReceipt`, validate, store, return 201.
- `GET /api/v1/receipts` — list receipts with optional filters.
- `GET /api/v1/health` — return 200.
- Error handling: 400 (bad request), 403 (unknown key), 409 (duplicate).

### 2.4 Config (`crates/marketplace-server/src/config.rs`)

**Goal:** Server config loading.

```rust
pub struct ServerConfig {
    pub listen_addr: String,           // "0.0.0.0:8443"
    pub key_registry_path: String,
    pub storage_path: String,
}
```

### 2.5 Tests

- Unit tests for receipt store (store, list, duplicate, signature).
- Integration tests for HTTP handlers (POST valid, POST invalid, GET list, health).

**Estimate for Phase 2:** ~300 lines of code + ~150 lines of tests.

---

## Phase 3: Config wizard

### 3.1 New crate scaffolding (`crates/vtessera-config/`)

**Goal:** Interactive CLI that generates valid TOML config.

- `Cargo.toml`: dialoguer, toml, serde
- `src/main.rs`: interactive prompts + flag parsing
- `src/validate.rs`: config validation before write

### 3.2 Interactive prompts

**Goal:** Step-by-step wizard that generates config + key registry.

- Prompt for: mode, CIDRs, require_ca, marketplace target, endpoint, key registry path, output path.
- Validate each input before proceeding.
- Generate TOML output.
- Write config + empty key registry.

### 3.3 Non-interactive mode

**Goal:** Flag-based for scripting.

- Flags: `--mode`, `--cidrs`, `--require-ca`, `--marketplace-target`, `--marketplace-endpoint`, `--key-registry`, `--output`.
- When all flags provided, skip prompts.

**Estimate for Phase 3:** ~200 lines of code + ~50 lines of tests.

---

## Phase 4: Documentation + integration

### 4.1 Update ROADMAP.md

- Mark §1e as implemented.
- Link to design spec and this plan.

### 4.2 Update BUILD.md

- Document new binaries (`marketplace-server`, `vtessera-config`).
- Document dependency additions for new crates.

### 4.3 Update main README.md

- Document private mode configuration.
- Link to design spec.

### 4.4 Update `packaging/`

- Add systemd unit for `marketplace-server`.
- Add man page entries for new binaries.

---

## Implementation order

| Step | Files | Depends on | PR? |
|------|-------|------------|-----|
| 1.1 | `cidr.rs` | — | — |
| 1.2 | `key_registry.rs` | — | — |
| 1.3 | `config.rs` | 1.1, 1.2 | — |
| 1.4 | `main.rs` | 1.3 | — |
| 2.1–2.5 | `marketplace-server/` | — (independent) | Separate PR |
| 3.1–3.3 | `vtessera-config/` | — (independent) | Separate PR |
| 4.1–4.4 | docs | 1.4 | — |

**Recommended PR structure:**
- **PR 1:** Phase 1 (daemon changes) — `cidr.rs`, `key_registry.rs`, `config.rs`, `main.rs`
- **PR 2:** Phase 2 (reference server) — `marketplace-server/`
- **PR 3:** Phase 3 (config wizard) — `vtessera-config/`
- **PR 4:** Phase 4 (docs) — ROADMAP, BUILD, README, packaging

---

## Risk assessment

| Risk | Mitigation |
|------|------------|
| CIDR parser bugs | Unit tests + fuzz with random IPs/CIDRs |
| Key registry format changes | Schema-versioned TOML (add `version` field if needed) |
| Reference server security | TLS + key validation; document hardening in README |
| Backward compat | All new fields have defaults; existing configs work unchanged |
| Dep budget (marketplace-server) | Separate crate, not added to vtesserad |
