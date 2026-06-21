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

use ed25519_dalek::{Signature, Verifier, VerifyingKey};
use sha2::{Digest, Sha256};

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

// ---------- Job contract + completion fraction ----------------------------

/// What the agent and the seller agreed at Module 2 contract time.
///
/// Settlement compares this against the metering the executor produced
/// to derive `f`. All fields are denominated in **device-seconds** for
/// the agreed device class — the same unit the offer quoted in.
#[derive(Debug, Clone, PartialEq)]
pub struct JobContract {
    /// Identifier shared with the job's signed receipts and metering.
    pub job_id: String,
    /// `node_id` of the seller (Module 2a offer's `node_id`).
    pub node_id: String,
    /// Device-seconds the buyer agreed to pay for.
    pub agreed_device_seconds: u64,
    /// Optional milestones for streaming partial release (ROADMAP.md §4b).
    /// Each value is a cumulative fraction in `[0, 1]`; entries must be
    /// strictly increasing. Empty means one final settlement.
    pub milestones: Vec<f64>,
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
#[derive(Debug, Clone, PartialEq)]
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
/// `f × price` (swapped to HNT, paid to seller) and `(1 − f) × price`
/// (refunded to buyer in the original stablecoin).
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
}
