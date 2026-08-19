//! Cloud Hypervisor GPU integration tests.
//!
//! Gated on:
//!   - `VTESSERA_CH_INTEGRATION=1`
//!   - `CH_INITRAMFS` pointing to a built initramfs
//!   - A real GPU bound to vfio-pci (checked via `vtessera-gpu list`)
//!
//! These tests only run on a GPU-equipped host. On a CPU-only host they
//! compile but are skipped at runtime.

use std::env;
use std::fs;
use std::path::PathBuf;
use std::process::Command;

use vtessera_executor::cloud_hypervisor::{CloudHypervisorConfig, CloudHypervisorExecutor};
use vtessera_executor::{
    Backend, DeviceClass, DeviceRequirements, Executor, ExitStatus, JobSpec, NetworkPolicy,
};

fn gpu_available() -> bool {
    if env::var("VTESSERA_CH_INTEGRATION").unwrap_or_default() != "1" {
        return false;
    }
    if env::var("CH_INITRAMFS").unwrap_or_default().is_empty() {
        return false;
    }
    // Check for VFIO-bound GPUs
    let output = Command::new("vtessera-gpu").arg("list").output().ok();
    match output {
        Some(o) if o.status.success() => {
            let devices: Vec<serde_json::Value> =
                serde_json::from_slice(&o.stdout).unwrap_or_default();
            !devices.is_empty()
        }
        _ => false,
    }
}

fn test_config() -> CloudHypervisorConfig {
    let bin = env::var("CH_BINARY").unwrap_or_else(|_| "/usr/bin/cloud-hypervisor".into());
    let kernel = env::var("CH_KERNEL").unwrap_or_else(|_| "/boot/vmlinuz".into());
    let initramfs = env::var("CH_INITRAMFS").unwrap();
    let vfsd = env::var("VIRTIOFSD_BINARY").unwrap_or_else(|_| "/usr/libexec/virtiofsd".into());
    let workdir = env::temp_dir().join(format!("vt-ch-gpu-{}", std::process::id()));
    fs::create_dir_all(&workdir).expect("create workdir");

    // Discover available GPUs
    let output = Command::new("vtessera-gpu")
        .arg("list")
        .output()
        .expect("run vtessera-gpu list");
    let devices: Vec<serde_json::Value> =
        serde_json::from_slice(&output.stdout).unwrap_or_default();
    let vfio_devices: Vec<String> = devices
        .iter()
        .filter_map(|d| d["pci_address"].as_str().map(String::from))
        .collect();

    CloudHypervisorConfig {
        ch_binary: PathBuf::from(bin),
        kernel: PathBuf::from(kernel),
        initramfs: PathBuf::from(initramfs),
        workdir,
        cmdline: "console=ttyS0".into(),
        virtiofsd_binary: PathBuf::from(vfsd),
        extra_args: vec![],
        keep_jobs: false,
        vfio_devices,
        gpu_helper: PathBuf::from("/usr/bin/vtessera-gpu"),
        gpu_time_slice: false,
        gpu_meter_poll_interval_secs: 5,
    }
}

