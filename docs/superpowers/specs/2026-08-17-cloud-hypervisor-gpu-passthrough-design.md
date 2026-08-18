# Cloud Hypervisor GPU passthrough — Module 1c

Date: 2026-08-17
Status: approved design
Related: `ROADMAP.md` §1c, `crates/executor`, `crates/executor/src/cloud_hypervisor.rs`

## Problem

Module 1c adds whole-GPU passthrough via VFIO to the Cloud Hypervisor
executor. The type system is ready (`DeviceClass::NvidiaGpu/Mig/AmdGpu`,
`JobMetering` has `gpu_seconds`/`vram_gb_hours`, settlement serializes
them), but all executor backends reject GPU jobs at admission. This pass
enables the core money-maker: an AI agent submits a GPU job, the executor
passes a physical GPU through to a CH microVM, the guest runs the
workload with vendor drivers, and metering records `gpu_seconds`.

Decision context (brainstorming, 2026-08-17):

- **Scope:** whole-GPU VFIO only. MIG, vGPU, and time-slicing are
  follow-ups (§1c continued).
- **Driver loading:** workload image contains driver modules + CUDA/ROCm
  alongside the workload. Guest init loads them from the virtiofs mount.
  No separate GPU initramfs.
- **GPU binding:** external helper (`vtessera-gpu`) manages PCI unbind/
  bind and writes a state file with GPU metadata. Executor reads the
  state file for discovery. Helper requires root/CAP_SYS_ADMIN.
- **Vendor support:** both NVIDIA and AMD in this iteration.
- **Metering:** `gpu_seconds = elapsed_secs` for GPU jobs. `vram_gb_hours`
  deferred to §1d (requires DCGM telemetry).
- **Testing:** code + compile + unit tests locally, integration tests
  gated on real GPU (same pattern as CPU backend).

## Scope

**In scope (this pass):**

- `vtessera-gpu` helper binary: `bind`, `unbind`, `list` subcommands
- GPU state file (`/var/lib/vtessera/gpus.json`)
- `CloudHypervisorConfig` extensions (`vfio_devices`, `gpu_helper`)
- `ch_admission` relaxed for GPU device classes
- CH command `--device host=XXXX:XX:XX.X` for VFIO devices
- Guest init: detect GPU via sysfs, `insmod` driver from virtiofs workload
- GPU discovery: match `DeviceRequirements` against state file
- `gpu_seconds` populated for GPU jobs
- `gpu` feature gate (separate from `cloud-hypervisor`)
- Integration tests gated on real GPU
- BUILD.md and ROADMAP.md updates

**Out of scope:**

- MIG partitioning (§1c continued)
- vGPU / mediated devices (§1c continued)
- Time-slicing (§1c continued)
- DCGM per-device metering (§1d)
- Guest networking for model downloads (§1e)
- Confidential computing / attestation hooks (§3)

## Architecture

### Host-side flow

```
1. Operator runs: vtessera-gpu bind --device 0000:01:00.0
   → unbinds GPU from native driver (nvidia/amdgpu)
   → binds to vfio-pci
   → writes metadata to /var/lib/vtessera/gpus.json

2. Executor reads gpus.json
   → matches DeviceRequirements (vendor, min_vram_mb)
   → selects PCI address

3. Executor spawns virtiofsd (same as CPU jobs)

4. Executor spawns CH with:
   --kernel /boot/vmlinuz-*
   --initramfs <initramfs>
   --fs tag=vtessera-job,socket=...,num_queues=1,queue_size=1024
   --memory size=<mem>,shared=on
   --cmdline "console=ttyS0"
   --device host=0000:01:00.0        ← NEW for GPU
   --serial file=<log>

5. Job completes → CH exits → executor kills virtiofsd
   (vfio-pci binding persists — operator unbinds manually)

6. Operator runs: vtessera-gpu unbind --device 0000:01:00.0
   → unbinds from vfio-pci
   → rebinds to native driver
```

### Guest-side flow

