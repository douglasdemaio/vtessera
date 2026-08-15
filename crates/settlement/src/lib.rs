//! Vtessera settlement — Module 3 (ROADMAP.md §3).
//!
//! Settlement turns signed receipts (from `vtesserad` plus per-job
//! metering from the executor) into two trustworthy outputs:
//!
//! 1. **Amounts** the escrow program can split against, denominated in
//!    the stablecoin the buyer paid in.
//! 2. The **completion fraction** `f ∈ [0, 1]` — how much of the
//!    contracted work was actually delivered. `f` is what makes
//!    pro-rata refund possible: at `f = 0.5` the buyer gets half their
//!    money back, at `f = 1.0` the seller earned it all.
//!
//! This crate is intentionally **non-TEE first** per the roadmap.
//! Adding SEV-SNP / TDX confidential-VM attestation is a follow-up;
//! the shape of [`verify_signed_receipt`] doesn't change, only the
//! deployment story around the binary that calls it.
//!
//! The receipt schema this crate verifies is documented in
//! `BUILD.md` §4 and implemented by `crates/vtesserad/src/receipt.rs`.
//! This crate **does not** depend on the daemon binary — it implements
//! the documented spec independently so it can deploy without the v0
//! binary or its CLI surface.

#![forbid(unsafe_code)]

use std::fs;
use std::io;
use std::path::Path;

use ed25519_dalek::{Signature, Signer, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use vtessera_executor::{Backend, DeviceClass, ExitStatus, JobMetering};

/// Re-export so callers of [`load_node_key`] / [`sign_job_receipt`] can
/// name the key type without depending on ed25519-dalek directly.
pub use ed25519_dalek::SigningKey;

// ---------- Receipt spec (mirror of crates/vtesserad/src/receipt.rs) -------

/// Receipt schema version. Increment when [`canonical_bytes`] changes
/// (must match `vtesserad`'s `schema_ver`).
pub const RECEIPT_SCHEMA_VER: u16 = 1;

/// Per-window totals as written by `vtesserad`.
#[derive(Debug, Clone, PartialEq)]
pub struct Totals {
    pub cpu_pct_avg: f64,
    pub mem_used_kb_avg: u64,
    pub disk_free_kb_avg: u64,
    pub sample_count: u32,
}

/// Plain receipt (no signature). Matches the wire format of
/// `vtesserad::receipt::Receipt`.
#[derive(Debug, Clone, PartialEq)]
pub struct Receipt {
    pub schema_ver: u16,
    pub node_id: String,
    pub payout_id: String,
    pub window_start: u64,
    pub window_end: u64,
    pub samples_digest: [u8; 32],
    pub totals: Totals,
}

/// Signed receipt: receipt + pubkey + Ed25519 signature.
#[derive(Debug, Clone)]
pub struct SignedReceipt {
    pub receipt: Receipt,
    pub pubkey: [u8; 32],
    pub sig: [u8; 64],
}

/// Canonical signing bytes (must match `vtesserad::receipt::canonical_bytes`).
///
/// If this drifts from the daemon's implementation, every receipt
/// settlement reads will fail signature verification — that's the
/// failure mode we want, not silent acceptance.
pub fn canonical_bytes(r: &Receipt) -> Vec<u8> {
    let mut buf = Vec::with_capacity(128);
    buf.extend_from_slice(&r.schema_ver.to_le_bytes());

    let nid = r.node_id.as_bytes();
    buf.extend_from_slice(&(nid.len() as u16).to_le_bytes());
    buf.extend_from_slice(nid);

    let pid = r.payout_id.as_bytes();
    buf.extend_from_slice(&(pid.len() as u16).to_le_bytes());
    buf.extend_from_slice(pid);

    buf.extend_from_slice(&r.window_start.to_le_bytes());
    buf.extend_from_slice(&r.window_end.to_le_bytes());
    buf.extend_from_slice(&r.samples_digest);
    buf.extend_from_slice(&r.totals.cpu_pct_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.mem_used_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.disk_free_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.sample_count.to_le_bytes());
    buf
}

/// Derive `node_id` from an Ed25519 public key — `SHA-256(pubkey)[..16]`,
/// hex-encoded. Must match `vtesserad::receipt::derive_node_id`.
pub fn derive_node_id(pubkey: &[u8; 32]) -> String {
    let mut h = Sha256::new();
    h.update(pubkey);
    let digest = h.finalize();
    hex::encode(&digest[..16])
}

#[derive(Debug)]
pub enum VerifyError {
    UnsupportedSchema(u16),
    BadPubkey,
    NodeIdMismatch,
    SignatureMismatch,
}

impl std::fmt::Display for VerifyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            VerifyError::UnsupportedSchema(v) => write!(f, "receipt schema_ver {v} not supported"),
            VerifyError::BadPubkey => write!(f, "pubkey is not a valid Ed25519 key"),
            VerifyError::NodeIdMismatch => write!(f, "node_id does not match pubkey"),
            VerifyError::SignatureMismatch => write!(f, "signature does not verify"),
        }
    }
}

