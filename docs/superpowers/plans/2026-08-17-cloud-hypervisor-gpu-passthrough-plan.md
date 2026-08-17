# Cloud Hypervisor GPU Passthrough — Implementation Plan

Spec: `docs/superpowers/specs/2026-08-17-cloud-hypervisor-gpu-passthrough-design.md`
Branch: `module1-ch-gpu` (new, off `main`). One PR at the end.

Host prerequisites (documented, not scripted here): `cloud-hypervisor`,
`busybox`, and `vfio-pci` module available. A discrete GPU bound to
vfio-pci. Helper binary installed at `/usr/bin/vtessera-gpu`.

No amendment to the spec needed — planning found only implementation-level
choices (detailed below), no design gaps.

## Phase 1 — Helper binary (`crates/executor/src/bin/vtessera_gpu.rs`)

1. New `crates/executor/src/bin/vtessera_gpu.rs`, gated behind
   `required-features = ["gpu"]` in `Cargo.toml`.

2. CLI via `std::env::args` (no clap dependency — minimal binary):
   - `vtessera-gpu bind --device <ADDR>`
   - `vtessera-gpu unbind --device <ADDR>`
   - `vtessera-gpu list`

3. `bind` subcommand:
   a. Validate `<ADDR>` matches PCI format (`XXXX:XX:XX.X`).
   b. Read `/sys/bus/pci/devices/<ADDR>/vendor` → map vendor ID to name
      (`0x10de` → "nvidia", `0x1002` → "amd"). Unknown → error.
   c. Read `/sys/bus/pci/devices/<ADDR>/device` → map device ID to
      model/VRAM via static lookup table. Unknown device ID → warning
      (still bind, but model = "unknown", vram_mb = 0).
   d. Check current driver: if already `vfio-pci`, skip unbind (idempotent).
   e. Unbind: write `<ADDR>` to `/sys/bus/pci/devices/<ADDR>/driver/unbind`.
   f. `modprobe vfio-pci` (if not already loaded).
   g. Bind: write `<ADDR>` to `/sys/bus/pci/drivers/vfio-pci/bind`.
   h. Write/update `/var/lib/vtessera/gpus.json`.

4. `unbind` subcommand:
   a. Check current driver: if not `vfio-pci`, skip (idempotent).
   b. Unbind from vfio-pci.
   c. Load native driver: `modprobe nvidia` or `modprobe amdgpu`.
   d. Probe: write `<ADDR>` to `/sys/bus/pci/drivers/<driver>/bind`.
   e. Remove entry from state file.

5. `list` subcommand:
   a. Read `/var/lib/vtessera/gpus.json`.
   b. Print to stdout as JSON array.
   c. Exit 0 with empty array if file doesn't exist.

6. PCI device ID lookup table (static):
   - NVIDIA: `0x1b30` → "H100-80GB", `0x20b0` → "H200-141GB",
     `0x20f1` → "H100-NVL", `0x1db5` → "A100-80GB",
     `0x1db6` → "A100-40GB", `0x25b6` → "A100-SXM4-80GB",
     `0x2782` → "L40S-48GB", `0x2783` → "L4-24GB",
     `0x20b5` → "RTX-6000-48GB", `0x2230` → "RTX-5090-32GB".
   - AMD: `0x740c` → "MI300X-192GB", `0x7408` → "MI300A-128GB",
     `0x738c` → "MI250X-128GB", `0x738e` → "MI250-128GB".
   - Unknown devices: model = "unknown", vram_mb = 0 (admission will
     require min_vram_mb = 0 or fail).

7. Unit tests (no KVM or GPU needed):
   - PCI address validation regex
   - State file read/write round-trip
   - Vendor ID mapping (nvidia, amd, unknown)
   - Idempotent bind/unbind (mock sysfs)

## Phase 2 — Executor GPU support (`crates/executor`)

1. `crates/executor/Cargo.toml`:
   - New feature: `gpu = ["cloud-hypervisor"]`.
   - Add `[[bin]] name = "vtessera-gpu"` with
     `required-features = ["gpu"]`.

2. `crates/executor/src/cloud_hypervisor.rs` — config extensions:
   - Add `vfio_devices: Vec<String>` and `gpu_helper: PathBuf` to
     `CloudHypervisorConfig`.
   - Update `Default` impl: `vfio_devices: vec![]`, `gpu_helper` =
     `/usr/bin/vtessera-gpu`.

3. `crates/executor/src/cloud_hypervisor.rs` — admission:
   - Relax `ch_admission`: allow `NvidiaGpu`, `AmdGpu` when
     `config.vfio_devices` is non-empty.
   - Reject `NvidiaMig` with "MIG not yet supported; use whole-GPU".
   - Keep `NetworkPolicy::None` requirement (§1e).

4. `crates/executor/src/cloud_hypervisor.rs` — CH command:
   - After existing args (`--kernel`, `--initramfs`, `--fs`, `--memory`,
     `--cmdline`, `--serial`), append `--device host=<ADDR>` for each
     address in `config.vfio_devices`.

