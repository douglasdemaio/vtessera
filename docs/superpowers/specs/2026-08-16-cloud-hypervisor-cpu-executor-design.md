# Cloud Hypervisor CPU executor — Module 1 first backend

Date: 2026-08-16
Status: approved design
Related: `ROADMAP.md` §1, `MAINNET-CHECKLIST.md` (audit §4), `crates/executor`

## Problem

Module 1 is the leap from "watching my CPU" to "an AI job runs on my box."
The executor skeleton (`crates/executor`) already defines the `Executor`
trait, `JobSpec`, `JobMetering`, and two development backends
(`NoopCpu`, `LocalCpu`). The production CPU backend is stubbed
(`KataCloudHypervisorExecutor` returns `BackendUnimplemented`). This pass
builds the first **real isolation** backend: a Cloud Hypervisor microVM
that runs a job's command in a disposable guest and returns metering.

Decision context (brainstorming, 2026-08-16):

- **Isolation level:** Cloud Hypervisor directly — not Kata, not OCI.
  True VM isolation now, smallest dependency surface, and
  `cloud-hypervisor` is packaged for openSUSE Tumbleweed (`/dev/kvm`
  present). The OCI/Kata path (§1a) stays a roadmap follow-up.
- **Guest boot:** host kernel (`/boot/vmlinuz-*`) + a custom initramfs
  (busybox + a job-runner agent). No rootfs image to maintain; the
  guest runs in RAM.
- **Job data path:** virtio-fs shared dir. Host stages `manifest.json`,
  guest writes `out/result.json` + `out/metering.json`, then
  `poweroff -f`. No guest network device.
- **Metering source:** guest-side runner reads `/proc` (CPU from
  `/proc/<pid>/stat` utime+stime, peak RSS from `VmPeak`). GPU/VRAM
  metering via DCGM is a later follow-up (§1c).
- **Privilege model:** the executor runs root inside the hardened
  `vtessera-node` unit; `cloud-hypervisor` is spawned as its child.
- **Initramfs production:** a build script with pinned inputs, versioned
  output, and a recorded SHA.

## Scope

**In scope (this pass):**

- `CloudHypervisorConfig` + `CloudHypervisorExecutor` implementing
  `Executor`, gated behind a `cloud-hypervisor` cargo feature in
  `crates/executor`.
- `manifest.json` write/read format between host and guest.
- Guest initramfs job-runner agent (POSIX sh + busybox), built by
  `scripts/build-initramfs.sh` into a deterministic `.cpio.gz`.
- Host-side wall-clock timeout enforcement (kill via api socket →
  `ExitStatus::TimedOut`).
- Workdir staging + cleanup on every exit path (debug `--keep-jobs`).
- Unit + integration tests (integration skipped without `/dev/kvm`).
- `vtessera-node --backend cloud-hypervisor` wiring (feature-gated,
  mirrors `local-cpu`).

**Out of scope (roadmap follow-ups):**

- GPU/accelerator passthrough (VFIO), DCGM metering, MIG/vGPU (§1c).
- `NetworkPolicy::OutboundHttps`/`Egress` (needs virtio-net + guest
  firewall, §1e).
- CPU pinning / NUMA awareness (§1b later tier).
- Attestation hooks / confidential compute (Module 3 link).
- Kata / OCI image execution (§1a).

## Architecture

```
JobSpec ──▶ CloudHypervisorExecutor (feature `cloud-hypervisor`)
              │
              1. admission_check(spec)                 (reuse existing)
              2. stage workdir /var/lib/vtessera/jobs/<job_id>/
                   { manifest.json, out/ }
              3. spawn cloud-hypervisor:
                   --kernel <kernel> --initramfs <pinned>.cpio.gz
                   --cpus boot=<vcpus> --memory size=<mem_kb>
                   --disk virtiofs <workdir> --api-socket <workdir>/ch.sock
                   (no virtio-net device — network policy None)
              4. guest runner: mount /mnt, read manifest.json,
                   run command, sample /proc,
                   write out/{result.json, metering.json}, poweroff -f
              5. executor waits on child exit
                   (or wall-clock timeout → kill via api socket)
              6. parse metering.json → JobMetering; cleanup workdir
```

Runs in-process inside `vtessera-node` (root, hardened unit), matching
how `noop-cpu` / `local-cpu` already wire in. The crate stays buildable
everywhere: `cargo build` without the feature adds no deps.

## Components

### `CloudHypervisorConfig`

Plain struct the node binary supplies:

```rust
pub struct CloudHypervisorConfig {
    pub ch_binary: PathBuf,     // default /usr/bin/cloud-hypervisor
    pub kernel: PathBuf,        // default /boot/vmlinuz-<running>
    pub initramfs: PathBuf,     // default /var/lib/vtessera/initramfs.cpio.gz
    pub workdir: PathBuf,       // default /var/lib/vtessera/jobs
    pub extra_args: Vec<String>,// pass-through for experimental flags
    pub keep_jobs: bool,        // debug: don't delete workdir after run
}
```

### `CloudHypervisorExecutor`

