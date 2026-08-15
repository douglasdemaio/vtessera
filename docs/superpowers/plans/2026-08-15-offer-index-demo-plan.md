# Offer-Index Live-Demo Wiring + FCFS Claims — Implementation Plan

Spec: `docs/superpowers/specs/2026-08-15-offer-index-demo-design.md`
Branch: `offer-index-demo` (new, off the settlement-service head that holds
the spec commit). After PR #34 merges, rebase onto `main` and drop the
settlement commits; the spec commit rides along into the offer-index PR.

No amendment to the spec needed — planning found only implementation-level
choices (detailed below), no design gaps.

## Phase 1 — Index claims (crates/offer-index lib)

1. `crates/offer-index/src/lib.rs`:
   - `IndexEntry` gains `pub claimed_by: Option<String>` and
     `pub claim_until_unix: u64` (0 = unclaimed).
   - `pub const DEFAULT_CLAIM_TTL_SECS: u64 = 60;`
   - `pub enum ClaimError { NotFound, Taken(String), NotOwner }` + Display.
   - `IndexState::claim(&mut self, node_id, agent_id, now_unix, ttl_secs) ->
     Result<(), ClaimError>`: prune first; missing → NotFound; `claimed_by`
     is `Some(other != agent_id)` with live claim → Taken; else set both
     fields (`now + ttl`) → Ok (same-agent re-claim renews).
   - `IndexState::release(&mut self, node_id, agent_id) -> Result<(),
     ClaimError>`: prune first; missing or unclaimed → NotFound; claimant is a
     different agent → NotOwner; else clear both fields → Ok.
   - `prune()` also clears expired claims (resets the two fields) without
     removing the offer entry.
   - `register()` **preserves** `claimed_by`/`claim_until_unix` from the prior
     entry when re-registering the same `node_id` (only if the claim is still
     live vs `now_unix`).
   - `OfferFilter` gains `available: bool`; `matches()` takes `&IndexEntry` +
     `now_unix` and requires no live claim when `available` is set.
   - `dispatch`: under the `/offers/{node_id}/` strip-prefix branch, handle
     `POST .../claim` (body `{"agent_id": ...}` → 201 `{"status":"claimed",
     "claimed_until_unix": n}` | 409 `{"status":"taken","reason":...}` | 404 |
     400 for missing/empty agent_id) and `DELETE .../claim` (same body → 200
     `{"status":"released"}` | 403 `{"status":"not-owner"}` | 404). Existing
     `GET`/`DELETE` on `/offers/{node_id}` untouched.
   - `parse_filter`: also read `available` (`1`/`true`).
   - `entry_to_value`/`entry_to_json`: include `claimed_by` (null when
     unclaimed) and `claimed_until_unix`.
2. Unit tests: FCFS first-wins + second-agent 409; same-agent renew extends
   `claim_until_unix`; expired claim reclaimable by a different agent; wrong-
   agent release → NotOwner; owner release clears; `prune` clears expired
   claim (offer entry stays, `available` flips back); `?available=1` hides
   claimed; re-register preserves a live claim (and drops an expired one);
   dispatch claim/release routes (201/409/403/404/400, DELETE by wrong agent).

Verify: `cargo fmt --check`; `cargo clippy -p vtessera-offer-index --all-targets
-- -D warnings`; `cargo test -p vtessera-offer-index --locked`.

## Phase 2 — Node publish + claim gate + MCP discover

3. `crates/node-api/Cargo.toml`: `serve` feature gains `"dep:ureq"`;
   `ureq = { workspace = true, optional = true }`. No dependency on
   `vtessera-offer-index` — the binary talks to the index over HTTP; the lib
   only needs the trait + query struct.
