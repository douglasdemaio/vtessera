# Implementation plan — Per-device GPU metering (Module 1d)

Implements the design in `docs/superpowers/specs/2026-08-18-per-device-gpu-metering-design.md`.

## Overview

| Phase | What | Files |
|-------|------|-------|
| 1 | `GpuMeter` + `GpuSample` polling thread | `crates/executor/src/gpu_meter.rs` (new) |
| 2 | `JobMetering` extension | `crates/executor/src/lib.rs` |
| 3 | Wire `GpuMeter` into CH executor | `crates/executor/src/cloud_hypervisor.rs` |
| 4 | Guest-side GPU self-reporting | `crates/executor/src/gpu_meter.rs` (GuestGpuMetering) |
| 5 | Cross-validation in `parse_metering` | `crates/executor/src/cloud_hypervisor.rs` |
| 6 | Unit tests | `gpu_meter.rs`, `cloud_hypervisor.rs` |
| 7 | Integration tests | `ch_gpu_integration.rs` |
| 8 | ROADMAP update | `ROADMAP.md` |

---

## Phase 1 — `GpuMeter` + `GpuSample` (new file)

Create `crates/executor/src/gpu_meter.rs`, feature-gated on `gpu`.

### 1a. `GpuSample` struct

```rust
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize, PartialEq)]
pub struct GpuSample {
    pub gpu_seconds: f64,
    pub vram_gb_hours: f64,
    pub avg_gpu_util_pct: f32,
    pub avg_power_watts: f32,
    pub peak_vram_mb: u32,
    pub samples: u32,
}
```

### 1b. `GpuMeter` struct + `start`/`stop`

```rust
pub struct GpuMeter {
    pci_address: String,
    poll_interval: Duration,
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<GpuSample>>,
}

impl GpuMeter {
    pub fn start(pci_address: &str, poll_interval: Duration) -> Self { ... }
    pub fn stop(&mut self) -> Option<GpuSample> { ... }
}
```

### 1c. Polling thread function

`fn poll_gpu(pci: &str, interval: Duration, stop: Arc<AtomicBool>) -> GpuSample`

- Loop: call `nvidia-smi --query-gpu=memory.used,utilization.gpu,power.draw --format=csv,noheader --id=<pci>`
- Parse CSV line
- Accumulate `vram_gb_hours += (vram_mb / 1024.0) * dt / 3600.0`
- Sleep `interval`, check `stop`
- On nvidia-smi failure: log warning once, zero for that sample, continue

### 1d. `GuestGpuMetering` struct

```rust
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
pub struct GuestGpuMetering {
    pub gpu_seconds: f64,
    pub vram_mb_peak: u32,
    pub vram_mb_avg: f32,
    pub gpu_util_avg_pct: f32,
    pub driver_version: String,
}
```

### 1e. Register module

In `crates/executor/src/lib.rs`, add:
```rust
#[cfg(feature = "gpu")]
pub mod gpu_meter;
```

---

## Phase 2 — `JobMetering` extension

In `crates/executor/src/lib.rs`:

### 2a. Add field to `JobMetering`

```rust
pub struct JobMetering {
    // ... existing fields ...
    /// Host-side GPU metering sample (None for CPU jobs).
    pub gpu_sample: Option<gpu_meter::GpuSample>,
}
```

### 2b. Update all `JobMetering` constructors

Every place that creates a `JobMetering` needs `gpu_sample: None` (or `Some(...)` for GPU paths):

| File | Line(s) | Change |
|------|---------|--------|
| `lib.rs` — `NoopCpuExecutor` | ~279 | Add `gpu_sample: None` |
| `lib.rs` — `LocalCpuExecutor` success | ~380 | Add `gpu_sample: None` |
| `lib.rs` — `LocalCpuExecutor` timeout | ~352 | Add `gpu_sample: None` |
| `cloud_hypervisor.rs` — `parse_metering` | ~167 | Add `gpu_sample: None` (will be wired in Phase 3) |
| `cloud_hypervisor.rs` — timeout path | ~609 | Add `gpu_sample: None` |

### 2c. Update tests that construct `JobMetering`

| File | Test | Change |
|------|------|--------|
| `settlement/src/lib.rs` | `device_seconds_for_selects_cpu_meter` (~916) | Add `gpu_sample: None` |
| `settlement/tests/settle_bin.rs` | `sample_job_metering` (~65) | Add `gpu_sample: None` |

---

## Phase 3 — Wire `GpuMeter` into CH executor

In `crates/executor/src/cloud_hypervisor.rs`:

### 3a. Config change

Add to `CloudHypervisorConfig`:
```rust
pub gpu_meter_poll_interval: Duration, // default: Duration::from_secs(1)
```

Update `Default` impl.

### 3b. `run()` integration

In the `Executor::run` impl for `CloudHypervisorExecutor`:

```
1. After CH launch (before wait), if GPU job:
   let mut gpu_meter = GpuMeter::start(&pci_address, config.gpu_meter_poll_interval);
2. After CH exits:
   let gpu_sample = gpu_meter.stop();
3. Pass gpu_sample into parse_metering
```

### 3c. Extend `parse_metering` signature

```rust
fn parse_metering(
    job_dir: &Path,
    spec: &JobSpec,
    backend: Backend,
    gpu_sample: Option<GpuSample>,  // new parameter
) -> Result<JobMetering, ExecutorError>
```

### 3d. Populate `gpu_sample` in `JobMetering`

- If `gpu_sample` is `Some(sample)` and job is GPU: set `gpu_sample: Some(sample.clone())`, set `gpu_seconds: sample.gpu_seconds`, set `vram_gb_hours: sample.vram_gb_hours`
- If `gpu_sample` is `None` or CPU job: `gpu_sample: None`

---

## Phase 4 — Guest-side GPU self-reporting

The guest runner is inside the initramfs. For this PR, add a helper function in `gpu_meter.rs` that can be called from the guest runner:

### 4a. `detect_guest_gpu() -> bool`

```bash
nvidia-smi --query-gpu=driver_version --format=csv,noheader
```

Returns true if GPU is detected.

### 4b. `sample_guest_gpu() -> Option<GuestGpuMetering>`

Polls `nvidia-smi --query-gpu=memory.used,memory.total,utilization.gpu --format=csv,noheader` and returns `Some(GuestGpuMetering)` with accumulated metrics.

### 4c. Integration with guest runner

The guest runner already writes `out/metering.json`. After the workload exits, if GPU is detected, also write `out/gpu_metering.json`.

**Note**: The guest runner binary is built separately (initramfs build). The struct definition goes in `gpu_meter.rs` so both host and guest can share the type. The actual guest runner integration may be a separate PR if the initramfs build is decoupled.

---

## Phase 5 — Cross-validation in `parse_metering`

In `parse_metering`, after both host `GpuSample` and guest `GuestGpuMetering` are available:

### 5a. Read guest GPU metering

```rust
let guest_gpu: Option<GuestGpuMetering> = fs::read_to_string(job_dir.join("out/gpu_metering.json"))
    .ok()
    .and_then(|s| serde_json::from_str(&s).ok());
```

### 5b. Cross-validate

```rust
if let (Some(ref host), Some(ref guest)) = (&gpu_sample, &guest_gpu) {
    if (host.gpu_seconds - guest.gpu_seconds).abs() > 5.0 {
        eprintln!("gpu metering mismatch: host gpu_seconds={} guest gpu_seconds={}", host.gpu_seconds, guest.gpu_seconds);
    }
    if host.vram_gb_hours > 0.0 {
        let diff_pct = (host.vram_gb_hours - guest.vram_gb_hours).abs() / host.vram_gb_hours;
        if diff_pct > 0.10 {
            eprintln!("gpu metering mismatch: host vram_gb_hours={} guest vram_gb_hours={}", host.vram_gb_hours, guest.vram_gb_hours);
        }
    }
}
```

### 5c. Update all `parse_metering` call sites

Every call to `parse_metering` must pass the new `gpu_sample` parameter:

| Location | File |
|----------|------|
| `run()` success path | `cloud_hypervisor.rs` ~587 |
| `run()` timeout path | `cloud_hypervisor.rs` ~593 |

---

## Phase 6 — Unit tests

### In `gpu_meter.rs`:

- `gpu_sample_default_is_zero` — all fields zero
- `gpu_sample_roundtrip_json` — serialize → deserialize → equal
- `gpu_meter_parse_csv_line` — parse `"16384, 85, 280.5"` correctly
- `gpu_meter_stop_returns_sample` — start → stop → Some(GpuSample)

### In `cloud_hypervisor.rs`:

- `parse_metering_populates_gpu_sample` — mock job dir with metering.json, pass Some(GpuSample) → verify JobMetering.gpu_sample is Some
- `parse_metering_warns_on_mismatch` — pass host and guest values that differ >10% → verify warning on stderr
- `parse_metering_skips_validation_without_guest` — no gpu_metering.json → no warning, host values used

---

## Phase 7 — Integration tests

In `crates/executor/tests/ch_gpu_integration.rs`:

- `gpu_meter_produces_nonzero_vram_hours` — run a GPU job, assert `vram_gb_hours > 0.0`
- `gpu_meter_guest_writes_gpu_metering_json` — assert `out/gpu_metering.json` exists (requires guest runner integration)

Both gated on `gpu_available()`.

---

## Phase 8 — ROADMAP update

In `ROADMAP.md` §1d, update the status to shipped and reference the spec.

---

## Verification

1. `cargo fmt --all`
2. `cargo clippy --workspace --all-targets --features gpu -- -D warnings`
3. `cargo test --workspace --exclude vtessera-gui`
4. `cargo test -p vtessera-executor --bin vtessera-gpu --features gpu`
5. Integration on GPU host (if available): verify `vram_gb_hours > 0`