impl std::error::Error for VerifyError {}

/// Verify a signed receipt end-to-end:
///
/// 1. schema_ver is one we understand,
/// 2. pubkey is a valid Ed25519 key,
/// 3. receipt.node_id matches `derive_node_id(pubkey)` (self-attesting),
/// 4. signature verifies against [`canonical_bytes`].
///
/// Any failure is a hard reject — settlement never credits work against
/// a receipt that doesn't fully verify.
pub fn verify_signed_receipt(sr: &SignedReceipt) -> Result<(), VerifyError> {
    if sr.receipt.schema_ver != RECEIPT_SCHEMA_VER {
        return Err(VerifyError::UnsupportedSchema(sr.receipt.schema_ver));
    }
    let vk = VerifyingKey::from_bytes(&sr.pubkey).map_err(|_| VerifyError::BadPubkey)?;
    if derive_node_id(&sr.pubkey) != sr.receipt.node_id {
        return Err(VerifyError::NodeIdMismatch);
    }
    let sig = Signature::from_bytes(&sr.sig);
    vk.verify(&canonical_bytes(&sr.receipt), &sig)
        .map_err(|_| VerifyError::SignatureMismatch)
}

// ---------- Job receipt (schema_ver 2) ------------------------------------

/// Job-receipt schema version. Distinct from the window-receipt schema
/// (`RECEIPT_SCHEMA_VER`): a job receipt is written once per executed job
/// and wraps the executor's [`JobMetering`], whereas window receipts are
/// the daemon's periodic usage summaries.
pub const JOB_RECEIPT_SCHEMA_VER: u16 = 2;

/// A per-job metering receipt. Signed by the node that ran the job with its
/// Ed25519 identity key; verified by settlement before any of its metering
/// is credited toward the completion fraction.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct JobReceipt {
    pub schema_ver: u16,
    /// Self-attesting node identity (`derive_node_id(pubkey)`).
    pub node_id: String,
    /// Seller payout wallet (from the signed offer's body).
    pub payout_id: String,
    /// The metering the executor reported for the job.
    pub metering: JobMetering,
}

/// A job receipt paired with its Ed25519 signature and public key.
///
/// Serializes `pubkey`/`sig` as hex strings (matching the offer crate's
/// JSON convention) because serde has no impl for `[u8; 64]`.
#[derive(Debug, Clone, PartialEq)]
pub struct SignedJobReceipt {
    pub receipt: JobReceipt,
    pub pubkey: [u8; 32],
    pub sig: [u8; 64],
}

impl Serialize for SignedJobReceipt {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        use serde::ser::SerializeStruct;
        let mut st = serializer.serialize_struct("SignedJobReceipt", 3)?;
        st.serialize_field("receipt", &self.receipt)?;
        st.serialize_field("pubkey", &hex::encode(self.pubkey))?;
        st.serialize_field("sig", &hex::encode(self.sig))?;
        st.end()
    }
}

impl<'de> Deserialize<'de> for SignedJobReceipt {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        use serde::de;
        #[derive(Deserialize)]
        struct Raw {
            receipt: JobReceipt,
            pubkey: String,
            sig: String,
        }
        let raw = Raw::deserialize(deserializer)?;
        let mut pubkey = [0u8; 32];
        decode_hex_into(&raw.pubkey, &mut pubkey).map_err(de::Error::custom)?;
        let mut sig = [0u8; 64];
        decode_hex_into(&raw.sig, &mut sig).map_err(de::Error::custom)?;
        Ok(SignedJobReceipt {
            receipt: raw.receipt,
            pubkey,
            sig,
        })
    }
}

fn decode_hex_into<const N: usize>(s: &str, out: &mut [u8; N]) -> Result<(), String> {
    let bytes = hex::decode(s).map_err(|e| format!("invalid hex: {e}"))?;
    if bytes.len() != N {
        return Err(format!("expected {N} bytes, got {}", bytes.len()));
    }
    out.copy_from_slice(&bytes);
    Ok(())
}

// Stable tag tables for the three enums inside `JobMetering`. These are the
// wire encoding of the canonical bytes; reordering or removing a value
// invalidates every receipt signed against the old table. Additions must
// append and lock the table with a test.

fn backend_tag(b: &Backend) -> u8 {
    match b {
        Backend::NoopCpu => 0,
        Backend::LocalCpu => 1,
        Backend::KataCloudHypervisor => 2,
        Backend::CloudHypervisor => 3,
        Backend::QemuVfio => 4,
    }
}

/// Device-class tag + length-prefixed string payloads.
fn device_tag(d: &DeviceClass) -> Vec<u8> {
    let mut buf = Vec::new();
    match d {
        DeviceClass::Cpu => buf.push(0),
        DeviceClass::NvidiaGpu { model } => {
            buf.push(1);
            push_str(&mut buf, model);
        }
        DeviceClass::NvidiaMig {
            parent_model,
            profile,
        } => {
            buf.push(2);
            push_str(&mut buf, parent_model);
            push_str(&mut buf, profile);
        }
        DeviceClass::AmdGpu { model } => {
            buf.push(3);
            push_str(&mut buf, model);
        }
    }
    buf
}