5. `crates/executor/src/cloud_hypervisor.rs` — GPU discovery:
   - New `fn select_gpu(spec: &JobSpec, state_path: &Path) -> Result<String, ExecutorError>`.
   - Read JSON state file, filter by vendor + min_vram_mb, return first
     match or admission error.

6. `crates/executor/src/cloud_hypervisor.rs` — metering:
   - In `parse_metering` (or the `Ok(JobMetering)` construction):
     ```rust
     gpu_seconds: if matches!(spec.devices.class, DeviceClass::NvidiaGpu { .. } | DeviceClass::AmdGpu { .. }) {
         elapsed_secs as f64
     } else {
         0.0
     },
     ```
   - `vram_gb_hours` stays at `0.0`.

7. Unit tests:
   - `ch_admission` allows NvidiaGpu when vfio_devices non-empty
   - `ch_admission` rejects NvidiaMig
   - `ch_admission` rejects NvidiaGpu when vfio_devices empty
   - `select_gpu` matches correct vendor and VRAM
   - `select_gpu` errors on no match
   - Config default has empty vfio_devices
   - GPU metering: gpu_seconds > 0 for GPU specs

## Phase 3 — Guest init GPU detection

1. `scripts/build-initramfs.sh` — no changes needed. The GPU detection
   logic is in the embedded `/init` script, not the build.

2. `scripts/initramfs.sh` (the `/init` content) — add GPU detection
   between virtiofs mount and manifest parse:
   - Scan `/sys/bus/pci/devices/*/class` for VGA (0x030000) or 3D
     (0x030200) controller.
   - Read vendor ID from parent directory.
   - `insmod /mnt/workload/driver/nvidia.ko` (NVIDIA) or
     `insmod /mnt/workload/driver/amdgpu.ko` (AMD).
   - Also load `nvidia-uvm.ko` for NVIDIA (CUDA unified memory).
   - If driver not found at expected path, log warning and continue.

3. Determinism: the init script changes don't affect initramfs
   reproducibility (same `touch -d @0` + `cpio --reproducible` +
   `gzip -n` pipeline). Record new SHA to `scripts/initramfs.sha256`.

4. Unit tests: none for init script (tested via integration tests on
   real hardware).

## Phase 4 — Integration tests

1. New `crates/executor/tests/ch_gpu_integration.rs`:
   - `required-features = ["cloud-hypervisor"]` in Cargo.toml.
   - Runtime gate: `VTESSERA_CH_INTEGRATION=1` + `CH_INITRAMFS` env var.
   - GPU gate: `vtessera-gpu list` returns non-empty → otherwise `skip!()`.

2. Test cases:
   - `gpu_true_exits_completed`: GPU job with `command: ["true"]` →
     `ExitStatus::Completed` + `gpu_seconds > 0.0`.
   - `gpu_mismatched_driver_fails`: request NvidiaGpu when host has AMD
     bound → `ExecutorError::Admission`.
   - `gpu_vram_too_small`: request 80000 MB when host has 24000 MB →
     `ExecutorError::Admission`.
   - `gpu_metering_populated`: `gpu_seconds > 0.0`, `vram_gb_hours == 0.0`.

3. Test helper: `fn gpu_spec(cmd, vcpus, mem_kb, model, vram_mb) -> JobSpec`
   that builds a GPU job spec with the given parameters.

4. Each test writes manifest + workload dir (with dummy `run.sh`),
   constructs `CloudHypervisorConfig` with vfio_devices from the
   discovered GPU, runs the executor, asserts metering.

## Phase 5 — Node API wiring + docs

1. `crates/node-api/Cargo.toml`:
   - `serve` feature threads `vtessera-executor/gpu`.

2. `crates/node-api/src/bin/vtessera_node.rs`:
   - When `--backend cloud-hypervisor` + GPU config, pass
     `vfio_devices` and `gpu_helper` to `CloudHypervisorConfig`.
   - Discovery: read state file, populate `vfio_devices` with matched
     PCI addresses.

3. `ROADMAP.md`: add §1c "Shipped" note.

4. `BUILD.md`: document:
   - `cargo build --features gpu`
   - Helper binary installation (`/usr/bin/vtessera-gpu`)
   - udev rules for vfio-pci binding
   - GPU workload image layout

5. `README.md`: update Module 1 status to reflect GPU support.

## Phase 6 — Verification

1. `cargo fmt --all`
2. `cargo clippy --workspace --all-targets --features gpu -- -D warnings`
3. `cargo test --workspace --features cloud-hypervisor` (CPU tests unchanged)
4. `cargo test --workspace --features gpu` (unit tests including GPU)
5. Integration tests on GPU-equipped host (if available)
6. `scripts/build-initramfs.sh` → verify new SHA
7. Verify `vtessera-gpu list` → empty array on host without GPU
