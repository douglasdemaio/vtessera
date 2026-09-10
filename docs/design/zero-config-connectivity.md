# Zero-Config Connectivity — Design

**Status:** Design review — no code written
**Scope doc:** `docs/design/zero-config-connectivity.md`
**Referenced plan:** `CONNECTIVITY-PLAN.md` §0–§5 (5 phases)
**Date:** 2026-09-08

This is the technical design review for converting the vtessera node from an
**inbound-dialable** model to an **outbound-only, pull-based** model over iroh
("zero-config connectivity"). It does **not** amend the PRD or FR labels
(deferred to T5.2) and does **not** begin implementation (deferred past this
review).

> **Reading the invariants.** The §0 invariants of `CONNECTIVITY-PLAN.md`
> describe the **target state**, not the current one. The current node binds an
> inbound TCP listener unconditionally, and FR-M3's `no_socket` test pins only
> the v0 metering daemon. Throughout this doc, every §0 invariant is treated as
> a **claim to be verified against the repo**, not a given.

---

## 1. Inbound-dependency inventory

Scope of the audit: every call site, test, config field, and doc that assumes
the node is dialable. Verdicts: **BREAKS** (must be rewritten), **ADAPTS**
(survives with non-structural change), **DELETED** (removed).

### 1.1 MCP server (FR-D2) — **ADAPTS**

`McpServer::handle` is a **pure function** over `(NodeState, &str)` — no I/O of
its own (`node-api/src/mcp.rs:65`, documented transport-agnostic at
`mcp.rs:8-11`). It already runs over both the TCP listener and the iroh QUIC
bridge (`vtessera_node.rs:1129-1131` → `dispatch` → `handle_mcp`), and the
stdio binary (`vtessera_mcp.rs`) proves the same path runs with **zero inbound
sockets**.

`NodeState` (`node-api/src/lib.rs:210-230`) carries no dialable address — its
`index` is an outbound `IndexClient`. **The MCP module itself needs no change**;
under outbound-only it is invoked by whatever the pull/dispatch loop unblocks.

**Verdict: ADAPTS.**

### 1.2 x402 402 challenge handler (FR-P2) — **ADAPTS**

`classify_job_request` (`node-api/src/lib.rs:356-373`) inspects an inbound
`HttpRequest` for the `x-payment` header and returns
`JobDecision::PaymentRequired`; `handle_jobs` emits the 402 synchronously
(`lib.rs:377-383`), and `payment_required_body` (`lib.rs:619-633`) formats the
JSON. All are pure functions over in-memory state (`offer`, `escrow_account`,
`network`).

The **only** model change is in how the *synchronous 402 response* turns into an
*asynchronously pushed challenge* under pull dispatch (T3.3): the challenge
travels **down the queue/subscription** the node pulled from, and the agent's
payment proof travels back up. No logic in `classify_job_request` /
`payment_required_body` changes.

**Verdict: ADAPTS.**

### 1.3 Offer `endpoint` field (FR-D1) — **ADAPTS in Phase 1; BREAKS if removed without Phase 2**

`OfferBody.endpoint` (`offer/src/lib.rs:174-176`) is **signature-critical**: it
is in `canonical_bytes` (`:208`), so removing/changing it changes the signed
bytes and invalidates existing signed offers. The signature domain and the
struct change must be updated in **lockstep**.

- **node-api `GET /offer`** (`lib.rs:289-291`): passes the offer through; no
  dependency on the *value*. Not broken.
- **offer-index `GET /offers`** (`offer-index/src/lib.rs`): stores/serves the
  offer verbatim; its richer `candidates`/`endpoint_id` fields are separate.
  Not broken.
- **agent-cli `submit`/`discover`/`offer`** (`agent-cli/src/main.rs:169,182,
  244,270`): **dials the endpoint string**. If `endpoint` is removed with no
  resolver in place, the agent has nothing to dial — **breaks**.
- **marketplace registration** (`vtessera_node.rs:1285-1336`): **re-signs** the
  offer with an IP:port override (WAN IP under `--upnp`). Under outbound-only
  this override intent disappears; registration would advertise a queue URL /
  `EndpointId` instead, and the external-IP/UPnP instrumentation becomes dead
  for reachability.

**Key finding:** `endpoint` must **remain** through Phase 1. With no DHT to
resolve `node_id`→address yet (Phase 2), the endpoint (repurposed to a queue
URL or kept and ignored once the queue carries addressing) is the only
reachability key. Under T2.1, `endpoint` → `node_id` only once a resolver
exists.

**Verdict: ADAPTS (keep in Phase 1); BREAKS in Phase 1 if `endpoint` is
removed without Phase 2.**

### 1.4 vtessera-agent submit (FR-D?) — **BREAKS**

`submit` (`agent-cli/src/main.rs:266-297`) is a **synchronous blocking
`POST {node}/jobs`** that expects the job metering **in the same HTTP response**
(`read_json` → prints `.status/.job_id/.backend/.metering`). There is **zero
iroh/EndpointId/queue support** in `agent-cli` (grep returns nothing). `offer`
and `health` are likewise direct GETs to `{node}/…`.

Under outbound-only pull dispatch:
1. The result-in-response contract breaks — the node runs the job when *it*
   pulls it, and the result arrives later via the queue; no single-HTTP
   correlation.
2. `--node http://IP:8402` has no listening socket → connection refused.

The CLI must become a queue **producer/consumer** (place a job, poll/receive the
result) rather than a dialer. This is a client rewrite, not a transport tweak.

**Verdict: BREAKS.**

### 1.5 Local-discovery JSON (FR-D5) — **ADAPTS**

Schema `DiscoveryFile { endpoint, node_id, index, pid }` (`agent-cli/src/main
.rs:55-62`; written by the GUI `daemon.rs:80-92,245-254` and
`scripts/local-stack.sh:279-286`). The agent uses `disc.endpoint` as a dialable
URL (`main.rs:108-109`) and checks `pid` liveness (`main.rs:79-86`, still
useful). `node_id` maps 1:1 to the iroh `EndpointId` (see §3).

Under outbound-only, `endpoint`'s meaning becomes an **EndpointId / queue URL**
rather than an `http://IP:port`; `index` (the queue) becomes primary. The writer
side — notably the GUI deriving `endpoint` from `opts.bind` (`daemon.rs:247`) —
must change because `--bind` itself becomes obsolete (see §1.8).

**Verdict: ADAPTS** (schema meaning shifts; `pid` liveness check survives).

### 1.6 offer-index register/claim flow (FR-D3/D4) — **ADAPTS (index survives); agent claim-then-dial BREAKS**

The index side is **already outbound-initiated**: the node registers
(`POST /offers`), claims (`POST /offers/{id}/claim`), and heartbeats
(`POST /offers/{id}/heartbeat`) as outbound POSTs (`offer-index/src/lib.rs:
511,537,581`; node `--publish` at `vtessera_node.rs:993-1010`). `NodeState`
uses an outbound `IndexClient` in `check_claim_gate` (`node-api/src/lib.rs:
571-601`). **Outbound-only does not break the index or the node's register/
claim/heartbeat.**

