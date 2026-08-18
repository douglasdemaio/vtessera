# Module 1c continued — GPU sharing (MIG, vGPU, time-slicing)

Date: 2026-08-18
Status: approved plan (one PR per mode, confirmed 2026-08-18)
Related: `ROADMAP.md` §1c, `crates/executor`, `docs/superpowers/specs/2026-08-17-cloud-hypervisor-gpu-passthrough-design.md`

## Decisions (confirmed 2026-08-18)

- **PR strategy:** one PR per sharing mode (MIG, vGPU, time-slicing)
- **vGPU modeling:** new `DeviceClass::NvidiaVgpu` variant (not extending
  NvidiaMig with a sharing-mode field)
- **Time-slicing:** config flag on `CloudHypervisorConfig` (not a new
  DeviceClass variant)

## What shipped in §1c (2026-08-17)

Whole-GPU VFIO passthrough via Cloud Hypervisor. The `vtessera-gpu` helper
manages PCI bind/unbind; the executor passes `--device host=...` to CH;
guest init loads vendor drivers from the workload image. NVIDIA + AMD.
MIG, vGPU, and time-slicing were explicitly out of scope.

## What §1c continued adds

Three GPU-sharing modes, ordered strongest to weakest isolation:

1. **MIG** — hardware-partitioned instances on A100/H100+. Strongest
   isolation. One tenant per MIG slice. The parent GPU is physically
   partitioned by NVIDIA firmware; each slice gets dedicated compute
   units, memory, and L2 cache.

2. **vGPU / mediated devices** — software-partitioned via NVIDIA vGPU
   (licensed) or AMD MxGPU. Medium isolation. The parent GPU time-shares
   at the hardware level with driver-enforced isolation. Requires vendor
   licensing server.

3. **Time-slicing** — software-only, single trusted tenant. Weakest
   isolation. Multiple jobs share the same whole GPU, pre-empted by the
   driver. Only appropriate when the node operator trusts the workload.

## What already exists in the type system

- `DeviceClass::NvidiaMig { parent_model, profile }` — defined in
  `executor/src/lib.rs`, serialization works, settlement handles it
  (tag byte `2`), offer indexing supports `nvidia_mig` filtering, MCP
  discover accepts it. **But `ch_admission` hard-rejects it.**
- `AdvertisedDevice::NvidiaMig { parent_model, profile, vram_mb }` —
  defined in `offer/src/lib.rs`, canonical tag `3`.
- `FilterDevice::NvidiaMig` — defined in `offer-index/src/lib.rs`,
  parses from `nvidia_mig` query param.

## PR strategy

Three separate PRs, merged incrementally:

1. **PR: MIG partitioning** — Phase 1 only. Extends `vtessera-gpu` with
   `mig-*` subcommands, relaxes `ch_admission` for `NvidiaMig`, adds
   MIG-aware `select_gpu`. Most value — hardware isolation on A100/H100+.

2. **PR: vGPU / mediated devices** — Phase 2 + Phase 4 (offer/MCP/index
   wiring for vGPU). New `DeviceClass::NvidiaVgpu` variant, `mdev-*`
   subcommands, full offer/settlement/MCP/index wiring. Depends on MIG
   PR for `GpuDevice` struct extensions.

3. **PR: Time-slicing** — Phase 3 + Phase 4 (time-slicing wiring).
   Config flag, admission enforcement. Smallest change.

## Scope

**In scope:**

- MIG: `vtessera-gpu` subcommands (`mig-list`, `mig-create`,
  `mig-destroy`), `GpuDevice` struct extension, `ch_admission` relaxed,
  `select_gpu` MIG-aware, guest init MIG detection, unit + integration
  tests.
- vGPU: new `DeviceClass::NvidiaVgpu` variant, `vtessera-gpu` mediated
  device subcommands, `ch_admission` + `select_gpu` for vGPU, offer +
  settlement + MCP + index wiring, tests.
- Time-slicing: config-level flag on `CloudHypervisorConfig`, admission
  enforcement (single-tenant only), no PCI passthrough change needed.

**Out of scope:**

- AMD MxGPU (licensed vGPU for AMD) — follow-up.
- Dynamic MIG reconfiguration at runtime (profiles are set by operator
  before jobs land).
- DCGM per-device metering (§1d).
- Guest networking (§1e).

---

## Phase 1 — MIG partitioning (A100/H100+)

### 1a. Extend `GpuDevice` struct

Add MIG-specific fields to the state file struct (both in
`crates/executor/src/bin/vtessera_gpu.rs` and
`crates/executor/src/cloud_hypervisor.rs`):

```rust
pub struct GpuDevice {
    pub pci_address: String,
    pub vendor: String,
    pub model: String,
    pub vram_mb: u32,
    pub bound_at: String,
    /// MIG profile if this is a MIG-capable GPU (e.g. "1g.10gb").
    /// None for non-MIG GPUs.
    pub mig_profiles: Vec<String>,
    /// Active MIG instance UUIDs, if any.
    pub mig_instances: Vec<MigInstance>,
}

pub struct MigInstance {
    pub uuid: String,
    pub profile: String,
    pub pci_address: String,  // MIG instance's VFIO PCI address
    pub vram_mb: u32,
}
```