fn exit_tag(e: &ExitStatus) -> Vec<u8> {
    let mut buf = Vec::new();
    match e {
        ExitStatus::Completed => buf.push(0),
        ExitStatus::Failed { code } => {
            buf.push(1);
            buf.extend_from_slice(&code.to_le_bytes());
        }
        ExitStatus::TimedOut => buf.push(2),
        ExitStatus::Cancelled => buf.push(3),
    }
    buf
}

fn push_str(buf: &mut Vec<u8>, s: &str) {
    let bytes = s.as_bytes();
    buf.extend_from_slice(&(bytes.len() as u16).to_le_bytes());
    buf.extend_from_slice(bytes);
}

/// Canonical serialization of a job receipt for signing.
///
/// Byte layout (little-endian throughout; see BUILD.md §4 / this file):
///
///   schema_ver                : u16
///   node_id_len               : u16 + node_id bytes
///   payout_id_len             : u16 + payout_id bytes
///   metering.job_id_len       : u16 + job_id bytes
///   metering.backend          : u8  (tag table)
///   metering.device           : u8 kind + payload (tag table)
///   metering.cpu_seconds      : f64
///   metering.peak_mem_kb      : u64
///   metering.gpu_seconds      : f64
///   metering.vram_gb_hours    : f64
///   metering.exit_status      : u8 kind + optional i32 code (tag table)
///   metering.elapsed_secs     : u64
///
/// Any change to this layout (or a tag table) requires bumping
/// `JOB_RECEIPT_SCHEMA_VER`.
pub fn job_receipt_canonical_bytes(r: &JobReceipt) -> Vec<u8> {
    let m = &r.metering;
    let mut buf = Vec::with_capacity(192);
    buf.extend_from_slice(&r.schema_ver.to_le_bytes());
    push_str(&mut buf, &r.node_id);
    push_str(&mut buf, &r.payout_id);
    push_str(&mut buf, &m.job_id);
    buf.push(backend_tag(&m.backend));
    buf.extend_from_slice(&device_tag(&m.device));
    buf.extend_from_slice(&m.cpu_seconds.to_le_bytes());
    buf.extend_from_slice(&m.peak_mem_kb.to_le_bytes());
    buf.extend_from_slice(&m.gpu_seconds.to_le_bytes());
    buf.extend_from_slice(&m.vram_gb_hours.to_le_bytes());
    buf.extend_from_slice(&exit_tag(&m.exit_status));
    buf.extend_from_slice(&m.elapsed_secs.to_le_bytes());
    buf
}

/// Sign a job receipt, producing a [`SignedJobReceipt`].
pub fn sign_job_receipt(receipt: &JobReceipt, key: &SigningKey) -> SignedJobReceipt {
    let canonical = job_receipt_canonical_bytes(receipt);
    let sig: Signature = key.sign(&canonical);
    SignedJobReceipt {
        receipt: receipt.clone(),
        pubkey: key.verifying_key().to_bytes(),
        sig: sig.to_bytes(),
    }
}

/// Verify a signed job receipt end-to-end:
///
/// 1. schema_ver is one we understand,
/// 2. pubkey is a valid Ed25519 key,
/// 3. receipt.node_id matches `derive_node_id(pubkey)` (self-attesting),
/// 4. signature verifies against [`job_receipt_canonical_bytes`].
///
/// Any failure is a hard reject — settlement never credits work against a
/// job receipt that doesn't fully verify.
pub fn verify_signed_job_receipt(sr: &SignedJobReceipt) -> Result<(), VerifyError> {
    if sr.receipt.schema_ver != JOB_RECEIPT_SCHEMA_VER {
        return Err(VerifyError::UnsupportedSchema(sr.receipt.schema_ver));
    }
    let vk = VerifyingKey::from_bytes(&sr.pubkey).map_err(|_| VerifyError::BadPubkey)?;
    if derive_node_id(&sr.pubkey) != sr.receipt.node_id {
        return Err(VerifyError::NodeIdMismatch);
    }
    let sig = Signature::from_bytes(&sr.sig);
    vk.verify(&job_receipt_canonical_bytes(&sr.receipt), &sig)
        .map_err(|_| VerifyError::SignatureMismatch)
}