What breaks is the **agent's "claim at the index, then dial the node" leg** —
`POST {node}/jobs` with `x-agent-id` after winning a 60s FCFS lease
(`DEFAULT_CLAIM_TTL_SECS`, `offer-index/src/lib.rs:28`). Under T3.2 the 60s
lease and the 409/503 fail-closed rejections are replaced by **queue leases**
(see §3), and `IndexEntry` already carries `candidates` + `endpoint_id`
(`offer-index/src/lib.rs:93-99`, populated via heartbeat) — the seed of that
model.

**Verdict: ADAPTS (index + node outbound machinery survive); the agent's
direct-dial job/MCP legs BREAK.**

### 1.7 systemd unit — **ADAPTS (already socket-free)**

`packaging/vtesserad.service` governs the **metering daemon** (`vtesserad`), not
the node API server. It has **no `ListenStream`**, `RestrictAddressFamilies=
AF_UNIX`, `IPAddressDeny=any`, `DynamicUser=yes`, `ProtectSystem=strict`. This
is already outbound-only in spirit.

Under an outbound-only *node* the same template applies but must **permit
outbound** to the index/queue/relay: `RestrictAddressFamilies=AF_INET AF_INET6`
and `IPAddressAllow` to the specific outbound endpoints (the existing
`--features submit` comment pattern is the exact precedent). The key invariant
is that inbound is still denied even when outbound is allowed.

**Verdict: ADAPTS** (if the node gets its own packaged service, it inherits the
metering-daemon template with outbound allow + inbound deny).

### 1.8 GUI — **BREAKS → ADAPTS**

The GUI's networking model is built around a **listening node**:
- Port spin + endpoint placeholder `http://your-public-ip:8402`
  (`main.rs:814-819,929`).
- **UPnP switch + hint** (`main.rs:1011,1016-1024`) — forwarding a port becomes
  moot when there is no inbound port.
- Local-network/public toggle with iroh mention (`main.rs:824,960-976`).
- Settings `port`/`endpoint`/`upnp_enabled` (`settings.rs:33-92`), with
  validation requiring `http(s)://` (`settings.rs:182-186`).
- Node spawn passes `--bind` (`daemon.rs:213-221`), `--upnp`
  (`daemon.rs:228-230`), and builds `bind = 0.0.0.0:port` + LAN endpoint
  auto-fill (`main.rs:612-643`).
- Reuse probe `healthz_up` via `TcpStream::connect` (`daemon.rs:119-138`) and
  node-reuse-on-port check (`daemon.rs:203-208`).

Under outbound-only: `port`, `endpoint`, the Port caption+placeholder, UPnP
switch+hint, `--bind`, the LAN-IP auto-fill, and the `TcpStream` reuse probe are
**DELETED or repurposed**; settings schema/validation changes to an
EndpointId/queue URL; the discovery write (`daemon.rs:245-254`) and
`StartOptions.bind` (`daemon.rs:37-77`) are revised.

**Verdict: BREAKS → ADAPTS** (this is a significant GUI rework, in addition to
the T5.1 consent-surface change).

### 1.9 `no_socket` test / node-bind test — **ADAPTS (strengthened); no node-bind test exists**

`crates/vtesserad/tests/no_socket.rs` snapshots `LISTEN` entries in
`/proc/self/net/tcp[6]` before/after spawning **`vtesserad`** and asserts
`new_listeners.is_empty()`. It pins only the v0 meter — **not** the node.

There is **no** test asserting `vtessera-node` binds a listener, and no "morph"
test. The node genuinely opens `TcpListener::bind(&args.bind)`
(`vtessera_node.rs:1106`) with no test snapshotting it. This is the gap the
directive's strengthened invariant (#1) would close: extend a `no_socket`-style
snapshot to `vtessera-node` under `--features serve` and assert **no LISTEN
socket in any mode** — even job-accepting mode.

**Verdict: ADAPTS** — existing test is unaffected; a new node-level no-socket
test is added (the directive's invariant #1 is *stronger* than current
behavior, so the current test does not need relaxing).

### 1.10 Scripts / CI — **BREAKS (scripts); ADAPTS (ci.yml)**

- **`.github/workflows/ci.yml`**: runs `cargo fmt/clippy/test` per crate and
  `systemd-analyze security`; **no integration test dials a live node over a
  socket**. Unaffected. The strengthened no-socket assertion could be added
  here.
- **`scripts/local-stack.sh`**: `--bind host:PORT`, inbound `healthz` poll
  (`:267`), advertised `IP:port` (`:243`), discovery `endpoint`
  (`:279-286`), `curl http://127.0.0.1:$PORT/healthz` (`:313-315`). **BREAKS →
  ADAPTS** (port/healthz/discovery become queue/EndpointId).
- **`scripts/x402-demo.sh`**: `NODE_URL=...:8402`, `curl .../healthz`
  (`:34,57,60`), `--bind 127.0.0.1:PORT` (`:47`), agent dials `$NODE_URL`
  (`:67`). **BREAKS → ADAPTS** (the 402 challenge must travel down the queue).
- **`scripts/offer-index-demo.sh`**: signs offers with LAN `--endpoint`
  (`:81-83`), `--bind` (`:86,92`), and the agent-dial job/MCP legs
  (`POST $A_URL/jobs`, `POST $A_URL/mcp`, `:127-140,154-161`). Index
  register/claim/heartbeat survive; the node-dial legs **BREAK → ADAPTS**.

### 1.11 Inventory summary

| # | Component | Verdic | Core evidence |
|---|---|---|---|
| 1.1 | MCP server | ADAPTS | pure func, transport-agnostic (`mcp.rs:8-11,65`) |
| 1.2 | x402 402 handler | ADAPTS | pure funcs (`lib.rs:356-390,619-633`) |
| 1.3 | offer `endpoint` | ADAPTS (Phase 1); BREAKS if removed w/o DHT | `offer/src/lib.rs:174-208`; agent dials it |
| 1.4 | agent submit/offer/health | BREAKS | sync result-in-response (`main.rs:266-297`); no iroh |
| 1.5 | local-discovery JSON | ADAPTS | endpoint→EndpointId/queue; pid survives |
| 1.6 | offer-index claim/dial | ADAPTS (index); BREAKS (agent dial leg) | outbound POSTs survive; `:28` lease→queue |
| 1.7 | systemd unit | ADAPTS | already socket-free; add outbound allow |
| 1.8 | GUI | BREAKS→ADAPTS | port/endpoint/UPnP/bind listener-based |
| 1.9 | no_socket test | ADAPTS (strengthened) | only pins vtesserad; add node-level test |
| 1.10 | scripts | BREAKS→ADAPTS | node-dial legs; ci.yml unaffected |

**Standout finding:** the *library* layer (node-api, mcp, offer, offer-index,
settlement) is nearly all transport-agnostic and survives outbound-only with
minimal change. The breakage concentrates in the **client-facing binaries and
UX** (agent-cli, GUI, demo scripts) and in the **offer schema's
signature-critical `endpoint` field**. The index and the node's outbound
register/claim/heartbeat already match an outbound-only topology.

---

## 2. Invariant conflict table