4. New `crates/node-api/src/index.rs`, wired as `#[cfg(feature = "serve")]
   pub mod index;`:
   - `#[derive(Debug, Clone, Default)] pub struct IndexQuery { pub mode:
     Option<String>, pub device: Option<String>, pub available: bool }`
     (strings, since node-api doesn't depend on offer-index; the bin maps
     them to the index's query params).
   - `pub enum AdmitError { Taken(String), Unreachable(String) }`.
   - `pub trait IndexClient: Send + Sync { fn admit(&self, agent_id: &str) ->
     Result<(), AdmitError>; fn discover(&self, query: &IndexQuery) ->
     Result<String, String>; }` — `admit` performs the single race-safe POST
     claim round trip; `discover` returns the index's `GET /offers?...` body.
     Node id + index URL are captured at construction.
5. `crates/node-api/src/lib.rs`:
   - `NodeState` gains `#[cfg(feature = "serve")] pub index: Option<Arc<dyn
     IndexClient>>`.
   - `fn check_claim_gate(state, agent_id: Option<&str>) -> Result<(),
     HttpResponse>` — serve: `index` None → Ok; agent_id None → 409 "agent
     identity required"; `admit` Ok → Ok; Taken(owner) → 409 "node claimed by
     <owner>"; Unreachable → 503 "cannot verify claim availability"
     (fail-closed). Non-serve stub always Ok — keeps the default build
     socket-free and behavior unchanged.
   - `run_free` becomes `pub(crate) fn run_free(state, body, agent_id: Option<
     &str>) -> HttpResponse` and calls `check_claim_gate` first. `handle_jobs`
     reads header `x-agent-id` and passes it through.
6. `crates/node-api/src/mcp.rs`:
   - `submit_job` gains optional `agent_id` argument → `x-agent-id` header on
     the `HttpRequest`.
   - The `RunFree` branch now calls `crate::run_free(&self.state, &body,
     agent_id)` and maps the `HttpResponse`: status 200 → `isError: false`,
     else `isError: true`; content text is the response body. One gate, both
     surfaces (fixes the current runner-inline duplication).
   - New `TOOL_DISCOVER = "discover"` (serve-gated): args `mode`/`device`/
     `available` → `IndexQuery` → `state.index` Some: `discover(query)` →
     content text = index JSON, `isError: false`; Err → text + `isError:
     true`. `index` None → `isError: true`, "index not configured".
   - `tools_list` advertises `discover` only under `serve` **and** when
     `state.index.is_some()`.
   - Side cleanup: `submit_job`'s stale "job execution is not yet wired in
     v0" description → note free jobs run when a runner is wired.
7. `crates/node-api/src/bin/vtessera_node.rs`:
   - New optional args `--publish <index-url>` and `--publish-interval <secs>`
     (default 60); update usage/help.
   - When `--publish` is set: build a `ureq::Agent`-backed `IndexClient`
     (index URL + own node_id from the loaded key) and set `NodeState.index`.
     Register the signed offer (`POST {index}/offers` with the same JSON
     `GET /offer` returns) at startup and re-post every interval; failures are
     logged and retried next tick — never exit. Run the loop on a background
     thread.

Verify: `cargo clippy -p vtessera-node-api --all-targets -- -D warnings` and
`--features serve`; `cargo test -p vtessera-node-api --locked` and
`--features serve`.

8. Node-api tests (serve feature): fake `IndexClient` via `dispatch`/`run_free`
   — gate admits unclaimed; 409 when taken; 409 when agent id missing; 503
   when unreachable; `index: None` behaves as today. MCP: `tools/list` lists
   `discover` with an index wired (and omits it when None); `tools/call
   discover` returns the index JSON / isError when None; `submit_job` forwards
   `agent_id` (fake client records the header).

## Phase 3 — gen_offer + demo script

9. `crates/node-api/examples/gen_offer.rs`: optional `--seed <u8>` (default
   42) → `SigningKey::from_bytes(&[seed; 32])`; optional `--endpoint <url>`
   (default `http://127.0.0.1:8402`).
10. New `scripts/offer-index-demo.sh` (mirrors settlement-demo.sh: mktemp
    workdir, `trap cleanup EXIT`, healthz-wait helper, python3 JSON parsing):
    1. Build + start index on `127.0.0.1:8403` (`--features serve`, push only).
    2. `gen_offer --seed 1 --endpoint http://127.0.0.1:8402 free --key-out`
       → node A on 8402; `gen_offer --seed 2 --endpoint
       http://127.0.0.1:8405 paid --key-out` → node B on 8405; both started
       with `--key`, `--state-dir`, `--publish http://127.0.0.1:8403`.
    3. Wait until `GET /offers` count == 2; print the listing.
    4. `agent-demo` claims node A → 201; `agent-other` claims node A → 409.
    5. `agent-demo` free job (busybox `NoopCpu` spec) with `X-Agent-Id` → 200;
       `agent-other` free job with `X-Agent-Id` → 409 refused.
    6. `POST /mcp` `tools/call discover` on node A → print the offers JSON
       (shows claim status).
    7. `DELETE .../claim` by `agent-demo` → 200; job with **no** agent id →
       409 "agent identity required"; job with `agent-other` → 200.
    Print PASS lines; exit non-zero on any failed assertion.

## Phase 4 — Docs + CI + e2e

11. Docs: README (offer-index demo + claims; node `--publish`/`--publish-
    interval`/`X-Agent-Id`; MCP `discover` tool); ROADMAP §2a status → demo
    wiring + FCFS claims shipped; DESIGN.md offer-index flow gains a claims
    sentence. BUILD.md: only if the node arg table lives there (it doesn't —
    README documents the node) — skip otherwise.
12. CI: confirm `.github/workflows/ci.yml` already covers `vtessera-offer-index`
    tests + node-api `serve` build (it does); no changes expected.
13. E2E: run `scripts/offer-index-demo.sh` end-to-end and confirm the PASS
    lines.

Final gates (before PR): full-workspace `cargo fmt --check`; per-crate clippy
`-D warnings` (node-api default + `--features serve`); `cargo test --locked`
(workspace, excluding vtessera-gui on host — GUI untouched, no sandbox run
needed); demo script e2e; rebase onto `main` after PR #34 merges.