/// Load the node's Ed25519 identity key from disk (raw 32-byte seed, the
/// same format `vtesserad` writes).
///
/// Refuses to load a key whose mode permits any group or world access
/// (mode & 0o077 != 0) or whose length isn't exactly the secret-key length.
pub fn load_node_key(key_path: &Path) -> io::Result<SigningKey> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = fs::metadata(key_path)?.permissions().mode() & 0o777;
        if mode & 0o077 != 0 {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                format!(
                    "key file {} has mode {:o}; must be 0600 (no group/world access)",
                    key_path.display(),
                    mode
                ),
            ));
        }
    }
    let raw = fs::read(key_path)?;
    if raw.len() != ed25519_dalek::SECRET_KEY_LENGTH {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!(
                "key file has wrong length: expected {}, got {}",
                ed25519_dalek::SECRET_KEY_LENGTH,
                raw.len()
            ),
        ));
    }
    let mut arr = [0u8; ed25519_dalek::SECRET_KEY_LENGTH];
    arr.copy_from_slice(&raw);
    Ok(SigningKey::from_bytes(&arr))
}

// ---------- Job contract + completion fraction ----------------------------

/// What the agent and the seller agreed at Module 2 contract time.
///
/// Settlement compares this against the metering the executor produced
/// to derive `f`. All fields are denominated in **device-seconds** for
/// the agreed device class — the same unit the offer quoted in.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct JobContract {
    /// Identifier shared with the job's signed receipts and metering.
    pub job_id: String,
    /// `node_id` of the seller (Module 2a offer's `node_id`).
    pub node_id: String,
    /// Device class the job agreed to run on. Settlement selects the meter
    /// to aggregate by this (CPU-seconds vs GPU-seconds) and uses it to
    /// reject downgrade attempts.
    pub device_class: DeviceClass,
    /// Device-seconds the buyer agreed to pay for.
    pub agreed_device_seconds: u64,
    /// Optional milestones for streaming partial release (ROADMAP.md §4b).
    /// Each value is a cumulative fraction in `[0, 1]`; entries must be
    /// strictly increasing. Empty means one final settlement.
    pub milestones: Vec<f64>,
}

/// The meter settlement credits for a job, selected by the agreed device
/// class: CPU jobs price in CPU-seconds, GPU jobs in GPU-seconds.
pub fn device_seconds_for(metering: &JobMetering, class: &DeviceClass) -> f64 {
    match class {
        DeviceClass::Cpu => metering.cpu_seconds,
        _ => metering.gpu_seconds,
    }
}

/// Aggregate of what an executor reported for a job. In production this
/// is the sum of `JobMetering` records from the executor crate, scoped
/// to one `job_id`.
#[derive(Debug, Clone, PartialEq)]
pub struct JobUsage {
    pub job_id: String,
    pub node_id: String,
    /// Total device-seconds the executor metered for this job. For GPU
    /// jobs this is `gpu_seconds`; for CPU jobs, `cpu_seconds`. The
    /// caller picks the right number per the device class agreed in the
    /// contract.
    pub device_seconds: f64,
}

/// Result of settling one job. The escrow program (Module 4) splits
/// the held stablecoin by [`Settlement::completion_fraction`].
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Settlement {
    pub job_id: String,
    /// `f ∈ [0, 1]`, clamped — extra delivered work above the agreed
    /// ceiling does not earn more than 100% of the contract.
    pub completion_fraction: f64,
    /// Which milestone tier the work landed on, when milestones are
    /// defined. `None` means a single final split.
    pub milestone_reached: Option<usize>,
}

#[derive(Debug)]
pub enum SettleError {
    /// `JobUsage.job_id` doesn't match `JobContract.job_id`.
    JobIdMismatch,
    /// `JobUsage.node_id` doesn't match `JobContract.node_id` — the
    /// usage was reported by a node the buyer didn't contract with.
    NodeMismatch,
    /// Contract's `agreed_device_seconds` is zero — would divide by
    /// zero. Either the contract is malformed or it should have been a
    /// free job (no settlement needed).
    ZeroAgreement,
    /// Milestones violated the strict-increasing-in-[0,1] invariant.
    BadMilestones,
}

impl std::fmt::Display for SettleError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            SettleError::JobIdMismatch => write!(f, "usage job_id does not match contract"),
            SettleError::NodeMismatch => write!(f, "usage node_id does not match contract"),
            SettleError::ZeroAgreement => write!(f, "contract agreed_device_seconds is zero"),
            SettleError::BadMilestones => {
                write!(f, "milestones must be strictly increasing in [0,1]")
            }
        }
    }
}

impl std::error::Error for SettleError {}

/// Compute the completion fraction for one job. The escrow program
/// uses this number to split the buyer's stablecoin between
/// `f × price` (paid to the seller in the same stablecoin) and
/// `(1 − f) × price` (refunded to the buyer).
pub fn settle(contract: &JobContract, usage: &JobUsage) -> Result<Settlement, SettleError> {
    if contract.job_id != usage.job_id {
        return Err(SettleError::JobIdMismatch);
    }
    if contract.node_id != usage.node_id {
        return Err(SettleError::NodeMismatch);
    }
    if contract.agreed_device_seconds == 0 {
        return Err(SettleError::ZeroAgreement);
    }
    validate_milestones(&contract.milestones)?;

    // f starts as raw ratio.
    let raw = usage.device_seconds / contract.agreed_device_seconds as f64;
    // Clamp to [0, 1]. Over-delivery does not increase the payout.
    let f = raw.clamp(0.0, 1.0);

    let milestone_reached = milestone_for(&contract.milestones, f);

    Ok(Settlement {
        job_id: contract.job_id.clone(),
        completion_fraction: f,
        milestone_reached,
    })
}

