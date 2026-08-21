use ed25519_dalek::{Signature, VerifyingKey};
use serde::{Deserialize, Serialize};
use std::fs;
use std::io::{self, BufRead, BufReader, Write};
use std::path::PathBuf;
use uuid::Uuid;

/// A signed receipt submitted by a vtesserad node.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignedReceipt {
    pub receipt: Receipt,
    /// Hex-encoded Ed25519 public key (64 hex chars = 32 bytes).
    pub pubkey: String,
    /// Hex-encoded Ed25519 signature (128 hex chars = 64 bytes).
    pub sig: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Receipt {
    pub schema_ver: u16,
    pub node_id: String,
    pub payout_id: String,
    pub window_start: u64,
    pub window_end: u64,
    /// Hex-encoded SHA-256 digest of samples.
    pub samples_digest: String,
    pub totals: Totals,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Totals {
    pub cpu_pct_avg: f64,
    pub mem_used_kb_avg: u64,
    pub disk_free_kb_avg: u64,
    pub sample_count: u32,
}

/// Errors that can occur during receipt storage.
#[derive(Debug)]
pub enum StoreError {
    /// Signing key not in the key registry.
    UnknownKey(String),
    /// Duplicate receipt (same node_id + window_start).
    Duplicate(String),
    /// I/O error.
    Io(io::Error),
}

impl From<io::Error> for StoreError {
    fn from(e: io::Error) -> Self {
        StoreError::Io(e)
    }
}

/// Key registry loaded from a TOML file.
#[derive(Debug)]
pub struct KeyRegistry {
    /// Set of allowed public keys (raw 32 bytes).
    keys: Vec<[u8; 32]>,
}

impl KeyRegistry {
    /// Load a key registry from a TOML file.
    pub fn load(path: &str) -> Result<Self, io::Error> {
        let contents = fs::read_to_string(path)?;
        let parsed: toml::Value =
            toml::from_str(&contents).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;

        let keys_array = parsed
            .get("keys")
            .and_then(|v| v.as_array())
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    "key registry must have a [[keys]] array",
                )
            })?;

        let mut keys = Vec::new();
        for entry in keys_array {
            let pubkey_str = entry
                .get("pubkey")
                .and_then(|v| v.as_str())
                .ok_or_else(|| {
                    io::Error::new(
                        io::ErrorKind::InvalidData,
                        "each key entry must have a 'pubkey' string",
                    )
                })?;

            let pubkey_bytes = decode_base58_pubkey(pubkey_str).map_err(|e| {
                io::Error::new(io::ErrorKind::InvalidData, format!("invalid pubkey: {e}"))
            })?;

            keys.push(pubkey_bytes);
        }

        Ok(KeyRegistry { keys })
    }

    /// Check if a public key is in the registry.
    pub fn contains(&self, pubkey: &[u8; 32]) -> bool {
        self.keys.iter().any(|k| k == pubkey)
    }

    /// Check if a hex-encoded public key is in the registry.
    pub fn contains_hex(&self, pubkey_hex: &str) -> bool {
        match hex::decode(pubkey_hex) {
            Ok(bytes) if bytes.len() == 32 => {
                let mut arr = [0u8; 32];
                arr.copy_from_slice(&bytes);
                self.contains(&arr)
            }
            _ => false,
        }
    }
}

/// Decode a base58-encoded Ed25519 public key to raw bytes.
fn decode_base58_pubkey(s: &str) -> Result<[u8; 32], String> {
    let bytes = base58_decode(s)?;
    if bytes.len() != 32 {
        return Err(format!("expected 32 bytes, got {}", bytes.len()));
    }
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&bytes);
    Ok(arr)
}