```
1. Init mounts proc, sys, devpts (existing)

2. Init mounts virtiofs workload at /mnt/workload (existing)

3. Init scans /sys/bus/pci/devices/*/class for VGA/3D controllers
   → reads vendor ID (0x10de = NVIDIA, 0x1002 = AMD)
   → insmod /mnt/workload/driver/nvidia.ko (or amdgpu.ko)
   → insmod /mnt/workload/driver/nvidia-uvm.ko (NVIDIA only)

4. Init parses manifest.json, runs command (existing)

5. Init meters via /proc, writes out/result.json + out/metering.json
   → gpu_seconds = elapsed_secs

6. sync + poweroff -f (existing)
```

### Helper binary

**Binary:** `crates/executor/src/bin/vtessera_gpu.rs`
**Name:** `vtessera-gpu`
**Feature gate:** `required-features = ["gpu"]`

**Subcommands:**

| Command | What it does |
|---------|-------------|
| `vtessera-gpu bind --device <ADDR>` | Unbind from native driver, bind to vfio-pci, write state |
| `vtessera-gpu unbind --device <ADDR>` | Unbind from vfio-pci, rebind to native driver |
| `vtessera-gpu list` | Print GPU state file as JSON |

**State file:** `/var/lib/vtessera/gpus.json`

```json
[
  {
    "pci_address": "0000:01:00.0",
    "vendor": "nvidia",
    "model": "H100-80GB",
    "vram_mb": 81920,
    "bound_at": "2026-08-17T10:00:00Z"
  }
]
```

**Bind flow:**

1. Read `/sys/bus/pci/devices/<ADDR>/vendor` → map vendor ID to name
   - `0x10de` → "nvidia"
   - `0x1002` → "amd"
2. Read `/sys/bus/pci/devices/<ADDR>/device` → map device ID to model/VRAM
   via static lookup table (same data as `lspci -nn`)
3. Unbind from current driver:
   `echo <ADDR> > /sys/bus/pci/devices/<ADDR>/driver/unbind`
4. Load vfio-pci: `modprobe vfio-pci`
5. Bind to vfio-pci:
   `echo <ADDR> > /sys/bus/pci/drivers/vfio-pci/bind`
6. Write/update state file

**Unbind flow:**

1. Unbind from vfio-pci:
   `echo <ADDR> > /sys/bus/pci/drivers/vfio-pci/unbind`
2. Load native driver: `modprobe nvidia` or `modprobe amdgpu`
3. Probe: `echo <ADDR> > /sys/bus/pci/drivers/<driver>/bind`
4. Remove entry from state file

**Idempotency:** `bind` on an already-bound device is a no-op (not an error).
`unbind` on an unbound device is a no-op.

### Executor changes

**Config extensions** (`CloudHypervisorConfig`):

```rust
/// PCI addresses of VFIO devices to pass through.
/// Empty for CPU-only jobs.
pub vfio_devices: Vec<String>,
/// Path to the vtessera-gpu binary.
pub gpu_helper: PathBuf,
```

**Admission** (`ch_admission`):

- GPU device classes now allowed when `vfio_devices` is non-empty
- `NvidiaMig` rejected with "MIG not yet supported; use whole-GPU"
- Network policy `None` still required (§1e adds networking)

**CH command:**

```rust
for device in &config.vfio_devices {
    cmd.args(["--device", &format!("host={device}")]);
}
```

**GPU discovery** (`select_gpu`):

1. Read `/var/lib/vtessera/gpus.json` directly (not via helper subprocess).
   `vtessera-gpu list` is the operator-facing equivalent of the same file.
2. Filter by vendor (NVIDIA/AMD from `DeviceClass`)
3. Filter by `min_vram_mb`
4. Return first match, or admission error with vendor/VRAM details

**Metering:**

```rust
gpu_seconds: if is_gpu_job { elapsed_secs as f64 } else { 0.0 },
vram_gb_hours: 0.0, // deferred to §1d
```

### Guest init changes

The `/init` script (embedded in the initramfs) gains a GPU detection
step between mounting virtiofs and parsing the manifest:

```sh
# Detect GPU and load driver
if [ -d /sys/bus/pci/devices ]; then
    for dev in /sys/bus/pci/devices/*/class; do
        class=$(cat "$dev" 2>/dev/null)
        case "$class" in
            0x030000|0x030200)  # VGA or 3D controller
                vendor=$(cat "$(dirname "$dev")/vendor" 2>/dev/null)
                case "$vendor" in
                    0x10de) # NVIDIA
                        [ -f /mnt/workload/driver/nvidia.ko ] && \
                            insmod /mnt/workload/driver/nvidia.ko
                        [ -f /mnt/workload/driver/nvidia-uvm.ko ] && \
                            insmod /mnt/workload/driver/nvidia-uvm.ko
                        ;;
                    0x1002) # AMD
                        [ -f /mnt/workload/driver/amdgpu.ko ] && \
                            insmod /mnt/workload/driver/amdgpu.ko
                        ;;
                esac
                break
                ;;
        esac
    done
fi
```