fn gpu_spec(
    job_id: &str,
    command: Vec<String>,
    max_duration_secs: u64,
    model: &str,
    min_vram_mb: u32,
) -> JobSpec {
    // Detect vendor from model name
    let class = if model.starts_with("MI") {
        DeviceClass::AmdGpu {
            model: model.into(),
        }
    } else {
        DeviceClass::NvidiaGpu {
            model: model.into(),
        }
    };
    JobSpec {
        job_id: job_id.into(),
        image: "n/a".into(),
        command,
        env: vec![("VT_TEST".into(), "1".into())],
        devices: DeviceRequirements {
            class,
            vcpus: 1,
            mem_kb: 256 * 1024,
            min_vram_mb,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs,
    }
}

fn first_gpu_model() -> Option<String> {
    let output = Command::new("vtessera-gpu").arg("list").output().ok()?;
    if !output.status.success() {
        return None;
    }
    let devices: Vec<serde_json::Value> = serde_json::from_slice(&output.stdout).ok()?;
    devices.first()?.get("model")?.as_str().map(String::from)
}

fn first_gpu_vram() -> Option<u32> {
    let output = Command::new("vtessera-gpu").arg("list").output().ok()?;
    if !output.status.success() {
        return None;
    }
    let devices: Vec<serde_json::Value> = serde_json::from_slice(&output.stdout).ok()?;
    devices.first()?.get("vram_mb")?.as_u64().map(|v| v as u32)
}

fn mig_available() -> bool {
    if !gpu_available() {
        return false;
    }
    // Check for MIG instances in the state file
    let output = Command::new("vtessera-gpu").arg("list").output().ok();
    match output {
        Some(o) if o.status.success() => {
            let devices: Vec<serde_json::Value> =
                serde_json::from_slice(&o.stdout).unwrap_or_default();
            devices.iter().any(|d| {
                d.get("mig_instances")
                    .and_then(|i| i.as_array())
                    .map(|arr| !arr.is_empty())
                    .unwrap_or(false)
            })
        }
        _ => false,
    }
}

fn first_mig_instance() -> Option<(String, String, String, u32)> {
    // Returns (parent_model, profile, pci_address, vram_mb)
    let output = Command::new("vtessera-gpu").arg("list").output().ok()?;
    if !output.status.success() {
        return None;
    }
    let devices: Vec<serde_json::Value> = serde_json::from_slice(&output.stdout).ok()?;
    for device in &devices {
        if let Some(instances) = device.get("mig_instances").and_then(|i| i.as_array()) {
            if let Some(inst) = instances.first() {
                let parent_model = device.get("model")?.as_str()?.to_string();
                let profile = inst.get("profile")?.as_str()?.to_string();
                let pci_address = inst.get("pci_address")?.as_str()?.to_string();
                let vram_mb = inst.get("vram_mb")?.as_u64()? as u32;
                return Some((parent_model, profile, pci_address, vram_mb));
            }
        }
    }
    None
}

#[test]
fn gpu_true_exits_completed() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: gpu_true_exits_completed (no GPU available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let model = first_gpu_model().expect("GPU model");
    let vram = first_gpu_vram().expect("GPU VRAM");
    let spec = gpu_spec("gpu-true", vec!["true".into()], 10, &model, vram);
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::Completed),
        "expected Completed, got {:?}",
        m.exit_status
    );
    assert!(
        m.gpu_seconds > 0.0,
        "gpu_seconds should be > 0, got {}",
        m.gpu_seconds
    );
    assert_eq!(
        m.vram_gb_hours, 0.0,
        "vram_gb_hours should be 0.0 (deferred to §1d)"
    );
    assert_eq!(m.backend, Backend::CloudHypervisor);
}

#[test]
fn gpu_mismatched_driver_fails() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: gpu_mismatched_driver_fails (no GPU available)");
        return;
    }
    let config = test_config();
    let model = first_gpu_model().expect("GPU model");
    let vram = first_gpu_vram().expect("GPU VRAM");
    // Request wrong vendor
    let wrong_model = if model.starts_with("MI") {
        "H100-80GB"
    } else {
        "MI300X-192GB"
    };
    let spec = gpu_spec("gpu-mismatch", vec!["true".into()], 10, wrong_model, vram);
    let result = CloudHypervisorExecutor { config }.run(&spec);
    assert!(
        matches!(result, Err(vtessera_executor::ExecutorError::Admission(_))),
        "expected Admission error for mismatched vendor, got {:?}",
        result
    );
}

#[test]
fn gpu_vram_too_small() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: gpu_vram_too_small (no GPU available)");
        return;
    }
    let config = test_config();
    let model = first_gpu_model().expect("GPU model");
    // Request more VRAM than available
    let spec = gpu_spec("gpu-vram", vec!["true".into()], 10, &model, 999999);
    let result = CloudHypervisorExecutor { config }.run(&spec);
    assert!(
        matches!(result, Err(vtessera_executor::ExecutorError::Admission(_))),
        "expected Admission error for insufficient VRAM, got {:?}",
        result
    );
}

#[test]
fn gpu_metering_populated() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: gpu_metering_populated (no GPU available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let model = first_gpu_model().expect("GPU model");
    let vram = first_gpu_vram().expect("GPU VRAM");
    let spec = gpu_spec("gpu-meter", vec!["true".into()], 10, &model, vram);
    let m = executor.run(&spec).expect("run should succeed");
    assert!(m.gpu_seconds > 0.0, "gpu_seconds should be > 0");
    assert_eq!(m.vram_gb_hours, 0.0, "vram_gb_hours deferred to §1d");
    assert!(m.cpu_seconds >= 0.0, "cpu_seconds should be >= 0");
}