/// Minimal base58 decoder (Bitcoin alphabet).
fn base58_decode(input: &str) -> Result<Vec<u8>, String> {
    const ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

    // Count leading '1' characters (represent leading zero bytes).
    let leading_ones = input.bytes().take_while(|&b| b == b'1').count();

    // Convert each character to its digit value.
    let mut digits: Vec<u64> = Vec::new();
    for &byte in input.as_bytes() {
        let pos = ALPHABET
            .iter()
            .position(|&b| b == byte)
            .ok_or_else(|| format!("invalid base58 character: {}", byte as char))?;
        digits.push(pos as u64);
    }

    // Convert base58 digits to bytes.
    let mut result: Vec<u8> = Vec::new();
    for &digit in &digits {
        let mut carry = digit;
        for r in result.iter_mut() {
            let val = (*r as u64) * 58 + carry;
            *r = (val & 0xff) as u8;
            carry = val >> 8;
        }
        while carry > 0 {
            result.push((carry & 0xff) as u8);
            carry >>= 8;
        }
    }

    // Reverse since we built it LSB-first.
    result.reverse();

    // Add leading zero bytes for leading '1' characters.
    let mut output = vec![0u8; leading_ones];
    output.append(&mut result);
    Ok(output)
}

/// Append-only JSON lines receipt store.
pub struct ReceiptStore {
    path: PathBuf,
}

impl ReceiptStore {
    /// Create a new receipt store pointing at the given file path.
    pub fn new(path: &str) -> Self {
        ReceiptStore {
            path: PathBuf::from(path),
        }
    }

    /// Store a signed receipt. Returns the assigned UUID on success.
    ///
    /// Validates the signature against the key registry, checks for
    /// duplicates, and appends to the JSON lines file.
    pub fn store(
        &self,
        sr: &SignedReceipt,
        registry: &KeyRegistry,
    ) -> Result<String, StoreError> {
        // Validate signature.
        let pubkey_bytes = hex::decode(&sr.pubkey).map_err(|e| {
            StoreError::Io(io::Error::new(io::ErrorKind::InvalidData, e))
        })?;
        if pubkey_bytes.len() != 32 {
            return Err(StoreError::Io(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("pubkey must be 32 bytes, got {}", pubkey_bytes.len()),
            )));
        }

        if !registry.contains_hex(&sr.pubkey) {
            return Err(StoreError::UnknownKey(sr.pubkey.clone()));
        }

        // Verify Ed25519 signature.
        let mut pubkey_arr = [0u8; 32];
        pubkey_arr.copy_from_slice(&pubkey_bytes);
        let verifying_key = VerifyingKey::from_bytes(&pubkey_arr).map_err(|e| {
            StoreError::Io(io::Error::new(io::ErrorKind::InvalidData, e))
        })?;

        let sig_bytes = hex::decode(&sr.sig).map_err(|e| {
            StoreError::Io(io::Error::new(io::ErrorKind::InvalidData, e))
        })?;
        if sig_bytes.len() != 64 {
            return Err(StoreError::Io(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("signature must be 64 bytes, got {}", sig_bytes.len()),
            )));
        }
        let mut sig_arr = [0u8; 64];
        sig_arr.copy_from_slice(&sig_bytes);
        let signature = Signature::from_bytes(&sig_arr);

        // Canonical bytes for verification (must match vtesserad's format).
        let canonical = canonical_bytes(&sr.receipt);
        verifying_key
            .verify_strict(&canonical, &signature)
            .map_err(|e| {
                StoreError::Io(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("signature verification failed: {e}"),
                ))
            })?;

        // Check for duplicate (same node_id + window_start).
        let dedup_key = format!("{}:{}", sr.receipt.node_id, sr.receipt.window_start);
        if self.exists(&dedup_key) {
            return Err(StoreError::Duplicate(dedup_key));
        }

        // Append to file.
        let id = Uuid::new_v4().to_string();
        let line = serde_json::to_string(sr).map_err(|e| {
            StoreError::Io(io::Error::new(io::ErrorKind::InvalidData, e))
        })?;

        let mut file = fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)?;

        writeln!(file, "{line}").map_err(StoreError::Io)?;
        file.flush().map_err(StoreError::Io)?;

        Ok(id)
    }

    /// List stored receipts, optionally filtered by node_id and/or since (window_start).
    pub fn list(&self, node_id: Option<&str>, since: Option<u64>) -> Vec<SignedReceipt> {
        let file = match fs::File::open(&self.path) {
            Ok(f) => f,
            Err(_) => return Vec::new(),
        };

        let reader = BufReader::new(file);
        let mut results = Vec::new();

        for line in reader.lines() {
            let line = match line {
                Ok(l) => l,
                Err(_) => continue,
            };
            if line.trim().is_empty() {
                continue;
            }

            let sr: SignedReceipt = match serde_json::from_str(&line) {
                Ok(sr) => sr,
                Err(_) => continue,
            };

            if let Some(nid) = node_id {
                if sr.receipt.node_id != nid {
                    continue;
                }
            }
            if let Some(s) = since {
                if sr.receipt.window_start < s {
                    continue;
                }
            }

            results.push(sr);
        }

        results
    }

    /// Check if a dedup key already exists in the file.
    fn exists(&self, dedup_key: &str) -> bool {
        let file = match fs::File::open(&self.path) {
            Ok(f) => f,
            Err(_) => return false,
        };

        let reader = BufReader::new(file);
        for line in reader.lines().map_while(Result::ok) {
            if line.trim().is_empty() {
                continue;
            }
            if let Ok(sr) = serde_json::from_str::<SignedReceipt>(&line) {
                let key = format!("{}:{}", sr.receipt.node_id, sr.receipt.window_start);
                if key == dedup_key {
                    return true;
                }
            }
        }
        false
    }
}

