# Kata Containers isolation — Module 1 production backend

Date: 2026-08-20
Status: approved design
Related: `ROADMAP.md` §1a, `crates/executor`

## Problem

The Cloud Hypervisor backend (shipped) runs a custom initramfs with a pinned
job runner. This works but buyers can't ship standard OCI images — they must
adapt to Vtessera's initramfs contract. Kata Containers closes this gap:
standard OCI images run directly, giving VM-grade isolation with OCI-native
workflow.

Decision context (brainstorming, 2026-08-20):

- **Runtime:** containerd + kata-shimv2 (modern path, not legacy v1).
- **Hypervisor:** Cloud Hypervisor underneath Kata.
- **Target:** fresh dedicated nodes, Flatpak/package handles setup.
- **GPU:** full VFIO passthrough, reusing `vtessera-gpu` helper.
- **Networking:** Kata CNI for guest plumbing, host nftables enforcement.
- **Metering:** sidecar container in multi-container pod (buyer's image untouched).
- **Scope:** CPU + GPU + networking + setup script, all in one pass.

## Scope

**In scope (this pass):**

- `KataCloudHypervisorConfig` + `KataCloudHypervisorExecutor` implementing
  `Executor`, gated behind a `kata` cargo feature in `crates/executor`.
- containerd gRPC client for pulling images, creating pods, managing lifecycle.
- Sidecar metering container (pre-built OCI image, reads manifest, writes
  `metering.json` via shared volume).
- GPU passthrough via `vtessera-gpc` (PCI bind/unbind) + Kata VFIO device config.
- Host-side nftables enforcement via `apply_host_net_policy()`.
- `scripts/kata-setup.sh` provisioning for fresh nodes.
- `--backend kata-cloud-hypervisor` wiring in `vtessera-node` and `vtessera-mcp`.
- Unit tests (mock containerd, mock GPU).

**Out of scope (future follow-ups):**

- CPU pinning / NUMA awareness (§1b later tier).
- Confidential compute / attestation hooks (Module 3 link).
- MIG/vGPU through Kata (§1c).
- Multi-node job orchestration (Module 2e).

## Architecture

```
JobSpec ──▶ KataCloudHypervisorExecutor (feature `kata`)
              │
              1. admission_check(spec)                   (reuse shared)
              2. select_gpu(spec) via vtessera-gpu       (reuse existing)
                 → bind GPU to vfio-pci
              3. stage workdir:
                 { manifest.json, out/ }
              4. containerd gRPC:
                 a. pull OCI image (JobSpec.image)
                 b. create pod with:
                      - workload container (image + command + env)
                      - metering sidecar container (shared volume)
                 c. start pod
              5. Kata+CH boots microVM with:
                 - VFIO GPU devices passed through
                 - shared filesystem (virtiofs)
                 - CNI networking (Kata-managed)
              6. host-side nftables rules applied
              7. wait for workload container to exit
              8. read metering.json from sidecar via shared volume
              9. cleanup:
                 - tear down pod
                 - unbind GPU via vtessera-gpu
                 - remove nftables rules
                 - clean workdir (unless keep_jobs)
```

## Components

### KataCloudHypervisorConfig

```rust
pub struct KataCloudHypervisorConfig {
    pub containerd_socket: String,       // /run/containerd/containerd.sock
    pub kata_runtime: String,            // "kata" (runtime class name)
    pub kata_config: String,             // /opt/kata/share/defaults/kata-containers/configuration.toml
    pub image_pull_policy: String,       // IfNotPresent
    pub workdir: String,                 // /var/lib/vtessera/jobs
    pub sidecar_image: String,           // ghcr.io/vtessera/metering-sidecar:latest
    pub keep_jobs: bool,                 // false
    pub vfio_devices: Vec<String>,       // PCI addresses for GPU
    pub gpu_helper: String,              // /usr/bin/vtessera-gpu
    pub gpu_time_slice: bool,            // false
    pub gpu_meter_poll_interval_secs: u64, // 5
    pub net_enforcement: String,         // "host"
}
```

Defaults match `CloudHypervisorConfig` where applicable. Kata-specific fields
(`containerd_socket`, `kata_runtime`, `kata_config`, `sidecar_image`) replace
CH-specific fields (`kernel`, `initramfs`, `cmdline`, `virtiofsd_binary`).

### KataCloudHypervisorExecutor

Implements `Executor` trait. Feature-gated behind `kata` cargo feature.

`run()` flow:
1. `kata_admission()` — calls `admission_check()`, validates containerd socket exists, validates Kata runtime is available
2. Memory floor (≥128 MiB, same as CH)
3. GPU selection via `select_gpu()` — reuse from `cloud_hypervisor.rs`
4. Stage workdir + `manifest.json`
5. Containerd gRPC calls:
   - `PullImage` with `ImagePullPolicy`
   - `CreateContainer` for workload container (image, command, env, volume mount)
   - `CreateContainer` for metering sidecar (sidecar image, shared volume mount)
   - `CreatePod` / `RunPod` to start both containers
6. Apply host-side nftables via `apply_host_net_policy()`
7. Poll container status (same 50ms interval as CH backend)
8. On timeout: `StopPod` + `RemovePod`
9. On completion: read `out/metering.json` from shared volume
10. Cleanup: remove pod, unbind GPU, remove nftables, clean workdir

### Metering sidecar

Pre-built OCI image containing a small Rust binary:
- Reads `manifest.json` from shared volume
- Polls CPU metrics from `/proc` (utime+stime, VmPeak)
- Polls GPU metrics via `nvidia-smi` if GPU is present
- Writes `out/metering.json` and `out/result.json` to shared volume
- Exits when workload container exits (Kata terminates sidecar automatically)

The sidecar binary reuses `GuestMetering` / `GuestResult` types from
`crates/executor/src/cloud_hypervisor.rs` (shared wire format).

### GPU passthrough

Same as CH backend:
1. `select_gpu()` reads `gpus.json`, picks matching device
2. `vtessera-gpu` binds PCI device to `vfio-pci`
3. Kata VFIO config passes device into guest
4. After run: `vtessera-gpu` unbinds from `vfio-pci`, restores NVIDIA driver

GPU metering: sidecar runs `nvidia-smi` polling inside guest, host also polls
via `GpuMeter` for cross-validation (same as CH backend).

### Networking

Kata CNI handles guest network namespace setup. Host-side nftables rules
applied after pod starts:
- `NetworkPolicy::None` → no egress (no rules)
- `NetworkPolicy::OutboundHttps` → DNS + TCP/443 + established
- `NetworkPolicy::Egress` → full egress with optional CIDR restrictions

Reuses `apply_host_net_policy()` from `cloud_hypervisor.rs`.

### Setup script

`scripts/kata-setup.sh` — idempotent provisioning for fresh nodes:

```
kata-setup.sh --install
```

Installs:
1. containerd (with Kata runtime class in `/etc/containerd/config.toml`)
2. kata-shim-v2 (from official releases)
3. Cloud Hypervisor (same binary as CH backend)
4. virtiofsd
5. Kata kernel + initramfs (guest boot assets)
6. CNI plugins
7. Kata `configuration.toml` (CH as hypervisor, virtiofs, VFIO passthrough)

Generated by the Flatpak/package post-install hook.

## Wire format

`Backend::KataCloudHypervisor` is already tag 2 in the `Backend` enum and
settlement receipt format. No wire changes needed.

## Testing

- Unit tests: mock containerd gRPC, mock GPU helper, verify admission/pod flow
- Integration tests: require containerd + kata-shimv2 + `/dev/kvm` (skip in CI)
- Settlement tests: backend tag 2 already tested

## Migration path

The CH backend continues to work on existing nodes. The Kata backend is for
new dedicated nodes only. No migration needed — both backends coexist.
