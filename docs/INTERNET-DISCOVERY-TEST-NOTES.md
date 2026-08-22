# Internet Discovery Test Notes — 2026-08-22

## Test Setup

| Component | Address |
|-----------|---------|
| Agent (me) | `192.168.178.47` (home LAN) |
| Node | `172.20.10.2:8402` (phone hotspot) |
| Node ID | `18fa157a9e975b4441cb9a4c2773d120` |
| STUN reflexive | `47.65.242.243:48956` |

## Result: ❌ Unreachable

The node is behind a mobile hotspot NAT. Three connection attempts failed:

| Address | Result |
|---------|--------|
| `172.20.10.2:8402` | Unreachable (different subnet) |
| `47.65.242.243:48956` | Unreachable (no port mapping) |
| `192.168.178.82:8402` | Unreachable (node moved networks) |

## Root Cause

STUN discovers the reflexive address but cannot create a port mapping
through the hotspot's NAT. The random port `48956` isn't forwarded to
`172.20.10.2:8402`. This is the expected behavior — STUN only works when:

1. The NAT is cone-based (not symmetric) AND
2. The application sends a packet outward to "punch" a hole

Vtessera's current transport layer probes STUN for the reflexive address
but never actually punches a hole. The candidate is advertised but not
routable.

## What's Missing for Internet Connectivity

### Must-have (v0 can ship without, but must document)

1. **Public index** — a shared REST server where nodes register and agents
   discover. The code exists (`vtessera-offer-index`), just needs deployment
   at a stable public URL.

2. **Port forwarding / UPnP** — for home routers, auto-discover the gateway
   and open a port. Not possible on mobile hotspots.

3. **TURN relay fallback** — when direct connection fails, proxy through a
   public server. This is the only reliable solution for symmetric NAT
   (mobile hotspots, corporate networks, CGNAT).

### Nice-to-have (v1)

4. **UDP hole punching** — both agent and relay connect to a rendezvous
   server simultaneously. Works for cone NAT but not symmetric.

5. **Tailscale/ WireGuard transport** — overlay network that bypasses NAT
   entirely. Requires both parties to run a daemon.

6. **QUIC transport** — faster connection setup, better NAT traversal than
   TCP. Currently advertised but not implemented.

## Recommendations for GitHub

### Issue: Internet connectivity only works for directly-reachable nodes

**Labels:** `enhancement`, `internet-connectivity`

The current implementation:
- ✅ Discovers reflexive addresses via STUN
- ✅ Advertises candidates in heartbeats
- ✅ Index aggregates offers from multiple nodes
- ❌ Cannot actually connect to nodes behind NAT
- ❌ No TURN relay
- ❌ No UDP hole punching
- ❌ No port forwarding/UPnP

**Impact:** Nodes on mobile hotspots, CGNAT, or corporate networks are
advertised in the index but unreachable. Agents will discover them,
attempt to connect, and fail silently.

**Suggested fix:** Either:
(a) Implement TURN relay as a v0.5 step before full internet launch
(b) Clearly document that internet mode requires port forwarding or a
    VPS deployment
(c) Add a "connectivity check" that verifies the node is actually
    reachable before publishing its reflexive candidate

### Issue: Offer endpoint should be the reachable address

**Labels:** `bug`, `marketplace`

The offer's `endpoint` field is set to `http://127.0.0.1:8402` regardless
of network configuration. Agents that use the endpoint (rather than the
candidates) will always fail to connect.

**Suggested fix:** Auto-detect the outbound IP and use it as the endpoint,
or use the best candidate address.

### Issue: No public index deployment

**Labels:** `infrastructure`, `marketplace`

The offer-index binary exists but there's no hosted instance. For internet
discovery to work, there needs to be a stable URL that both nodes and
agents can reach.

**Options:**
- Host on a VPS (cheapest: Hetzner ~€5/mo)
- Bundle with the GUI (mDNS for LAN, public index for internet)
- Use a decentralized approach (DNS-SD, DHT) — more complex

## Summary

| Capability | Status |
|------------|--------|
| LAN discovery (same subnet) | ✅ Working |
| LAN job submission (x402) | ✅ Working |
| Internet discovery (different networks) | ❌ No routable path |
| Internet job submission | ❌ Blocked by NAT |
| STUN reflexive discovery | ✅ Working (but useless without hole punching) |
| TURN relay | ❌ Not implemented |
| Public index | ❌ Not deployed |
