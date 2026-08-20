//! Cloud Hypervisor CPU backend — Module 1's first real isolation (ROADMAP
//! §1). Feature-gated behind `cloud-hypervisor`; the default build never
//! compiles this module.
//!
//! Each job runs in a **disposable microVM** booted from the host kernel +
//! a custom initramfs (built by `scripts/build-initramfs.sh`). The guest
//! gets the job via a virtio-fs shared directory (`manifest.json`), runs it,
//! writes `out/result.json` + `out/metering.json`, then powers off. Guest
//! networking is policy-driven: `None` (default, no NIC), `OutboundHttps`
//! (TCP/443 + DNS only), or `Egress` (full egress). Enforcement happens at
//! guest-side (iptables in initramfs) and optionally host-side (nftables on
//! a TAP/bridge). See the design spec
//! `docs/superpowers/specs/2026-08-18-network-policy-enforcement-design.md`.
//!
//! Metering is guest-side (the runner reads `/proc`); the host is
//! authoritative for the wall-clock timeout. See the design spec
//! `docs/superpowers/specs/2026-08-16-cloud-hypervisor-cpu-executor-design.md`.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

use crate::{
    DeviceClass, Executor, ExecutorError, ExitStatus, JobMetering, JobSpec, NetworkPolicy,
};

/// Configuration for the Cloud Hypervisor backend. The node binary supplies
/// this; the executor stays a pure library.
#[derive(Debug, Clone)]
pub struct CloudHypervisorConfig {
    /// Path to the `cloud-hypervisor` binary.
    pub ch_binary: PathBuf,
    /// Host kernel image the guest boots (`/boot/vmlinuz-*`).
    pub kernel: PathBuf,
    /// Initramfs produced by `scripts/build-initramfs.sh`.
    pub initramfs: PathBuf,
    /// Parent dir under which per-job workdirs are staged.
    pub workdir: PathBuf,
    /// Kernel command line (e.g. `"console=ttyS0"`).
    pub cmdline: String,
    /// Path to the virtiofsd binary.
    pub virtiofsd_binary: PathBuf,
    /// Extra flags passed to cloud-hypervisor.
    pub extra_args: Vec<String>,
    /// Debug: keep the workdir after the job for inspection.
    pub keep_jobs: bool,
    /// PCI addresses of VFIO devices to pass through (GPU jobs).
    /// Empty for CPU-only jobs.
    pub vfio_devices: Vec<String>,
    /// Path to the vtessera-gpu helper binary.
    pub gpu_helper: PathBuf,
    /// Allow time-sliced GPU access (multiple jobs sharing one GPU).
    /// Only appropriate when the node operator trusts all workloads.
    /// Default: false (whole-GPU only).
    pub gpu_time_slice: bool,
    /// Interval in seconds between nvidia-smi GPU polling samples.
    /// Lower = more accurate but higher overhead. 0 = disable host-side
    /// GPU metering (guest self-reporting only). Default: 5.
    pub gpu_meter_poll_interval_secs: u64,
    /// Network backend for CH when policy != None.
    /// "tap" (default) creates a TAP device + bridge.
    /// "macvtap" uses macvtap (better perf, harder to firewall).
    pub net_backend: String,
    /// Bridge name for tap backend.
    pub net_bridge: String,
    /// Host CIDR ranges allowed when network = Egress.
    /// Empty = all egress allowed. Non-empty = only these CIDRs.
    pub net_allowed_cidrs: Vec<String>,
    /// Enforcement layer: "guest" (iptables in guest),
    /// "host" (nftables on bridge), or "both".
    pub net_enforcement: String,
}

impl Default for CloudHypervisorConfig {
    fn default() -> Self {
        Self {
            ch_binary: PathBuf::from("/usr/bin/cloud-hypervisor"),
            kernel: PathBuf::from("/boot/vmlinuz"),
            initramfs: PathBuf::from("/var/lib/vtessera/initramfs.cpio.gz"),
            workdir: PathBuf::from("/var/lib/vtessera/jobs"),
            cmdline: "console=ttyS0".into(),
            virtiofsd_binary: PathBuf::from("/usr/libexec/virtiofsd"),
            extra_args: Vec::new(),
            keep_jobs: false,
            vfio_devices: Vec::new(),
            gpu_helper: PathBuf::from("/usr/bin/vtessera-gpu"),
            gpu_time_slice: false,
            gpu_meter_poll_interval_secs: 5,
            net_backend: "tap".into(),
            net_bridge: "virbr0".into(),
            net_allowed_cidrs: Vec::new(),
            net_enforcement: "guest".into(),
        }
    }
}

/// Production CPU backend: runs each job in a Cloud Hypervisor microVM.
#[derive(Default)]
pub struct CloudHypervisorExecutor {
    pub config: CloudHypervisorConfig,
}

/// Wire format written by the host into the shared dir before boot.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobManifest {
    pub job_id: String,
    pub command: Vec<String>,
    pub env: Vec<(String, String)>,
    pub vcpus: u32,
    pub mem_kb: u64,
    pub max_duration_secs: u64,
    /// Network policy for this job ("none", "outbound_https", "egress").
    pub network_policy: String,
    /// CIDRs allowed for egress (only for "egress" policy).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub allowed_cidrs: Vec<String>,
}

/// Wire format the guest runner writes back.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GuestMetering {
    pub cpu_seconds: f64,
    pub peak_mem_kb: u64,
    pub elapsed_secs: u64,
    /// Guest-reported GPU metrics (absent for CPU-only jobs).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gpu: Option<GuestGpuSelfReport>,
}

