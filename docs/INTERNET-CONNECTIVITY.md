# Internet Connectivity Architecture

This document specifies how vtessera nodes become reachable from the
public internet, how agents discover and connect to them, and how the
system handles NAT traversal — the hard problem that makes or breaks a
decentralized compute marketplace.

**Design principles:**
- The index is untrusted infrastructure — it introduces peers but never
  reads or MITMs traffic.
- Nodes are identified by keypair, not address. Registrations are signed.
- Transports are pluggable: `{type, addr}` — QUIC-direct, Tailscale,
  relay.
- Authorization is per-node, not per-network.
- v0 uses public STUN/TURN; self-hosted relay is a future milestone.

---

## 1. Components

```
┌─────────┐     ┌──────────────────┐     ┌─────────┐
│  Agent   │────▶│  Public Index     │◀────│  Node    │
│  (buyer) │     │  (signaling +     │     │  (seller)│
│          │     │   discovery)      │     │          │
└────┬─────┘     └──────────────────┘     └────┬─────┘
     │                                          │
     │         ┌──────────────┐                 │
     └────────▶│  QUIC / HTTP  │◀────────────────┘
               │  (direct or   │
               │   via relay)  │
               └──────────────┘
```

| Component | Role | Deployment |
|-----------|------|------------|
| **Public Index** | Discovery, signaling, candidate exchange | VPS / cloud |
| **STUN server** | Reflexive address discovery | Public (Google, Cloudflare) for v0 |
| **TURN relay** | Fallback for symmetric NAT pairs | Public (OpenRelay) for v0, self-hosted later |
| **Node** | Advertises compute, serves jobs | Seller's machine |
| **Agent** | Submits jobs, pays for compute | Buyer's machine |

---

## 2. Public Index — Push-Capable Signaling Server

The existing offer-index (`crates/offer-index/`) is REST-only and
pull-based. For internet connectivity it needs three upgrades:

### 2.1 Signed Registrations

A node's `POST /offers` already sends a `SignedOffer`. The index
verifies the Ed25519 signature and binds the entry to the public key.
No change needed here — this is already correct.

### 2.2 Heartbeats with TTL

Nodes must heartbeat to prove liveness. Without this, the index
serves stale entries to agents who then waste time trying unreachable
peers.

```
POST /offers/{node_id}/heartbeat
Headers: X-Signature: <ed25519-sign("heartbeat:{node_id}:{timestamp}")>
Body: { "timestamp": 1234567890, "candidates": [...] }
```

- Index updates `last_seen_unix` and replaces `candidates`.
- Entries older than `TTL` (default 120s) are pruned.
- Heartbeat interval: 30s (node-side), configurable.

The signature prevents a third party from injecting fake heartbeats
for another node's keypair.

### 2.3 Push-Capable: WebSocket + SSE

For hole-punching, both sides must exchange candidate addresses
simultaneously. A REST polling index can't do this. The index must
push events to connected clients.

**Implementation:** WebSocket upgrade on `GET /ws` (or SSE fallback
for constrained clients).

```
GET /ws
Upgrade: websocket
```

Events pushed to agents:
```json
{
  "type": "node_registered",
  "node_id": "...",
  "capabilities": {...},
  "timestamp": 1234567890
}
```

Events pushed to nodes (for candidate exchange):
```json
{
  "type": "connect_request",
  "from_agent": "...",
  "target_node": "...",
  "session_id": "...",
  "candidates": [...]
}
```

**For v0:** SSE is simpler and sufficient for the signaling channel.
WebSocket is a later optimization for bidirectional candidate exchange.

---

## 3. Candidate Exchange — ICE-Style

When an agent wants to connect to a node, both sides gather candidate
addresses and exchange them through the index.

### 3.1 Candidate Types

```rust
pub enum CandidateType {
    Host,       // local IP:port (LAN)
    ServerReflexive, // STUN-derived public IP:port
    PeerReflexive,   // discovered during connectivity check
    Relayed,    // TURN relay address
}

pub struct Candidate {
    pub kind: CandidateType,
    pub transport: Transport,  // QUIC, Tailscale, HTTP
    pub addr: SocketAddr,
    pub priority: u32,
    pub foundation: String,
}
```

