# Internet Discovery Test Notes — 2026-08-22

## Test Setup

| Component | Address |
|-----------|---------|
| Agent (me) | `192.168.178.47` (home LAN) |
| Node | `172.20.10.2:8402` (phone hotspot) |
| Node ID | `18fa157a9e975b4441cb9a4c2773d120` |
| STUN reflexive | `47.65.242.243:48956` |

## Result: Unreachable

The node is behind a mobile hotspot NAT. Three connection attempts failed:

| Address | Result |
|---------|--------|
| `172.20.10.2:8402` | Unreachable (different subnet) |
| `47.65.242.243:48956` | Unreachable (no port mapping) |
| `192.168.178.82:8402` | Unreachable (node moved networks) |

## Root Cause

STUN discovers the reflexive address but cannot create a port mapping
through the hotspot's NAT. The random port `48956` isn't forwarded to
`172.20.10.2:8402`. Additionally, the reflexive port changed constantly
(32825 -> 34310 -> 44596) confirming **symmetric NAT** — each new STUN
probe gets a different port mapping, making UDP hole punching impossible.

## Review Findings (2026-08-22)

A code review identified the following issues with the hand-rolled
approach:

### Critical bugs in existing code

1. **Candidates gathered once at startup** — when a node changes networks
   it advertises stale addresses until restart. Must re-gather inside the
   heartbeat loop.
2. **Heartbeats are unauthenticated** — any client can POST heartbeats
   with forged candidates, overwriting a node's real address list and
   redirecting traffic.
3. **Relay is unauthenticated** — `vtessera-relay` accepts plain string
   REGISTER with no auth. Any client can claim or evict any node.
4. **Relay has no per-node lock** — concurrent agent requests can
   interleave writes, delivering plaintext to the wrong agent.
5. **Offer endpoint is 127.0.0.1** — agents actually use this to submit
   jobs, so it must be a reachable address.

### Architectural decision: adopt iroh

The hand-rolled STUN/relay/QUIC stack was never going to work for nodes
behind symmetric NAT. Rather than building our own TURN relay, hole
punching, and QUIC transport, we adopt **iroh** which provides all of
this off the shelf:

| What we hand-rolled | What iroh provides |
|---------------------|--------------------|
| `stun_probe()` (90 lines) | Built-in NAT traversal |
| `vtessera-relay` TCP tunnel | Relay servers (public + self-hostable) |
| Candidate/transport model | `EndpointAddr` with live path migration |
| Planned QUIC + key pinning | QUIC with Ed25519 key identity |
| Planned hole punching | DCUtR hole punching |
| Planned TURN fallback | Relay fallback (automatic) |

### What stays unchanged

- **Offer-index** — signed offers, claims, TTL heartbeats (centralized-first
  per ROADMAP, sound design)
- **Ed25519 identity** — maps 1:1 to iroh's `EndpointId`
- **x402 payment flow** — on-chain escrow, no changes needed
- **mini-http server** — iroh is a sidecar, not a replacement for the
  HTTP surface

### What gets removed

- `stun_probe()`, `discover_reflexive_addr()`, `DEFAULT_STUN_SERVERS` —
  replaced by iroh endpoint discovery
- `vtessera-relay` binary — replaced by iroh relay infrastructure
- `CandidateKind::ServerReflexive`, `CandidateKind::Relayed` — iroh
  manages connectivity transparently
- `gather_candidates()` — iroh endpoint tracks live addresses
- mDNS `_vtessera._tcp` registration — dead code, nothing browses it

### What gets added

- `iroh` crate as a connectivity sidecar in `crates/transport/`
- iroh `Endpoint` creation using the existing Ed25519 `SecretKey`
- `EndpointId` stored in the offer-index (replaces candidate list)
- Node connects to iroh relay on startup, maintains connection
- Agent dials by `EndpointId`, iroh handles relay + hole punching
- Live path migration when node changes networks (no restart needed)

## Summary

| Capability | Before (hand-rolled) | After (iroh) |
|------------|---------------------|--------------|
| LAN discovery | Working | Working (iroh LAN candidate) |
| Internet discovery | Broken (STUN only) | Working (relay + hole punch) |
| Symmetric NAT | Unreachable | Relay fallback |
| IP changes | Requires restart | Live path migration |
| Authentication | None | Ed25519 key-based |
| Encryption | None (plaintext relay) | QUIC end-to-end |

## Implementation Status (2026-08-23)

### Completed
- **iroh sidecar** (`crates/transport/src/iroh_sidecar.rs`): `IrohEndpoint`
  wraps `iroh::Endpoint`, maps vtessera's Ed25519 identity to QUIC auth
- **Node binary wired**: `start_iroh_endpoint()` creates endpoint on
  dedicated tokio runtime, `spawn_iroh_router()` handles accept loop
- **Accept loop**: `VtesseraHandler` implements `iroh::protocol::Router`,
  accepts QUIC connections, reads HTTP from bi-directional streams,
  dispatches through same handler as TCP
- **Heartbeat uses live candidates**: iroh endpoint provides relay + direct
  addresses, re-gathered every 30s
- **7 transport tests pass**: endpoint creation, relay connection, relay
  candidates, full QUIC echo roundtrip through relay
- **Dead code removed**: `vtessera-relay` crate, hand-rolled STUN client,
  mDNS registration from GUI

### Remaining
- **Agent-side iroh client**: agents need to connect to nodes via iroh.
  x402-client is excluded from workspace (pins solana-sdk 1.18); better
  implemented as separate `vtessera-agent` crate or workspace example
- **Self-hosted relays**: Number 0's public relays work for now; may want
  to run our own for production
- **Offer-index stores EndpointId**: the index currently stores candidate
  lists; should store `EndpointId` for iroh dial-by-key
