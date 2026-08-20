# Kata Containers isolation — Implementation Plan

Spec: `docs/superpowers/specs/2026-08-20-kata-isolation-design.md`
Branch: `module1-kata-isolation` (new, off `main`). One PR at the end.

Host prerequisites (documented, not scripted here): containerd,
kata-shim-v2, Cloud Hypervisor, virtiofsd, CNI plugins installed;
`/dev/kvm` present; VFIO-capable GPU bound to `vfio-pci` (or available
for binding via `vtessera-gpu`).

No amendment to the spec needed — planning found only implementation-level
choices (detailed below), no design gaps.

## Phase 1 — Executor backend (`crates/executor`)

1. `crates/executor/Cargo.toml`:
   - `[features] kata = ["dep:serde_json", "dep:tonic", "dep:prost"]`
   - Add optional deps: `tonic` (gRPC client), `prost` (protobuf), `serde_json`
   - Default build stays dep-free; `kata` feature pulls gRPC deps

2. New `crates/executor/src/kata.rs`, wired as
   `#[cfg(feature = "kata")] pub mod kata;` in `lib.rs`. Contents:

   a. `KataConfig` struct:
      - `containerd_socket: PathBuf` (default `/run/containerd/containerd.sock`)
      - `kata_runtime: String` (default `"kata"`)
      - `workdir: PathBuf` (default `/var/lib/vtessera/jobs`)
      - `sidecar_image: String` (default `"ghcr.io/vtessera/metering-sidecar:latest"`)
      - `keep_jobs: bool` (default `false`)
      - `vfio_devices: Vec<String>`
      - `gpu_helper: PathBuf` (default `/usr/bin/vtessera-gpu`)
      - `gpu_time_slice: bool` (default `false`)
      - `gpu_meter_poll_interval_secs: u64` (default `5`)
      - `net_enforcement: String` (default `"host"`)
      - `image_pull_policy: String` (default `"IfNotPresent"`)

   b. `KataExecutor` struct:
      - `config: KataConfig`

   c. `impl Executor for KataExecutor`:
      - `run()` flow:
        1. `kata_admission()` — calls `admission_check()`, validates
           containerd socket exists, validates kata-shim-v2 is available
        2. Memory floor ≥ 128 MiB (reuse CH logic)
        3. GPU selection via `select_gpu()` — reuse from `cloud_hypervisor.rs`
        4. Stage workdir + `manifest.json`
        5. Containerd gRPC calls:
           - `PullImage` with configured policy
           - `CreateContainer` for workload (image, command, env, volume)
           - `CreateContainer` for sidecar (sidecar image, shared volume)
           - `CreatePod` / `RunPod`
        6. Apply host-side nftables via `apply_host_net_policy()`
        7. Poll container status (50ms interval)
        8. On timeout: `StopPod` + `RemovePod`
        9. On completion: read `out/metering.json` from shared volume
        10. Cleanup: remove pod, unbind GPU, remove nftables, clean workdir

   d. Helper functions:
      - `write_manifest()` — same format as CH backend
      - `parse_metering()` — reuse from `cloud_hypervisor.rs`
      - `apply_host_net_policy()` — reuse from `cloud_hypervisor.rs`
      - `select_gpu()` — reuse from `cloud_hypervisor.rs`

3. Unit tests (mock containerd gRPC, mock GPU):
   - Manifest round-trip
   - `parse_metering`: valid/invalid JSON
   - Admission: GPU class accepted (unlike CH which rejects), network policies accepted
   - Duplicate workdir → error
   - Containerd unavailable → clear error

Verify: `cargo fmt --check`; `cargo clippy -p vtessera-executor --all-targets -- -D warnings`; `cargo test -p vtessera-executor --locked` and `--features kata`

## Phase 2 — Metering sidecar image

4. New `crates/metering-sidecar/` crate (binary):
   - Reads `manifest.json` from shared volume (`/mnt/vtessera`)
   - Polls CPU metrics from `/proc/<pid>/stat` (utime+stime)
   - Polls GPU metrics via `nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader,nounits` if GPU present
   - Writes `out/metering.json` and `out/result.json` to shared volume
   - Exits when workload container exits (Kata terminates sidecar automatically)

5. Reuse `GuestMetering` / `GuestResult` types from `cloud_hypervisor.rs`
   (shared wire format between CH and Kata backends).

