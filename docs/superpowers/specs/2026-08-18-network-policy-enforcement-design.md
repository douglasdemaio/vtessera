# Design spec — Network policy enforcement (Module 1e)

**Date:** 2026-08-18
**Status:** Draft
**Supersedes:** (none — first design for this area)

## Motivation

GPU jobs need network access for model downloads, but the current executor
boots every VM with no NIC. The `NetworkPolicy` enum (`None`, `OutboundHttps`,
`Egress`) exists as a type placeholder but only `None` is accepted — the CH
backend rejects any other policy. This spec wires networking into the CH
backend with three enforcement layers: admission, host-side nftables, and
guest-side iptables.

## Goals

1. Allow `OutboundHttps` and `Egress` network policies in CH jobs.
2. Guest-side iptables enforcement (primary, always active).
3. Optional host-side nftables enforcement on a TAP/bridge for untrusted workloads.
4. CIDR-restricted egress for enterprise/private environments.
5. No network by default (unchanged `None` behavior).

## Non-goals (this spec)

- Private/enterprise network mode (#52) — separate design.
- Job scheduling, queuing, concurrency — separate design.
- systemd-analyze hardening — separate design.
- macvtap backend — config option exists but not tested.

## Architecture

Three enforcement layers, stacked:

```
Job Request (network: "outbound_https")
    │
    ▼
[Admission] ── ch_admission accepts OutboundHttps/Egress
    │
    ▼
[Host tap/bridge + nftables] ── optional, for untrusted workloads
    │
    ▼
[Guest iptables in initramfs] ── always, primary enforcement
```

### Layer 1 — Admission

`ch_admission` in `cloud_hypervisor.rs` stops rejecting non-None policies.
It accepts `None`, `OutboundHttps`, and `Egress`. The policy is passed
through to the CH command and guest manifest.

### Layer 2 — Host-side (optional)

When `net_enforcement = "host"` or `"both"` in config:

1. Before CH launch, executor creates a TAP device and attaches it to a
   Linux bridge.
2. nftables rules are applied on the bridge interface to restrict egress
   according to the policy.
3. After the job ends, the TAP device is cleaned up.

TAP device naming: `vtap-<job_id_first_8_hex>` to avoid collisions.

### Layer 3 — Guest-side (always)

The initramfs runner reads `network_policy` from the manifest and applies
iptables rules inside the guest before executing the job.

## Data flow

### New config fields (`CloudHypervisorConfig`)

```rust
/// Network backend for CH when policy != None.
/// "tap" (default) creates a TAP device + bridge.
/// "macvtap" uses macvtap (better perf, harder to firewall).
pub net_backend: String,           // default: "tap"

/// Bridge name for tap backend.
pub net_bridge: String,            // default: "virbr0"

/// Host CIDR ranges allowed when network = Egress.
/// Empty = all egress allowed. Non-empty = only these CIDRs.
pub net_allowed_cidrs: Vec<String>,

/// Enforcement layer: "guest" (iptables in guest),
/// "host" (nftables on bridge), or "both".
pub net_enforcement: String,       // default: "guest"
```

### Job manifest extension

```rust
pub struct JobManifest {
    // ... existing fields ...
    /// Network policy for this job.
    pub network_policy: String,    // "none" | "outbound_https" | "egress"
    /// CIDRs allowed for egress (only for "egress" policy).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub allowed_cidrs: Vec<String>,
}
```

### CH command changes

When `network_policy != "none"`:

```
--net tap=<tap_dev>,id=<mac_addr>
```

The TAP device is pre-created by the executor. MAC address is derived
deterministically from the job ID for reproducibility.

### Guest init changes

`scripts/build-initramfs.sh` reads `network_policy` from `manifest.json`:

| Policy | Guest behavior |
|--------|---------------|
| `none` | No NIC (current behavior, unchanged) |
| `outbound_https` | Bring up NIC, allow TCP/443, drop all else |
| `egress` | Bring up NIC, allow all (optionally CIDR-restricted) |

iptables rules applied after driver loading, before job execution.

## Detailed changes

### `crates/executor/src/cloud_hypervisor.rs`

1. **Config struct** — add `net_backend`, `net_bridge`, `net_allowed_cidrs`,
   `net_enforcement` fields with defaults.
2. **`ch_admission`** — accept `OutboundHttps` and `Egress` (currently rejects
   anything except `None`).
3. **`JobManifest`** — add `network_policy` and `allowed_cidrs` fields.
4. **`run()` method** — when policy != None:
   - Create TAP device via `ip tuntap add`
   - Attach to bridge via `ip link set ... master`
   - Add `--net tap=<dev>,id=<mac>` to CH command
   - If host-side enforcement: apply nftables rules on bridge
   - After job: delete TAP, remove nftables rules
5. **Host-side nftables** (new function `apply_host_net_policy`):
   - `None` → no rules
   - `OutboundHttps` → `ip daddr != { CIDRs } tcp dport != 443 drop`
   - `Egress` → `ip daddr != { CIDRs } drop` (if CIDRs configured)

### `crates/node-api/src/bin/vtessera_node.rs`

1. Add CLI flags: `--net-backend`, `--net-bridge`, `--net-enforcement`
2. Pass values into `CloudHypervisorConfig`

### `scripts/build-initramfs.sh`

1. Read `network_policy` and `allowed_cidrs` from manifest
2. If policy != "none": load `virtio_net` module, bring up `eth0`, run `udhcpc`
3. Apply iptables rules based on policy:
   - `outbound_https`: `-A OUTPUT -p tcp --dport 443 -j ACCEPT` + `-A OUTPUT -j DROP`
   - `egress` with CIDRs: `-A OUTPUT -d <cidr> -j ACCEPT` (per CIDR) + `-A OUTPUT -j DROP`
   - `egress` without CIDRs: no restrictions

### `ROADMAP.md`

Mark §1e network policy as shipped.

## Testing

### Unit tests

- Config default values for new fields
- Manifest serialization/deserialization with network_policy
- iptables rule generation for each policy
- nftables rule generation for each policy

### Integration tests

- CH boot with `--net tap=...`, verify guest has connectivity (`Egress`)
- Verify `OutboundHttps` blocks non-443 traffic
- Verify `None` has no NIC (existing behavior)
- Host-side: verify nftables rules appear in `nft list ruleset`
- Cleanup: verify TAP device is removed after job ends

### Security tests

- Guest cannot bypass iptables (rule persistence check)
- Host nftables rules survive guest iptables flush
- CIDR restriction blocks non-whitelisted destinations

## Error handling

- TAP creation failure → return `ExecutorError::Backend`, don't start CH
- Bridge not found → return `ExecutorError::Backend` with clear message
- nftables not installed → warn and fall back to guest-only enforcement
- Guest iptables not available → warn but don't fail (guest runs unprotected)

## Design decisions

1. **DHCP vs static IP:** Use a built-in DHCP server (dnsmasq or a minimal
   Rust DHCP responder) on the bridge. This avoids IP range planning and
   works with any guest. The DHCP server runs as a child process alongside
   the CH VM and is cleaned up after the job. For environments where DHCP
   is not desired, a `net_static_ip` config option will be added later.

2. **DNS:** Yes — `OutboundHttps` policy also allows UDP/53 (DNS) and
   TCP/53 (DNS-over-TCP). Without DNS, HTTPS connections to hostnames
   fail. The iptables rules are:
   ```
   -A OUTPUT -p udp --dport 53 -j ACCEPT
   -A OUTPUT -p tcp --dport 53 -j ACCEPT
   -A OUTPUT -p tcp --dport 443 -j ACCEPT
   -A OUTPUT -j DROP
   ```

3. **Bridge setup:** The daemon auto-creates the bridge on first use if it
   doesn't exist (`ip link add virbr0 type bridge`). Requires
   `CAP_NET_ADMIN` (root or appropriate capability). If bridge creation
   fails, return `ExecutorError::Backend` with a message telling the
   operator to create it manually or grant the capability. The bridge
   persists across jobs (created once, reused).