### 1b. MIG subcommands for `vtessera-gpu`

New subcommands:

| Command | What it does |
|---------|-------------|
| `vtessera-gpu mig-list --device <ADDR>` | List available MIG profiles and active instances for a GPU |
| `vtessera-gpu mig-create --device <ADDR> --profile <PROFILE>` | Create a MIG instance, bind to vfio-pci, update state |
| `vtessera-gpu mig-destroy --device <ADDR> --uuid <UUID>` | Destroy a MIG instance, remove from state |

**MIG profile detection** (sysfs):
- Available profiles: `/sys/bus/pci/devices/<addr>/mig_manager/available_profiles`
  or `nvidia-smi mig --list` output parsing.
- Active instances: `/sys/bus/pci/devices/<addr>/mig_manager/instances/`
  or `nvidia-smi mig --list-devices` output parsing.
- MIG instance PCI address: discovered from sysfs after creation.

**MIG create flow:**
1. Read available profiles from sysfs/nvidia-smi
2. Validate requested profile exists
3. `nvidia-smi mig --create-gpu-instance <profile> --gpu <pci_addr>`
4. Discover the MIG instance's PCI address from sysfs
5. Unbind MIG instance from nvidia driver, bind to vfio-pci
6. Write/update state file with MIG instance entry

**MIG destroy flow:**
1. Unbind MIG instance from vfio-pci
2. `nvidia-smi mig --destroy-gpu-instance --instance <uuid>`
3. Remove from state file

### 1c. Relax `ch_admission` for MIG

Change `ch_admission` to allow `NvidiaMig` when the parent GPU is
available and the requested profile exists:

```rust
DeviceClass::NvidiaMig { parent_model, profile } => {
    if config.vfio_devices.is_empty() {
        return Err(ExecutorError::Admission(
            "MIG job requires vfio_devices in config".into(),
        ));
    }
    // Profile validation happens in select_gpu
}
```

### 1d. MIG-aware `select_gpu`

Extend `select_gpu` to handle `NvidiaMig`:
1. Read state file
2. Find parent GPU matching `parent_model`
3. Find active MIG instance matching `profile`
4. Return the MIG instance's VFIO PCI address
5. Error if no matching MIG instance found

### 1e. CH command for MIG

MIG instances appear as separate VFIO PCI devices. The CH command
already does `--device host=<addr>` per VFIO device. No change needed —
the MIG instance's PCI address is just another VFIO device.

### 1f. Guest init for MIG

MIG instances appear as regular PCI GPUs to the guest. The existing
GPU detection in `build-initramfs.sh` already scans for VGA/3D
controllers and loads drivers. No change needed — the guest sees a
regular NVIDIA GPU.

### 1g. Unit tests

- `ch_admission` allows NvidiaMig when parent GPU available
- `ch_admission` rejects NvidiaMig when vfio_devices empty
- `select_gpu` matches correct MIG profile
- `select_gpu` errors on missing profile
- `GpuDevice` serialization with MIG fields
- `MigInstance` serialization round-trip

### 1h. Integration tests

- `mig_create_and_destroy`: create MIG instance, verify state, destroy
- `mig_admission_matches_profile`: job requesting specific profile gets
  matched to correct instance
- `mig_rejects_wrong_profile`: job requesting unavailable profile fails

---

## Phase 2 — vGPU / mediated devices

### 2a. New `DeviceClass::NvidiaVgpu`

Add variant to `DeviceClass` in `executor/src/lib.rs`:

```rust
/// NVIDIA vGPU instance (software-partitioned, licensed).
/// `parent_model` is the host GPU, `profile` is the vGPU type
/// (e.g. "Tesla-M10-8Q", "A100-80GB-5C").
NvidiaVgpu {
    parent_model: String,
    profile: String,
},
```

### 2b. Offer crate wiring

Add `AdvertisedDevice::NvidiaVgpu` in `offer/src/lib.rs`:

```rust
NvidiaVgpu {
    parent_model: String,
    profile: String,
    vram_mb: u32,
},
```

Canonical tag byte: `5` (appended after existing tags 1-4).

### 2c. Settlement wiring

Add `device_tag` entry for `NvidiaVgpu` in `settlement/src/lib.rs`:

```rust
DeviceClass::NvidiaVgpu { parent_model, profile } => {
    buf.push(4);
    // ... serialize parent_model and profile
}
```

`device_seconds_for` already uses `_ => gpu_seconds` wildcard, so vGPU
jobs naturally settle against `gpu_seconds`.

### 2d. `vtessera-gpu` mediated device subcommands

| Command | What it does |
|---------|-------------|
| `vtessera-gpu mdev-list --device <ADDR>` | List available mediated device types |
| `vtessera-gpu mdev-create --device <ADDR> --type <TYPE>` | Create mediated device, bind to vfio-pci |
| `vtessera-gpu mdev-destroy --uuid <UUID>` | Destroy mediated device |