/// Guest-side GPU self-report (written by the guest runner inside the VM).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GuestGpuSelfReport {
    /// GPU-seconds from the guest's perspective.
    pub gpu_seconds: f64,
    /// Peak VRAM used inside the guest (MB).
    pub vram_mb_peak: u32,
    /// Average VRAM used inside the guest (MB).
    pub vram_mb_avg: f32,
    /// Average GPU utilization (0–100).
    pub gpu_util_avg_pct: f32,
    /// Guest NVIDIA driver version.
    pub driver_version: String,
}

/// Wire format for the guest's exit status.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GuestResult {
    pub exit_code: i32,
}

pub(crate) fn write_manifest(
    path: &Path,
    spec: &JobSpec,
    config: &CloudHypervisorConfig,
) -> Result<(), ExecutorError> {
    let network_policy = match spec.network {
        NetworkPolicy::None => "none".to_string(),
        NetworkPolicy::OutboundHttps => "outbound_https".to_string(),
        NetworkPolicy::Egress => "egress".to_string(),
    };
    let allowed_cidrs = if spec.network == NetworkPolicy::Egress {
        config.net_allowed_cidrs.clone()
    } else {
        Vec::new()
    };
    let manifest = JobManifest {
        job_id: spec.job_id.clone(),
        command: spec.command.clone(),
        env: spec.env.clone(),
        vcpus: spec.devices.vcpus,
        mem_kb: spec.devices.mem_kb,
        max_duration_secs: spec.max_duration_secs,
        network_policy,
        allowed_cidrs,
    };
    let json = serde_json::to_vec(&manifest)
        .map_err(|e| ExecutorError::Backend(format!("encode manifest: {e}")))?;
    fs::write(path, json)
        .map_err(|e| ExecutorError::Backend(format!("write {}: {e}", path.display())))
}

/// Parse the guest's metering and fold in host-side facts the guest can't
/// know (backend, device class, exit status from `result.json`).
pub(crate) fn parse_metering(
    dir: &Path,
    spec: &JobSpec,
    backend: crate::Backend,
    gpu_sample: Option<crate::gpu_meter::GpuSample>,
) -> Result<JobMetering, ExecutorError> {
    let metering_path = dir.join("out").join("metering.json");
    let result_path = dir.join("out").join("result.json");

    let metering_bytes = fs::read(&metering_path).map_err(|e| {
        ExecutorError::Backend(format!(
            "guest did not produce {}: {e}",
            metering_path.display()
        ))
    })?;
    let guest: GuestMetering = serde_json::from_slice(&metering_bytes).map_err(|e| {
        ExecutorError::Backend(format!("malformed {}: {e}", metering_path.display()))
    })?;

    let result_bytes = fs::read(&result_path).map_err(|e| {
        ExecutorError::Backend(format!(
            "guest did not produce {}: {e}",
            result_path.display()
        ))
    })?;
    let result: GuestResult = serde_json::from_slice(&result_bytes)
        .map_err(|e| ExecutorError::Backend(format!("malformed {}: {e}", result_path.display())))?;

    let exit_status = if result.exit_code == 0 {
        ExitStatus::Completed
    } else {
        ExitStatus::Failed {
            code: result.exit_code,
        }
    };

    let is_gpu = matches!(
        spec.devices.class,
        DeviceClass::NvidiaGpu { .. }
            | DeviceClass::NvidiaMig { .. }
            | DeviceClass::NvidiaVgpu { .. }
            | DeviceClass::AmdGpu { .. }
    );

    // Cross-validate host vs guest GPU metrics. Host is authoritative;
    // guest self-report is advisory. Warn on significant mismatch.
    let (host_gpu_seconds, host_vram_gb_hours) = if let Some(ref sample) = gpu_sample {
        if let Some(ref guest_gpu) = guest.gpu {
            let seconds_diff = (sample.gpu_seconds - guest_gpu.gpu_seconds).abs();
            let diff_pct = if guest_gpu.gpu_seconds > 0.0 {
                seconds_diff / guest_gpu.gpu_seconds * 100.0
            } else {
                0.0
            };
            if diff_pct > 20.0 && seconds_diff > 5.0 {
                eprintln!(
                    "gpu_meter: cross-validation warn: host gpu_seconds {:.1} vs guest {:.1} ({:.0}% diff)",
                    sample.gpu_seconds, guest_gpu.gpu_seconds, diff_pct
                );
            }
            // Also warn on VRAM peak mismatch > 30%
            let host_peak = sample.peak_vram_mb as f32;
            let guest_peak = guest_gpu.vram_mb_peak as f32;
            if guest_peak > 0.0 {
                let vram_diff_pct = ((host_peak - guest_peak).abs() / guest_peak) * 100.0;
                if vram_diff_pct > 30.0 {
                    eprintln!(
                        "gpu_meter: cross-validation warn: host peak_vram_mb {} vs guest {} ({:.0}% diff)",
                        sample.peak_vram_mb, guest_gpu.vram_mb_peak, vram_diff_pct
                    );
                }
            }
        }
        (sample.gpu_seconds, sample.vram_gb_hours)
    } else {
        (0.0, 0.0)
    };

    Ok(JobMetering {
        job_id: spec.job_id.clone(),
        backend,
        device: spec.devices.class.clone(),
        cpu_seconds: guest.cpu_seconds,
        peak_mem_kb: guest.peak_mem_kb,
        gpu_seconds: if host_gpu_seconds > 0.0 {
            host_gpu_seconds
        } else if is_gpu {
            guest.elapsed_secs as f64
        } else {
            0.0
        },
        vram_gb_hours: host_vram_gb_hours,
        exit_status,
        elapsed_secs: guest.elapsed_secs,
        gpu_sample,
    })
}