### 3.2 Transport Enumeration

```rust
pub enum Transport {
    /// Direct QUIC connection to the candidate address.
    QuicDirect,
    /// Tailscale WireGuard tunnel — addr is the Tailscale IP.
    Tailscale { tailscale_ip: IpAddr },
    /// HTTP/HTTPS fallback for environments where QUIC is blocked.
    Https,
}
```

Transports are advertised in the offer's `transport` field. The index
stores them; agents pick which transport to attempt.

### 3.3 Connection Flow

```
Agent                          Index                          Node
  │                              │                              │
  │── GET /offers ──────────────▶│                              │
  │◀── [{node_id, candidates}] ──│                              │
  │                              │                              │
  │── POST /offers/{id}/connect ─▶                              │
  │   {session_id, candidates}   │── WS push ──────────────────▶│
  │                              │                              │
  │◀── WS push ─────────────────│◀── POST /sessions/{id}/answer─│
  │   {candidates}               │                              │
  │                              │                              │
  │──── connectivity checks (STUN binding requests) ────────────▶│
  │◀──────────────────────────────────────────────────────────── │
  │                              │                              │
  │─────── successful pair found, QUIC handshake ───────────────▶│
  │◀──────────────────────────────────────────────────────────── │
  │                              │                              │
  │─────── job submission over QUIC ────────────────────────────▶│
```

### 3.4 Priority and Pair Nomination

Candidates are prioritized:
1. LAN (Host) — lowest latency
2. Tailscale — encrypted, no NAT traversal needed
3. Server Reflexive (STUN) — direct, no relay cost
4. Relayed (TURN) — works for symmetric NAT, bandwidth cost

The agent tries candidates in priority order (trickle ICE). First
successful connectivity check wins.

---

## 4. NAT Traversal

### 4.1 STUN — Reflexive Address Discovery

Nodes probe a public STUN server to learn their public (reflexive)
address. This is a simple UDP request/response:

```
Node ──STUN Binding Request──▶ stun.l.google.com:19302
Node◀── STUN Binding Response (XOR-MAPPED-ADDRESS) ──
```

The reflexive address is included in heartbeats. If the node is behind
a cone NAT, agents can connect directly to the reflexive address.

**v0 STUN servers:** `stun.l.google.com:19302`,
`stun.cloudflare.com:3478`

**Crate:** `stun` (or `webrtc-rs/stun`) — lightweight, no async
runtime dependency.

### 4.2 UDP Hole Punching

For cone NATs (most home routers), both sides send UDP packets to each
other's reflexive addresses simultaneously. The NAT creates a mapping
that allows the return packet through.

```
Agent NAT                          Node NAT
    │                                  │
    │◀──── agent sends to node_reflex ─│
    │──── node sends to agent_reflex ──▶│
    │                                  │
    │  (NAT creates mapping, packets   │
    │   now flow both directions)      │
```

**Failure mode:** Symmetric NAT (both sides) — packets go to the
wrong port on the peer's NAT. This is where TURN is needed.

### 4.3 TURN Relay Fallback

When hole-punching fails (symmetric NAT on both ends), the index
provides a TURN relay allocation. The relay forwards packets between
agent and node, trading latency and bandwidth for connectivity.

**v0 TURN servers:** Public TURN from OpenRelay
(`openrelay.metered.ca:80` / `:443`) or metered.ca free tier.

**Crate:** `webrtc-rs/turn` for relay allocation, or a simple
custom relay over QUIC.

**Cost model:** The relay is bandwidth-metered. For v0 with public
TURN, the cost is borne by the relay operator. For self-hosted relay,
it's the node operator's VPS bandwidth.

### 4.4 Relay as Last Resort

```rust
pub enum ConnectionStrategy {
    DirectQuic(SocketAddr),        // try QUIC to reflexive/host addr
    Tailscale(TailscaleAddr),      // WireGuard tunnel
    Relay(RelayAllocation),        // TURN relay allocation
}
```

