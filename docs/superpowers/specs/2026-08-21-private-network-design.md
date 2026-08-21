# Private / Enterprise Network Mode

> **Issue:** [#52](https://github.com/douglasdemaio/vtessera/issues/52)
> **Date:** 2026-08-21
> **Status:** Approved

## Summary

Add an opt-in `mode = "private"` to `vtesserad` that lets the daemon run
entirely inside a company's own firewalled network — either fully isolated
from the public marketplace, or pointed at a **private/internal marketplace
endpoint** the company controls. This enables internal compute accounting
(charge-back/show-back between teams, on-prem GPU pools, edge fleets)
without depending on, or exposing anything to, the public internet.

## Motivation

- Enterprises with their own hardware want the same metering/receipt
  mechanics vtessera offers, but under their own trust boundary.
- Compliance/security requirements (air-gapped networks, DMZ'd
  environments, no third-party egress) make the current "submit to public
  marketplace" design a non-starter for internal use.
- A private marketplace mode opens vtessera up as an internal
  cluster-accounting tool, not just a public compute-rental client.

## What this does NOT change

- Public marketplace mode remains the default — this is purely additive.
- No new inbound listener is introduced; `vtesserad` remains outbound-only.
- The existing `submit` feature gate is unchanged; private mode works with
  or without it.
- No new dependencies are introduced for CIDR parsing (hand-rolled, matches
  BUILD.md §1.3 dep policy).

---

## 1. Config additions

### `[network]` section

```toml
[network]
mode = "private"              # "public" (default) | "private"
allowed_cidrs = ["10.0.0.0/8", "172.16.0.0/12"]
require_internal_ca = true
key_registry_path = "/etc/vtessera/keys.toml"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | `"public"` \| `"private"` | `"public"` | Network scope. Public = current behavior. Private = restrict to internal CIDRs and optional key registry. |
| `allowed_cidrs` | `Vec<String>` | `[]` | CIDR ranges the daemon operates within. In private mode, the daemon logs a warning if the outbound endpoint is outside these ranges. Empty = no restriction (still private, just no CIDR enforcement). |
| `require_internal_ca` | `bool` | `false` | When true, validate that the node's signing key is present in the key registry before submitting receipts. |
| `key_registry_path` | `Option<String>` | `None` | Path to the key registry TOML file. Required when `require_internal_ca = true`. |

### `[marketplace]` section

```toml
[marketplace]
target = "internal"
endpoint = "https://compute.internal.corp/api/v1/receipts"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `target` | `"public"` \| `"internal"` \| `"none"` | `"public"` | Where signed receipts are submitted. Public = current `submit_endpoint`. Internal = company-run endpoint. None = local spool only. |
| `endpoint` | `Option<String>` | `None` | URL to POST signed receipts to. Required when `target = "public"` or `target = "internal"`. Ignored when `target = "none"`. |

### Validation rules

- `mode = "private"` requires `[marketplace]` section to be present.
- `target = "public"` requires `submit_endpoint` (existing behavior, unchanged).
- `target = "internal"` requires `endpoint` in `[marketplace]`.
- `target = "none"` disables outbound submission; `endpoint` is ignored.
- `require_internal_ca = true` requires `key_registry_path` to be set and
  the file to exist and be parseable.
- `allowed_cidrs` entries must parse as valid CIDR notation; invalid entries
  cause a startup error.

### Existing fields — no changes

`submit_endpoint` (top-level) remains for backward compatibility. When
`[marketplace].target = "public"`, the daemon uses `submit_endpoint` as
before. When `target = "internal"`, it uses `[marketplace].endpoint`.
The top-level `submit_endpoint` is ignored in private mode with
`target != "public"`.

---

## 2. CIDR validation (`crates/vtesserad/src/cidr.rs`)

Hand-rolled CIDR parser — no new dependencies (BUILD.md §1.3).

### Parsing

```
10.0.0.0/8     → IpNet { addr: 10.0.0.0, prefix_len: 8 }
172.16.0.0/12  → IpNet { addr: 172.16.0.0, prefix_len: 12 }
192.168.1.0/24 → IpNet { addr: 192.168.1.0, prefix_len: 24 }
```

- Parse dotted-decimal IPv4 address + `/N` prefix length.
- Validate each octet is 0–255.
- Validate prefix length is 0–32.
- Return `Err` with a clear message for malformed input.

### Matching

```rust
pub fn ip_in_cidrs(ip: Ipv4Addr, cidrs: &[IpNet]) -> bool
```

- Convert IP to a u32, mask by prefix length, compare against each CIDR.
- Empty `cidrs` list returns `true` (no restriction — private mode without
  CIDR enforcement).

### Why hand-rolled

- The executor crate already has CIDR machinery for guest VM network policy
  (`net_allowed_cidrs` in `cloud_hypervisor.rs`). This is the same pattern.
- Adding `ipnet` or `cidr` as a dependency widens the audited surface for
  a trivial parser. The program is ~30 lines.

---

## 3. Key registry (`crates/vtesserad/src/key_registry.rs`)

TOML file listing allowed Ed25519 public keys.

### Format

```toml
# /etc/vtessera/keys.toml
[[keys]]
name = "team-alpha"
pubkey = "7Xf9...Ed25519PubKey..."

[[keys]]
name = "team-beta"
pubkey = "3Kz8...Ed25519PubKey..."
```

### Loading

```rust
pub struct KeyRegistry {
    keys: Vec<AllowedKey>,
}

pub struct AllowedKey {
    pub name: String,
    pub pubkey: Pubkey,  // Ed25519 public key
}

impl KeyRegistry {
    pub fn load(path: &str) -> Result<Self, ConfigError>;
    pub fn contains(&self, pubkey: &Pubkey) -> bool;
}
```

- `load()` reads the TOML file, parses each `[[keys]]` entry, validates
  that `pubkey` is a valid base58-encoded Ed25519 public key (32 bytes).
- `contains()` checks if a given pubkey is in the registry.
- On load error (file missing, parse error, invalid key), returns
  `ConfigError` with a clear message.

### Validation at startup

When `require_internal_ca = true`:
1. Load the key registry from `key_registry_path`.
2. Check that the daemon's own signing key (from `key_path`) is in the
   registry. If not, log a warning and continue (the daemon can still
   meter, but receipt submission will be blocked).
3. When submitting a receipt, check that the signing key is in the
   registry. If not, skip submission and log a warning.

---

## 4. Submission routing (`crates/vtesserad/src/main.rs`)

### Current flow

```
finalize_window() → sign(receipt) → spool_to_disk(signed_receipt)
                                   → submit_receipt(endpoint, signed_receipt)  [if submit feature + endpoint set]
```

### New flow

```
finalize_window() → sign(receipt) → spool_to_disk(signed_receipt)
                                   → route_submission(signed_receipt)
```

```rust
fn route_submission(signed: &SignedReceipt, config: &Config) {
    match config.marketplace.target.as_str() {
        "public" => {
            // Existing behavior: POST to submit_endpoint (or marketplace.endpoint)
            if let Some(ep) = config.marketplace.endpoint.as_ref()
                .or(config.submit_endpoint.as_ref()) {
                submit_receipt(ep, signed);
            }
        }
        "internal" => {
            if config.network.require_internal_ca {
                if !key_registry.contains(&signed.pubkey) {
                    log::warn!("signing key not in registry, skipping submission");
                    return;
                }
            }
            if let Some(ep) = config.marketplace.endpoint.as_ref() {
                submit_to_internal(ep, signed);
            }
        }
        "none" => {
            // Local spool only — no outbound submission.
            log::debug!("marketplace.target=none, receipt spooled locally");
        }
        _ => unreachable!("validated at config load"),
    }
}
```

### `submit_to_internal()` — same as `submit_receipt()`

The internal marketplace endpoint accepts the same `SignedReceipt` JSON
format. The function signature is identical to `submit_receipt()`; the
only difference is the endpoint URL comes from `[marketplace].endpoint`
instead of the top-level `submit_endpoint`.

This means the reference server (§6) can reuse the same receipt
validation logic. No format conversion needed.

---

## 5. CIDR warning at startup

When `mode = "private"` and `allowed_cidrs` is non-empty:

```rust
if config.network.mode == "private" {
    if let Some(endpoint) = config.marketplace.endpoint.as_ref() {
        let ep_ip = resolve_endpoint_ip(endpoint);
        if !cidr::ip_in_cidrs(ep_ip, &config.network.allowed_cidrs) {
            log::warn!(
                "marketplace endpoint {} resolves to {} which is outside \
                 allowed CIDRs {:?} — submission may fail",
                endpoint, ep_ip, config.network.allowed_cidrs
            );
        }
    }
}
```

This is a warning, not an error — DNS resolution may be dynamic, and
the operator may intentionally route through a proxy. The CIDR check is
advisory in private mode.

---

## 6. Reference marketplace server (`crates/marketplace-server/`)

A lightweight Axum-based HTTP server that receives, validates, and stores
signed receipts from internal vtesserad nodes.

### API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/receipts` | Submit a signed receipt. Validates signature against key registry, stores to disk. Returns 201 on success. |
| `GET` | `/api/v1/receipts` | List stored receipts (JSON array). Optional query params: `?node_id=...`, `?since=...`. |
| `GET` | `/api/v1/health` | Health check. Returns 200. |

### Request/response

**POST /api/v1/receipts**

Request body: `SignedReceipt` JSON (same format as vtessera's receipt crate).

```json
{
  "receipt": {
    "schema_ver": 1,
    "node_id": "abc123...",
    "payout_id": "DEFG...",
    "window_start": 1724246400,
    "window_end": 1724250000,
    "samples_digest": "a1b2c3...",
    "totals": { "cpu_secs": 3600.0, "mem_peak_kb": 2048000, "disk_read_bytes": 0, "disk_write_bytes": 0 }
  },
  "pubkey": "7Xf9...",
  "sig": "base64..."
}
```

Response: `201 Created` with `{"status": "stored", "id": "<uuid>"}`.

Error responses:
- `400 Bad Request` — malformed JSON or missing fields.
- `403 Forbidden` — signing key not in the server's key registry.
- `409 Conflict` — duplicate receipt (same `node_id` + `window_start`).

**GET /api/v1/receipts**

Response: JSON array of stored `SignedReceipt` objects.

### Storage

JSON lines file (`receipts.jsonl`) — one receipt per line, append-only.
No database dependency. Suitable for internal use; operators can pipe the
file into their own analytics.

### Config

```toml
# marketplace-server.toml
listen_addr = "0.0.0.0:8443"
key_registry_path = "/etc/vtessera/keys.toml"
storage_path = "/var/lib/vtessera-marketplace/receipts.jsonl"
```

### Security

- TLS via axum-server with self-signed or internal CA certs (configurable).
- Key registry validation on every POST (same as vtesserad's
  `require_internal_ca` logic).
- No authentication beyond Ed25519 signature verification — the trust
  model is: if your key is in the registry, you're authorized.

---

## 7. Config wizard (`crates/vtessera-config/`)

Interactive CLI that generates a valid TOML config + key registry.

### Flow

```
$ vtessera-config

? Mode (public/private): private
? Allowed CIDRs (comma-separated, e.g. 10.0.0.0/8): 10.0.0.0/8,172.16.0.0/12
? Require internal CA key registry? (y/n): y
? Marketplace target (public/internal/none): internal
? Internal marketplace endpoint: https://compute.internal.corp/api/v1/receipts
? Key registry path: /etc/vtessera/keys.toml
? Output config path: /etc/vtessera/vtesserad.toml

✓ Config written to /etc/vtessera/vtesserad.toml
✓ Key registry written to /etc/vtessera/keys.toml (empty, add keys manually)
```

### Validation before write

- Parses all CIDR entries; rejects invalid ones at prompt time.
- Validates URL format for marketplace endpoint.
- Checks that output paths are writable.
- Generates a valid TOML that passes `Config::validate()`.

### Flags for scripting

```
vtessera-config --mode private \
  --cidrs 10.0.0.0/8,172.16.0.0/12 \
  --require-ca \
  --marketplace-target internal \
  --marketplace-endpoint https://compute.internal.corp/api/v1/receipts \
  --key-registry /etc/vtessera/keys.toml \
  --output /etc/vtessera/vtesserad.toml
```

When all flags are provided, runs non-interactively (no prompts).

---

## 8. Testing

### Unit tests

- `cidr.rs`: parse valid/invalid CIDRs, IP matching, empty list behavior.
- `key_registry.rs`: load valid/invalid TOML, missing file, contains check.
- `config.rs`: validate new fields, reject invalid combinations.

### Integration tests

- `mode = "private"` with `target = "none"`: receipt spooled locally, no
  outbound submission.
- `mode = "private"` with `target = "internal"`: receipt POSTed to internal
  endpoint.
- `require_internal_ca = true` with key in registry: submission succeeds.
- `require_internal_ca = true` with key NOT in registry: submission blocked.
- CIDR warning logged when endpoint is outside allowed range.

### Reference server tests

- POST valid receipt → 201.
- POST malformed receipt → 400.
- POST receipt with unknown key → 403.
- POST duplicate receipt → 409.
- GET receipts → list returned.

---

## 9. Files to create/modify

| File | Change | New? |
|------|--------|------|
| `crates/vtesserad/src/config.rs` | Add `NetworkConfig`, `MarketplaceConfig`, validation | No |
| `crates/vtesserad/src/cidr.rs` | CIDR parser + IP matching | Yes |
| `crates/vtesserad/src/key_registry.rs` | Key registry loading + lookup | Yes |
| `crates/vtesserad/src/main.rs` | Route submission based on `marketplace.target` | No |
| `crates/vtesserad/src/submit.rs` | Add `submit_to_internal()` (same signature as `submit_receipt`) | No |
| `crates/vtesserad/src/lib.rs` | Re-export new modules | No |
| `crates/vtesserad/Cargo.toml` | No new deps (hand-rolled CIDR, toml already present) | No |
| `crates/marketplace-server/` | New crate: reference server | Yes |
| `crates/marketplace-server/Cargo.toml` | Axum + tokio + serde | Yes |
| `crates/marketplace-server/src/main.rs` | Server entry point | Yes |
| `crates/marketplace-server/src/receipt_store.rs` | JSON lines storage | Yes |
| `crates/vtessera-config/` | New crate: config wizard | Yes |
| `crates/vtessera-config/Cargo.toml` | dialoguer + toml | Yes |
| `crates/vtessera-config/src/main.rs` | Interactive wizard | Yes |
| `docs/superpowers/specs/2026-08-21-private-network-design.md` | This spec | Yes |
| `ROADMAP.md` | Update §1e with implementation status | No |

---

## 10. Build.md compliance

- **No new inbound listener in vtesserad.** The reference server is a
  separate binary; vtesserad remains outbound-only.
- **No new dependencies in vtesserad.** CIDR parser is hand-rolled (~30
  lines). TOML parsing already exists.
- **Dependency budget for marketplace-server.** Axum + tokio are
  well-audited; this is a new crate with its own dep surface, not added
  to the daemon.
- **Dependency budget for vtessera-config.** `dialoguer` is a thin
  interactive-prompt crate; acceptable for a config tool.
