use sha2::{Digest, Sha256};

/// A single usage receipt covering a sampling window.
#[derive(Debug, Clone)]
pub struct Receipt {
    pub schema_ver: u16,
    /// Self-attesting node identity. SHA-256(pubkey) truncated to 16 bytes,
    /// hex-encoded (32 chars). See `derive_node_id`.
    pub node_id: String,
    /// Operator payout destination, validated at config load. Distinct from
    /// node_id so the operator can rotate payout without breaking identity.
    pub payout_id: String,
    pub window_start: u64,
    pub window_end: u64,
    pub samples_digest: [u8; 32],
    pub totals: Totals,
}

/// Derive a stable, self-attesting node ID from an Ed25519 public key.
///
/// Returns hex of `SHA-256(pubkey)[..16]` — 32 lowercase hex chars.
/// A verifier with the signed receipt's pubkey can recompute and match
/// without trusting the daemon.
pub fn derive_node_id(pubkey: &[u8; 32]) -> String {
    let mut h = Sha256::new();
    h.update(pubkey);
    let digest = h.finalize();
    hex::encode(&digest[..16])
}

/// Aggregated resource totals over the window.
#[derive(Debug, Clone)]
pub struct Totals {
    pub cpu_pct_avg: f64,
    pub mem_used_kb_avg: u64,
    pub disk_free_kb_avg: u64,
    pub sample_count: u32,
}

/// A receipt paired with its Ed25519 signature and public key.
#[derive(Debug, Clone)]
pub struct SignedReceipt {
    pub receipt: Receipt,
    pub pubkey: [u8; 32],
    pub sig: [u8; 64],
}

/// Canonical serialization of a receipt for signing.
///
/// Byte layout (little-endian throughout; see BUILD.md §4 / receipt.rs spec):
///
///   schema_ver         : u16
///   node_id_len        : u16             (length-prefix; prevents collision)
///   node_id            : utf-8 bytes
///   payout_id_len      : u16
///   payout_id          : utf-8 bytes
///   window_start       : u64
///   window_end         : u64
///   samples_digest     : [u8; 32]
///   totals.cpu_pct_avg : f64             (IEEE 754 binary64 LE; see §4 caveat)
///   totals.mem_used_kb_avg : u64
///   totals.disk_free_kb_avg: u64
///   totals.sample_count    : u32
///
/// Any change to this layout requires bumping `schema_ver`.
pub fn canonical_bytes(r: &Receipt) -> Vec<u8> {
    let mut buf = Vec::new();
    buf.extend_from_slice(&r.schema_ver.to_le_bytes());

    let node_id_bytes = r.node_id.as_bytes();
    buf.extend_from_slice(&(node_id_bytes.len() as u16).to_le_bytes());
    buf.extend_from_slice(node_id_bytes);

    let payout_id_bytes = r.payout_id.as_bytes();
    buf.extend_from_slice(&(payout_id_bytes.len() as u16).to_le_bytes());
    buf.extend_from_slice(payout_id_bytes);

    buf.extend_from_slice(&r.window_start.to_le_bytes());
    buf.extend_from_slice(&r.window_end.to_le_bytes());
    buf.extend_from_slice(&r.samples_digest);
    buf.extend_from_slice(&r.totals.cpu_pct_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.mem_used_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.disk_free_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.sample_count.to_le_bytes());
    buf
}

/// Compute SHA-256 digest over a slice of serialized samples.
pub fn sample_digest(samples: &[u8]) -> [u8; 32] {
    let mut hasher = Sha256::new();
    hasher.update(samples);
    let result = hasher.finalize();
    let mut out = [0u8; 32];
    out.copy_from_slice(&result);
    out
}

/// Serialize a signed receipt as JSON bytes (manual, no serde).
pub fn signed_receipt_to_json(sr: &SignedReceipt) -> Vec<u8> {
    let mut json = String::new();
    json.push_str("{\"receipt\":{");
    json.push_str(&format!(
        "\"schema_ver\":{},\"node_id\":{:?},\"payout_id\":{:?},\"window_start\":{},\"window_end\":{},\"samples_digest\":\"{}\",\"totals\":{{\"cpu_pct_avg\":{},\"mem_used_kb_avg\":{},\"disk_free_kb_avg\":{},\"sample_count\":{}}}",
        sr.receipt.schema_ver,
        sr.receipt.node_id,
        sr.receipt.payout_id,
        sr.receipt.window_start,
        sr.receipt.window_end,
        hex::encode(sr.receipt.samples_digest),
        sr.receipt.totals.cpu_pct_avg,
        sr.receipt.totals.mem_used_kb_avg,
        sr.receipt.totals.disk_free_kb_avg,
        sr.receipt.totals.sample_count,
    ));
    json.push_str(&format!(
        "}},\"pubkey\":\"{}\",\"sig\":\"{}\"}}",
        hex::encode(sr.pubkey),
        hex::encode(sr.sig),
    ));
    json.into_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_canonical_bytes_stable() {
        let r = Receipt {
            schema_ver: 1,
            node_id: "test-node".into(),
            payout_id: "test-payout".into(),
            window_start: 1000,
            window_end: 2000,
            samples_digest: [0x42; 32],
            totals: Totals {
                cpu_pct_avg: 45.5,
                mem_used_kb_avg: 1024,
                disk_free_kb_avg: 99999,
                sample_count: 10,
            },
        };
        let a = canonical_bytes(&r);
        let b = canonical_bytes(&r);
        assert_eq!(a, b, "canonical bytes must be deterministic");
    }

    #[test]
    fn test_canonical_bytes_length_prefix_disambiguates() {
        // Without length prefixes, ("abc", "def") and ("ab", "cdef") would
        // share canonical bytes. The length prefix prevents that.
        let mut a = Receipt {
            schema_ver: 1,
            node_id: "abc".into(),
            payout_id: "def".into(),
            window_start: 0,
            window_end: 0,
            samples_digest: [0; 32],
            totals: Totals {
                cpu_pct_avg: 0.0,
                mem_used_kb_avg: 0,
                disk_free_kb_avg: 0,
                sample_count: 0,
            },
        };
        let bytes_a = canonical_bytes(&a);
        a.node_id = "ab".into();
        a.payout_id = "cdef".into();
        let bytes_b = canonical_bytes(&a);
        assert_ne!(bytes_a, bytes_b, "length prefix must disambiguate");
    }

    #[test]
    fn test_derive_node_id_stable() {
        let pk = [0u8; 32];
        let id1 = derive_node_id(&pk);
        let id2 = derive_node_id(&pk);
        assert_eq!(id1, id2);
        assert_eq!(id1.len(), 32, "node_id is 16 bytes hex-encoded = 32 chars");
        let pk2 = [1u8; 32];
        assert_ne!(derive_node_id(&pk2), id1);
    }

    #[test]
    fn test_sample_digest() {
        let d = sample_digest(b"hello");
        assert_eq!(d.len(), 32);
        assert_eq!(
            hex::encode(d),
            "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
        );
    }
}