/// CH-specific admission: GPU allowed when vfio_devices configured,
/// network-policy `None` only. When `gpu_time_slice` is false (default),
/// GPU jobs are whole-GPU exclusive; when true, multiple jobs may share
/// one GPU (trusted-tenant only).
fn ch_admission(spec: &JobSpec, config: &CloudHypervisorConfig) -> Result<(), ExecutorError> {
    crate::admission_check(spec)?;
    match &spec.devices.class {
        DeviceClass::Cpu => {}
        DeviceClass::NvidiaMig { .. } => {
            if config.vfio_devices.is_empty() {
                return Err(ExecutorError::Admission(
                    "MIG job requires vfio_devices in config".into(),
                ));
            }
            // Profile validation happens in select_gpu
        }
        DeviceClass::NvidiaVgpu { .. } => {
            if config.vfio_devices.is_empty() {
                return Err(ExecutorError::Admission(
                    "vGPU job requires vfio_devices in config".into(),
                ));
            }
            // Profile validation happens in select_gpu
        }
        DeviceClass::NvidiaGpu { .. } | DeviceClass::AmdGpu { .. } => {
            if config.vfio_devices.is_empty() {
                return Err(ExecutorError::Admission(
                    "GPU job requires vfio_devices in config".into(),
                ));
            }
            // Time-slicing policy: when gpu_time_slice is false, the
            // scheduler must ensure only one GPU job runs at a time on
            // each device. When true, multiple jobs may share a GPU.
            // The actual occupancy check is enforced by select_gpu at
            // schedule time, not here.
        }
    }
    // Network policy: all policies accepted. Enforcement happens at
    // guest-side (iptables in initramfs) and optionally host-side
    // (nftables on bridge). See §1e network policy enforcement spec.
    Ok(())
}

/// GPU state entry read from the helper's state file.
#[derive(Debug, Clone, serde::Deserialize)]
#[allow(dead_code)]
pub(crate) struct GpuDevice {
    pub pci_address: String,
    pub vendor: String,
    pub model: String,
    pub vram_mb: u32,
    pub bound_at: String,
    /// MIG profiles this GPU supports (e.g. "1g.10gb", "3g.40gb").
    #[serde(default)]
    pub mig_profiles: Vec<String>,
    /// Active MIG instances on this GPU.
    #[serde(default)]
    pub mig_instances: Vec<MigInstance>,
    /// Available mediated device types (e.g. "nvidia-256", "nvidia-16").
    #[serde(default)]
    pub mdev_types: Vec<String>,
    /// Active mediated device (vGPU) instances.
    #[serde(default)]
    pub mdev_instances: Vec<MdevInstance>,
}

/// A single MIG instance created on a parent GPU.
#[derive(Debug, Clone, serde::Deserialize)]
#[allow(dead_code)]
pub(crate) struct MigInstance {
    pub uuid: String,
    pub profile: String,
    pub pci_address: String,
    pub vram_mb: u32,
}

/// A single mediated device (vGPU) instance created on a parent GPU.
#[derive(Debug, Clone, serde::Deserialize)]
#[allow(dead_code)]
pub(crate) struct MdevInstance {
    pub uuid: String,
    pub vgpu_type: String,
    pub pci_address: String,
    pub vram_mb: u32,
}

