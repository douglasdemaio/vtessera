//! End-to-end tests for the `vtessera-settle` binary via its spool dirs.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use ed25519_dalek::SigningKey;
use vtessera_executor::{Backend, DeviceClass, ExitStatus, JobMetering};
use vtessera_settlement::{
    derive_node_id, sign_job_receipt, JobContract, JobReceipt, SettlementRecord,
    JOB_RECEIPT_SCHEMA_VER,
};

const BIN: &str = env!("CARGO_BIN_EXE_vtessera-settle");

struct TestDir {
    root: PathBuf,
}

impl TestDir {
    fn new(name: &str) -> TestDir {
        let root =
            std::env::temp_dir().join(format!("vtessera_settle_bin_{name}_{}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        fs::create_dir_all(&root).unwrap();
        TestDir { root }
    }

    fn contract_dir(&self) -> PathBuf {
        self.root.join("contracts")
    }
    fn receipts_dir(&self) -> PathBuf {
        self.root.join("job-receipts")
    }
    fn settlements_dir(&self) -> PathBuf {
        self.root.join("settlements")
    }
}

impl Drop for TestDir {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

fn run_once(state_dir: &Path) -> std::process::Output {
    Command::new(BIN)
        .args(["--state-dir", state_dir.to_str().unwrap(), "--once"])
        .output()
        .expect("vtessera-settle should run")
}

fn key_for(byte: u8) -> SigningKey {
    SigningKey::from_bytes(&[byte; 32])
}

fn metering(job_id: &str, cpu_seconds: f64) -> JobMetering {
    JobMetering {
        job_id: job_id.into(),
        backend: Backend::LocalCpu,
        device: DeviceClass::Cpu,
        cpu_seconds,
        peak_mem_kb: 64 * 1024,
        gpu_seconds: 0.0,
        vram_gb_hours: 0.0,
        exit_status: ExitStatus::Completed,
        elapsed_secs: cpu_seconds as u64,
    }
}

fn write_contract(dir: &TestDir, job_id: &str, node_id: &str, agreed: u64) {
    fs::create_dir_all(dir.contract_dir()).unwrap();
    let contract = JobContract {
        job_id: job_id.into(),
        node_id: node_id.into(),
        device_class: DeviceClass::Cpu,
        agreed_device_seconds: agreed,
        milestones: vec![],
    };
    let json = serde_json::to_vec_pretty(&contract).unwrap();
    fs::write(dir.contract_dir().join(format!("{job_id}.json")), json).unwrap();
}

fn write_signed_receipt(dir: &TestDir, job_id: &str, key: &SigningKey, cpu_seconds: f64) {
    fs::create_dir_all(dir.receipts_dir()).unwrap();
    let receipt = JobReceipt {
        schema_ver: JOB_RECEIPT_SCHEMA_VER,
        node_id: derive_node_id(&key.verifying_key().to_bytes()),
        payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
        metering: metering(job_id, cpu_seconds),
    };
    let signed = sign_job_receipt(&receipt, key);
    let json = serde_json::to_vec_pretty(&signed).unwrap();
    fs::write(dir.receipts_dir().join(format!("{job_id}.json")), json).unwrap();
}

#[test]
fn settle_writes_record_for_matching_receipt() {
    let dir = TestDir::new("ok");
    let key = key_for(0x31);
    let node_id = derive_node_id(&key.verifying_key().to_bytes());
    write_contract(&dir, "job-ok", &node_id, 10);
    write_signed_receipt(&dir, "job-ok", &key, 5.0);

    let out = run_once(&dir.root);
    let stdout = String::from_utf8_lossy(&out.stdout);
    assert!(
        out.status.success(),
        "settle exited nonzero: {stdout} {}",
        String::from_utf8_lossy(&out.stderr)
    );
    assert!(
        stdout.contains("settled job-ok"),
        "unexpected stdout: {stdout}"
    );

    let record_path = dir.settlements_dir().join("job-ok.json");
    assert!(record_path.exists(), "settlement record not written");
    let record: SettlementRecord =
        serde_json::from_slice(&fs::read(&record_path).unwrap()).unwrap();
    assert!((record.completion_fraction - 0.5).abs() < 1e-9);
    assert_eq!(record.agreed_device_seconds, 10);
    assert_eq!(record.receipt_count, 1);
}

#[test]
fn settle_without_receipt_stays_pending_and_exits_zero() {
    let dir = TestDir::new("pending");
    let key = key_for(0x32);
    let node_id = derive_node_id(&key.verifying_key().to_bytes());
    write_contract(&dir, "job-pending", &node_id, 10);

    let out = run_once(&dir.root);
    assert!(out.status.success());
    assert!(String::from_utf8_lossy(&out.stdout).contains("pending job-pending"));
    assert!(!dir.settlements_dir().join("job-pending.json").exists());
}

#[test]
fn settle_rejects_tampered_receipt_and_exits_nonzero() {
    let dir = TestDir::new("tamper");
    let key = key_for(0x33);
    let node_id = derive_node_id(&key.verifying_key().to_bytes());
    write_contract(&dir, "job-tamper", &node_id, 10);
    write_signed_receipt(&dir, "job-tamper", &key, 5.0);

    // Tamper with the stored receipt after signing.
    let path = dir.receipts_dir().join("job-tamper.json");
    let mut signed: vtessera_settlement::SignedJobReceipt =
        serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    signed.receipt.metering.cpu_seconds += 100.0;
    fs::write(&path, serde_json::to_vec_pretty(&signed).unwrap()).unwrap();

    let out = run_once(&dir.root);
    assert!(!out.status.success(), "tampered receipt must exit nonzero");
    assert!(String::from_utf8_lossy(&out.stderr).contains("REJECTED job-tamper"));
    assert!(!dir.settlements_dir().join("job-tamper.json").exists());
}

#[test]
fn settle_is_idempotent_across_runs() {
    let dir = TestDir::new("idem");
    let key = key_for(0x34);
    let node_id = derive_node_id(&key.verifying_key().to_bytes());
    write_contract(&dir, "job-idem", &node_id, 10);
    write_signed_receipt(&dir, "job-idem", &key, 2.0);

    assert!(run_once(&dir.root).status.success());
    let record_path = dir.settlements_dir().join("job-idem.json");
    let first = fs::read(&record_path).unwrap();

    // Second run must skip the already-settled job and stay green.
    let out = run_once(&dir.root);
    assert!(out.status.success());
    let second = fs::read(&record_path).unwrap();
    assert_eq!(
        first, second,
        "settlement record must not change on re-sweep"
    );
}
