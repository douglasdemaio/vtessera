# Offer-Index Live-Demo Wiring + First-Come-First-Served Claims — Design

Date: 2026-08-15
Status: Approved (design review)

## Problem

The offer index (`crates/offer-index`) verifies and serves signed offers, but
nothing in the product actually uses it: no node publishes its offer, no agent
discovers through it, and no demo exercises it. It is exercised only by its own
unit tests. Separately, an agent that *does* discover an offer has no way to
signal intent to use the node, so a popular node can be stampeded by many agents
with no way to reserve it.

## Goal

Wire the index into the real flow and add first-come-first-served (FCFS)
signalling:

1. `vtessera-node --publish <index-url>` registers its signed offer on startup
   and refreshes it on an interval, so real nodes populate the index.
2. A new MCP `discover` tool on any `--publish`-configured node queries that
   index and returns current offers (with claim status), so agents can find a
   node and read its endpoint.
3. **FCFS claims**: the index is the claim authority. An agent claims a node
   via `POST /offers/{node_id}/claim`; the node *enforces* the claim by refusing
   jobs from any other agent until the claim expires or is released.

User decisions during design: scope = node `--publish` + MCP `discover` tool
(not script-only, not a GUI change); one `--publish` flag drives both publishing
and the `discover` tool's index; claims are index-authoritative with node
enforcement (not advisory-only, not node-authoritative).

## Design

### Component 1 — Index claims (`crates/offer-index`)

The index keeps the claim state in memory (same lifetime as its offer map).
No persistence: an index restart clears claims, which is acceptable at v0 scale
(agents re-claim; claims auto-expire by TTL).

- `IndexEntry` gains:
  - `claimed_by: Option<String>` — the claiming agent's identifier, or `None`.
  - `claim_until_unix: u64` — `0` when unclaimed, else `now + TTL` at claim time.
- `IndexState` gains:
  - `claim(&mut self, node_id, agent_id, now_unix, ttl_secs) -> Result<(), ClaimError>`:
    - unknown or expired offer → `ClaimError::NotFound`;
    - offer claimed by a *different* agent and not expired → `ClaimError::Taken`;
    - otherwise (unclaimed, expired claim, or same agent) → set
      `claimed_by = Some(agent_id)`, `claim_until_unix = now + ttl_secs`, `Ok`.
      Re-claiming by the same agent renews the claim (agents can extend).
  - `release(&mut self, node_id, agent_id) -> Result<(), ClaimError>`:
    - unknown offer → `NotFound`;
    - unclaimed → `NotFound`;
    - current claimant is a *different* agent → `NotOwner`;
    - otherwise clear the claim.
  - `prune()` additionally clears expired claims (alongside expired offers).
  - `list()` (which prunes first) therefore serves only live offers, and
    `?available=1` filters to entries with no active claim.
- `register()` **preserves** an active claim when re-registering the same
  `node_id` (the node's own publish refresh must not wipe claims).
- New routes in `dispatch`:
  - `POST /offers/{node_id}/claim`, body `{"agent_id": "<id>"}` →
    `201 {"status":"claimed"}` | `409 {"status":"taken","reason":...}` |
    `404` | `400` (missing/malformed agent_id).
  - `DELETE /offers/{node_id}/claim`, body `{"agent_id": "<id>"}` →
    `200 {"status":"released"}` | `403 {"status":"not-owner"}` | `404`.
- `GET /offers` / `GET /offers/{node_id}` entries gain
  `claimed_by` (`null` or string) and `claimed_until_unix`.
- Claim TTL constant: `DEFAULT_CLAIM_TTL_SECS = 60`.

### Component 2 — Node publish + enforcement (`vtessera-node`)

New optional args: `--publish <index-url>` and `--publish-interval <secs>`
(default 60).

- **Publish loop**: after the startup identity check (key `node_id` == offer
  `node_id`) passes, POST the node's own signed offer JSON (the exact body
  `GET /offer` returns — the same shape the index's `POST /offers` accepts) to
  `{index}/offers`; repeat every interval. Failures are logged and retried on
  the next tick; the process never exits on a publish failure. The index keeps
  the last good offer meanwhile.
- **Agent identity**: HTTP `/jobs` reads the optional `X-Agent-Id` header; MCP
  `submit_job` gains an optional `agent_id` argument that is forwarded as that
  header. Both paths already converge on the same handler, so one gate covers
  both.
- **ClaimGate**: a trait in `crates/node-api` (serve-gated, so the default lib
  stays socket-free): `NodeState` gains
  `claim_gate: Option<Arc<dyn ClaimGate>>`, where
  `fn admit(&self, agent_id: Option<&str>) -> Result<(), String>`. The binary
  implements it over HTTP with `ureq` (the offer-index binary's existing client;
  `serve` pulls it into node-api). `admit` performs one race-safe round trip:
  POST `{index}/offers/{node_id}/claim {"agent_id": ...}` and maps:
  - `201` → admitted (claimed-for-self, or claim already mine);
  - `409` → refuse, `409 {"status":"refused","reason":"node claimed by <agent>"}`;
  - index unreachable → **fail-closed** `503` "cannot verify claim availability"
    (a claimed node admitting strangers defeats the point of claiming);
  - gate configured but no agent id supplied → `409` "agent identity required".
- **No `--publish`** → no gate → today's behavior unchanged (anonymous free
  jobs run).