/// Match a GPU job's DeviceRequirements against available VFIO-bound GPUs.
pub(crate) fn select_gpu(
    spec: &JobSpec,
    config: &CloudHypervisorConfig,
) -> Result<Vec<String>, ExecutorError> {
    let state_path = config
        .gpu_helper
        .parent()
        .unwrap_or(&PathBuf::from("/usr/bin"))
        .join("..")
        .join("lib")
        .join("vtessera")
        .join("gpus.json");
    let state_path = if state_path.exists() {
        state_path
    } else {
        PathBuf::from("/var/lib/vtessera/gpus.json")
    };

    let devices: Vec<GpuDevice> = fs::read_to_string(&state_path)
        .ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();

    match &spec.devices.class {
        DeviceClass::NvidiaGpu { .. } => {
            let matched: Vec<String> = devices
                .iter()
                .filter(|g| g.vendor == "nvidia" && g.vram_mb >= spec.devices.min_vram_mb)
                .filter(|g| config.vfio_devices.contains(&g.pci_address))
                .map(|g| g.pci_address.clone())
                .collect();

            if matched.is_empty() {
                let available: Vec<String> = devices
                    .iter()
                    .map(|g| format!("{} ({}, {}MB)", g.pci_address, g.vendor, g.vram_mb))
                    .collect();
                return Err(ExecutorError::Admission(format!(
                    "no matching GPU: vendor=nvidia, min_vram={}MB, available=[{}]",
                    spec.devices.min_vram_mb,
                    available.join(", ")
                )));
            }
            Ok(matched)
        }
        DeviceClass::AmdGpu { .. } => {
            let matched: Vec<String> = devices
                .iter()
                .filter(|g| g.vendor == "amd" && g.vram_mb >= spec.devices.min_vram_mb)
                .filter(|g| config.vfio_devices.contains(&g.pci_address))
                .map(|g| g.pci_address.clone())
                .collect();

            if matched.is_empty() {
                let available: Vec<String> = devices
                    .iter()
                    .map(|g| format!("{} ({}, {}MB)", g.pci_address, g.vendor, g.vram_mb))
                    .collect();
                return Err(ExecutorError::Admission(format!(
                    "no matching GPU: vendor=amd, min_vram={}MB, available=[{}]",
                    spec.devices.min_vram_mb,
                    available.join(", ")
                )));
            }
            Ok(matched)
        }
        DeviceClass::NvidiaMig {
            parent_model,
            profile,
        } => {
            // Find a parent GPU matching the model with an active MIG instance
            // matching the requested profile, whose VFIO PCI address is in our config.
            let matched: Vec<String> = devices
                .iter()
                .filter(|g| {
                    g.vendor == "nvidia"
                        && (parent_model.is_empty() || g.model.contains(parent_model.as_str()))
                })
                .flat_map(|g| {
                    g.mig_instances
                        .iter()
                        .filter(move |m| m.profile == *profile)
                        .map(move |m| (g, m))
                })
                .filter(|(_, m)| config.vfio_devices.contains(&m.pci_address))
                .map(|(_, m)| m.pci_address.clone())
                .collect();

            if matched.is_empty() {
                let available: Vec<String> = devices
                    .iter()
                    .flat_map(|g| {
                        g.mig_instances.iter().map(move |m| {
                            format!(
                                "{} ({}, profile={}, {}MB)",
                                m.pci_address, g.model, m.profile, m.vram_mb
                            )
                        })
                    })
                    .collect();
                return Err(ExecutorError::Admission(format!(
                    "no matching MIG instance: parent_model={parent_model}, profile={profile}, available=[{}]",
                    available.join(", ")
                )));
            }
            Ok(matched)
        }
        DeviceClass::NvidiaVgpu {
            parent_model,
            profile,
        } => {
            // Find a parent GPU matching the model with an active mediated device
            // matching the requested vGPU type, whose VFIO PCI address is in our config.
            let matched: Vec<String> = devices
                .iter()
                .filter(|g| {
                    g.vendor == "nvidia"
                        && (parent_model.is_empty() || g.model.contains(parent_model.as_str()))
                })
                .flat_map(|g| {
                    g.mdev_instances
                        .iter()
                        .filter(move |m| m.vgpu_type == *profile)
                        .map(move |m| (g, m))
                })
                .filter(|(_, m)| config.vfio_devices.contains(&m.pci_address))
                .map(|(_, m)| m.pci_address.clone())
                .collect();

            if matched.is_empty() {
                let available: Vec<String> = devices
                    .iter()
                    .flat_map(|g| {
                        g.mdev_instances.iter().map(move |m| {
                            format!(
                                "{} ({}, type={}, {}MB)",
                                m.pci_address, g.model, m.vgpu_type, m.vram_mb
                            )
                        })
                    })
                    .collect();
                return Err(ExecutorError::Admission(format!(
                    "no matching vGPU instance: parent_model={parent_model}, profile={profile}, available=[{}]",
                    available.join(", ")
                )));
            }
            Ok(matched)
        }
        DeviceClass::Cpu => Ok(Vec::new()),
    }
}

