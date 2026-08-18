//! Integration tests for the Cloud Hypervisor CPU executor.
//!
//! These tests boot a real VM and need:
//! - `/dev/kvm` present and readable
//! - `cloud-hypervisor` at `/usr/bin/cloud-hypervisor` (or CH_BINARY override)
//! - An initramfs built by `scripts/build-initramfs.sh` (or CH_INITRAMFS override)
//! - A bootable kernel (or CH_KERNEL override)
//!
//! Skip conditions:
//! - `/dev/kvm` absent → skip (CI runners without KVM)
//! - `VTESSERA_CH_INTEGRATION` not set to `1` → skip (opt-in for local runs)
//!
//! Run: VTESSERA_CH_INTEGRATION=1 cargo test -p vtessera-executor \
//!      --features cloud-hypervisor --test ch_cpu_integration -- --nocapture

use std::env;
use std::fs;
use std::path::{Path, PathBuf};

use vtessera_executor::cloud_hypervisor::{CloudHypervisorConfig, CloudHypervisorExecutor};
use vtessera_executor::{
    DeviceClass, DeviceRequirements, Executor, ExitStatus, JobSpec, NetworkPolicy,
};

fn ch_available() -> bool {
    if env::var("VTESSERA_CH_INTEGRATION").as_deref() != Ok("1") {
        return false;
    }
    if !Path::new("/dev/kvm").exists() {
        eprintln!("skipped: /dev/kvm not present");
        return false;
    }
    let bin = env::var("CH_BINARY").unwrap_or_else(|_| "/usr/bin/cloud-hypervisor".into());
    if !Path::new(&bin).exists() {
        eprintln!("skipped: cloud-hypervisor not found at {bin}");
        return false;
    }
    let vfsd = env::var("VIRTIOFSD_BINARY").unwrap_or_else(|_| "/usr/libexec/virtiofsd".into());
    if !Path::new(&vfsd).exists() {
        eprintln!("skipped: virtiofsd not found at {vfsd}");
        return false;
    }
    // Require an initramfs (either default or override).
    let initramfs =
        env::var("CH_INITRAMFS").unwrap_or_else(|_| "/var/lib/vtessera/initramfs.cpio.gz".into());
    if !Path::new(&initramfs).exists() {
        eprintln!("skipped: initramfs not found at {initramfs} (run scripts/build-initramfs.sh)");
        return false;
    }
    true
}

fn test_config() -> CloudHypervisorConfig {
    let bin = env::var("CH_BINARY").unwrap_or_else(|_| "/usr/bin/cloud-hypervisor".into());
    let kernel = env::var("CH_KERNEL").unwrap_or_else(|_| "/boot/vmlinuz".into());
    let initramfs =
        env::var("CH_INITRAMFS").unwrap_or_else(|_| "/var/lib/vtessera/initramfs.cpio.gz".into());
    let vfsd = env::var("VIRTIOFSD_BINARY").unwrap_or_else(|_| "/usr/libexec/virtiofsd".into());
    let workdir = env::temp_dir().join(format!("vt-ch-test-{}", std::process::id()));
    fs::create_dir_all(&workdir).expect("create workdir");
    CloudHypervisorConfig {
        ch_binary: PathBuf::from(bin),
        kernel: PathBuf::from(kernel),
        initramfs: PathBuf::from(initramfs),
        workdir,
        cmdline: "console=ttyS0".into(),
        virtiofsd_binary: PathBuf::from(vfsd),
        extra_args: vec![],
        keep_jobs: false,
        vfio_devices: vec![],
        gpu_helper: PathBuf::from("/usr/bin/vtessera-gpu"),
    }
}

fn ch_spec(job_id: &str, command: Vec<String>, max_duration_secs: u64) -> JobSpec {
    JobSpec {
        job_id: job_id.into(),
        image: "n/a".into(),
        command,
        env: vec![("VT_TEST".into(), "1".into())],
        devices: DeviceRequirements {
            class: DeviceClass::Cpu,
            vcpus: 1,
            mem_kb: 128 * 1024,
            min_vram_mb: 0,
            driver_hint: None,
        },
        network: NetworkPolicy::None,
        max_duration_secs,
    }
}

#[test]
fn ch_true_returns_completed() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_true_returns_completed (KVM not available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let spec = ch_spec("ch-true", vec!["true".into()], 10);
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::Completed),
        "expected Completed, got {:?}",
        m.exit_status
    );
    assert_eq!(m.backend, vtessera_executor::Backend::CloudHypervisor);
}

#[test]
fn ch_exit_3_returns_failed() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_exit_3_returns_failed (KVM not available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let spec = ch_spec(
        "ch-exit3",
        vec!["sh".into(), "-c".into(), "exit 3".into()],
        10,
    );
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::Failed { code: 3 }),
        "expected Failed(3), got {:?}",
        m.exit_status
    );
}

#[test]
fn ch_sleep_60_with_short_cap_times_out() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_sleep_60_with_short_cap_times_out (KVM not available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let spec = ch_spec("ch-sleep60", vec!["sleep".into(), "60".into()], 2);
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::TimedOut),
        "expected TimedOut, got {:?}",
        m.exit_status
    );
    // elapsed_secs should be close to the 2s cap (give 1s slack for boot).
    assert!(
        m.elapsed_secs <= 5,
        "elapsed_secs {} is too high for a 2s cap",
        m.elapsed_secs
    );
}

#[test]
fn ch_metering_sane_on_true() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_metering_sane_on_true (KVM not available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let spec = ch_spec("ch-meter-true", vec!["true".into()], 10);
    let m = executor.run(&spec).expect("run should succeed");
    // elapsed_secs may be 0 for a fast boot — just assert metering parsed.
    assert_eq!(m.device, DeviceClass::Cpu);
}

#[test]
fn ch_env_visible_inside_guest() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_env_visible_inside_guest (KVM not available)");
        return;
    }
    let config = test_config();
    let executor = CloudHypervisorExecutor { config };
    let mut spec = ch_spec(
        "ch-env",
        vec!["sh".into(), "-c".into(), "[ \"$VT_TEST\" = 1 ]".into()],
        10,
    );
    spec.env = vec![("VT_TEST".into(), "1".into())];
    let m = executor.run(&spec).expect("run should succeed");
    assert!(
        matches!(m.exit_status, ExitStatus::Completed),
        "env check should pass (exit 0), got {:?}",
        m.exit_status
    );
}

#[test]
fn ch_workdir_cleaned_after_run() {
    if !ch_available() {
        eprintln!("TEST SKIPPED: ch_workdir_cleaned_after_run (KVM not available)");
        return;
    }
    let config = test_config();
    let workdir = config.workdir.clone();
    let executor = CloudHypervisorExecutor { config };
    let spec = ch_spec("ch-cleanup", vec!["true".into()], 10);
    let _ = executor.run(&spec);
    assert!(
        !workdir.join("ch-cleanup").exists(),
        "workdir should be cleaned up after run"
    );
}