- Claims are **not auto-released** after a job; they live for the TTL or until
  the agent DELETE-releases. The demo releases explicitly.

### Component 3 — MCP `discover` tool (`crates/node-api`, serve-gated)

- New tool `discover` alongside `submit_job`. Registered in `tools/list` only
  when the node has `--publish`; without it, `tools/call discover` returns an
  honest error (`isError: true`, "index not configured").
- Arguments (all optional): `mode` (`"free"|"paid"`), `device`
  (`"cpu"|"nvidia_gpu"|"nvidia_mig"|"amd_gpu"`), `available` (bool — only
  unclaimed).
- Behavior: `GET /offers?mode=&device=&available=` on the configured index and
  return its JSON body (count + offers, each with `claimed_by`/
  `claimed_until_unix`, endpoint, device, price) as a text content block. The
  agent reads the endpoint from an offer body and submits its job there
  directly; discovery and execution stay separate concerns.
- The index client is one trait implemented in the binary:
  `fn claim(&self, agent_id: &str) -> Result<(), String>` and
  `fn discover(&self, query: &IndexQuery) -> Result<String, String>` (returns
  the index's response body). `NodeState` carries it so the claim gate and the
  MCP server share one connection to the same index.

### Component 4 — `gen_offer` demo helper

New optional args:
- `--seed <u8>` (default 42) — derives a distinct identity key
  (`SigningKey::from_bytes([seed; 32])`) so a demo can mint multiple nodes.
- `--endpoint <url>` (default `http://127.0.0.1:8402`) — the address the offer
  advertises, so each node publishes its real reachable URL.

### Component 5 — Demo (`scripts/offer-index-demo.sh`)

Headless, mirrors `scripts/settlement-demo.sh` style:

1. Build + start the index on `127.0.0.1:8403` (`--features serve`).
2. `gen_offer --seed 1 --endpoint http://127.0.0.1:8402 free --key-out` → node A
   (`127.0.0.1:8402`); `gen_offer --seed 2 --endpoint http://127.0.0.1:8405
   paid --key-out` → node B (`127.0.0.1:8405`); both started with
   `--publish http://127.0.0.1:8403`.
3. Wait until `GET /offers` returns both; print the listing.
4. Agent claims node A (`POST /offers/<id_A>/claim`); a second agent gets
   **409** (FCFS shown).
5. The first agent runs a free job on node A with `X-Agent-Id` → **200**; the
   second agent's job → **409 refused** (enforcement shown).
6. `POST /mcp` `tools/call discover` on node A → returns both offers with claim
   status (discovery shown).
7. `DELETE /offers/<id_A>/claim` → release; a job without an agent id → refused;
   a job with an agent id → admitted again.

## Public API changes

- `vtessera-offer-index` (lib): `IndexEntry.claimed_by`/`claim_until_unix`,
  `IndexState::claim`/`release`, `ClaimError`, claim TTL const, `?available=1`
  filter, claim routes in `dispatch`.
- `vtessera-node` (bin): optional `--publish`/`--publish-interval`; `X-Agent-Id`
  header on `/jobs`; MCP `submit_job` `agent_id` argument; MCP `discover` tool.
- `crates/node-api` (lib, serve-gated): `ClaimGate` trait, `IndexClient` trait
  + `IndexQuery`, `NodeState.claim_gate`/`index`.
- `gen_offer`: optional `--seed`, `--endpoint`.

## Error handling

- Claim 409 (taken), release 403 (not owner), 404 (unknown/expired), 400 (bad
  body).
- Index unreachable at admit → fail-closed 503 (when `--publish` configured);
  no gate without `--publish`.
- Publish-loop failures → log + retry next tick; never crash.
- `discover` without `--publish` → MCP error, `isError: true`.

## Testing

- `offer-index` unit: FCFS first-wins / second-409; same-agent renew; expired
  claim releasable; wrong-agent release denied; prune clears expired claims;
  `?available=1`; register preserves active claims; claim/release dispatch
  routes (201/409/403/404/400).
- `crates/node-api` (serve): fake `IndexClient` → gate admits same-agent and
  unclaimed, refuses taken; missing agent id refused when a gate is present; no
  gate behaves as today; MCP `tools/list` advertises `discover`; `tools/call
  discover` returns the index's offers.
- Demo script is the e2e (manual, like `settlement-demo.sh`; not in CI).

## Docs

- README: offer-index demo + claims section; node `--publish`/`X-Agent-Id`.
- ROADMAP §2a: mark the demo wiring + claims as shipped.
- BUILD.md: only if the node's arg table needs it (the node is a module crate,
  documented in README; BUILD.md stays the v0 daemon spec).

## Out of scope (follow-ups)

- GUI `--publish` wiring / claim UI.
- Claim persistence across index restart (in-memory; TTL is the safety net).
- MCP *claim* tool (claiming is index HTTP, not MCP).
- Paid-path changes (still an honest 501 until the verifier lands).
- A2A agent-card changes.
- Rate limiting / moderation on the index.
