//! Cloud Hypervisor CPU backend — Module 1's first real isolation (ROADMAP
//! §1). Feature-gated behind `cloud-hypervisor`; the default build never
//! compiles this module.
//!
//! Each job runs in a **disposable microVM** booted from the host kernel +
//! a custom initramfs (built by `scripts/build-initramfs.sh`). The guest
//! gets the job via a virtio-fs shared directory (`manifest.json`), runs it,
//! writes `out/result.json` + `out/metering.json`, then powers off. There is
//! **no guest network device** — `NetworkPolicy::None` is the only policy
//! this backend accepts until §1e networking lands.
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
}

/// Wire format the guest runner writes back.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GuestMetering {
    pub cpu_seconds: f64,
    pub peak_mem_kb: u64,
    pub elapsed_secs: u64,
}

/// Wire format for the guest's exit status.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GuestResult {
    pub exit_code: i32,
}

fn write_manifest(path: &Path, spec: &JobSpec) -> Result<(), ExecutorError> {
    let manifest = JobManifest {
        job_id: spec.job_id.clone(),
        command: spec.command.clone(),
        env: spec.env.clone(),
        vcpus: spec.devices.vcpus,
        mem_kb: spec.devices.mem_kb,
        max_duration_secs: spec.max_duration_secs,
    };
    let json = serde_json::to_vec(&manifest)
        .map_err(|e| ExecutorError::Backend(format!("encode manifest: {e}")))?;
    fs::write(path, json)
        .map_err(|e| ExecutorError::Backend(format!("write {}: {e}", path.display())))
}

/// Parse the guest's metering and fold in host-side facts the guest can't
/// know (backend, device class, exit status from `result.json`).
fn parse_metering(
    dir: &Path,
    spec: &JobSpec,
    backend: crate::Backend,
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

    Ok(JobMetering {
        job_id: spec.job_id.clone(),
        backend,
        device: spec.devices.class.clone(),
        cpu_seconds: guest.cpu_seconds,
        peak_mem_kb: guest.peak_mem_kb,
        gpu_seconds: if is_gpu {
            guest.elapsed_secs as f64
        } else {
            0.0
        },
        vram_gb_hours: 0.0,
        exit_status,
        elapsed_secs: guest.elapsed_secs,
    })
}

/// CH-specific admission: GPU allowed when vfio_devices configured,
/// network-policy `None` only.
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
        }
    }
    if spec.network != NetworkPolicy::None {
        return Err(ExecutorError::Admission(
            "CloudHypervisorExecutor has no guest network yet; only NetworkPolicy::None is supported"
                .into(),
        ));
    }
    Ok(())
}

/// GPU state entry read from the helper's state file.
#[derive(Debug, Clone, serde::Deserialize)]
pub struct GpuDevice {
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
pub struct MigInstance {
    pub uuid: String,
    pub profile: String,
    pub pci_address: String,
    pub vram_mb: u32,
}

/// A single mediated device (vGPU) instance created on a parent GPU.
#[derive(Debug, Clone, serde::Deserialize)]
pub struct MdevInstance {
    pub uuid: String,
    pub vgpu_type: String,
    pub pci_address: String,
    pub vram_mb: u32,
}

/// Match a GPU job's DeviceRequirements against available VFIO-bound GPUs.
fn select_gpu(
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

        if let Err(e) = write_manifest(&job_dir.join("manifest.json"), spec) {
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
        if !self.config.vfio_devices.is_empty() {
            // Validate that requested GPUs are actually bound.
            let _matched = select_gpu(spec, &self.config)?;
            for device in &self.config.vfio_devices {
                cmd.args(["--device", &format!("host={device}")]);
            }
        }

        cmd.stdin(Stdio::null());
        cmd.stdout(Stdio::null());
        cmd.stderr(Stdio::null());

        let started = Instant::now();
        let mut child = cmd.spawn().map_err(|e| {
            let err =
                ExecutorError::Backend(format!("spawn {}: {e}", self.config.ch_binary.display()));
            let _ = vfsd_child.kill();
            let _ = vfsd_child.wait();
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
                    cleanup(self.config.keep_jobs);
                    return Err(err);
                }
            }
        };

        // Tear down virtiofsd after the VM is gone.
        let _ = vfsd_child.kill();
        let _ = vfsd_child.wait();

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
            })
        } else {
            parse_metering(&job_dir, spec, crate::Backend::CloudHypervisor)
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
        let dir = std::env::temp_dir().join(format!("vt-rt-{}", std::process::id()));
        let _ = fs::create_dir_all(&dir);
        write_manifest(&dir.join("manifest.json"), &spec).expect("write");
        let bytes = fs::read(dir.join("manifest.json")).expect("read");
        let m: JobManifest = serde_json::from_slice(&bytes).expect("parse");
        assert_eq!(m.job_id, "roundtrip");
        assert_eq!(m.command, spec.command);
        assert_eq!(m.env, spec.env);
        assert_eq!(m.vcpus, 1);
        assert_eq!(m.mem_kb, 64 * 1024);
        assert_eq!(m.max_duration_secs, 10);
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
        let m = parse_metering(&dir, &spec, crate::Backend::CloudHypervisor).expect("parse");
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
            parse_metering(&dir, &spec, crate::Backend::CloudHypervisor),
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
            parse_metering(&dir, &spec, crate::Backend::CloudHypervisor),
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
        let m = parse_metering(&dir, &spec, crate::Backend::CloudHypervisor).expect("parse");
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
    fn admission_rejects_network() {
        let mut spec = cpu_spec("net");
        spec.network = NetworkPolicy::OutboundHttps;
        let config = CloudHypervisorConfig::default();
        assert!(matches!(
            ch_admission(&spec, &config),
            Err(ExecutorError::Admission(_))
        ));
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
}