fn validate_milestones(ms: &[f64]) -> Result<(), SettleError> {
    let mut prev = 0.0_f64;
    for &m in ms {
        if !(m > prev && m <= 1.0 + f64::EPSILON) {
            return Err(SettleError::BadMilestones);
        }
        prev = m;
    }
    Ok(())
}

fn milestone_for(ms: &[f64], f: f64) -> Option<usize> {
    if ms.is_empty() {
        return None;
    }
    let mut hit: Option<usize> = None;
    for (i, &m) in ms.iter().enumerate() {
        if f >= m {
            hit = Some(i);
        } else {
            break;
        }
    }
    hit
}

// ---------- Spool sweep (drives the vtessera-settle binary) ---------------

/// The record persisted to `settlements/<job_id>.json` by `vtessera-settle`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SettlementRecord {
    pub job_id: String,
    pub node_id: String,
    pub device_class: DeviceClass,
    /// Total device-seconds credited, summed across the job's signed
    /// receipts using the meter selected by `device_class`.
    pub device_seconds: f64,
    pub agreed_device_seconds: u64,
    /// Number of signed receipts aggregated into `device_seconds`.
    pub receipt_count: u32,
    /// `f ∈ [0, 1]` — how much of the contracted work was delivered.
    pub completion_fraction: f64,
    pub milestone_reached: Option<usize>,
}

/// Outcome of one sweep across the spool.
#[derive(Debug, Default)]
pub struct SweepReport {
    /// Job ids whose settlement records were written this sweep.
    pub settled: Vec<String>,
    /// `(job_id, why)` — not ready yet; retried on the next sweep.
    pub pending: Vec<(String, String)>,
    /// `(job_id, why)` — permanently unrecoverable; operator intervention
    /// required. No partial credit is ever written.
    pub rejected: Vec<(String, String)>,
}

/// Per-job outcome of one settlement attempt.
enum SettleState {
    /// Receipt not available yet (transient — retry next sweep).
    Pending(String),
    /// Verification or contract failure (permanent).
    Rejected(String),
}

/// Perform one sweep of `--state-dir`:
///
/// 1. scan `contracts/<job_id>.json`,
/// 2. skip jobs that already have `settlements/<job_id>.json` (idempotent),
/// 3. for each remaining job, verify its signed job receipt and credit the
///    agreed meter, or leave it pending/rejected per [`SettleState`].
pub fn sweep(state_dir: &Path) -> io::Result<SweepReport> {
    let mut report = SweepReport::default();
    let contracts_dir = state_dir.join("contracts");
    let contracts = match read_contracts(&contracts_dir) {
        Ok(c) => c,
        // No contracts dir yet — nothing to settle. Not an error.
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(report),
        Err(e) => return Err(e),
    };

    let receipts_dir = state_dir.join("job-receipts");
    let settlements_dir = state_dir.join("settlements");

    for (job_id, contract) in contracts {
        let record_path = settlements_dir.join(format!("{job_id}.json"));
        if record_path.exists() {
            continue;
        }
        match settle_one(&receipts_dir, &settlements_dir, &contract, &record_path) {
            Ok(()) => report.settled.push(job_id),
            Err(SettleState::Pending(why)) => report.pending.push((job_id, why)),
            Err(SettleState::Rejected(why)) => report.rejected.push((job_id, why)),
        }
    }
    Ok(report)
}

/// List `contracts/<name>.json` as `(name, JobContract)`.
fn read_contracts(dir: &Path) -> io::Result<Vec<(String, JobContract)>> {
    let mut out = Vec::new();
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        if path.extension().and_then(|e| e.to_str()) != Some("json") {
            continue;
        }
        let raw = fs::read(&path)?;
        let contract: JobContract = serde_json::from_slice(&raw).map_err(io_err)?;
        let name = path
            .file_stem()
            .and_then(|s| s.to_str())
            .unwrap_or_default()
            .to_string();
        out.push((name, contract));
    }
    out.sort_by(|a, b| a.0.cmp(&b.0));
    Ok(out)
}

fn io_err<E: std::fmt::Display>(e: E) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, e.to_string())
}