The agent tries strategies in order. If all direct strategies fail
within a timeout (5s), it falls back to relay. The index provides relay
allocations via `POST /relay/allocate`.

---

## 5. Transport Layer

### 5.1 Pluggable Transport

```rust
pub struct TransportConfig {
    pub kind: TransportKind,
    pub bind_addr: SocketAddr,
    pub tls: Option<TlsConfig>,
}

pub enum TransportKind {
    /// Direct QUIC connection.
    QuicDirect,
    /// Tailscale — uses the Tailscale network stack.
    Tailscale { hostname: String },
    /// HTTP/1.1 or HTTP/2 fallback.
    Https,
    /// Local only — for LAN/mDNS discovery.
    LocalOnly,
}
```

Nodes advertise which transports they support in their offer. The index
stores this; agents pick the best available.

### 5.2 QUIC with Public Key Pinning

QUIC is the preferred transport for internet connections. It provides:
- Built-in TLS 1.3 (no separate handshake)
- Connection migration (useful for mobile agents)
- 0-RTT connection resumption
- Multiplexed streams

**Public key pinning:** The QUIC TLS certificate is the node's
Ed25519 public key (or a certificate derived from it). The agent
verifies the peer's certificate matches the `node_id` from the index.
This means the index can't MITM traffic — it can only introduce peers.

```rust
// Node generates a self-signed cert from its Ed25519 keypair
// Agent pins: expected_pubkey = node_id (decoded from base58)
// QUIC handshake verifies: cert.public_key == expected_pubkey
```

**Crate:** `quinn` (QUIC) + `rustls` (TLS) — no async runtime dep,
works with sync or tokio.

### 5.3 Tailscale as Optional Transport

For private networks (enterprises, privacy-conscious users), Tailscale
provides an encrypted WireGuard tunnel without NAT traversal. The node
advertises its Tailscale IP as a candidate. Agents on the same tailnet
connect directly; agents outside can't.

```rust
pub struct TailscaleCandidate {
    pub tailscale_ip: IpAddr,
    pub hostname: String,
}
```

This is orthogonal to QUIC — Tailscale can carry QUIC or any other
protocol.

---

## 6. Authorization Model

### 6.1 Per-Node Authorization

Authorization is per-node, not per-network. Each node decides who may
request work:

```rust
pub enum AuthorizationMode {
    /// Anyone can request work (free tier).
    Open,
    /// Only agents whose public key is in the allowlist.
    AllowList { keys: Vec<String> },
    /// Agent must present a capability token signed by the node.
    CapabilityToken { issuer_pubkey: String },
}
```

### 6.2 Capability Tokens

For paid work, the x402 payment proof already serves as authorization.
For free work, nodes can issue time-limited capability tokens:

```rust
pub struct CapabilityToken {
    pub agent_pubkey: String,
    pub node_id: String,
    pub expires_at: u64,
    pub max_jobs: u32,
    pub signature: String,  // signed by node's Ed25519 key
}
```

### 6.3 Rate Limits and Quotas

Nodes enforce per-agent rate limits locally. The index doesn't enforce
authorization — it only stores signed offers and heartbeats.

---

## 7. Data Types (Wire Protocol)

### 7.1 Registration (POST /offers) — Already exists

```json
{
  "offer": { /* SignedOffer */ },
  "source": "push"
}
```

### 7.2 Heartbeat (POST /offers/{node_id}/heartbeat)

```json
{
  "timestamp": 1234567890,
  "candidates": [
    {
      "kind": "server_reflexive",
      "transport": "quic_direct",
      "addr": "203.0.113.1:8402",
      "priority": 100
    },
    {
      "kind": "host",
      "transport": "tailscale",
      "addr": "100.64.0.1:8402",
      "priority": 200
    }
  ]
}
```

### 7.3 Connect Request (Agent → Index → Node via WebSocket)

```json
{
  "type": "connect_request",
  "session_id": "abc-123",
  "from_agent": "agent_pubkey_base58",
  "target_node": "node_pubkey_base58",
  "candidates": [
    {
      "kind": "server_reflexive",
      "transport": "quic_direct",
      "addr": "198.51.100.1:9000",
      "priority": 100
    }
  ]
}
```