#[test]
fn mig_true_exits_completed() {
    if !mig_available() {
        eprintln!("TEST SKIPPED: mig_true_exits_completed (no MIG instance available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let (parent_model, profile, _pci, vram) = first_mig_instance().expect("MIG instance");
    let spec = JobSpec {
        job_id: "mig-true".into(),
        image: "n/a".into(),
        command: vec!["true".into()],
        env: vec![("VT_TEST".into(), "1".into())],
        devices: DeviceRequirements {
            class: DeviceClass::NvidiaMig {
                parent_model,
                profile,
            },
            vcpus: 1,
            mem_kb: 256 * 1024,
            min_vram_mb: vram,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs: 10,
    };
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::Completed),
        "expected Completed, got {:?}",
        m.exit_status
    );
    assert!(
        m.gpu_seconds > 0.0,
        "gpu_seconds should be > 0 for MIG jobs, got {}",
        m.gpu_seconds
    );
    assert_eq!(m.backend, Backend::CloudHypervisor);
}

#[test]
fn mig_rejects_wrong_profile() {
    if !mig_available() {
        eprintln!("TEST SKIPPED: mig_rejects_wrong_profile (no MIG instance available)");
        return;
    }
    let config = test_config();
    let (parent_model, _profile, _pci, vram) = first_mig_instance().expect("MIG instance");
    // Request a profile that doesn't exist
    let spec = JobSpec {
        job_id: "mig-wrong".into(),
        image: "n/a".into(),
        command: vec!["true".into()],
        env: vec![],
        devices: DeviceRequirements {
            class: DeviceClass::NvidiaMig {
                parent_model,
                profile: "7g.80gb".into(), // Unlikely to be available
            },
            vcpus: 1,
            mem_kb: 256 * 1024,
            min_vram_mb: vram,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs: 10,
    };
    let result = CloudHypervisorExecutor { config }.run(&spec);
    assert!(
        matches!(result, Err(vtessera_executor::ExecutorError::Admission(_))),
        "expected Admission error for wrong MIG profile, got {:?}",
        result
    );
}

// --- vGPU (mediated device) tests ---

/// Helper to detect if any mediated device instances are available.
fn first_mdev_instance() -> Option<(String, String, String, u32)> {
    // Read gpus.json state file for mdev_instances
    let state_path = "/var/lib/vtessera/gpus.json";
    let content = fs::read_to_string(state_path).ok()?;
    let devices: Vec<serde_json::Value> = serde_json::from_str(&content).ok()?;
    for dev in &devices {
        let parent_model = dev.get("model")?.as_str()?.to_string();
        let instances = dev.get("mdev_instances")?.as_array()?;
        for inst in instances {
            let vgpu_type = inst.get("vgpu_type")?.as_str()?.to_string();
            let pci = inst.get("pci_address")?.as_str()?.to_string();
            let vram = inst.get("vram_mb")?.as_u64()? as u32;
            if !pci.is_empty() && !pci.starts_with("mdev-") {
                return Some((parent_model, vgpu_type, pci, vram));
            }
        }
    }
    None
}

#[test]
fn vgpu_admission_requires_vfio() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: vgpu_admission_requires_vfio (no GPU)");
        return;
    }
    let config = CloudHypervisorConfig::default(); // No vfio_devices
    let spec = JobSpec {
        job_id: "vgpu-novfio".into(),
        image: "n/a".into(),
        command: vec!["true".into()],
        env: vec![],
        devices: DeviceRequirements {
            class: DeviceClass::NvidiaVgpu {
                parent_model: "A100".into(),
                profile: "A100-80GB-5C".into(),
            },
            vcpus: 1,
            mem_kb: 256 * 1024,
            min_vram_mb: 16000,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs: 10,
    };
    let result = CloudHypervisorExecutor { config }.run(&spec);
    assert!(
        matches!(result, Err(vtessera_executor::ExecutorError::Admission(_))),
        "expected Admission error for vGPU without vfio_devices, got {:?}",
        result
    );
}

#[test]
fn vgpu_rejects_wrong_type() {
    if !gpu_available() {
        eprintln!("TEST SKIPPED: vgpu_rejects_wrong_type (no GPU)");
        return;
    }
    if first_mdev_instance().is_none() {
        eprintln!("TEST SKIPPED: vgpu_rejects_wrong_type (no mdev instance)");
        return;
    }
    let config = test_config();
    let (parent_model, _vgpu_type, _pci, vram) = first_mdev_instance().expect("mdev instance");
    // Request a type that doesn't exist
    let spec = JobSpec {
        job_id: "vgpu-wrong".into(),
        image: "n/a".into(),
        command: vec!["true".into()],
        env: vec![],
        devices: DeviceRequirements {
            class: DeviceClass::NvidiaVgpu {
                parent_model,
                profile: "nonexistent-type".into(),
            },
            vcpus: 1,
            mem_kb: 256 * 1024,
            min_vram_mb: vram,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs: 10,
    };
    let result = CloudHypervisorExecutor { config }.run(&spec);
    assert!(
        matches!(result, Err(vtessera_executor::ExecutorError::Admission(_))),
        "expected Admission error for wrong vGPU type, got {:?}",
        result
    );
}