One row per §0 invariant: current state, target state, what must change, and
the gap type. **Flagged** blank-cell notes call out invariants that are
unachievable or in tension with another invariant.

| § | Current state | Target state | What has to change | Gap type |
|---|---|---|---|---|
| 1 | Node binds `TcpListener` unconditionally (`vtessera_node.rs:1106`); `no_socket.rs` pins only `vtesserad` | No listening socket in any mode, metering **and** job-accepting | Remove inbound bind from node; add node-level no-socket test(s); port the `serve` bottleneck to outbound dispatch | **code + test** |
| 2 | Relay is N0 public (Number 0); node re-signed offer carries IP:port endpoint to marketplace | Relay sees ciphertext only; never payment/hold/funds/jobs; **relay list is plural, not a single provider** | No functional change to relay *today* (iroh relays are dumb); enforce that all vtessera logic (payment, escrow, receipts) stays node-local; document relay neutrality; **require relay plurality** — the node's relay pool must accept multiple, independently-operated relays (self-hosted + third-party), not only N0's, because the relay is the TCP fallback for UDP-blocked networks (see flagged finding) | **disclosure + test + config** |
| 3 | Payment verified node-local via `SolanaPaymentVerifier` (`--rpc-url`, `vtessera_node.rs:1092-1094`) | Verification stays node-local; no relay/coordinator input | Keep the verifier invocation on the node in T3.3; do not move `getTransaction` / `getSignatureStatuses` to any third party | **code guard** (already satisfied; keep in T3.3) |
| 4 | Offers Ed25519-signed by node key (`sign`/`verify`, `offer/src/lib.rs:237-309`) | No intermediary can forge/mutate offers or heartbeats | Transport must not alter `canonical_bytes`; heartbeats must carry signature (index heartbeat is unsigned today — see flag). T2.1 `endpoint` change must update `canonical_bytes` in lockstep | **code + test + config** |
| 5 | Escrow/receipts/settlement live in modules 3–4 (settlement, escrow program), transport-agnostic | Untouched by this work | Do not reach into `vtessera-settlement` or `programs/vtessera-escrow` for Phase 1–4; T3.2 lease-expiry receipt path touches `settlement` **by design** (see §4/payment blast radius) | **scope guard** |
| 5b | Coordinator is a new role (federation, §4b); no money path exists yet because none is built | **Coordinator can never touch money.** No escrow path, no payment-verification input, no influence over `finalize_pro_rata` inputs (`f`, `payout_id`) | Define the coordinator's surface so it has **no** stake in payment: dispatch/queue/routing only; payment verification stays node-local (invariant #3); settlement inputs computed only by node/operator-local modules; add a unit test asserting a coordinator message cannot alter `f`/`payout_id` and that no coordinator code path even *parses* escrow or payment fields | **code + test + scope guard** |
| 6 | GUI surfaces `--upnp` toggle + public-IP hint (`main.rs:1011,1016-1024`) | No user is asked about their router; UPnP never depended on | Delete UPnP/port-forward UX; never surface "open port X"; silent UPnP attempt is allowed but must not be a dependency | **code + disclosure** |

### Flagged invariants

- **§0 invariant 4 vs. current heartbeat.** The *offer-index* heartbeat
  (`POST /offers/{node_id}/heartbeat`, `offer-index/src/lib.rs:581-607`)
  updates `candidates`/`endpoint_id` with **no signature** today. The design doc
  `docs/INTERNET-CONNECTIVITY.md` specified a signed heartbeat
  (X-Signature over `heartbeat:{node_id}:{timestamp}`) but it is **not
  implemented**. The directive's T1.4 "heartbeats are signed" would add this.
  This is a real code gap, not a doc-only item. **Current state does not yet
  satisfy invariant #4** for the heartbeat path. (Offer signatures are already
  good.)