### 7.4 Connect Answer (Node → Index → Agent via WebSocket)

```json
{
  "type": "connect_answer",
  "session_id": "abc-123",
  "selected_candidate": {
    "kind": "server_reflexive",
    "transport": "quic_direct",
    "addr": "203.0.113.1:8402",
    "priority": 100
  }
}
```

### 7.5 Relay Allocate (POST /relay/allocate)

```json
// Request
{
  "session_id": "abc-123",
  "node_id": "...",
  "signature": "..."
}

// Response
{
  "relay_addr": "relay.example.com:3478",
  "relay_username": "session:abc-123",
  "relay_password": "...",
  "expires_in": 300
}
```

---

## 8. Crate Layout (Proposed)

```
crates/
  offer-index/           # Existing — add WS/SSE push, heartbeat endpoint
  node-api/              # Existing — add candidate gathering, QUIC transport
  transport/             # NEW — pluggable transport layer
    src/
      lib.rs             # Transport trait, TransportConfig, candidate types
      quic.rs            # QUIC transport (quinn + rustls)
      tailscale.rs       # Tailscale transport (shell out to `tailscale`)
      relay.rs           # TURN relay client
  stun/                  # NEW — STUN client for reflexive address discovery
    src/
      lib.rs             # STUN binding request/response, reflexive addr
```

### Existing crate changes

| Crate | Changes |
|-------|---------|
| `offer-index` | Add `heartbeat` endpoint, WebSocket/SSE push, candidate storage |
| `node-api` | Add candidate gathering (STUN probe), QUIC listener, connection negotiation |
| `vtessera-gui` | Add `--transport` flag, relay allocation on startup |

---

## 9. v0 Scope (Public Index + Public STUN/TURN)

| Feature | v0 | v1 |
|---------|----|----|
| Public index (REST) | ✅ | ✅ + SSE push |
| Signed registrations | ✅ | ✅ |
| Heartbeats with TTL | ✅ | ✅ |
| STUN reflexive discovery | ✅ (public servers) | ✅ (self-hosted) |
| UDP hole punching | ❌ design-only | ✅ |
| TURN relay fallback | ❌ design-only | ✅ (public TURN) |
| QUIC transport | ❌ design-only | ✅ |
| HTTP transport (current) | ✅ | ✅ |
| Tailscale transport | ❌ | ✅ |
| Public key pinning | ❌ design-only | ✅ |
| Capability tokens | ❌ | ✅ |
| Self-hosted relay | ❌ | ✅ |
| Rate limiting (per-key) | ✅ | ✅ |

**v0 reality:** Nodes advertise HTTP candidates (LAN + STUN reflexive).
Agents discover nodes via the index, then connect over HTTP. NAT
traversal works only if the node's HTTP port is reachable (public IP or
port-forward). STUN discovers the reflexive address but without hole
punching or TURN, agents behind symmetric NAT cannot connect. Full
NAT traversal (hole punching, TURN relay, QUIC with key pinning) is v1.

---

## 10. Threat Model

| Threat | Mitigation |
|--------|------------|
| Index injects fake offers | Signatures verified; index can't forge |
| Index MITMs traffic | Public key pinning on QUIC TLS |
| Index tracks node IPs | Node sends heartbeat; IP is public anyway for STUN |
| Agent sends malformed job | Node validates JobSpec, rejects with 400 |
| NAT traversal fails for symmetric NAT pair | TURN relay fallback |
| Relay reads traffic | End-to-end encryption (QUIC TLS); relay sees ciphertext |
| Stale entries in index | TTL heartbeats prune dead nodes in 120s |
| DDoS via relay | Rate limit relay allocations per node; bandwidth caps |

---

## 11. Deployment (Future)

| Component | Where | Cost |
|-----------|-------|------|
| Public index | VPS (1 vCPU, 1GB RAM) | ~$5/mo |
| Self-hosted TURN | Same VPS | + bandwidth |
| STUN | Public (Google, Cloudflare) | Free |
| TURN (v0) | OpenRelay / metered.ca | Free tier |
