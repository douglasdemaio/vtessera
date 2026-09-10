# Vtessera Agent `overview` — Implementation Plan

**Spec:** `docs/design/specs/2026-09-10-overview-go-over-index-design.md`
**Date:** 2026-09-10

All changes in `crates/agent-cli/src/main.rs` (+ its `#[cfg(test)] mod tests`).
No new dependencies (uses `clap`, `serde_json`, `ureq`, `vtessera_transport`
constants already in the crate). No server-side changes.

## Phase 1: Shared `fetch_and_merge_offers` helper

Extract the fetch+merge logic from `discover` (`main.rs:237-298`):

- `fn fetch_index_offers(index: &str, query: &str) -> Option<Value>` — `GET
  {index}/offers{query}`, returns the parsed JSON (or `None` on failure).
- `fn fetch_marketplace(url: &str) -> Option<Value>` — `GET <url>`, parsed.
- `fn merge_offers(local: Option<&Value>, market: Option<&Value>) ->
  Vec<Value>` — the dedup loop (`main.rs:255-298`): identity key precedence
  entry-level `endpoint_id` → offer-body `endpoint_id` → `offer_endpoint`;
  local wins, marketplace dedup-fills; returns index-entry-shaped values
  (normalized so both sources yield `{offer, claimed_*, endpoint_id, last_heartbeat, candidates, source}`).

Refactor `discover` to call `merge_offers`; its rendered output must remain
byte-for-byte (existing expectations).

**Verify:** `cargo test -p vtessera-agent-cli` — `discover` tests still pass;
merge dedup unit test covers the precedence chain.

## Phase 2: `overview` command — CLI arg + enum

**Changes:**
- `Commands` gains `Overview { #[arg(long)] mode: Option<OverviewMode>, #[arg(long)] device: Option<OverviewDevice>, #[arg(long)] available: bool }`.
- `enum OverviewMode { Free, Paid }` (clap `ValueEnum`, snake_case).
- `enum OverviewDevice { Cpu, NvidiaGpu, NvidiaMig, NvidiaVgpu, AmdGpu }` (clap `ValueEnum`, snake_case).
- `main()` dispatch: `Commands::Overview { mode, device, available } => overview(index, marketplace, mode, device, available, json)`.

**Verify:** `cargo build -p vtessera-agent-cli`; `vtessera-agent overview --help`
shows the three flags; `--mode bogus` exits non-zero (clap rejection).

## Phase 3: `overview` function

**Changes:** `fn overview(index: &str, marketplace: Option<&str>, mode, device, available, json) -> Result<(), String>`:

1. Build the query string from `mode`/`device`/`available` (`?mode=free`,
   `&device=cpu`, `&available=1`). `""` when none set.
2. `fetch_index_offers(index, &query)` + optional `fetch_marketplace`; if the
   index fetch failed **and** there's no marketplace → `Err`. If the index
   failed but marketplace succeeded → proceed (mirrors `discover` convention).
3. `merge_offers(...)`; build a row per offer via a new pure helper:
   `fn overview_row(o: &Value, now_unix: u64) -> OverviewRow` with
   `{node_id, device, price_str, reach, dial, claim, cand, heartbeat}`.
   - `price_str`: `free` | `"{:.6} {cur}/s"` computed as
     `per_device_second_micros / 1_000_000.0`; currency from
     `currency` (lowercase).
   - `reach`: `offer_endpoint(body)` else `iroh:<endpoint_id>` else `?`.
   - `dial`: `quic` when offer-body `endpoint_id` present else `http`.
   - `claim`: entry-level `claimed_by` string, else `–`.
   - `cand`: `candidates` array length (entry-level; offer-body fallback).
   - `heartbeat`: `never` when entry-level `last_heartbeat` is 0; `fresh`
     when `now - last_heartbeat <= DEFAULT_ENTRY_TTL_SECS` (120); else `stale`.
4. Human render (when `!json`): header
   `NODE_ID DEVICE PRICE REACH DIAL CLAIM CAND HEARTBEAT` (fixed-width
   columns, `-`.repeat(120)), one row per offer, footer line `{} node(s) in
   view` + the `--node-id` submit hint from `discover`. Empty → `no nodes in
   view` + marketplace tip (same phrasing pattern as `discover:310-318`).
5. JSON render (when `json`): `{"count": N, "offers": [normalized rows]}` per
   spec §5.4 — `node_id`, `endpoint_id`, `device` (object), `price`
   (`{"mode":"free"}` or `{"mode":"paid","currency","per_device_second_micros"}`),
   `reach`, `dial`, `candidates` (array), `claimed_by` (null|string),
   `claimed_until_unix`, `last_heartbeat_unix`. Missing fields → `null`/`0`/`[]`.
   `now_unix` once via `SystemTime` for the whole render.

**Verify:** unit tests (Phase 4) + `cargo clippy --all-targets -- -D warnings`.

## Phase 4: Unit tests

In the existing `mod tests` (`main.rs:1020`), fixture JSON only (no network):

1. `overview_merge_prefers_index_identity` — same `endpoint_id` in index +
   marketplace → 1 row, index entry wins.
2. `overview_identity_falls_back_endpoint_id_to_endpoint` — no entry-level id:
   offer-body `endpoint_id` keys; no `endpoint_id` at all → HTTP `endpoint`
   keys identity but `DIAL` stays `http`.
3. `overview_price_renders_free_and_paid` — free → `free`; paid
   (per_device_second_micros 2792, eurc) → `0.002792 eurc/s` exactly.
4. `overview_claim_rendering` — absent claim → `–`; claimed_by → agent id.
5. `overview_heartbeat_classification` — 0 → `never`; now−hb ≤ 120 → `fresh`;
   > 120 → `stale` (boundary exact).
6. `overview_json_shape_normalized` — full row round-trips; absent fields are
   `null`/`0`/`[]`; `count == offers.len()`; paid price object.
7. `discover_columns_unchanged` — pins `discover`'s rendered header string.

**Verify:** `cargo test -p vtessera-agent-cli`.

## Phase 5: Docs + CI

**Changes:**
- `AGENTS.md` finding-nodes section: one line for `vtessera-agent overview`
  (all nodes, free+paid, `--mode`/`--device`/`--available`, `--json`).
- Update the Phase-2 debt table row in `docs/design/zero-config-connectivity.md:830`
  from `–` to the implementation location.
- Full CI per repo convention (`.github/workflows/ci.yml`): `cargo fmt --check`,
  `cargo clippy --workspace --all-targets -- -D warnings`, `cargo test
  --workspace --locked`, `cargo deny check`. No new deps → no Flatpak
  cargo-sources regen, no `Cargo.lock` churn.

**Verify:** All green.

## Dependencies

None new. `DEFAULT_ENTRY_TTL_SECS` already re-exported from
`vtessera_transport` (or inlined constant in the test/impl when the crate
doesn't re-export it at the agent-cli dep level — check at implementation time;
prefer the crate constant).

## Estimated Effort

| Phase | Effort |
|-------|--------|
| 1. Shared fetch/merge helper + discover refactor | 45 min |
| 2. CLI arg + enum | 20 min |
| 3. `overview` fn + render | 1 h |
| 4. Unit tests | 1 h |
| 5. Docs + CI | 30 min |
| **Total** | **~3.5 h** |