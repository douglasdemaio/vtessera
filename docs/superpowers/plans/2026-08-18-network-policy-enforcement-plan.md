# Implementation plan — Network policy enforcement (Module 1e)

**Date:** 2026-08-18
**Spec:** `docs/superpowers/specs/2026-08-18-network-policy-enforcement-design.md`
**Branch:** `module1-ch-cpu`

## Overview

Wire networking into the Cloud Hypervisor backend with three enforcement
layers: admission accepts non-None policies, guest iptables enforce in the
VM, and optional host nftables enforce on a TAP/bridge.

## Phases

### Phase 1: Config + manifest extension

**Files:** `crates/executor/src/cloud_hypervisor.rs`, `crates/node-api/src/bin/vtessera_node.rs`

1. Add to `CloudHypervisorConfig`:
   - `net_backend: String` (default `"tap"`)
   - `net_bridge: String` (default `"virbr0"`)
   - `net_allowed_cidrs: Vec<String>` (default empty)
   - `net_enforcement: String` (default `"guest"`)
2. Add to `JobManifest`:
   - `network_policy: String` (default `"none"`)
   - `allowed_cidrs: Vec<String>` (default empty, skip_serializing_if)
3. Add CLI flags to `vtessera-node`: `--net-backend`, `--net-bridge`, `--net-enforcement`
4. Pass values into `CloudHypervisorConfig` in `BackendChoice::build`
5. Update `ch_admission` to accept `OutboundHttps` and `Egress` (currently rejects non-None)
6. Write manifest with `network_policy` from `spec.network`
7. Unit tests: config defaults, manifest roundtrip, admission accepts new policies

### Phase 2: Guest-side enforcement

**Files:** `scripts/build-initramfs.sh`

1. Read `network_policy` and `allowed_cidrs` from `manifest.json`
2. If policy != "none":
   - Load `virtio_net` module
   - Bring up `eth0` via `ip link set eth0 up`
   - Run `udhcpc -i eth0` (or static IP if configured)
3. Apply iptables rules based on policy:
   - `outbound_https`: allow UDP/53, TCP/53, TCP/443, drop rest
   - `egress` with CIDRs: allow listed CIDRs, drop rest
   - `egress` without CIDRs: no restrictions
4. If policy == "none": don't bring up NIC (current behavior)
5. Integration test: boot CH with `Egress`, verify guest has connectivity

### Phase 3: CH command wiring

**Files:** `crates/executor/src/cloud_hypervisor.rs`

1. When `spec.network != NetworkPolicy::None`:
   - Create TAP device: `ip tuntap add dev vtap-<job_id_short> mode tap`
   - Attach to bridge: `ip link set vtap-<job_id_short> master <bridge>`
   - Bring up: `ip link set vtap-<job_id_short> up`
   - Generate MAC from job ID (deterministic)
   - Add `--net tap=vtap-<job_id_short>,id=<mac>` to CH command
2. After job ends (success or timeout):
   - Delete TAP: `ip tuntap del dev vtap-<job_id_short> mode tap`
3. Bridge auto-creation: if bridge doesn't exist, create it (`ip link add <bridge> type bridge`)
4. Error handling: TAP/bridge failures → `ExecutorError::Backend`
5. Integration test: CH boot with `--net tap=...`, verify guest gets IP

### Phase 4: Host-side enforcement (optional)

**Files:** `crates/executor/src/cloud_hypervisor.rs`

1. New function `apply_host_net_policy(policy, bridge, cidrs)`:
   - Creates nftables table + chain for the job
   - `OutboundHttps`: drop non-443, allow DNS, allow 443
   - `Egress` + CIDRs: drop non-CIDR traffic
   - `Egress` no CIDRs: no rules
2. New function `remove_host_net_policy(job_id)`:
   - Removes nftables rules for the job
3. Called from `run()` when `net_enforcement == "host"` or `"both"`
4. nftables not available → warn and fall back to guest-only
5. Unit tests: rule generation for each policy

### Phase 5: Tests + ROADMAP

**Files:** `crates/executor/tests/ch_cpu_integration.rs`, `ROADMAP.md`

1. Integration tests:
   - CH boot with `Egress`, verify guest has IP and can reach external
   - CH boot with `OutboundHttps`, verify TCP/443 works, other ports blocked
   - CH boot with `None`, verify no NIC (existing test, unchanged)
   - TAP cleanup: verify device removed after job
   - Bridge auto-creation: verify bridge created on first use
2. Unit tests:
   - iptables rule generation (guest-side)
   - nftables rule generation (host-side)
   - Config defaults
3. Update ROADMAP.md §1e: mark network policy enforcement as shipped
4. Run clippy, fmt, full test suite

## File change summary

| File | Lines (est.) | Phase |
|------|-------------|-------|
| `crates/executor/src/cloud_hypervisor.rs` | +120 | 1, 3, 4 |
| `crates/node-api/src/bin/vtessera_node.rs` | +20 | 1 |
| `scripts/build-initramfs.sh` | +40 | 2 |
| `crates/executor/tests/ch_cpu_integration.rs` | +60 | 5 |
| `ROADMAP.md` | ~5 | 5 |

**Total estimate:** ~245 lines across 5 files.