- **§0 invariant 1 vs. Phase 2 dependency.** Making the node truly
  outbound-only in Phase 1 (before Phase 2's queue exists) has no place for the
  agent to *reach* it: the offer `endpoint` (§1.3) and the local-discovery file
  (§1.5) must still carry *some* reachability key. We recommend a queue/pull URL
  in the "endpoint" field during the transition rather than removing it
  (see §4, migration), and — per the queue-reversal decision — building the
  **coordinator/queue in Phase 1** so it is available when the node flips
  outbound-only. This is a sequencing tension between invariant #1
  (no listener) and "agent must still be able to get work to the node."

- **Coordinator vs. invariant #2 (no money touch).** In Phase 1 the coordinator
  does not touch money by construction (§2 row 5b). The risk is *drift*: because
  dispatch gating and rate limiting live on the coordinator, later work could
  tempt adding payment/verification there. That must be prevented at the type
  level (coordinator never parses escrow/payment fields) and audited in review —
  see §4b "no money touch".

- **§0 invariant 1 vs. `vtesserad` v0 contract.** The v0 daemon's no-socket
  guarantee is deliberately narrow (it is a *meter*, never a server). Extending
  it to the *node* is the right call, but note it makes the node binary
  fundamentally different from its current `serve` design — the hardened
  `vtesserad.service` (invariant 7) stays as-is; the *node* needs its own
  outbound-only unit if packaged as a service.

### Invariant 2 / T1.3 — relay wire transport resolves the UDP-blocked case

**Finding (corrected after checking iroh's relay wire protocol, not just
`TransportAddr`):** iroh's relay path **works over TCP/443 with all outbound UDP
blocked.** `TransportAddr` variants describe *addressing* (`Relay(RelayUrl)`,
`Ip(SocketAddr)`, `Custom`); they do **not** describe the relay's wire
transport. The relay client connects to a relay over **WebSocket RFC 6455 on
TCP** (`iroh-relay-1.1.0/src/client.rs:302-318` — `tokio_websockets::ClientBuilder`
over `MaybeTlsStream<TcpStream>` in `client/streams.rs:10-20,105-107`), and the
relay **server** accepts those WebSocket upgrades on a TCP listener
(`server/server.rs:47,804`; `server/http_server.rs:22` — ports 80/443, line 466
`HTTP/WS` vs `HTTPS/WSS`). The QUIC server is **optional**
(`server.rs:114-115` `quic: Option<QuicConfig>`).

So a node behind a firewall that blocks outbound **UDP** but permits TCP (and
ideally 443/TLS) still reaches a relay via **WebSocket-over-TCP(S)** and keeps a
working relayed connection. **Direct peer connections** still need QUIC/UDP, but
if direct fails or UDP is blocked, the relayed path over WebSocket/TCP carries
the connection.

**Consequence for T1.3:** the "WSS fallback for UDP-blocked networks" requirement
**largely resolves without a custom transport or external bridge** — iroh's relay
*is* a WebSocket-over-TCP path. T1.3 narrows to: (a) ensure the node *prefers*
direct (UDP/QUIC) when available and falls back to relay (WebSocket/TCP) when
UDP is blocked, (b) surface the connection mode (relay vs direct) for T1.2's
metric and the Status tab, and (c) log the fallback. Retained edge cases for
review: a network that blocks **all outbound TCP as well** (rare) and the relay
relying on port 443 — those remain accepted gaps, but the common
"UDP-blocked" case is handled by stock iroh.

Note this also **reinforces the relay plurality requirement** below: because the
relay is the TCP fallback, the node's relay list must be plural and operator-
runnable, not only N0's public infrastructure.

---

## 3. Identity decision

### Question

Can the iroh `EndpointId` bind to / derive from the existing Ed25519 node key
(FR-M2, `0600`), or is a second keypair unavoidable?

### Answer: derive — no second keypair, no new stored secret.

Verified against `iroh-base-1.1.0/src/key.rs`:
- `pub struct SecretKey(SigningKey)` — iroh's secret key **is**
  `ed25519_dalek::SigningKey` (line 15, 261).
- `SecretKey::from_bytes(&[u8;32])` → `SigningKey::from_bytes` (line 337-338);
  `to_bytes` → 32 bytes (line 332-333). **Same curve, same key size, same
  byte format as vtessera's raw `0600` seed.**
- `pub type EndpointId = PublicKey` (line 70); `let key = SecretKey::from_bytes(&rng.random()).public()` produces it (line 982).

So: `EndpointId = SecretKey::from_bytes(node_key_bytes).public()` —
**deterministically derived** from the existing FR-M2 key, with **no additional
key material on disk**.

This is already how the code works: `start_iroh_endpoint` passes
`signing_key.to_bytes()` → `SecretKey::from_bytes` →
`iroh_sidecar::create_endpoint` (`vtessera_node.rs:1440-1460`,
`transport/src/iroh_sidecar.rs:134-139`). The node already reuses its one key
for both offer signing and the iroh endpoint.

### Consequences

- **No consent/disclosure change** for "a second key stored on the machine" —
  because there isn't one. The consent disclosure that *does* become necessary
  is the **persistent outbound connection** (T5.1), not a new key.
- The iroh `EndpointId` maps 1:1 to `derive_node_id`'s pubkey; the offer's
  `node_id` (SHA-256(pubkey)[..16], hex) is a *short form* of the pubkey, not
  the full `EndpointId` — so `node_id` and `EndpointId` are related but **not
  byte-identical**. Where a full key is needed for dialing, the offer must carry
  the **full pubkey / EndpointId**, not the truncated `node_id`. This is a real
  schema consequence for T2.1 (`endpoint` → `node_id` must actually be
  `endpoint` → `endpoint_id`/pubkey, or a resolver keyed on `node_id`).

- **Offer schema version becomes mandatory.** Because the offer now carries the
  full pubkey (not the short `node_id`) and a possibly-list `endpoint`
  (IP/port *and* queue URL — §4a), an old-version consumer must not
  mis-parse it. Add an explicit `schema_version` field to `OfferBody`, kept in
  **lockstep with `canonical_bytes`** (which signs the exact serialized bytes),
  and have consumers **reject offers with an unsupported/lower schema version**
  rather than guessing. This is the mechanism that lets us evolve the
  `endpoint`/`node_id` semantics without breaking signature verification. A unit
  test asserts: an offer signed at version N verifies, an offer claiming a
  version but whose bytes do not match that version's layout fails canonical
  acceptance, and a consumer rejects an unsupported version.

---

## 4. Migration strategy

### Recommendation: dual-stack (listener retained behind a flag) during transition.

**Chosen approach.** Keep the existing inbound TCP listener behind an explicit
flag (default recovered from today's behavior) while the outbound-only path
lands, then flip the default to outbound-only and finally remove the listener.
This preserves the **known-good devnet soak path** (pay → run → settle → split,
`scripts/x402-demo.sh`, `crates/devnet-demo` soak) so escrow/settlement keeps
soaking in its final-adjacent shape while dispatch inversion is proven.

**Why dual-stack is right here:**
1. **Payment-path sequencing (§0 sequencing note + user's constraint).** Phase 3
   touches the payment path, and the escrow program must **not** be frozen
   immutable until that flow soaks on devnet. Keeping the listener lets the
   devnet soak keep running on a stable path while the new pull-based flow
   matures separately.
2. **Rollback story.** If pull-dispatch breaks, the flag restores today's
   behavior with a one-line config change — no redistributed binary, no
   re-issued keys, no schema change.
3. **Interop during transition.** Existing agents, GUI, and demo scripts keep
   working against the listener until the client rewrites (§1.4, §1.8) land;
   otherwise the node becomes unreachable to every existing client on day one.

**Flag design (target, not yet implemented):**
```
--connectivity outbound-only | inbound+dialable   (default inbound+dialable during transition)
--connectivity outbound-only                       (final default)
```
During transition, both TCP and iroh stay on in `inbound+dialable`; only
`outbound-only` skips `TcpListener::bind` (§1.9's new test asserts no LISTEN in
that mode).

**Rollback:** revert the flag default (and, if a regression is found, the config
line) — the outbound path cannot silently strand agents because the listener
remains one flag away.

### 4a. Reachability key during Phase 1 (the invariant-1 tension)

Because Phase 1 outbound-only (no Phase 2 resolver) still needs a reachability
key, the offer `endpoint` field (§1.3) should carry a **queue/pull URL** in
addition to (or instead of) the IP:port, rather than being deleted. Concretely:
the node registers to a **coordinator/work queue** (the pull point) and
advertises that queue URL; agents POST jobs to the queue; the node pulls. This
is the bridge between the outgoing-only node and "agents can still get work to
it" without an inbound port. It does **not** require the agent to dial the node.

**Queue-reversal decision (§ user's reversal of the Phase 3→Phase 1 plan):** the
coordinator/queue is **built in Phase 1**, not Phase 3. Rationale: this is where
dispatch gating, rate limiting, and abuse response live (open PRD §11
shortfalls), and Phase 1's outbound-only node needs a rendezvous. Crucially, it
must be **federation-capable from the start** — many independent coordinators,
each with its own policy — because no single operator's concentration may become
a protocol assumption. See §4b.

**The queue rides inside iroh (invariant #2, non-negotiable).** The work queue
must be carried over the **existing iroh QUIC path** (end-to-end encryption with
the node's Ed25519 key pinned, `vtessera_ALPN`), **not** a new plaintext HTTP
queue. A plaintext queue would expose job contents and payment proofs to the
queue operator, violating invariant #2. This is the single most important
constraint to carry into Phase 3 (see §5). The coordinator is reached by
`EndpointId` (derived prep), and all queue messages are end-to-end encrypted to
the node.

### 4b. Federation: the coordinator is one of *many*, never the single point

The coordinator must be **federation-capable from Phase 1**. "Federation" here
means: many independent coordinators may exist, each operated by a different
entity with its own policy, and **no single operator's degree of concentration
may be a correctness assumption of the protocol.** This is the reverse of the
original plan (coordinator deferred to Phase 3); the reversal is deliberate
because dispatch gating, rate limiting, and abuse response (PRD §11) all live
here, and because a Phase-1 coordinate must not paint the protocol into a
single-operator corner.

**Encoding these constraints:**

1. **Coordinator identity = public key, not hostname.** A coordinator is
   addressed by its Ed25519 public key (an iroh `EndpointId`), exactly like a
   node. There is no global DNS/namespace that names a coordinator; you trust the
   one whose key you're pinned to.
2. **Coordinators sign what they emit.** Important coordinator-produced messages
   (queue-assignment, lease grant/expiry, dispatch decisions offered to a node)
   carry the coordinator's signature so a node can attribute and audit them. A
   signed **receipt-history query** is the portable abuse-evidence mechanism: any
   party can ask a coordinator for a signed log relevant to a dispute, and that
   evidence travels across coordinators.
3. **No single global namespace.** Job IDs, lease IDs, and offer registrations
   are all **scoped per coordinator**. Two coordinators may independently assign
   the same opaque id; nothing assumes global uniqueness. (Where disambiguation
   is needed, the pair `(coordinator_pubkey, id)` is used.)
4. **A node may register with several coordinators** and must **not double-commit
   its capacity** — concurrency (one job on one slice of capacity) is enforced by
   the node, which remains the sole holder of its execution state, regardless of
   how many coordinators advertise it. Leases are **per-coordinator** (T3.2).
5. **Dispatch gating is per-coordinator and *advisory*.** A coordinator can
   refuse to send work, revoke a lease, or delist an offer from *its own* index —
   but it **cannot eject a node globally** and cannot force a node to take or drop
   work. Gating is a supply-side lever (see honest-description note below), not a
   global enforcement mechanism.
6. **Revocation lists are advisory only.** A coordinator may publish a
   reputation/revocation list about a node, but other coordinators and nodes are
   free to ignore it. Nothing is globally delisted by fiat.
7. **No coordinator touches money.** No escrow path, no payment-verification
   input, no influence over `finalize_pro_rata` inputs. Payment verification stays
   node-local (invariant #3); settlement inputs are computed node/operator-local
   (invariant #5/#5b). A coordinator's job is routing and dispatch, nothing more.
   **Test:** assert a coordinator message cannot alter `f`/`payout_id`, and no
   coordinator code path parses escrow or payment fields.
8. **Direct dial retained.** The direct `EndpointId`-to-`EndpointId` dial (outbound
   iroh connection, PRD persona 3 — private / LAN / fleet-admin) stays available
   and is the **Phase 1 fallback** when no coordinator is reachable. Federation
   does not remove the direct path; it adds an organized rendezvous on top.

### 4c. Degradation: cascading fallback, never fail-closed

Connectivity degrades in this order, **never producing a hard `503`**:

1. **Coordinator A reachable** → dispatch through A.
2. **Coordinator A unreachable** → fail over to coordinator B (if registered),
   else **direct dial** via the offer's direct `EndpointId`, else local queue.
3. **All coordinators + direct unreachable** → the node keeps serving already-
   accepted work to completion, stays "connected" at the transport layer (iroh
   relay), and **retries with backoff** (§6a-2). It does *not* fail closed into a
   hard error: new work simply cannot arrive until something is reachable again,
   and nothing is stranded — the node keeps draining already-accepted work and
   both sides keep the backoff/reconnect loop. There is **no FR-D4-style 503**.

Degradation must not compromise invariant #2: whichever path carries the job, it
is end-to-end encrypted to the node (iroh QUIC), so fail-over between
coordinators does not introduce a plaintext hop.

### 4d. Honest description (what a coordinator *cannot* do)

- **Gating is supply-side, not enforcement.** A coordinator that stops sending a
  node work does not "ban" it; it simply routes work elsewhere. Other
  coordinators are unaffected. This is disclosed in `docs/CONSENT.md` (a
  coordinator can stop sending a seller work) — see §7c/P1.9.
- **Metadata remains observable.** Even with end-to-end encryption, the
  coordinator through which a connection is relayed observes **timing, payload
  sizes, availability, and the buyer↔seller graph** (who talks to whom). **Federation
  splits this exposure across operators; it does not remove it.** Any node or
  buyer should weigh which coordinator(s) it pins accordingly. This is an
  explicit, documented threat-model limit, not a silent gap.
- **Coordinators are not a guarantee of demand or supply.** A coordinator can only
  route work that exists and capacity that is offered; it cannot conjure either.

---

## 5. Payment-path blast radius (Phase 3, deferred but scoped now)

This matters now, because **we cannot freeze the escrow program immutable until
this flow is settled** — so Phase 1–2 must not foreclose any Phase 3 option.

### What T3.1/T3.3 do to FR-P2/P3

- **FR-P2 (x402 challenge)**: the challenge stops being a `402` response to an
  inbound request and becomes a message **pushed down the pull channel** (the
  subscription/queue handle the node already holds). The *content* of the
  challenge (`scheme:x402`, `network`, `escrow_account`, `offer`) is unchanged
  (§1.2). Agent's proof travels back up as a queue message. The agent may poll
  the queue for the challenge+result instead of one blocking HTTP call.
- **FR-P3 (verification)**: `SolanaPaymentVerifier` must **stay on the node**
  against the node's own `--rpc-url` (invariant #3). The queue operator / relay
  must have **no** input to payment verification. This currently holds
  (verifier is node-local) and must be preserved when the challenge/result
  channel changes.

### Escrow interaction

- `pay_for_compute` is initiated by the **buyer** (the agent) on-chain — this
  does not require the node to be dialable at all. Good: outbound-only does not
  impede the buyer's deposit.
- `finalize_pro_rata` is signed by the **settlement authority** (operator key,
  pinned in `Config`). Its input `f` comes from `vtessera-settle` (module 3,
  transported via the shared state dir / receipts). **Outbound-only does not
  change where `f` is computed** — settlement stays node/operator-local. The
  only Phase-3-specific change to the escrow-relevant path is the **lease-expiry
  receipt** (T3.2): a node that vanishes mid-job must produce a receipt that
  lets settlement compute `f` and refund via `finalize_pro_rata`, so escrowed
  funds are never stranded. That requires a **lease-expiry / node-death receipt
  path in `settlement`** — which is a deliberate, if narrow, reach into module 3
  (invariant #5 is a *guard against unrelated* changes, not a bar on this
  specific, required path). Under federation, **leases are per-coordinator**
  (§4b-4), so the lease-expiry receipt references the coordinator whose lease
  lapsed — which does not change where `f` is computed (still
  node/operator-local). The coordinator **never** supplies `f` or any
  `finalize_pro_rata` input (§4b-7).

### Does Phase 1 foreclose anything?

**No**, provided two things hold:
1. **`endpoint`/reaching info stays in the offer through Phase 1** (§1.3) so the
   queue URL can be advertised; if `endpoint` were removed prematurely, the
   pull-based placement would have no rendezvous. We are not removing it (§4).
2. **The coordinator / relay never sees payment or job content** (invariant
   #2) and **never touches money** (invariant #5b). As long as the pull channel
   is end-to-end encrypted to the node (iroh QUIC with the node's Ed25519
   pinning, `vtessera_ALPN`), the coordinator is a dumb conduit. This is
   unchanged from today's iroh bridge, extended to the coordinator surface
   (§4b-7).

**Risk to watch:** if the pull channel is a *plaintext* HTTP queue (not over an
iroh tunnel), then job contents and payment proofs would transit an intermediary
— violating invariant #2. Therefore the work queue must be **carried over the
existing iroh QUIC path (or equivalent end-to-end encryption)**, not a new
plaintext HTTP queue. This is the single most important constraint to carry from
Phase 1 into Phase 3.

---

## 6. Test plan split

### 6a. Reproducible now (in-repo CI-capable)

These run in the normal workspace without real network hostility:

1. **No-listening-socket in every mode.** Extend the `no_socket.rs` snapshot
   pattern (`/proc/self/net/tcp[6]` LISTEN diff) from `vtesserad` to
   `vtessera-node` under `--features serve`: assert **no new LISTEN socket** in
   metering-only, job-accepting (inbound+dialable is exempt by definition;
   `outbound-only` mode must be empty), and after an iroh endpoint starts.
   This is invariant #1's regression test.
2. **Reconnect / backoff convergence against a fake transport.** A test-only
   transport that **drops connections in-process** (a stub `Transport` whose
   `open`/`send` fails on a schedule) and assert: on N forced drops, the
   reconnect loop re-establishes with **exponential backoff + jitter**, never
   spins, and reaches a steady connected state. Also assert no **duplicate
   registrations** after reconnect. This is T1.4's convergence, tested without
   CGNAT.
3. **Heartbeat signature verification (unit).** Add signing to the heartbeat
   payload (invariant #4/T1.4) and a unit test that `verify()` accepts a
   correctly-signed heartbeat and rejects a forged/mutated one. Mirrors the
   offer sign/verify pattern (`offer/src/lib.rs:237-309`).
4. **Dual-stack flag matrix (unit/config).** Assert that `outbound-only` skips
   `TcpListener::bind` and `inbound+dialable` retains it; assert the default.
5. **EndpointId derivation (unit).** `SecretKey::from_bytes(node_key).public()`
   yields a stable `EndpointId`, and the same node key yields the same
   `EndpointId` (no second key) — pins §3.
6. **x402 logic unit tests unchanged.** `classify_job_request` /
   `payment_required_body` still pass (they're transport-agnostic; §1.2) —
   guards that the queue-based challenge reuses the same payload.
7. **Coordinator cannot touch money (§4b-7 / invariant 5b).** Unit/ser-de test:
   construct a coordinator message and assert it cannot carry or alter
   `f`/`payout_id`/payment fields (the coordinator message type simply has no
   such fields), and that no coordinator code path parses escrow or payment
   input. Under federation, also assert two coordinators can independently assign
   the same opaque id without collision (per-coordinator namespace, §4b-3).
8. **Offer schema-version lockstep (§3).** Unit tests: an offer signed at version
   N verifies; a signed offer whose bytes do not match its claimed version's
   layout fails canonical acceptance; a consumer rejects an unsupported/lower
   schema version instead of guessing; a version N offer with the **full
   pubkey** + **list endpoint** parses and dials by `EndpointId` (§3, §4a).
9. **Relay plurality / TCP fallback (unit + config).** Assert the node's relay
   pool accepts a **list** of relays (self-hosted + third-party), not a single
   N0 URL (invariant #2 row); assert fallback order direct→relay; assert the
   relay URL can be `wss://…:443` and that a config change to the relay list is
   picked up.
10. **Federation failover (unit/in-process).** With a test-only coordinator stub,
    assert the degradation chain A→B→direct (§4c) never yields a 503, that a
    coordinator's advisory delist/refuse does **not** eject the node globally or
    affect a second coordinator, and that lease-expiry receipts are
    per-coordinator (§4b-4, §5).

### 6b. Requiring infra we don't have (skipped tests + manual runbook)

Written as **correct-but-skipped** integration tests (`#[ignore]` with a
documented manual runbook), because they need real hostile networks / hardware
we can't provision here:

1. **Connected-from-behind-CGNAT (§T1.1).** `#[ignore]` test: node reaches
   "connected" with zero config and `ss -ltn` shows no process-owned LISTEN
   socket. **Runbook:** run `vtessera-node --connectivity outbound-only...` on a
   CGNAT/Starlink/4G hotspot; check `ss -ltn` + the connection-mode metric.
2. **Stay-relayed job (§T1.2).** `#[ignore]` test: a job completes end-to-end on
   a connection that stays relayed for its full duration. **Runbook:** on a
   hotspot, force relay (block direct UDP hole-punch to peer), submit, confirm
   `relay` mode metric and job success.
3. **UDP-all-blocked → relay-over-TCP fallback (§T1.3).** `#[ignore]` — *now
   expected to pass via iroh's WebSocket-over-TCP relay path (§2 resolved
   finding), not a custom transport*. **Runbook:** block all outbound UDP on the
   host firewall (`-A OUTPUT -p udp -j DROP`) while TCP (and ideally 443)
   remains up, confirm the node keeps a working relayed connection over
   WebSocket/TCP, mode shown in Status tab, no user prompt.
4. **100 forced disconnects + suspend/resume (§T1.4).** `#[ignore]` stress.
   **Runbook:** loop `ip link set dev <iface> down/up` / toggle the VPN 100×;
   suspend/resume a laptop; assert recovery with no manual intervention and no
   duplicate registrations.
5. **Index-killed mid-run degrades, breaks nothing (§T2.2).** `#[ignore]`.
   **Runbook:** run `offer-index-demo`, `kill` the index process, confirm the
   node degrades to DHT/queue resolution (not failure) and jobs still flow.
6. **Node killed mid-job → partial settlement (§T3.2).** `#[ignore]`.
   **Runbook:** start a paid job, kill the node mid-run, confirm lease expiry
   produces a receipt path allowing settlement to compute `f` and refund the
   remainder via `finalize_pro_rata` — not a hung escrow.
7. **devnet pay→run→settle→split over relayed transport (§T3.3).** `#[ignore]`
   (pins the Solana 1.18-excluded `devnet-demo` tree). **Runbook:**
   `scripts/x402-demo.sh` re-pointed at a relayed-only node; confirm zero
   regressions vs the existing soak suite.
8. **10-node payload swarm (§T4.1)** and **relay egress dominated by control
   (§T4.2).** `#[ignore]` scale tests. **Runbook:** spin 10 nodes on a LAN; one
   agent origin serves a multi-GB blob; measure origin bandwidth vs 10
   independent downloads; measure relay egress share.

### Where the tests live

- New `crates/transport/tests/` for T1.1–T1.4 convergence + EndpointId (6a-2/3/5),
  relay-plurality/fallback (6a-9), and federation-failover (6a-10).
- New `crates/coordinator/` (Phase 1, §7b) with `crates/coordinator/tests/` for
  no-money (6a-7), per-coordinator namespaces, and advisory-only gating.
- New `crates/node-api/tests/no_socket_node.rs` (6a-1) gated on `--features serve`.
- `crates/offer-index/tests/` or `crates/offer/tests/` for heartbeat
  sign/verify (6a-3) and schema-version lockstep (6a-8).
- `#[ignore]` integration tests alongside the above (6b), each with a runbook
  header comment.

---

## 7. Reviewer decisions (resolved) and Phase 1 breakdown

### 7a. Prior open decisions — now resolved

| # | Decision | Resolution |
|---|---|---|
| 1 | WSS fallback (T1.3) | **Deferred/no-op** — iroh's relay path is WebSocket-over-TCP(S), so UDP-blocked networks reach a relay over TCP/443 without a custom transport (§2 finding). Remaining edge (all-TCP-blocked) is a documented accepted gap. |
| 2 | Queue rendezvous in Phase 1 | **Confirmed** — queue/coordinator URL rides in the offer `endpoint` field, which **stays** in the schema and accommodates a **list** (IP/port + queue URL) (§4a). Queue-reversal: coordinator **built in Phase 1**. |
| 3 | Dual-stack flag name/default | **Confirmed** — `inbound+dialable` default during transition, flip later, remove listener last (§4). |
| 4 | Heartbeat signature retrofit | **Confirmed** — implement to `docs/INTERNET-CONNECTIVITY.md` spec (invariant #4/T1.4). |
| 5 | `endpoint`-vs-`node_id` semantics for T2.1 | **Confirmed** — offer carries the **full pubkey/EndpointId**, not truncated `node_id`; add an explicit **offer `schema_version`** field in lockstep with `canonical_bytes`; old-version offers rejected (§3). |
| 6 | T3.2 lease-expiry receipt | **Confirmed in scope** — narrow reach into `settlement` is required to avoid hung escrows; leases are now **per-coordinator** (§5). |
| 7 | Consent disclosure scope (§4d) | **Doc-only for now** — `docs/CONSENT.md` lines added; GUI disclosure surface deferred to P1.8 with the GUI rework (persistent-outbound T5.1) (§7c). |
| 8 | Coordinator trust default | **Opt-in, no coordinator by default** — v1 nodes default to direct dial; operator explicitly pins a coordinator (§7c). |

**Carried into §0 §4b federation obligations:** coordinator identity = pubkey;
coordinators sign what they emit; no global namespace; multi-coordinator without
double-commit; gating advisory; revocation lists advisory; **no coordinator
touches money** (invariant 5b + test); direct dial retained; degradation not
fail-closed; metadata threat model disclosed; honest description
(`docs/CONSENT.md`).

### 7b. Phase 1 task breakdown

Scoped from the §1 inventory + the federation/queue/relay findings. Design-only;
no code this session. Ordered so the queue/coordinator is available when the
node flips outbound-only, and payment/escrow modules are untouched.

**P1.1 — Offer schema evolution (`crates/offer`).**
- Add explicit `schema_version`; make `endpoint` a **list** (IP/port + queue
  URL); carry the **full pubkey/EndpointId** (not truncated `node_id`); keep
  `canonical_bytes` in lockstep with the version; consumers reject unsupported
  versions.
- Tests: 6a-8 schema lockstep + rejection.

**P1.2 — Signed heartbeat (`crates/offer-index`, `crates/offer`).**
- Add heartbeat signing to `docs/INTERNET-CONNECTIVITY.md` spec (invariant #4;
  today unsigned at `offer-index/src/lib.rs:581-607`).
- Tests: 6a-3 sign/verify + forge-reject.

**P1.3 — Outbound-only flag + no-listener node (`crates/node-api`, `vtessera_node`).**
- Add `--connectivity outbound-only | inbound+dialable` (default
  inbound+dialable during transition); `outbound-only` skips
  `TcpListener::bind` (`vtessera_node.rs:1106`); port the `serve` bottleneck to
  outbound dispatch.
- Tests: 6a-1 node-level no-socket; 6a-4 flag matrix.

**P1.4 — EndpointId derivation prep (`crates/transport`).**
- Pin that the node's iroh endpoint derives deterministically from the FR-M2
  key (`SecretKey::from_bytes(node_key).public()`), full-pubkey dial target.
- Test: 6a-5.

**P1.5 — Coordinator service (new `crates/coordinator`, federation-capable, opt-in).**
- Coordinator identity = pubkey; signs what it emits; per-coordinator namespaces
  (job/lease/registration ids); multi-coordinator register without capacity
  double-commit; advisory gating (refuse work / revoke lease / delist from its
  own index); per-coordinator leases; **no money touch** (no escrow/payment
  fields parse); direct dial fallback; degradation A→B→direct, never 503.
- **Opt-in by default (§7c):** a node defaults to **no coordinator** (direct
  dial); pinning a coordinator is an explicit operator choice, so the N0 (or any)
  coordinator is never the default trust posture.
- **Carried over the iroh QUIC path** (Ed25519 pinned, `vtessera_ALPN`), not
  plaintext HTTP (invariant #2).
- Tests: 6a-7 no-money, 6a-10 failover; federation unit tests.

**P1.6 — Relay plurality (`crates/transport` config).**
- Node relay pool accepts a list (self-hosted + third-party); fallback
  direct→relay; `wss://…:443` URL support.
- Test: 6a-9.

**P1.7 — Agent + discovery adapt (`crates/agent-cli`).**
- `submit` (sync-result-in-response, `main.rs:266-297`) and `health`/`offer`
  adapt to queue rendezvous + full-pubkey dial; local-discovery JSON
  (`main.rs:55-62`) endpoint→EndpointId/queue.
- Scripts: `local-stack.sh`, `x402-demo.sh`, `offer-index-demo.sh` node-dial legs
  repointed at the queue.

**P1.8 — GUI adapt (`crates/vtessera-gui`).**
- Port/endpoint/UPnP/`--bind` listener model → EndpointId/queue URL
  (settings `schema`, daemon discovery write, reuse probe); T5.1 consent surface
  (persistent outbound connection).
- **Consent disclosure scope is doc-only for now (§7c):** no separate GUI
  coordinator prompt in v1's initial GUI; the coordinator/metadata and
  persistent-outbound disclosures land via `docs/CONSENT.md` (its §3
  precision-in-claims table, rows already added), with GUI surfacing scoped to
  T5.1's persistent-outbound status.

**P1.9 — `docs/CONSENT.md` + hardening docs.**
- Coordinator-can-stop-sending-work disclosure; relay plurality; metadata
  threat model; no-money boundary. (CONSENT rows for the first two are already
  added; see §7c/P1.8 for the deferred GUI surface.)

**Out of Phase 1 scope (deferred deliberately):** escrow program changes,
`finalize_pro_rata`/settlement-input changes (Phase 3), WSS custom transport
(no-op per §2 finding).

### 7c. Flag to reviewer — resolved

- **Consent disclosure scope (§4d / P1.9):** **doc-only for now.** The
  coordinator-only exposure (metadata graph, advisory gating) and
  persistent-outbound behavior get `docs/CONSENT.md` lines (done — its §3
  precision-in-claims table) now; a GUI disclosure surface is deferred to Phase
  1 **P1.8** (when the GUI is reworked for the EndpointId/queue model and the
  persistent-outbound consent surface T5.1 lands). Not a separate prompt in v1's
  initial GUI.
- **Coordinator trust default:** **opt-in — no coordinator by default.** A v1
  node defaults to **direct dial** only (outbound iroh connection, no
  coordinator pinned); an operator explicitly pins a coordinator to use one.
  This best matches the §4b requirement that "no single operator's concentration
  is a protocol assumption." The N0-operated coordinator is available but never
  the default.

### 7d. Phase 2 status — resolver (T2.1) + index convergence + §6b tests

Phase 2 implementing the resolver from §4a/§1.3 ("`endpoint` → `node_id` once a
resolver exists"). `CONNECTIVITY-PLAN.md` (referenced in §8) is **not in this
repo**, so Phase 2 is scoped from this doc's own T2.1, §6a, and §6b text.

| Item | What landed | Where |
|---|---|---|
| T2.1 resolver type | `parse_endpoint_id` (hex **and** iroh blob form) + `endpoint_addr_from_candidates` — an index entry (`endpoint_id` + `candidates`) reconstructs a dialable `EndpointAddr` | `crates/transport/src/iroh_sidecar.rs` |
| Agent dial-by-id | `--node-id <EndpointId>` for `health`/`offer`/`submit`: resolve via offer-index → dial over iroh QUIC → HTTP-over-QUIC on `vtessera/0` (the node's `VtesseraHandler` wire format). No offer `endpoint` required | `crates/agent-cli/src/main.rs` (`quic_health`/`quic_offer`/`quic_submit`, `resolve_addr`) |
| `discover` | Includes outbound-only nodes (unchanged behavior — previously dropped when `endpoint` was empty); dedups by `endpoint_id` (fallback `endpoint`); new `REACH`/`DIAL` columns + dial-by-id hint | `crates/agent-cli/src/main.rs` |
| 6a-8 (schema lockstep) | Transport-level resolve-→dial end-to-end test over offline loopback | `crates/transport/src/iroh_sidecar.rs` tests |
| Index convergence (T1.4/6a-2) | `register` no longer wipes `candidates` / `endpoint_id` / `last_heartbeat_unix` on re-register (publish loop refresh); tests: re-register preserves resolver state; re-heartbeat after re-register updates candidates with one entry (no duplicates) | `crates/offer-index/src/lib.rs` |
| Aggregated "go over index" overview | (recommended §6a/§6b follow-up) — see checks | – |
| Honest reachability (4b-7/§4b) | `QueueClient::probe` performs a real QUIC dial; agent `--queue health/offer` reports `reachability` based on the actual handshake, erroring (non-zero) when the coordinator is unreachable; live-coordinator and dead-coordinator probe tests | `crates/coordinator/src/iroh.rs`, `crates/agent-cli/src/main.rs` (`queue_render`) |
| §6b infra tests | `crates/transport/tests/infra.rs` with runbook headers: stay-relayed, relay plurality, 100-cycle disconnect/reconnect soak, two-peer resolver interdial. Network cases `#[ignore]` (run `-- --ignored`) | `crates/transport/tests/infra.rs` |
| x402 paid parity (1.2/FR-P2)* | Agent `submit` (HTTP **and** `--node-id` QUIC) now reads the HTTP status: 402 renders the full x402 challenge + pay-then-resubmit guidance; `--payment '{"tx","amount_micros"}'` attaches the `x-payment` proof header and re-submits; a proof that is still rejected is a hard error. HTTP path builds its own ureq agent with `http_status_as_error(false)` so the 402 body survives; shared classifier + renderer | `crates/agent-cli/src/main.rs` (`post_job`, `render_submit_outcome`, `print_x402_challenge`, `--payment`) |
| Marketplace resolver (nodes.json)** | Nodes publish iroh `candidates` + `endpoint_id` with marketplace registration (same data as an index heartbeat); the `marketplace.yml` workflow persists both into the entry. `--node-id` resolution tries the offer-index first (freshest heartbeats) then the marketplace entry's candidates, so a node visible only on the marketplace is dialable over iroh QUIC. Backward compatible: legacy entries without `candidates` keep matching by the offer body's `endpoint_id` | `crates/node-api/src/bin/vtessera_node.rs` (`register_with_marketplace_with_ip`, `spawn_marketplace_registration`), `.github/workflows/marketplace.yml`, `crates/agent-cli/src/main.rs` (`resolve_node_candidates`, `marketplace_entry_for_node`, `marketplace_find_node`) |
| Coordinator federation (§4b-4/§4c, 6a-10)*** | Repeated `--coordinator-addr` pins a **list** of coordinators (preference order A, B, …); the node runs one pull task per coordinator. A dead coordinator is isolated/retried — jobs on the others still flow, the process never fails closed into a 503, and one coordinator's death ejects nothing globally. Per-coordinator registration is exercised by a best-effort signed lease request per coordinator. e2e failover test: node pinned to [A, B]; a job on A runs, A's router+endpoint are killed, a job on B still runs and both queues drain | `crates/node-api/src/bin/vtessera_node.rs` (`spawn_coordinator_pull`, `request_coordinator_lease`, `COORDINATOR_DIAL_TIMEOUT`), `crates/node-api/tests/coordinator_pull.rs` (`node_fails_over_from_dead_coordinator_a_to_b`); per-coordinator namespace + advisory-only gating + lease scoping already unit-tested in `crates/coordinator/src/lib.rs` |

\* x402 client flow: shorter x402 parity carries the agent through the 402
challenge; the actual `spl-token transfer` to the escrow happens out-of-band
(AGENTS.md). Queue-rendezvous + `--payment` rejects loudly (not supported).

\*\* Marketplace resolver: deploying the workflow change requires merging to
`main` (GitHub reads the workflow from the default branch); existing
`nodes.json` entries gain `candidates` only after their node re-registers
(hourly).

\*\*\* Coordinator federation: the node drains **all** pinned coordinators each
poll cycle (each gets one `COORDINATOR_DIAL_TIMEOUT` budget), so a job posted
to any of them reaches the node while that coordinator is reachable. §4c's
"dispatch through A, fail over to B, else direct" is expressed at the agent
(which chooses the coordinator it pins) and at the node as "A down → B still
drains, direct dial never needed from an outbound-only node". The agent's
`choose_dispatch_path` A→B→direct cascade is exercised at the lib level in
`crates/coordinator`.

All Phase 2 debt from this table is closed. Next-phase candidate (out of this
doc's scope): the aggregated "go over index" overview from §6b.

---

## 8. References

- `CONNECTIVITY-PLAN.md` — the 5-phase directive this design reviews.
- `crates/node-api/`, `crates/offer/`, `crates/offer-index/`,
  `crates/transport/`, `crates/agent-cli/`, `crates/vtessera-gui/`,
  `crates/vtesserad/tests/no_socket.rs`, `crates/settlement/`,
  `programs/vtessera-escrow/`.
- `packaging/vtesserad.service`, `scripts/*.sh`.
- `docs/INTERNET-CONNECTIVITY.md`, `docs/CONSENT.md`, `docs/PRD.md`,
  `ROADMAP.md` §2e.
- `iroh-base-1.1.0/src/key.rs`, `iroh-base-1.1.0/src/endpoint_addr.rs`,
  `iroh-1.1.0/src/socket.rs`.
- `iroh-relay-1.1.0/src/client.rs`, `client/streams.rs`, `server/server.rs`,
  `server/http_server.rs`, `defaults.rs` — relay wire protocol (WebSocket-over-
  TCP, optional QUIC) for the §2/T1.3 finding.