6. `Dockerfile` for sidecar image:
   - Base: `rust:slim` (multi-stage build)
   - Binary: `/usr/local/bin/metering-sidecar`
   - Entry: `["/usr/local/bin/metering-sidecar"]`
   - Published to `ghcr.io/vtessera/metering-sidecar:latest`

7. Unit tests:
   - Manifest parsing
   - CPU metric collection (mock `/proc`)
   - GPU metric collection (mock `nvidia-smi` output)
   - Metering JSON output format

## Phase 3 — containerd gRPC client

8. New `crates/executor/src/containerd.rs` (feature-gated behind `kata`):
   - Minimal gRPC client using `tonic` + containerd's protobuf definitions
   - Functions needed:
     - `pull_image(socket, image, policy)` → `Result<()>`
     - `create_container(socket, pod_id, container_config)` → `Result<ContainerId>`
     - `create_pod(socket, pod_config, containers)` → `Result<PodId>`
     - `run_pod(socket, pod_id)` → `Result<()>`
     - `stop_pod(socket, pod_id)` → `Result<()>`
     - `remove_pod(socket, pod_id)` → `Result<()>`
     - `wait_container(socket, container_id)` → `Result<ExitStatus>`
   - Types: `PodConfig`, `ContainerConfig`, `VolumeMount`

9. Unit tests:
   - Config serialization
   - Error handling (socket unavailable, image pull failure)

## Phase 4 — Setup script

10. New `scripts/kata-setup.sh`:
    - Idempotent provisioning for fresh nodes
    - `--install` flag: installs all Kata dependencies
    - Steps:
      1. Install containerd (`zypper in containerd` or official repo)
      2. Install kata-shim-v2 (from GitHub releases)
      3. Install Cloud Hypervisor (from GitHub releases)
      4. Install virtiofsd (from official repo)
      5. Install CNI plugins (from GitHub releases)
      6. Configure containerd with Kata runtime class:
         ```toml
         [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata]
           runtime_type = "io.containerd.kata.v2"
         ```
      7. Generate Kata `configuration.toml`:
         - Hypervisor: Cloud Hypervisor
         - Path: `/opt/kata/bin/cloud-hypervisor`
         - Kernel: `/opt/kata/share/kata-containers/vmlinuz.container`
         - Image: `/opt/kata/share/kata-containers/kata-containers.img`
         - Virtiofs: enabled
         - VFIO: enabled
      8. Start/restart containerd
      9. Verify: `kata-runtime --version`

    - `--check` flag: verify all components are installed and configured
    - `--uninstall` flag: remove Kata components (optional)

11. Integration into Flatpak/package:
    - Post-install hook calls `kata-setup.sh --install`
    - Document in `INSTALL.md` or `README.md`

## Phase 5 — Binary wiring

12. `crates/node-api/src/bin/vtessera_node.rs`:
    - Add `KataCloudHypervisor` variant to `BackendChoice` enum
    - Parse `--backend kata-cloud-hypervisor`
    - Create `KataExecutor` with `KataConfig::default()` (or from flags)
    - Feature-gate behind `#[cfg(feature = "kata")]`

13. `crates/node-api/src/bin/vtessera_mcp.rs`:
    - Same `BackendChoice` + parsing
    - Feature-gate behind `#[cfg(feature = "kata")]`

14. `crates/node-api/Cargo.toml`:
    - Add `kata` feature that pulls `vtessera-executor/kata`

Verify: `cargo build --features kata`; manual test with containerd + kata-shim-v2 on a node with `/dev/kvm`

## Phase 6 — Settlement + ROADMAP

15. `crates/settlement/src/lib.rs`:
    - `Backend::KataCloudHypervisor` already maps to tag 2
    - No changes needed (already wired)

16. `ROADMAP.md`:
    - Mark §1a (VMM Choice) as shipped
    - Mark M1 as shipped (Kata + CH running OCI workloads)

## Verification checklist

- [ ] `cargo fmt --check` clean
- [ ] `cargo clippy --all-targets -- -D warnings` clean
- [ ] `cargo test --locked` (default features) — all pass
- [ ] `cargo test --features kata` — all pass
- [ ] `cargo test --features serve` — all pass
- [ ] Manual test: pull OCI image via containerd, run Kata pod
- [ ] Manual test: GPU passthrough via Kata
- [ ] Manual test: metering sidecar writes correct JSON
- [ ] Manual test: `kata-setup.sh --install` on fresh node