/// Canonical serialization of a receipt for signing (must match vtesserad's format).
fn canonical_bytes(r: &Receipt) -> Vec<u8> {
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

    // samples_digest is hex-encoded; decode to bytes.
    let digest_bytes = hex::decode(&r.samples_digest).unwrap_or_default();
    buf.extend_from_slice(&digest_bytes);

    buf.extend_from_slice(&r.totals.cpu_pct_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.mem_used_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.disk_free_kb_avg.to_le_bytes());
    buf.extend_from_slice(&r.totals.sample_count.to_le_bytes());
    buf
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn make_signed_receipt(node_id: &str, window_start: u64) -> SignedReceipt {
        SignedReceipt {
            receipt: Receipt {
                schema_ver: 1,
                node_id: node_id.into(),
                payout_id: "test-payout".into(),
                window_start,
                window_end: window_start + 3600,
                samples_digest: "a1b2c3d4e5f6".into(),
                totals: Totals {
                    cpu_pct_avg: 50.0,
                    mem_used_kb_avg: 2048,
                    disk_free_kb_avg: 50000,
                    sample_count: 10,
                },
            },
            pubkey: "aabbccdd".repeat(8), // dummy hex
            sig: "11223344".repeat(16),    // dummy hex
        }
    }

    #[test]
    fn test_store_rejects_unknown_key() {
        let dir = TempDir::new().unwrap();
        let store_path = dir.path().join("receipts.jsonl");
        let reg_path = dir.path().join("keys.toml");

        fs::write(
            &reg_path,
            r#"
[[keys]]
name = "test"
pubkey = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
"#,
        )
        .unwrap();

        let store = ReceiptStore::new(store_path.to_str().unwrap());
        let registry = KeyRegistry::load(reg_path.to_str().unwrap()).unwrap();

        let sr = make_signed_receipt("node1", 1000);
        let result = store.store(&sr, &registry);
        assert!(matches!(result, Err(StoreError::UnknownKey(_))));
    }

    #[test]
    fn test_list_empty_when_no_file() {
        let store = ReceiptStore::new("/nonexistent/receipts.jsonl");
        let results = store.list(None, None);
        assert!(results.is_empty());
    }

    #[test]
    fn test_key_registry_load_valid() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("keys.toml");

        fs::write(
            &path,
            r#"
[[keys]]
name = "alpha"
pubkey = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
"#,
        )
        .unwrap();

        let registry = KeyRegistry::load(path.to_str().unwrap()).unwrap();
        // Registry stores raw bytes from base58 decode; verify contains works.
        let raw_bytes = base58_decode("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM").unwrap();
        let mut arr = [0u8; 32];
        arr.copy_from_slice(&raw_bytes);
        assert!(registry.contains(&arr));
        // Wrong key should not match.
        let wrong = [0xAA; 32];
        assert!(!registry.contains(&wrong));
    }

    #[test]
    fn test_key_registry_load_missing() {
        let result = KeyRegistry::load("/nonexistent/keys.toml");
        assert!(result.is_err());
    }
}