**Mediated device detection** (sysfs):
- Available types: `/sys/bus/pci/devices/<addr>/mdev_supported_types/`
- Active devices: `/sys/bus/pci/devices/<addr>/mdev_supported_types/<type>/devices/`
- UUID: directory name under the type

### 2e. Extend `GpuDevice` for vGPU

```rust
pub struct GpuDevice {
    // ... existing fields ...
    pub mig_profiles: Vec<String>,
    pub mig_instances: Vec<MigInstance>,
    /// Available mediated device types (e.g. "nvidia-256").
    pub mdev_types: Vec<String>,
    /// Active mediated device instances.
    pub mdev_instances: Vec<MdevInstance>,
}

pub struct MdevInstance {
    pub uuid: String,
    pub vgpu_type: String,
    pub pci_address: String,
    pub vram_mb: u32,
}
```

### 2f. `ch_admission` and `select_gpu` for vGPU

Similar to MIG but matching on `NvidiaVgpu`:
1. Find parent GPU
2. Find active mediated device matching `profile`
3. Return the mdev's VFIO PCI address

### 2g. Unit + integration tests

- New `DeviceClass::NvidiaVgpu` serialization round-trip
- `ch_admission` allows NvidiaVgpu when mdev configured
- `select_gpu` matches vGPU profile
- Offer canonical tag stability test extended
- Integration tests gated on licensed vGPU host

---

## Phase 3 — Time-slicing

### 3a. Config-level flag

Add to `CloudHypervisorConfig`:

```rust
/// Allow time-sliced GPU access (multiple jobs sharing one GPU).
/// Only appropriate when the node operator trusts all workloads.
/// Default: false (whole-GPU only).
pub gpu_time_slice: bool,
```

### 3b. Admission enforcement

When `gpu_time_slice` is false (default), reject GPU jobs if the GPU
is already in use. When true, allow multiple jobs on the same GPU.

Time-slicing reuses the existing `NvidiaGpu` variant — no new
`DeviceClass` variant needed. The PCI address is the same whole-GPU
address. The difference is purely in admission policy.

### 3c. No PCI passthrough change

Time-slicing doesn't change how the GPU is passed to CH. The same
`--device host=<addr>` is used. The NVIDIA driver handles preemption
internally.

### 3d. Guest init

No change. The guest sees a regular GPU. Driver-level time-slicing is
transparent to the guest.

### 3e. Unit tests

- `ch_admission` rejects GPU job when GPU busy and `gpu_time_slice=false`
- `ch_admission` allows GPU job when GPU busy and `gpu_time_slice=true`
- Config default has `gpu_time_slice=false`

---

## Phase 4 — Offer + MCP + Index wiring

### 4a. MCP discover tool

Add `"nvidia_vgpu"` and `"time_slice"` to the device filter enum in
`crates/node-api/src/mcp.rs`.

### 4b. Offer index

Add `FilterDevice::NvidiaVgpu` and `FilterDevice::TimeSlice` to
`crates/offer-index/src/lib.rs`, with query string parsing.

### 4c. Node binary

- Add `--gpu-time-slice` CLI flag to `vtessera-node`
- Wire through to `CloudHypervisorConfig.gpu_time_slice`

---

## Phase 5 — Verification

1. `cargo fmt --all`
2. `cargo clippy --workspace --all-targets --features gpu -- -D warnings`
3. `cargo test --workspace --features cloud-hypervisor` (CPU tests unchanged)
4. `cargo test --workspace --features gpu` (unit tests including MIG/vGPU)
5. Integration tests on MIG-equipped host (if available)
6. Offer canonical tag stability test passes
7. Settlement device_tag test passes with new variants

---

## File change summary

| File | Change |
|------|--------|
| `crates/executor/src/lib.rs` | Add `NvidiaVgpu` variant to `DeviceClass` |
| `crates/executor/src/cloud_hypervisor.rs` | Relax `ch_admission`, extend `select_gpu`, add `gpu_time_slice` to config |
| `crates/executor/src/bin/vtessera_gpu.rs` | Extend `GpuDevice`, add `MigInstance`/`MdevInstance`, add `mig-*`/`mdev-*` subcommands |
| `crates/executor/Cargo.toml` | No change (MIG/vGPU/time-slicing fold into existing `gpu` feature) |
| `crates/executor/tests/ch_gpu_integration.rs` | Add MIG/vGPU/time-slicing integration tests |
| `crates/offer/src/lib.rs` | Add `AdvertisedDevice::NvidiaVgpu`, canonical tag `5` |
| `crates/settlement/src/lib.rs` | Add `device_tag` for `NvidiaVgpu`, stability test |
| `crates/node-api/src/mcp.rs` | Add `nvidia_vgpu` to device filter enum |
| `crates/node-api/src/bin/vtessera_node.rs` | Add `--gpu-time-slice` flag, wire config |
| `crates/offer-index/src/lib.rs` | Add `FilterDevice::NvidiaVgpu`, query parsing |
| `ROADMAP.md` | Update §1c status |
| `docs/superpowers/specs/` | New design spec for GPU sharing |