/// Verify + settle one job. Writes its settlement record atomically on
/// success; otherwise returns a [`SettleState`] for the caller to classify.
fn settle_one(
    receipts_dir: &Path,
    settlements_dir: &Path,
    contract: &JobContract,
    record_path: &Path,
) -> Result<(), SettleState> {
    let job_id = &contract.job_id;
    let receipt_path = receipts_dir.join(format!("{job_id}.json"));
    let raw = match fs::read(&receipt_path) {
        Ok(b) => b,
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            return Err(SettleState::Pending(format!(
                "no signed receipt yet for {job_id}"
            )));
        }
        Err(e) => {
            return Err(SettleState::Pending(format!(
                "could not read receipt {receipt_path:?}: {e}"
            )));
        }
    };
    let signed: SignedJobReceipt = match serde_json::from_slice(&raw) {
        Ok(s) => s,
        // Transient: the node may be mid-write. Never a permanent reject.
        Err(e) => {
            return Err(SettleState::Pending(format!(
                "receipt for {job_id} not parseable yet: {e}"
            )));
        }
    };

    if let Err(e) = verify_signed_job_receipt(&signed) {
        return Err(SettleState::Rejected(format!(
            "receipt for {job_id} failed verification: {e} — no settlement credited"
        )));
    }
    if signed.receipt.node_id != contract.node_id {
        return Err(SettleState::Rejected(format!(
            "receipt for {job_id} was signed by {} but the contract was with {}",
            signed.receipt.node_id, contract.node_id
        )));
    }

    // The meter is selected by the agreed device class: a GPU contract
    // settled against CPU-only receipts credits `gpu_seconds` (0), never
    // CPU-seconds — no downgrade credit.
    let device_seconds = device_seconds_for(&signed.receipt.metering, &contract.device_class);
    let usage = JobUsage {
        job_id: contract.job_id.clone(),
        node_id: contract.node_id.clone(),
        device_seconds,
    };
    let settlement = settle(contract, &usage)
        .map_err(|e| SettleState::Rejected(format!("contract for {job_id} is invalid: {e}")))?;

    let record = SettlementRecord {
        job_id: contract.job_id.clone(),
        node_id: contract.node_id.clone(),
        device_class: contract.device_class.clone(),
        device_seconds,
        agreed_device_seconds: contract.agreed_device_seconds,
        receipt_count: 1,
        completion_fraction: settlement.completion_fraction,
        milestone_reached: settlement.milestone_reached,
    };

    fs::create_dir_all(settlements_dir).map_err(|e| {
        SettleState::Rejected(format!(
            "could not create {}: {e}",
            settlements_dir.display()
        ))
    })?;
    write_json_atomic(record_path, &record).map_err(|e| {
        SettleState::Rejected(format!("could not persist settlement for {job_id}: {e}"))
    })?;
    Ok(())
}

