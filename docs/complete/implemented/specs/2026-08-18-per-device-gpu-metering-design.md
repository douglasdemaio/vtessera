# Per-device GPU metering — Module 1d

## Summary

Populate the GPU metering fields in `JobMetering` that are currently
hardcoded to `0.0`. Add a host-side polling thread (`GpuMeter`) that
measures VRAM-GB-hours, GPU utilization, and power draw during job
execution. Extend the guest runner to self-report GPU metrics for
cross-validation. Host values are authoritative; mismatches produce a
warning but do not reject the receipt.

**Scope**: GPU metering only. CPU/mem metering improvements (cgroups-based
host accounting) are deferred to a follow-up.

**Telemetry source**: nvidia-smi polling as the primary path. DCGM
integration is deferred to a future feature-gated enhancement (same
`GpuSample` output struct, different collection backend).

---

## 1. Host-side GPU metering — `GpuMeter`

### 1a. New file: `crates/executor/src/gpu_meter.rs`

Feature-gated on `gpu` (same gate as the rest of the executor GPU code).

```rust
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

/// Accumulated GPU metrics from host-side polling.
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize, PartialEq)]
pub struct GpuSample {
    /// Wall-clock seconds the GPU was accessible to this job.
    pub gpu_seconds: f64,
    /// Time-weighted VRAM integral in GB-hours.
    pub vram_gb_hours: f64,
    /// Average GPU utilization percentage (0–100).
    pub avg_gpu_util_pct: f32,
    /// Average power draw in watts.
    pub avg_power_watts: f32,
    /// Peak VRAM usage in MB.
    pub peak_vram_mb: u32,
    /// Number of polling samples taken.
    pub samples: u32,
}

/// Background GPU meter spawned alongside a Cloud Hypervisor job.
pub struct GpuMeter {
    pci_address: String,
    poll_interval: Duration,
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<GpuSample>>,
}
```

### 1b. Polling thread

`GpuMeter::start(pci_address, poll_interval)` spawns a thread that:

1. Calls `nvidia-smi --query-gpu=memory.used,utilization.gpu,power.draw
   --format=csv,noheader --id=<pci>` via `std::process::Command`.
2. Parses the CSV line: `<vram_used_mb>, <gpu_util_pct>, <power_watts>`.
3. Accumulates:
   - `dt = elapsed since last sample`
   - `vram_gb_hours += (vram_used_mb as f64 / 1024.0) * dt.as_secs_f64() / 3600.0`
   - `gpu_seconds += dt.as_secs_f64()`
   - Running averages for `avg_gpu_util_pct` and `avg_power_watts`
   - `peak_vram_mb = max(peak_vram_mb, vram_used_mb)`
   - `samples += 1`
4. Sleeps for `poll_interval`.
5. Checks `stop` flag; if set, returns the accumulated `GpuSample`.
6. If `nvidia-smi` fails (process not found, GPU removed), logs a warning
   and continues with zero values for that sample — does not abort.

**Default poll interval**: 1 second. Configurable via
`CloudHypervisorConfig::gpu_meter_poll_interval`.

### 1c. Lifecycle

```
GpuMeter::start(pci, interval)     // before CH launch
  ├── thread spawns, polls nvidia-smi every 1s
  ├── CH runs guest VM
  └── CH exits
GpuMeter::stop() -> GpuSample      // after CH exits, joins thread
```

`stop()` sets the `AtomicBool`, joins the thread, and returns the
accumulated `GpuSample`.

### 1d. Error handling

- If `nvidia-smi` is not installed or not in PATH: the polling thread
  returns a zero-valued `GpuSample` with `samples: 0`. A warning is
  logged once.
- If a single poll fails: that interval contributes zero; polling
  continues. A warning is logged for the first failure.
- If the PCI address is invalid: `start()` returns `None`, the executor
  proceeds without metering (same as CPU-only path).

---

## 2. Guest-side GPU self-reporting

### 2a. Guest runner extension

The guest runner (inside the initramfs) already writes
`out/metering.json` with `cpu_seconds`, `peak_mem_kb`, `elapsed_secs`.
When a GPU is detected inside the guest, it also writes
`out/gpu_metering.json`:

```rust
/// Guest-side GPU metrics (written by the guest runner).
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
pub struct GuestGpuMetering {
    /// Wall-clock GPU time from the guest's perspective.
    pub gpu_seconds: f64,
    /// Peak VRAM used inside the guest (MB).
    pub vram_mb_peak: u32,
    /// Average VRAM used inside the guest (MB).
    pub vram_mb_avg: f32,
    /// Average GPU utilization percentage (0–100).
    pub gpu_util_avg_pct: f32,
    /// Guest NVIDIA driver version string.
    pub driver_version: String,
}
```

### 2b. GPU detection inside guest

```bash
nvidia-smi --query-gpu=driver_version --format=csv,noheader
```

If this succeeds, GPU is available. The runner polls at 1s intervals
(until the workload exits) and accumulates the same metrics as the host
side.

If `nvidia-smi` is not available (AMD, or driver not loaded), the runner
skips GPU metering entirely and does not write `gpu_metering.json`.

### 2c. Output

Written to `<job_workdir>/out/gpu_metering.json` alongside the existing
`out/metering.json`. The executor reads this file during
`parse_metering`.

---

## 3. Cross-validation

### 3a. Policy