**Workload image layout (GPU):**

```
/mnt/workload/
├── driver/
│   ├── nvidia.ko
│   └── nvidia-uvm.ko
├── cuda/
│   └── lib64/
├── manifest.json
└── run.sh
```

**Workload image layout (CPU, unchanged):**

```
/mnt/workload/
├── manifest.json
└── run.sh
```

**Driver mismatch:** If the driver module is not found at the expected
path, init logs a warning and continues. The workload will fail with a
clear error (e.g., no `/dev/nvidia0`) rather than init crashing.

### Feature gate

```toml
[features]
default = []
cloud-hypervisor = ["dep:serde_json"]
gpu = ["cloud-hypervisor"]
```

The `gpu` feature enables:
- `vtessera-gpu` binary (`required-features = ["gpu"]`)
- GPU admission path in `cloud_hypervisor.rs`
- `vfio_devices` and `gpu_helper` fields on `CloudHypervisorConfig`

**Node API:** `serve` feature threads `vtessera-executor/gpu`.

### Integration tests

**File:** `crates/executor/tests/ch_gpu_integration.rs`
**Gate:** `VTESSERA_CH_INTEGRATION=1` + real GPU present
**Skip condition:** `vtessera-gpu list` returns empty → `skip!()`

| Test | What it validates |
|------|-------------------|
| `gpu_true_exits_completed` | GPU job with `command: ["true"]` → `ExitStatus::Completed` + `gpu_seconds > 0` |
| `gpu_mismatched_driver_fails` | Job requests NVIDIA but host has AMD → admission error |
| `gpu_vram_too_small` | Job requests 80GB but host has 24GB → admission error |
| `gpu_metering_populated` | `gpu_seconds > 0.0`, `vram_gb_hours == 0.0` |

### Error handling

**Executor errors:**

| Condition | Error message |
|-----------|--------------|
| GPU job, `vfio_devices` empty | "GPU job requires vfio_devices in config" |
| No matching GPU | "no matching GPU: vendor=nvidia, min_vram=80000MB" |
| MIG requested | "MIG not yet supported; use whole-GPU" |
| Helper binary missing | `ExecutorError::Io` |

**Helper errors:**

| Condition | Error message |
|-----------|--------------|
| Device not in sysfs | "PCI device not found" |
| Already bound | no-op (idempotent) |
| Permission denied | "permission denied: run as root or with CAP_SYS_ADMIN" |
| Unknown vendor | "unknown GPU vendor: 0xXXXX" |

### Security

- **VFIO gives guest DMA** — weakens VM boundary vs CPU-only. Documented
  in ROADMAP §1c. Mitigation: confidential computing (H100 CC + SEV-SNP/
  TDX) is the future path; attestation hooks baked in early (§3).
- **Helper requires root** — PCI unbind/bind needs elevated privileges.
  Binary should be setuid or run via sudo. Document in BUILD.md.
- **State file permissions** — `/var/lib/vtessera/gpus.json` should be
  `0640` (root:vtessera). No secrets, but PCI addresses are host-specific.
- **Guest driver loading** — `insmod` from virtiofs is safe because the
  workload image is controlled by the job submitter. Driver modules are
  signed by the vendor (NVIDIA/AMD). Tampered modules rejected by kernel
  signature verification.

## Success criteria

1. `cargo build --features gpu` compiles on a box without a GPU
2. `cargo test --features cloud-hypervisor` passes (CPU tests unchanged)
3. Unit tests for `select_gpu`, `ch_admission` with GPU, config defaults
4. Integration tests pass on a GPU-equipped host with VFIO-bound device
5. Guest loads nvidia.ko/amdgpu.ko from virtiofs workload
6. `gpu_seconds > 0` in metering output for GPU jobs
7. `vram_gb_hours == 0.0` (deferred to §1d)