/// Apply host-side nftables rules for a VM's network policy.
/// Uses a per-job chain under the `vtessera` table to avoid collisions.
/// Returns Ok(true) if rules were applied, Ok(false) if nftables is unavailable.
pub(crate) fn apply_host_net_policy(
    job_id: &str,
    tap_dev: &str,
    policy: &NetworkPolicy,
    cidrs: &[String],
) -> Result<bool, ExecutorError> {
    // Check nftables is available.
    if Command::new("nft")
        .args(["--version"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_err()
    {
        return Ok(false);
    }

    let chain = format!("job_{}", &job_id[..job_id.len().min(16)]);

    // Ensure the table exists.
    let _ = Command::new("nft")
        .args(["add", "table", "inet", "vtessera"])
        .status();

    // Create the chain (type filter hook forward, policy accept).
    let _ = Command::new("nft")
        .args([
            "add", "chain", "inet", "vtessera", &chain, "{", "type", "filter", "hook", "forward",
            "priority", "0", ";", "policy", "accept", ";", "}",
        ])
        .status();

    // Flush any previous rules for this chain.
    let _ = Command::new("nft")
        .args(["flush", "chain", "inet", "vtessera", &chain])
        .status();

    match policy {
        NetworkPolicy::OutboundHttps => {
            // Allow established/related.
            let _ = Command::new("nft")
                .args([
                    "add",
                    "rule",
                    "inet",
                    "vtessera",
                    &chain,
                    "iifname",
                    tap_dev,
                    "ct",
                    "state",
                    "established,related",
                    "accept",
                ])
                .status();
            // Allow DNS.
            let _ = Command::new("nft")
                .args([
                    "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "udp", "dport",
                    "53", "accept",
                ])
                .status();
            let _ = Command::new("nft")
                .args([
                    "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "tcp", "dport",
                    "53", "accept",
                ])
                .status();
            // Allow HTTPS.
            let _ = Command::new("nft")
                .args([
                    "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "tcp", "dport",
                    "443", "accept",
                ])
                .status();
            // Drop everything else from this TAP.
            let _ = Command::new("nft")
                .args([
                    "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "drop",
                ])
                .status();
        }
        NetworkPolicy::Egress if !cidrs.is_empty() => {
            let _ = Command::new("nft")
                .args([
                    "add",
                    "rule",
                    "inet",
                    "vtessera",
                    &chain,
                    "iifname",
                    tap_dev,
                    "ct",
                    "state",
                    "established,related",
                    "accept",
                ])
                .status();
            for cidr in cidrs {
                let _ = Command::new("nft")
                    .args([
                        "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "ip",
                        "daddr", cidr, "accept",
                    ])
                    .status();
            }
            let _ = Command::new("nft")
                .args([
                    "add", "rule", "inet", "vtessera", &chain, "iifname", tap_dev, "drop",
                ])
                .status();
        }
        _ => {} // Egress without CIDRs or None: no host restrictions.
    }

    Ok(true)
}

/// Remove host-side nftables rules for a job.
pub(crate) fn remove_host_net_policy(job_id: &str) {
    let chain = format!("job_{}", &job_id[..job_id.len().min(16)]);
    let _ = Command::new("nft")
        .args(["delete", "chain", "inet", "vtessera", &chain])
        .status();
}

impl Executor for CloudHypervisorExecutor {
    fn run(&self, spec: &JobSpec) -> Result<JobMetering, ExecutorError> {
        ch_admission(spec, &self.config)?;

        // virtiofs shared memory + kernel overhead requires >= 128 MiB.
        const MIN_MEM_KB: u64 = 128 * 1024;
        if spec.devices.mem_kb < MIN_MEM_KB {
            return Err(ExecutorError::Admission(format!(
                "mem_kb must be >= {} (128 MiB) for virtiofs shared memory; got {}",
                MIN_MEM_KB, spec.devices.mem_kb,
            )));
        }

        for (what, p) in [
            ("cloud-hypervisor binary", &self.config.ch_binary),
            ("virtiofsd binary", &self.config.virtiofsd_binary),
            ("kernel image", &self.config.kernel),
            ("initramfs", &self.config.initramfs),
        ] {
            if !p.exists() {
                return Err(ExecutorError::Backend(format!(
                    "{what} not found: {}",
                    p.display()
                )));
            }
        }
        if !Path::new("/dev/kvm").exists() {
            return Err(ExecutorError::Backend(
                "/dev/kvm not present; the Cloud Hypervisor backend needs KVM".into(),
            ));
        }

        let job_dir = self.config.workdir.join(&spec.job_id);
        let out_dir = job_dir.join("out");
        if job_dir.exists() {
            return Err(ExecutorError::Backend(format!(
                "workdir already exists for job {} (duplicate job_id?)",
                spec.job_id
            )));
        }
        fs::create_dir_all(&out_dir).map_err(|e| {
            ExecutorError::Backend(format!("stage workdir {}: {e}", job_dir.display()))
        })?;

        let cleanup = |keep: bool| {
            if !keep {
                let _ = fs::remove_dir_all(&job_dir);
            }
        };

        if let Err(e) = write_manifest(&job_dir.join("manifest.json"), spec, &self.config) {
            cleanup(self.config.keep_jobs);
            return Err(e);
        }

        let fs_sock = job_dir.join("virtiofs.sock");

        // --- start virtiofsd ---
        let mut vfsd_cmd = Command::new(&self.config.virtiofsd_binary);
        vfsd_cmd
            .arg("--shared-dir")
            .arg(&job_dir)
            .arg("--socket-path")
            .arg(&fs_sock)
            .arg("--tag")
            .arg("vtessera-job");
        vfsd_cmd.stdin(Stdio::null());
        vfsd_cmd.stdout(Stdio::null());
        vfsd_cmd.stderr(Stdio::null());

        let mut vfsd_child = vfsd_cmd.spawn().map_err(|e| {
            let err = ExecutorError::Backend(format!(
                "spawn {}: {e}",
                self.config.virtiofsd_binary.display()
            ));
            cleanup(self.config.keep_jobs);
            err
        })?;

        // Wait for the socket to appear (up to 5 s).
        let socket_wait = Instant::now();
        loop {
            if fs_sock.exists() {
                break;
            }
            if socket_wait.elapsed() > Duration::from_secs(5) {
                let _ = vfsd_child.kill();
                let _ = vfsd_child.wait();
                cleanup(self.config.keep_jobs);
                return Err(ExecutorError::Backend(
                    "virtiofsd socket did not appear within 5 s".into(),
                ));
            }
            std::thread::sleep(Duration::from_millis(10));
        }

        // --- build cloud-hypervisor command ---
        let mut cmd = Command::new(&self.config.ch_binary);
        cmd.arg("--kernel")
            .arg(&self.config.kernel)
            .arg("--initramfs")
            .arg(&self.config.initramfs)
            .arg("--cmdline")
            .arg(&self.config.cmdline)
            .arg("--cpus")
            .arg(format!("boot={}", spec.devices.vcpus))
            .arg("--memory")
            .arg(format!("size={}K,shared=on", spec.devices.mem_kb))
            .arg("--fs")
            .arg(format!(
                "tag=vtessera-job,socket={},num_queues=1,queue_size=1024",
                fs_sock.display()
            ))
            .arg("--serial")
            .arg("off")
            .arg("--console")
            .arg("off")
            .arg("--api-socket")
            .arg(job_dir.join("ch.sock"))
            .args(&self.config.extra_args);

        // GPU: pass VFIO devices through to the guest.
        #[cfg(feature = "gpu")]
        let mut gpu_meter: Option<crate::gpu_meter::GpuMeter> = None;
        #[cfg(not(feature = "gpu"))]
        let _gpu_meter: Option<()> = None;
        if !self.config.vfio_devices.is_empty() {
            // Validate that requested GPUs are actually bound.
            let _matched = select_gpu(spec, &self.config)?;
            for device in &self.config.vfio_devices {
                cmd.args(["--device", &format!("host={device}")]);
            }
            // Start host-side GPU metering if interval is configured.
            if self.config.gpu_meter_poll_interval_secs > 0 {
                #[cfg(feature = "gpu")]
                {
                    let poll_interval =
                        Duration::from_secs(self.config.gpu_meter_poll_interval_secs);
                    gpu_meter = Some(crate::gpu_meter::GpuMeter::start(
                        &self.config.vfio_devices[0],
                        poll_interval,
                    ));
                }
            }
        }

        // --- networking: create TAP + bridge when policy != None ---
        let needs_net = spec.network != NetworkPolicy::None;
        let tap_dev = if needs_net {
            let job_hex = &spec.job_id[..spec.job_id.len().min(8)];
            let dev = format!("vtap-{job_hex}");
            let mac = format!(
                "02:{:02x}:{:02x}:{:02x}:{:02x}:{:02x}",
                job_hex.as_bytes().first().copied().unwrap_or(0),
                job_hex.as_bytes().get(1).copied().unwrap_or(0),
                job_hex.as_bytes().get(2).copied().unwrap_or(0),
                job_hex.as_bytes().get(3).copied().unwrap_or(0),
                job_hex.as_bytes().get(4).copied().unwrap_or(0),
            );

            // Ensure bridge exists.
            let bridge_exists = Command::new("ip")
                .args(["link", "show", &self.config.net_bridge])
                .output()
                .map(|o| o.status.success())
                .unwrap_or(false);
            if !bridge_exists {
                let status = Command::new("ip")
                    .args(["link", "add", &self.config.net_bridge, "type", "bridge"])
                    .status()
                    .map_err(|e| {
                        ExecutorError::Backend(format!(
                            "create bridge {}: {e}",
                            self.config.net_bridge
                        ))
                    })?;
                if !status.success() {
                    return Err(ExecutorError::Backend(format!(
                        "failed to create bridge {} (need CAP_NET_ADMIN?)",
                        self.config.net_bridge
                    )));
                }
                // Bring up the bridge.
                let _ = Command::new("ip")
                    .args(["link", "set", &self.config.net_bridge, "up"])
                    .status();
            }

            // Create TAP device.
            let status = Command::new("ip")
                .args(["tuntap", "add", "dev", &dev, "mode", "tap"])
                .status()
                .map_err(|e| ExecutorError::Backend(format!("create TAP {dev}: {e}")))?;
            if !status.success() {
                return Err(ExecutorError::Backend(format!(
                    "failed to create TAP {dev} (need CAP_NET_ADMIN?)"
                )));
            }

            // Attach to bridge and bring up.
            let _ = Command::new("ip")
                .args(["link", "set", &dev, "master", &self.config.net_bridge])
                .status();
            let _ = Command::new("ip")
                .args(["link", "set", &dev, "up"])
                .status();

            cmd.args(["--net", &format!("tap={dev},id={mac}")]);
            Some(dev)
        } else {
            None
        };

        // Apply host-side nftables enforcement if configured.
        let _host_nft = if let Some(ref dev) = tap_dev {
            let enforce = &self.config.net_enforcement;
            if enforce == "host" || enforce == "both" {
                match apply_host_net_policy(
                    &spec.job_id,
                    dev,
                    &spec.network,
                    &self.config.net_allowed_cidrs,
                ) {
                    Ok(true) => Some(()),
                    _ => None,
                }
            } else {
                None
            }
        } else {
            None
        };

        cmd.stdin(Stdio::null());
        cmd.stdout(Stdio::null());
        cmd.stderr(Stdio::null());

        let started = Instant::now();
        let mut child = cmd.spawn().map_err(|e| {
            let err =
                ExecutorError::Backend(format!("spawn {}: {e}", self.config.ch_binary.display()));
            let _ = vfsd_child.kill();
            let _ = vfsd_child.wait();
            if let Some(ref dev) = tap_dev {
                let _ = Command::new("ip")
                    .args(["link", "set", dev, "down"])
                    .status();
                let _ = Command::new("ip").args(["link", "delete", dev]).status();
                remove_host_net_policy(&spec.job_id);
            }
            cleanup(self.config.keep_jobs);
            err
        })?;

        let max = Duration::from_secs(spec.max_duration_secs);
        let timed_out = loop {
            match child.try_wait() {
                Ok(Some(_)) => break false,
                Ok(None) => {
                    if started.elapsed() >= max {
                        let _ = child.kill();
                        let _ = child.wait();
                        break true;
                    }
                    std::thread::sleep(Duration::from_millis(50));
                }
                Err(e) => {
                    let err = ExecutorError::Backend(format!("wait: {e}"));
                    let _ = vfsd_child.kill();
                    let _ = vfsd_child.wait();
                    if let Some(ref dev) = tap_dev {
                        let _ = Command::new("ip")
                            .args(["link", "set", dev, "down"])
                            .status();
                        let _ = Command::new("ip").args(["link", "delete", dev]).status();
                        remove_host_net_policy(&spec.job_id);
                    }
                    cleanup(self.config.keep_jobs);
                    return Err(err);
                }
            }
        };

        // Tear down virtiofsd after the VM is gone.
        let _ = vfsd_child.kill();
        let _ = vfsd_child.wait();

        // Tear down TAP device.
        if let Some(ref dev) = tap_dev {
            let _ = Command::new("ip")
                .args(["link", "set", dev, "down"])
                .status();
            let _ = Command::new("ip").args(["link", "delete", dev]).status();
            // Remove host nftables rules for this job.
            remove_host_net_policy(&spec.job_id);
        }

        // Stop GPU metering and capture the sample.
        #[cfg(feature = "gpu")]
        let gpu_sample = gpu_meter.and_then(|mut m| m.stop());
        #[cfg(not(feature = "gpu"))]
        let gpu_sample: Option<crate::gpu_meter::GpuSample> = None;

        let result = if timed_out {
            let elapsed_secs = spec.max_duration_secs.max(1);
            let is_gpu = matches!(
                spec.devices.class,
                DeviceClass::NvidiaGpu { .. }
                    | DeviceClass::NvidiaMig { .. }
                    | DeviceClass::NvidiaVgpu { .. }
                    | DeviceClass::AmdGpu { .. }
            );
            Ok(JobMetering {
                job_id: spec.job_id.clone(),
                backend: crate::Backend::CloudHypervisor,
                device: spec.devices.class.clone(),
                cpu_seconds: 0.0,
                peak_mem_kb: 0,
                gpu_seconds: if is_gpu { elapsed_secs as f64 } else { 0.0 },
                vram_gb_hours: 0.0,
                exit_status: ExitStatus::TimedOut,
                elapsed_secs,
                gpu_sample: None,
            })
        } else {
            parse_metering(&job_dir, spec, crate::Backend::CloudHypervisor, gpu_sample)
        };

        cleanup(self.config.keep_jobs);
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{DeviceClass, DeviceRequirements};

    fn cpu_spec(job_id: &str) -> JobSpec {
        JobSpec {
            job_id: job_id.into(),
            image: "n/a".into(),
            command: vec!["true".into()],
            env: vec![("K".into(), "V".into())],
            devices: DeviceRequirements {
                class: DeviceClass::Cpu,
                vcpus: 1,
                mem_kb: 64 * 1024,
                min_vram_mb: 0,
                driver_hint: None,
            },
            network: NetworkPolicy::None,
            max_duration_secs: 10,
        }
    }

    #[test]
    fn manifest_roundtrips_through_json() {
        let spec = cpu_spec("roundtrip");
        let config = CloudHypervisorConfig::default();
        let dir = std::env::temp_dir().join(format!("vt-rt-{}", std::process::id()));
        let _ = fs::create_dir_all(&dir);
        write_manifest(&dir.join("manifest.json"), &spec, &config).expect("write");
        let bytes = fs::read(dir.join("manifest.json")).expect("read");
        let m: JobManifest = serde_json::from_slice(&bytes).expect("parse");
        assert_eq!(m.job_id, "roundtrip");
        assert_eq!(m.command, spec.command);
        assert_eq!(m.env, spec.env);
        assert_eq!(m.vcpus, 1);
        assert_eq!(m.mem_kb, 64 * 1024);
        assert_eq!(m.max_duration_secs, 10);
        assert_eq!(m.network_policy, "none");
        assert!(m.allowed_cidrs.is_empty());
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn parse_metering_ok() {
        let dir = std::env::temp_dir().join(format!("vt-parse-{}", std::process::id()));
        fs::create_dir_all(dir.join("out")).expect("mkdir");
        fs::write(
            dir.join("out/metering.json"),
            r#"{"cpu_seconds":3.5,"peak_mem_kb":2048,"elapsed_secs":4}"#,
        )
        .expect("write metering");
        fs::write(dir.join("out/result.json"), r#"{"exit_code":0}"#).expect("write result");
        let spec = cpu_spec("parse-ok");
        let m = parse_metering(&dir, &spec, crate::Backend::CloudHypervisor, None).expect("parse");
        assert_eq!(m.job_id, "parse-ok");
        assert_eq!(m.cpu_seconds, 3.5);
        assert_eq!(m.peak_mem_kb, 2048);
        assert_eq!(m.elapsed_secs, 4);
        assert!(matches!(m.exit_status, ExitStatus::Completed));
        assert!(matches!(m.backend, crate::Backend::CloudHypervisor));
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn parse_metering_rejects_malformed() {
        let dir = std::env::temp_dir().join(format!("vt-parse-bad-{}", std::process::id()));
        fs::create_dir_all(dir.join("out")).expect("mkdir");
        fs::write(dir.join("out/metering.json"), "not json").expect("write");
        fs::write(dir.join("out/result.json"), r#"{"exit_code":0}"#).expect("write");
        let spec = cpu_spec("parse-bad");
        assert!(matches!(
            parse_metering(&dir, &spec, crate::Backend::CloudHypervisor, None),
            Err(ExecutorError::Backend(_))
        ));
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn parse_metering_rejects_missing_files() {
        let dir = std::env::temp_dir().join(format!("vt-parse-missing-{}", std::process::id()));
        fs::create_dir_all(dir.join("out")).expect("mkdir");
        let spec = cpu_spec("parse-missing");
        assert!(matches!(
            parse_metering(&dir, &spec, crate::Backend::CloudHypervisor, None),
            Err(ExecutorError::Backend(_))
        ));
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn parse_metering_maps_nonzero_exit() {
        let dir = std::env::temp_dir().join(format!("vt-parse-exit-{}", std::process::id()));
        fs::create_dir_all(dir.join("out")).expect("mkdir");
        fs::write(
            dir.join("out/metering.json"),
            r#"{"cpu_seconds":1.0,"peak_mem_kb":128,"elapsed_secs":1}"#,
        )
        .expect("write metering");
        fs::write(dir.join("out/result.json"), r#"{"exit_code":3}"#).expect("write result");
        let spec = cpu_spec("parse-exit");
        let m = parse_metering(&dir, &spec, crate::Backend::CloudHypervisor, None).expect("parse");
        assert!(matches!(m.exit_status, ExitStatus::Failed { code: 3 }));
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn admission_rejects_gpu() {
        let mut spec = cpu_spec("gpu");
        spec.devices.class = DeviceClass::NvidiaGpu {
            model: "H100".into(),
        };
        let config = CloudHypervisorConfig::default();
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn admission_accepts_outbound_https() {
        let mut spec = cpu_spec("net");
        spec.network = NetworkPolicy::OutboundHttps;
        let config = CloudHypervisorConfig::default();
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_accepts_egress() {
        let mut spec = cpu_spec("net");
        spec.network = NetworkPolicy::Egress;
        let config = CloudHypervisorConfig::default();
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_accepts_cpu_none() {
        let config = CloudHypervisorConfig::default();
        assert!(ch_admission(&cpu_spec("ok"), &config).is_ok());
    }

    #[test]
    fn admission_allows_gpu_with_vfio() {
        let mut spec = cpu_spec("gpu-ok");
        spec.devices.class = DeviceClass::NvidiaGpu {
            model: "H100".into(),
        };
        spec.devices.min_vram_mb = 80000;
        let config = CloudHypervisorConfig {
            vfio_devices: vec!["0000:01:00.0".into()],
            ..Default::default()
        };
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_rejects_gpu_without_vfio() {
        let mut spec = cpu_spec("gpu-novfio");
        spec.devices.class = DeviceClass::NvidiaGpu {
            model: "H100".into(),
        };
        spec.devices.min_vram_mb = 80000;
        let config = CloudHypervisorConfig::default();
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn admission_allows_mig_with_vfio() {
        let mut spec = cpu_spec("mig-ok");
        spec.devices.class = DeviceClass::NvidiaMig {
            parent_model: "H100".into(),
            profile: "1g.10gb".into(),
        };
        spec.devices.min_vram_mb = 10000;
        let config = CloudHypervisorConfig {
            vfio_devices: vec!["0000:01:00.1".into()],
            ..Default::default()
        };
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_rejects_mig_without_vfio() {
        let mut spec = cpu_spec("mig-novfio");
        spec.devices.class = DeviceClass::NvidiaMig {
            parent_model: "H100".into(),
            profile: "1g.10gb".into(),
        };
        spec.devices.min_vram_mb = 10000;
        let config = CloudHypervisorConfig::default();
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn admission_allows_amd_gpu_with_vfio() {
        let mut spec = cpu_spec("amd-gpu");
        spec.devices.class = DeviceClass::AmdGpu {
            model: "MI300X".into(),
        };
        spec.devices.min_vram_mb = 192000;
        let config = CloudHypervisorConfig {
            vfio_devices: vec!["0000:01:00.0".into()],
            ..Default::default()
        };
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_allows_vgpu_with_vfio() {
        let mut spec = cpu_spec("vgpu-ok");
        spec.devices.class = DeviceClass::NvidiaVgpu {
            parent_model: "A100".into(),
            profile: "A100-80GB-5C".into(),
        };
        spec.devices.min_vram_mb = 16000;
        let config = CloudHypervisorConfig {
            vfio_devices: vec!["0000:01:00.0".into()],
            ..Default::default()
        };
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn admission_rejects_vgpu_without_vfio() {
        let mut spec = cpu_spec("vgpu-novfio");
        spec.devices.class = DeviceClass::NvidiaVgpu {
            parent_model: "A100".into(),
            profile: "A100-80GB-5C".into(),
        };
        spec.devices.min_vram_mb = 16000;
        let config = CloudHypervisorConfig::default();
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn gpu_time_slice_default_is_false() {
        let config = CloudHypervisorConfig::default();
        assert!(!config.gpu_time_slice);
    }

    #[test]
    fn gpu_time_slice_allows_gpu_with_vfio() {
        let mut spec = cpu_spec("ts-ok");
        spec.devices.class = DeviceClass::NvidiaGpu {
            model: "H100".into(),
        };
        spec.devices.min_vram_mb = 80000;
        let config = CloudHypervisorConfig {
            vfio_devices: vec!["0000:01:00.0".into()],
            gpu_time_slice: true,
            ..Default::default()
        };
        assert!(ch_admission(&spec, &config).is_ok());
    }

    #[test]
    fn gpu_time_slice_rejects_without_vfio() {
        let mut spec = cpu_spec("ts-novfio");
        spec.devices.class = DeviceClass::NvidiaGpu {
            model: "H100".into(),
        };
        spec.devices.min_vram_mb = 80000;
        let config = CloudHypervisorConfig {
            gpu_time_slice: true,
            ..Default::default()
        };
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
    }

    #[test]
    fn config_defaults_have_net_fields() {
        let config = CloudHypervisorConfig::default();
        assert_eq!(config.net_backend, "tap");
        assert_eq!(config.net_bridge, "virbr0");
        assert!(config.net_allowed_cidrs.is_empty());
        assert_eq!(config.net_enforcement, "guest");
    }

    #[test]
    fn manifest_includes_network_policy() {
        let mut spec = cpu_spec("manifest-net");
        spec.network = NetworkPolicy::OutboundHttps;
        let config = CloudHypervisorConfig::default();
        let dir = std::env::temp_dir().join(format!("vt-manifest-net-{}", std::process::id()));
        let _ = fs::create_dir_all(&dir);
        write_manifest(&dir.join("manifest.json"), &spec, &config).expect("write");
        let bytes = fs::read(dir.join("manifest.json")).expect("read");
        let m: JobManifest = serde_json::from_slice(&bytes).expect("parse");
        assert_eq!(m.network_policy, "outbound_https");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn manifest_egress_with_cidrs() {
        let mut spec = cpu_spec("manifest-egress");
        spec.network = NetworkPolicy::Egress;
        let config = CloudHypervisorConfig {
            net_allowed_cidrs: vec!["10.0.0.0/8".into(), "172.16.0.0/12".into()],
            ..Default::default()
        };
        let dir = std::env::temp_dir().join(format!("vt-manifest-egress-{}", std::process::id()));
        let _ = fs::create_dir_all(&dir);
        write_manifest(&dir.join("manifest.json"), &spec, &config).expect("write");
        let bytes = fs::read(dir.join("manifest.json")).expect("read");
        let m: JobManifest = serde_json::from_slice(&bytes).expect("parse");
        assert_eq!(m.network_policy, "egress");
        assert_eq!(m.allowed_cidrs, vec!["10.0.0.0/8", "172.16.0.0/12"]);
        let _ = fs::remove_dir_all(&dir);
    }
}