In `parse_metering`, after both host `GpuSample` and guest
`GuestGpuMetering` are available:

1. Compare `gpu_seconds`: `|host.gpu_seconds - guest.gpu_seconds| > 5.0`
   → warn
2. Compare `vram_gb_hours`: if `host.vram_gb_hours > 0` and
   `|host.vram_gb_hours - guest.vram_gb_hours| / host.vram_gb_hours >
   0.10` → warn
3. **Host values are always authoritative** for the receipt
4. Both host and guest values are included in the JSON receipt for audit
   (but NOT in the canonical signed bytes)

### 3b. Warning format

```
gpu metering mismatch: host={GpuSample:?} guest={GuestGpuMetering:?}
— accepting host values
```

Logged at `eprintln!` level (same as other executor warnings).

### 3c. Missing guest data

If `gpu_metering.json` is absent (guest has no GPU driver, or guest
runner doesn't support GPU metering yet), skip cross-validation silently.
Host values are still used.

---

## 4. `JobMetering` extension

### 4a. New field

```rust
pub struct JobMetering {
    // ... all existing fields unchanged ...
    /// Host-side GPU metering sample. None for CPU-only jobs.
    pub gpu_sample: Option<GpuSample>,
}
```

### 4b. Population

- In `parse_metering`: set `gpu_sample` to `Some(gpu_sample)` when the
  job is a GPU job and the host meter returned data.
- In timeout path: set `gpu_sample` to `Some(default)` with the
  partial sample accumulated before timeout.
- In CPU jobs: `gpu_sample: None`.
- In `NoopCpuExecutor` and `LocalCpuExecutor`: `gpu_sample: None`.

### 4c. Canonical bytes — no change

`gpu_sample` is **not** included in the canonical receipt bytes. Only the
existing `gpu_seconds` and `vram_gb_hours` fields are (they already are
in `job_receipt_canonical_bytes` at `settlement/src/lib.rs:328`). The
`gpu_sample` is metadata in the JSON receipt for audit and debugging.

This avoids changing the settlement signing format or requiring a schema
version bump.

### 4d. JSON receipt

The `gpu_sample` field serializes as a nested JSON object in the signed
receipt:

```json
{
  "metering": {
    "gpu_seconds": 3600.0,
    "vram_gb_hours": 0.42,
    "gpu_sample": {
      "gpu_seconds": 3600.0,
      "vram_gb_hours": 0.42,
      "avg_gpu_util_pct": 85.3,
      "avg_power_watts": 280.5,
      "peak_vram_mb": 16384,
      "samples": 3600
    }
  }
}
```

---

## 5. Config changes

### `CloudHypervisorConfig`

```rust
pub struct CloudHypervisorConfig {
    // ... existing fields ...
    /// Polling interval for host-side GPU metering.
    /// Default: 1 second. Set to Duration::ZERO to disable host GPU metering.
    pub gpu_meter_poll_interval: Duration,
}
```

Default: `Duration::from_secs(1)`.

---

## 6. File change summary

| File | Change |
|------|--------|
| `crates/executor/src/gpu_meter.rs` | **New** — `GpuMeter`, `GpuSample`, polling thread |
| `crates/executor/src/cloud_hypervisor.rs` | Wire `GpuMeter` into `run()`, extend `parse_metering` with cross-validation |
| `crates/executor/src/lib.rs` | Add `gpu_sample: Option<GpuSample>` to `JobMetering`, update `NoopCpuExecutor`/`LocalCpuExecutor` |
| `crates/executor/Cargo.toml` | No new dependencies (uses `std::process::Command`) |
| `crates/executor/tests/ch_gpu_integration.rs` | Verify `vram_gb_hours > 0`, cross-validation test |
| `ROADMAP.md` | Mark §1d as shipped |

---

## 7. Testing

### Unit tests (in `gpu_meter.rs`)

- `gpu_sample_default_is_zero` — `GpuSample::default()` has all zeros
- `gpu_sample_roundtrip_json` — serialize/deserialize preserves values
- `gpu_meter_parse_csv_line` — parse nvidia-smi CSV output correctly

### Unit tests (in `cloud_hypervisor.rs`)

- `parse_metering_populates_gpu_sample` — mock nvidia-smi output → correct
  `GpuSample` in `JobMetering`
- `parse_metering_warns_on_mismatch` — host/guest values differ >10% →
  warning logged (capture stderr)
- `parse_metering_accepts_host_on_mismatch` — host values used despite
  mismatch
- `parse_metering_skips_validation_without_guest` — no `gpu_metering.json`
  → host values used silently

### Integration tests (gated on GPU hardware)

- `gpu_meter_produces_nonzero_vram_hours` — run a GPU job, verify
  `vram_gb_hours > 0.0` in the returned `JobMetering`
- `gpu_meter_guest_writes_gpu_metering_json` — verify
  `out/gpu_metering.json` exists after GPU job

---

## 8. Future work

- **DCGM integration**: feature-gated `dcgm` feature that replaces
  nvidia-smi polling with DCGM library calls. Same `GpuSample` output
  struct, different collection backend.
- **Cgroups-based CPU/mem**: host-side accounting via `cpu.stat` and
  `memory.peak` cgroup files, replacing guest self-reporting as the
  authoritative source.
- **AMD GPU metering**: `rocm-smi` polling path, same `GpuSample` struct.
