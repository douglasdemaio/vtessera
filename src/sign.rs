use std::fs;
use std::io;
use std::path::Path;

use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey, SECRET_KEY_LENGTH};
use rand::rngs::OsRng;

use crate::receipt::{canonical_bytes, Receipt, SignedReceipt};

/// Load or generate an Ed25519 keypair at `key_path`.
///
/// On load, refuses to start if the key file's mode permits any group or
/// world access (mode & 0o077 != 0). The daemon expects exclusive ownership
/// of the signing key.
pub fn load_or_generate(key_path: &Path) -> io::Result<SigningKey> {
    if key_path.exists() {
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
        if raw.len() != SECRET_KEY_LENGTH {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!(
                    "key file has wrong length: expected {SECRET_KEY_LENGTH}, got {}",
                    raw.len()
                ),
            ));
        }
        let mut arr = [0u8; SECRET_KEY_LENGTH];
        arr.copy_from_slice(&raw);
        Ok(SigningKey::from_bytes(&arr))
    } else {
        let signing_key = SigningKey::generate(&mut OsRng);
        if let Some(parent) = key_path.parent() {
            fs::create_dir_all(parent)?;
        }
        fs::write(key_path, signing_key.to_bytes())?;

        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(key_path, fs::Permissions::from_mode(0o600))?;
        }

        Ok(signing_key)
    }
}

/// Sign a receipt, producing a SignedReceipt.
pub fn sign(signing_key: &SigningKey, receipt: &Receipt) -> SignedReceipt {
    let canonical = canonical_bytes(receipt);
    let sig: Signature = signing_key.sign(&canonical);
    let verifying_key: VerifyingKey = signing_key.verifying_key();

    SignedReceipt {
        receipt: receipt.clone(),
        pubkey: verifying_key.to_bytes(),
        sig: sig.to_bytes(),
    }
}

/// Verify a signed receipt. Returns Ok(()) if the signature matches.
#[allow(dead_code)]
pub fn verify(sr: &SignedReceipt) -> Result<(), ed25519_dalek::SignatureError> {
    let verifying_key = VerifyingKey::from_bytes(&sr.pubkey)?;
    let canonical = canonical_bytes(&sr.receipt);
    let sig = Signature::from_bytes(&sr.sig);
    verifying_key.verify_strict(&canonical, &sig)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::receipt::{Receipt, Totals};

    fn dummy_receipt() -> Receipt {
        Receipt {
            schema_ver: 1,
            node_id: "test-node".into(),
            payout_id: "test-payout".into(),
            window_start: 1000,
            window_end: 2000,
            samples_digest: [0x42; 32],
            totals: Totals {
                cpu_pct_avg: 50.0,
                mem_used_kb_avg: 2048,
                disk_free_kb_avg: 50000,
                sample_count: 5,
            },
        }
    }

    #[test]
    fn test_sign_verify_roundtrip() {
        let sk = SigningKey::generate(&mut OsRng);
        let receipt = dummy_receipt();
        let sr = sign(&sk, &receipt);
        assert!(verify(&sr).is_ok(), "signature should verify");
    }

    #[test]
    fn test_verify_rejects_tampered() {
        let sk = SigningKey::generate(&mut OsRng);
        let receipt = dummy_receipt();
        let mut sr = sign(&sk, &receipt);
        sr.sig[0] ^= 0x01;
        assert!(verify(&sr).is_err(), "tampered signature should fail");
    }

    #[test]
    fn test_load_or_generate_creates_file() {
        let dir = std::env::temp_dir().join("vtessera_test_keys");
        let _ = fs::remove_dir_all(&dir);
        let path = dir.join("test_key");
        let sk = load_or_generate(&path).expect("should create key");
        assert!(path.exists(), "key file should exist");
        let loaded = load_or_generate(&path).expect("should load existing key");
        assert_eq!(sk.to_bytes(), loaded.to_bytes(), "keys should match");
        let _ = fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[test]
    fn test_generated_key_is_mode_0600() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join("vtessera_test_keys_mode");
        let _ = fs::remove_dir_all(&dir);
        let path = dir.join("k");
        load_or_generate(&path).expect("should create");
        let mode = fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "key file mode must be 0600, got {mode:o}");
        let _ = fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[test]
    fn test_load_refuses_world_readable_key() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join("vtessera_test_keys_perm");
        let _ = fs::remove_dir_all(&dir);
        let path = dir.join("k");
        load_or_generate(&path).expect("create");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).expect("chmod");
        let err = load_or_generate(&path).expect_err("should refuse 0644");
        assert_eq!(err.kind(), io::ErrorKind::PermissionDenied);
        let _ = fs::remove_dir_all(&dir);
    }
}