/// Write a JSON file via temp-file + rename so a concurrent reader never
/// observes a partially written settlement record.
fn write_json_atomic(path: &Path, value: &impl Serialize) -> io::Result<()> {
    let tmp = path.with_extension("tmp");
    let json = serde_json::to_vec_pretty(value).map_err(io_err)?;
    fs::write(&tmp, json)?;
    fs::rename(&tmp, path)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer, SigningKey};

    fn sample_receipt(node_id: &str) -> Receipt {
        Receipt {
            schema_ver: RECEIPT_SCHEMA_VER,
            node_id: node_id.into(),
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
            window_start: 100,
            window_end: 160,
            samples_digest: [0x55; 32],
            totals: Totals {
                cpu_pct_avg: 12.5,
                mem_used_kb_avg: 4_096_000,
                disk_free_kb_avg: 100_000_000,
                sample_count: 60,
            },
        }
    }

    fn sign(r: &Receipt, key: &SigningKey) -> SignedReceipt {
        let sig = key.sign(&canonical_bytes(r));
        SignedReceipt {
            receipt: r.clone(),
            pubkey: key.verifying_key().to_bytes(),
            sig: sig.to_bytes(),
        }
    }

    fn det_key(byte: u8) -> SigningKey {
        SigningKey::from_bytes(&[byte; 32])
    }

    #[test]
    fn verify_accepts_a_well_formed_receipt() {
        let key = det_key(11);
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let sr = sign(&sample_receipt(&node_id), &key);
        verify_signed_receipt(&sr).expect("well-formed receipt should verify");
    }

    #[test]
    fn verify_rejects_node_id_spoof() {
        let key = det_key(12);
        let mut r = sample_receipt("0000000000000000000000000000000000");
        r.node_id = "0".repeat(32);
        let sr = sign(&r, &key);
        assert!(matches!(
            verify_signed_receipt(&sr),
            Err(VerifyError::NodeIdMismatch)
        ));
    }

    #[test]
    fn verify_rejects_tampered_totals() {
        let key = det_key(13);
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let mut sr = sign(&sample_receipt(&node_id), &key);
        sr.receipt.totals.cpu_pct_avg = 100.0;
        assert!(matches!(
            verify_signed_receipt(&sr),
            Err(VerifyError::SignatureMismatch)
        ));
    }

    #[test]
    fn verify_rejects_unknown_schema() {
        let key = det_key(14);
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let mut r = sample_receipt(&node_id);
        r.schema_ver = 9_999;
        let sr = sign(&r, &key);
        assert!(matches!(
            verify_signed_receipt(&sr),
            Err(VerifyError::UnsupportedSchema(9_999))
        ));
    }

    fn contract(agreed: u64, milestones: Vec<f64>) -> JobContract {
        JobContract {
            job_id: "job-1".into(),
            node_id: "node-aaaa".into(),
            device_class: DeviceClass::Cpu,
            agreed_device_seconds: agreed,
            milestones,
        }
    }

    fn usage(device_seconds: f64) -> JobUsage {
        JobUsage {
            job_id: "job-1".into(),
            node_id: "node-aaaa".into(),
            device_seconds,
        }
    }

    #[test]
    fn completion_fraction_is_zero_when_nothing_delivered() {
        let s = settle(&contract(1000, vec![]), &usage(0.0)).unwrap();
        assert_eq!(s.completion_fraction, 0.0);
        assert!(s.milestone_reached.is_none());
    }

    #[test]
    fn completion_fraction_is_clamped_to_one() {
        let s = settle(&contract(1000, vec![]), &usage(2000.0)).unwrap();
        assert_eq!(s.completion_fraction, 1.0);
    }

    #[test]
    fn completion_fraction_is_proportional_in_between() {
        let s = settle(&contract(1000, vec![]), &usage(500.0)).unwrap();
        assert!((s.completion_fraction - 0.5).abs() < 1e-9);
    }

    #[test]
    fn milestone_reached_is_the_highest_below_or_equal_to_f() {
        let s = settle(&contract(1000, vec![0.25, 0.5, 0.75, 1.0]), &usage(600.0)).unwrap();
        // f = 0.6, milestones at 0.25, 0.5, 0.75, 1.0 → highest hit is index 1 (0.5).
        assert_eq!(s.milestone_reached, Some(1));
    }

    #[test]
    fn settle_rejects_job_id_mismatch() {
        let c = contract(1000, vec![]);
        let mut u = usage(100.0);
        u.job_id = "other".into();
        assert!(matches!(settle(&c, &u), Err(SettleError::JobIdMismatch)));
    }

    #[test]
    fn settle_rejects_node_mismatch() {
        let c = contract(1000, vec![]);
        let mut u = usage(100.0);
        u.node_id = "imposter".into();
        assert!(matches!(settle(&c, &u), Err(SettleError::NodeMismatch)));
    }

    #[test]
    fn settle_rejects_zero_agreement() {
        let c = contract(0, vec![]);
        assert!(matches!(
            settle(&c, &usage(100.0)),
            Err(SettleError::ZeroAgreement)
        ));
    }

    #[test]
    fn settle_rejects_non_monotonic_milestones() {
        let c = contract(1000, vec![0.5, 0.3]);
        assert!(matches!(
            settle(&c, &usage(100.0)),
            Err(SettleError::BadMilestones)
        ));
    }

    #[test]
    fn settle_rejects_milestones_above_one() {
        let c = contract(1000, vec![0.5, 1.5]);
        assert!(matches!(
            settle(&c, &usage(100.0)),
            Err(SettleError::BadMilestones)
        ));
    }

    // ---------- Job receipt tests -----------------------------------------

    fn job_metering(job_id: &str) -> JobMetering {
        JobMetering {
            job_id: job_id.into(),
            backend: Backend::LocalCpu,
            device: DeviceClass::Cpu,
            cpu_seconds: 12.5,
            peak_mem_kb: 64 * 1024,
            gpu_seconds: 0.0,
            vram_gb_hours: 0.0,
            exit_status: ExitStatus::Completed,
            elapsed_secs: 13,
        }
    }

    fn job_receipt(key: &SigningKey, job_id: &str) -> JobReceipt {
        JobReceipt {
            schema_ver: JOB_RECEIPT_SCHEMA_VER,
            node_id: derive_node_id(&key.verifying_key().to_bytes()),
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
            metering: job_metering(job_id),
        }
    }

    #[test]
    fn job_receipt_canonical_bytes_is_deterministic() {
        let key = det_key(21);
        let a = job_receipt_canonical_bytes(&job_receipt(&key, "job-x"));
        let b = job_receipt_canonical_bytes(&job_receipt(&key, "job-x"));
        assert_eq!(a, b);
    }

    #[test]
    fn job_receipt_canonical_bytes_differ_across_fields() {
        let key = det_key(22);
        let a = job_receipt_canonical_bytes(&job_receipt(&key, "job-x"));
        let b = job_receipt_canonical_bytes(&job_receipt(&key, "job-y"));
        assert_ne!(a, b, "job_id must be part of the signed bytes");
    }

    #[test]
    fn backend_tag_table_is_stable() {
        assert_eq!(backend_tag(&Backend::NoopCpu), 0);
        assert_eq!(backend_tag(&Backend::LocalCpu), 1);
        assert_eq!(backend_tag(&Backend::KataCloudHypervisor), 2);
        assert_eq!(backend_tag(&Backend::CloudHypervisor), 3);
        assert_eq!(backend_tag(&Backend::QemuVfio), 4);
    }

    #[test]
    fn device_tag_table_is_stable() {
        assert_eq!(device_tag(&DeviceClass::Cpu), vec![0]);
        assert_eq!(
            device_tag(&DeviceClass::NvidiaGpu {
                model: "H100".into()
            }),
            vec![1, 4, 0, b'H', b'1', b'0', b'0']
        );
    }

    #[test]
    fn exit_tag_table_is_stable() {
        assert_eq!(exit_tag(&ExitStatus::Completed), vec![0]);
        assert_eq!(
            exit_tag(&ExitStatus::Failed { code: -3 }),
            vec![1, 0xFD, 0xFF, 0xFF, 0xFF]
        );
        assert_eq!(exit_tag(&ExitStatus::TimedOut), vec![2]);
        assert_eq!(exit_tag(&ExitStatus::Cancelled), vec![3]);
    }

    #[test]
    fn sign_verify_job_receipt_roundtrip() {
        let key = det_key(23);
        let sr = sign_job_receipt(&job_receipt(&key, "job-roundtrip"), &key);
        verify_signed_job_receipt(&sr).expect("well-formed job receipt should verify");
    }

    #[test]
    fn verify_job_receipt_rejects_tampered_metering() {
        let key = det_key(24);
        let mut sr = sign_job_receipt(&job_receipt(&key, "job-tamper"), &key);
        sr.receipt.metering.cpu_seconds += 100.0;
        assert!(matches!(
            verify_signed_job_receipt(&sr),
            Err(VerifyError::SignatureMismatch)
        ));
    }

    #[test]
    fn verify_job_receipt_rejects_node_id_spoof() {
        let key = det_key(25);
        let mut sr = sign_job_receipt(&job_receipt(&key, "job-spoof"), &key);
        sr.receipt.node_id = "0".repeat(32);
        assert!(matches!(
            verify_signed_job_receipt(&sr),
            Err(VerifyError::NodeIdMismatch)
        ));
    }

    #[test]
    fn verify_job_receipt_rejects_unknown_schema() {
        let key = det_key(26);
        let mut sr = sign_job_receipt(&job_receipt(&key, "job-schema"), &key);
        sr.receipt.schema_ver = 9_999;
        assert!(matches!(
            verify_signed_job_receipt(&sr),
            Err(VerifyError::UnsupportedSchema(9_999))
        ));
    }

    #[test]
    fn signed_job_receipt_roundtrips_through_serde_json() {
        let key = det_key(27);
        let sr = sign_job_receipt(&job_receipt(&key, "job-json"), &key);
        let json = serde_json::to_string(&sr).expect("serialize");
        let back: SignedJobReceipt = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(back, sr);
        verify_signed_job_receipt(&back).expect("round-tripped receipt still verifies");
    }

    #[test]
    fn device_seconds_for_selects_cpu_meter_for_cpu_class() {
        let mut m = job_metering("job-dc");
        m.cpu_seconds = 7.0;
        m.gpu_seconds = 99.0;
        assert_eq!(device_seconds_for(&m, &DeviceClass::Cpu), 7.0);
        assert_eq!(
            device_seconds_for(
                &m,
                &DeviceClass::NvidiaGpu {
                    model: "H100".into()
                }
            ),
            99.0
        );
    }

    #[cfg(unix)]
    #[test]
    fn load_node_key_loads_valid_seed_and_refuses_loose_modes() {
        use std::os::unix::fs::PermissionsExt;

        let dir = std::env::temp_dir().join("vtessera_settle_keys");
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();

        let good = dir.join("good.key");
        fs::write(&good, [7u8; 32]).unwrap();
        fs::set_permissions(&good, fs::Permissions::from_mode(0o600)).unwrap();
        let key = load_node_key(&good).expect("0600 key should load");
        assert_eq!(key.to_bytes(), [7u8; 32]);

        let loose = dir.join("loose.key");
        fs::write(&loose, [7u8; 32]).unwrap();
        fs::set_permissions(&loose, fs::Permissions::from_mode(0o644)).unwrap();
        let err = load_node_key(&loose).expect_err("0644 key must be refused");
        assert_eq!(err.kind(), io::ErrorKind::PermissionDenied);

        let short = dir.join("short.key");
        fs::write(&short, [7u8; 8]).unwrap();
        fs::set_permissions(&short, fs::Permissions::from_mode(0o600)).unwrap();
        let err = load_node_key(&short).expect_err("wrong length must be refused");
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn device_class_roundtrips_through_serde_json_in_contract() {
        let key = det_key(28);
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let mut c = contract(1000, vec![]);
        c.device_class = DeviceClass::NvidiaMig {
            parent_model: "H100-80GB".into(),
            profile: "1g.10gb".into(),
        };
        c.node_id = node_id;
        let json = serde_json::to_string(&c).expect("serialize contract");
        let back: JobContract = serde_json::from_str(&json).expect("deserialize contract");
        assert_eq!(back, c);
    }
}
