# Cloud Hypervisor CPU Executor — Implementation Plan

Spec: `docs/superpowers/specs/2026-08-16-cloud-hypervisor-cpu-executor-design.md`
Branch: `module1-ch-cpu` (new, off `main`). One PR at the end.

Host prerequisites (documented, not scripted here): `cloud-hypervisor`
and `busybox` installed (`zypper in cloud-hypervisor busybox`); `/dev/kvm`
present; kernel modules `virtiofs.ko.zst` + `fuse.ko.zst` available in
`/usr/lib/modules/$(uname -r)` (verified present on Tumbleweed).

No amendment to the spec needed — planning found only implementation-level
choices (detailed below), no design gaps.

## Phase 1 — Executor backend (`crates/executor`)

1. `crates/executor/Cargo.toml`:
   - `[features] cloud-hypervisor = []` (empty — spawns the ch binary,
     no new deps).
   - Add `serde_json = { workspace = true }` as an **optional** dep, pulled
     by `cloud-hypervisor` (`cloud-hypervisor = ["dep:serde_json"]`) — the
     manifest/metering are JSON and the default build stays dep-free.
2. New `crates/executor/src/cloud_hypervisor.rs`, wired as
   `#[cfg(feature = "cloud-hypervisor")] pub mod cloud_hypervisor;` in
   `lib.rs`. Contents:
   - `pub struct CloudHypervisorConfig { pub ch_binary: PathBuf, pub
     kernel: PathBuf, pub initramfs: PathBuf, pub workdir: PathBuf, pub
     extra_args: Vec<String>, pub keep_jobs: bool }` + `Default`
     (paths as in the spec).
   - `pub struct CloudHypervisorExecutor { pub config: CloudHypervisorConfig }`.
   - `impl Executor for CloudHypervisorExecutor`:
     a. `admission_check(spec)` (reuse from `lib.rs`); CH-specific: reject
        non-`DeviceClass::Cpu` and any `network != NetworkPolicy::None`.
     b. Sanity-check the config paths exist (`ch_binary`, `kernel`,
        `initramfs`) and `/dev/kvm` is readable → clear `ExecutorError::Backend`.
     c. Stage `<workdir>/<job_id>/`: fail if it exists (dup job_id); write
        `manifest.json`; `mkdir out/`.
     d. Spawn `cloud-hypervisor` (child of the executor; inherits root +
        `/dev/kvm` from the unit): `--kernel`, `--initramfs`,
        `--cpus boot=<vcpus>`, `--memory size=<mem_kb>`, `--disk
        path=<workdir>,mount_tag=vtessera-job` (virtiofs), `--api-socket
        <workdir>/ch.sock`, plus `config.extra_args`. No `--net` device.
        Non-interactive: stdin nulled, stdout/stderr to a log file in
        `out/`.
     e. Poll `child.try_wait()` on a 50ms loop like `LocalCpuExecutor`;
        on wall-clock >= `max_duration_secs` kill the VM (best-effort
        `PUT /api/v1/vm.shutdown` to `ch.sock` via a `Command`-spawned
        `cloud-hypervisor --api-socket <sock> ...`? **No** — decision:
        SIGKILL the child directly; the api socket adds a second ch
        invocation and the guest already self-terminates best-effort).
        Return `ExitStatus::TimedOut` metering (elapsed = cap).
     f. On clean exit read `out/metering.json` → `JobMetering`. Missing /
        malformed → `ExecutorError::Backend` (no credit).
     g. Cleanup workdir unless `keep_jobs`.
   - Helper `fn write_manifest(path, &JobSpec)`, `fn parse_metering(bytes)
     -> Result<JobMetering, ExecutorError>` (maps guest's
     `{cpu_seconds, peak_mem_kb, elapsed_secs}` + host-side exit code into
     the full `JobMetering` — the guest doesn't know `backend`/`device`).
3. Unit tests (no feature needed for the pure parts — gate the module
   tests behind the feature since the module only compiles there):
   - manifest round-trip (write then read back; env preserved).
   - `parse_metering`: valid JSON → fields; malformed JSON → Backend error;
     missing fields → Backend error.
   - admission: GPU class rejected; `OutboundHttps`/`Egress` rejected.
   - dup workdir → error (against a tempdir workdir).
   - timeout logic: fake `cloud-hypervisor` shim script (writes `manifest`
     out only when a marker file appears; sleeps) — assert SIGKILL path
     returns `TimedOut` within cap + epsilon.

Verify: `cargo fmt --check`; `cargo clippy -p vtessera-executor
--all-targets -- -D warnings`; `cargo test -p vtessera-executor --locked`
(default) and `--features cloud-hypervisor`.

## Phase 2 — Guest initramfs runner + build script

4. New `scripts/build-initramfs.sh`:
   - Pinned inputs: `BUSYBOX_VERSION` (e.g. `1.36.1`), the runner script
     (embedded heredoc), the **running** kernel's virtiofs + fuse modules
     (copied from `/usr/lib/modules/$(uname -r)/kernel/fs/…`), a
     precompiled `manifest` parser? **No** — runner is pure busybox sh
     (uses busybox `sh`, `cat`, `mount`, `awk`, `grep`, `poweroff`; JSON
     parsed with `sed`/`grep` — manifest is flat key/value we emit).
   - Deterministic assembly: `find . | cpio -o -H newc` piped to `gzip -n`,
     stable ordering (`find … | sort`), timestamps zeroed (`touch -d
     @0`), no host paths embedded. Emit `initramfs.cpio.gz` to a path the
     executor default reads (`/var/lib/vtessera/initramfs.cpio.gz`),
     `sha256sum` → `scripts/initramfs.sha256`.
   - Runner (embedded in the initramfs as `/init`):
     ```
     mount -t virtiofs vtessera-job /mnt
     <parse manifest from /mnt/manifest.json>
     cd /mnt
     start=$(date +%s)
     <run command with stdin</dev/null; capture exit>
     cpu=$(awk from /proc/<pid>/stat utime+stime)  # ticks → secs
     peak=$(grep VmPeak /proc/<pid>/status)
     echo '{"exit_code":…}' > /mnt/out/result.json
     echo '{"cpu_seconds":…,"peak_mem_kb":…,"elapsed_secs":…}' > /mnt/out/metering.json
     poweroff -f
     ```
     `max_duration_secs` enforced guest-side best-effort (background
     `sleep` kills the job pid and still writes metering).
5. Node-side convenience: `vtessera-node` gains `--initramfs <path>` and
   `--kernel <path>` overrides? **No** (YAGNI) — defaults are compiled into
   `CloudHypervisorConfig::default()`; document them in the README. Build
   script is the only knob for now.

Verify: run `scripts/build-initramfs.sh` on this host; `lsinitrd
initramfs.cpio.gz` shows `/init` + modules; `sha256sum` matches the
recorded SHA.

## Phase 3 — Integration tests (KVM-gated)

6. New `crates/executor/tests/ch_cpu_integration.rs`, `#[ignore]`-gated +
   runtime-skip when `/dev/kvm` absent or `VTESSERA_CH_INTEGRATION=1` not
   set (the default test matrix must stay green without KVM):
   - boot real VM: `true` → `Completed`; `sh -c 'exit 3'` → `Failed{3}`;
     `sleep 60` with `max_duration_secs = 2` → `TimedOut`;
     metering sane (`cpu_seconds > 0`, `peak_mem_kb > 0`, `elapsed` ≈
     wall within tolerance).
   - Each test stages under a temp workdir (no `keep_jobs` pollution);
     single-threaded (VMs are heavy) via a serial test harness.

Verify: `VTESSERA_CH_INTEGRATION=1 cargo test -p vtessera-executor
--features cloud-hypervisor --test ch_cpu_integration -- --nocapture` on
this host (KVM present).

## Phase 4 — Node wiring + docs + CI

7. `crates/node-api/src/bin/vtessera_node.rs`:
   - `BackendChoice` gains `CloudHypervisor`; `parse` accepts
     `cloud-hypervisor` **only** when the executor feature is on. The bin
     already lists `vtessera-executor` optionally — gate via a new
     `vtessera-node-api` feature `ch-executor = ["dep:vtessera-executor",
     "vtessera-executor/cloud-hypervisor"]`, added to `serve`? **Decision:**
     add `ch-executor` as a separate feature off `serve` is not possible
     (feature = dep at bin level); simplest: when `serve` is on and the
     `--backend cloud-hypervisor` string is requested, build
     `CloudHypervisorExecutor::default()` — the feature must be enabled at
     build time. Wire: `node-api` `serve` feature gains
     `"vtessera-executor/cloud-hypervisor"`. This forces the ch backend to
     compile with every `serve` build — acceptable (it only spawns a
     binary; no new deps).
   - `BackendChoice::build` for `CloudHypervisor` → `ExecutorRunner` with
     `Box::new(vtessera_executor::cloud_hypervisor::CloudHypervisorExecutor::default())`.
   - usage text + `backend_tag` already cover the variant (verified).
8. Docs:
   - `ROADMAP.md` §1: add a "Shipped" note — CH CPU backend behind
     `cloud-hypervisor` feature; `scripts/build-initramfs.sh`; KVM-gated
     integration tests; GPU/networking remain follow-ups.
   - `programs/vtessera-escrow/SECURITY.md`: add the Module 1 executor
     section (threat model + plaintext-manifest limitation) per spec §6.
   - `crates/executor/src/lib.rs` doc header: `KataCloudHypervisorExecutor`
     stub note → "see `cloud_hypervisor` module behind the feature".
   - README: `--backend cloud-hypervisor` + host prerequisites + the
     build-initramfs script.
9. CI: `.github/workflows/ci.yml` — add `cargo test -p vtessera-executor
   --features cloud-hypervisor` (unit tests; integration stays
   `#[ignore]` + KVM-gated so runners without KVM stay green). No other
   CI changes expected.

Final gates (before PR): full-workspace `cargo fmt --check`; per-crate
clippy `-D warnings`; `cargo test --locked` (workspace, excluding
`vtessera-gui` on host); `VTESSERA_CH_INTEGRATION=1` ch integration suite
on this host; `scripts/build-initramfs.sh` determinism check (run twice,
same SHA); rebase onto `main` after PR merge.