Implements `Executor`:

- `run(spec)`:
  1. `admission_check(spec)` (reuse). CH-specific admission: reject
     non-`DeviceClass::Cpu` ("GPU is a follow-up"); reject
     `NetworkPolicy::Egress`/`OutboundHttps` ("networking not wired").
  2. Create workdir `<workdir>/<job_id>` (reject if it already exists —
     job_id must be unique). Write `manifest.json`. Create `out/`.
  3. Spawn `cloud-hypervisor` with the args above. `ch` needs
     `/dev/kvm`; spawn failure surfaces a clear `ExecutorError::Backend`.
  4. Wait on child with a wall-clock timer at `max_duration_secs`. On
     timeout, kill the VM (api socket, then SIGKILL the child) and
     return `ExitStatus::TimedOut` metering with elapsed = cap.
  5. On clean exit, read `out/metering.json`; map to `JobMetering`.
     Missing/malformed → `ExecutorError::Backend` (no credit).
  6. Cleanup workdir unless `keep_jobs`.
- `Drop`/init: sweep stale workdirs from killed VMs.

### Manifest (`manifest.json`)

Written by host, read by guest:

```json
{
  "job_id": "job-0001",
  "command": ["python", "infer.py"],
  "env": [["HF_HOME", "/mnt/hf"]],
  "vcpus": 4,
  "mem_kb": 33554432,
  "max_duration_secs": 3600
}
```

Plaintext (env included) on the shared dir — documented in SECURITY.md
as a known limitation. No secrets policy: env must not contain
credentials (host policy; runner does not sanitize).

### Guest initramfs runner

Busybox + POSIX sh agent baked into the initramfs:

1. `mount -t virtiofs vtessera-job /mnt`
2. read `/mnt/manifest.json`
3. `sh -c` the command with stdin closed, stdout/stderr to a log file
   in `/mnt/out/`
4. sample `/proc/<pid>/stat` (utime+stime, ticks) for CPU and
   `/proc/<pid>/status` (`VmPeak`) for peak RSS; wall-clock elapsed
5. write `/mnt/out/result.json` `{ exit_code, signal }`
6. write `/mnt/out/metering.json` `{ cpu_seconds, peak_mem_kb,
   elapsed_secs }`
7. `poweroff -f`

The guest enforces `max_duration_secs` best-effort (kill the job,
still write metering); the **host** timer is authoritative.

### `scripts/build-initramfs.sh`

- Pins: busybox static binary version, runner script contents, required
  kernel modules (virtiofs/fuse) for the pinned kernel.
- Emits `initramfs.cpio.gz` deterministically to a fixed path; records
  the SHA-256 to `scripts/initramfs.sha256`.
- Re-run manually or by CI when kernel/runtime inputs change.

## Data flow / error handling / limits

| Outcome | Behavior |
| --- | --- |
| Guest completes | `metering.json` → `JobMetering` → node signs receipt |
| `exit_code == 0` | `ExitStatus::Completed` |
| `exit_code != 0` | `ExitStatus::Failed { code }` |
| Host wall-clock cap hit | kill VM → `ExitStatus::TimedOut`, elapsed = cap |
| ch spawn fails | `ExecutorError::Backend` (missing bin/kernel, no kvm) |
| metering.json missing/malformed | `ExecutorError::Backend` — no credit |
| workdir exists (dup job_id) | `ExecutorError::Backend` |
| Cleanup | workdir deleted on every exit path unless `keep_jobs` |

## Testing

- **Unit (no feature, everywhere):** CH admission rules; manifest JSON
  round-trip; `metering.json` parse (valid/malformed/missing); timeout
  kill logic against a fake `cloud-hypervisor` shim; workdir cleanup on
  all paths.
- **Integration (`--features cloud-hypervisor`, skipped without
  `/dev/kvm`):** boot a real VM — `true` → Completed; `sh -c 'exit 3'`
  → Failed; `sleep 60` with 2s cap → TimedOut; metering sane
  (`cpu_seconds > 0`, `peak_mem_kb > 0`, `elapsed ≈ wall`).
- **Node-level:** `vtessera-node --backend cloud-hypervisor` runs a
  free-offer job end-to-end and returns the signed receipt (the
  `backend_tag` map already lists the variant).
- **Security bar:** `systemd-analyze security` on the vtessera-node unit
  after the backend lands; keep `cargo deny` / `cargo audit` green.

## SECURITY.md updates (this pass)

- New "Module 1 executor (Cloud Hypervisor CPU backend)" section:
  threat model (guest = untrusted code, isolated by the microVM; host
  exposure limited to the `cloud-hypervisor` child, the shared dir, and
  `/dev/kvm`), and known limitations (plaintext manifest/env on the
  shared dir; single-tenant-per-VM; no guest network; no attestation yet).

## Out of scope (roadmap)

GPU/accelerator passthrough (VFIO, MIG/vGPU, DCGM metering), guest
networking, CPU pinning/NUMA, attestation/confidential compute, and the
Kata/OCI image path — all tracked in `ROADMAP.md` §1.
