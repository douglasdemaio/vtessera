#![allow(dead_code)]
/// Key registry — TOML file listing allowed Ed25519 public keys.
///
/// Used in private/enterprise mode (`require_internal_ca = true`) to
/// validate that only known signing keys can submit receipts. The file
/// format is simple: one `[[keys]]` entry per allowed key.
use std::fs;
use std::path::Path;

use serde::Deserialize;

use crate::config::ConfigError;

#[derive(Debug, Clone, Deserialize)]
pub struct AllowedKey {
    pub name: String,
    pub pubkey: String, // base58-encoded Ed25519 public key (32 bytes)
}

#[derive(Debug, Clone, Deserialize)]
struct KeyRegistryFile {
    keys: Vec<AllowedKey>,
}

#[derive(Debug, Clone)]
pub struct KeyRegistry {
    keys: Vec<AllowedKey>,
}

impl KeyRegistry {
    /// Load a key registry from a TOML file.
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        let raw = fs::read_to_string(path.as_ref()).map_err(|e| {
            ConfigError::Validation(format!(
                "failed to read key registry {}: {e}",
                path.as_ref().display()
            ))
        })?;

        let file: KeyRegistryFile = toml::from_str(&raw).map_err(|e| {
            ConfigError::Validation(format!(
                "failed to parse key registry {}: {e}",
                path.as_ref().display()
            ))
        })?;

        // Validate each key is a valid base58 string of the right length.
        for key in &file.keys {
            validate_pubkey(&key.pubkey).map_err(|e| {
                ConfigError::Validation(format!("key registry entry '{}': {e}", key.name))
            })?;
        }

        Ok(KeyRegistry { keys: file.keys })
    }

    /// Check whether a public key (as bytes) is in the registry.
    pub fn contains(&self, pubkey_bytes: &[u8; 32]) -> bool {
        let b58 = base58_encode(pubkey_bytes);
        self.keys.iter().any(|k| k.pubkey == b58)
    }

    /// Check whether a base58-encoded public key string is in the registry.
    pub fn contains_str(&self, pubkey_b58: &str) -> bool {
        self.keys.iter().any(|k| k.pubkey == pubkey_b58)
    }

    /// Return the number of keys in the registry.
    pub fn len(&self) -> usize {
        self.keys.len()
    }

    /// Check if the registry is empty.
    pub fn is_empty(&self) -> bool {
        self.keys.is_empty()
    }
}

/// Validate that a string is a valid base58-encoded Ed25519 public key.
fn validate_pubkey(s: &str) -> Result<(), String> {
    if s.is_empty() {
        return Err("public key must not be empty".into());
    }
    const BASE58_ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    for c in s.bytes() {
        if !BASE58_ALPHABET.contains(&c) {
            return Err(format!(
                "public key contains non-base58 character {:?}",
                c as char
            ));
        }
    }
    // Decode and verify it produces exactly 32 bytes (Ed25519 public key).
    let bytes = base58_decode(s).map_err(|e| format!("invalid base58 key: {e}"))?;
    if bytes.len() != 32 {
        return Err(format!(
            "public key must decode to 32 bytes, got {}",
            bytes.len()
        ));
    }
    Ok(())
}

/// Minimal base58 decoder (Bitcoin alphabet). No external deps.
fn base58_decode(input: &str) -> Result<Vec<u8>, String> {
    const ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

    let leading_ones = input.bytes().take_while(|&b| b == b'1').count();

    let mut digits: Vec<u64> = Vec::new();
    for &byte in input.as_bytes() {
        let pos = ALPHABET
            .iter()
            .position(|&b| b == byte)
            .ok_or_else(|| format!("invalid base58 character: {}", byte as char))?;
        digits.push(pos as u64);
    }

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

    result.reverse();

    let mut output = vec![0u8; leading_ones];
    output.append(&mut result);
    Ok(output)
}

/// Minimal base58 encoder for 32-byte public keys. No external deps.
fn base58_encode(input: &[u8]) -> String {
    assert!(
        input.len() <= 32,
        "base58_encode supports at most 32 bytes, got {}",
        input.len()
    );
    const ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    // Extra headroom at front for carry propagation.
    let headroom = 4;
    let mut digits = vec![0u8; headroom + input.len() * 2];
    let mut carry: u16;
    let mut start = headroom;

    for &byte in input {
        carry = byte as u16;
        let mut j = digits.len() - 1;
        while j >= start {
            carry += (digits[j] as u16) << 8;
            digits[j] = (carry % 58) as u8;
            carry /= 58;
            j -= 1;
        }
        while carry > 0 {
            start -= 1;
            digits[start] = (carry % 58) as u8;
            carry /= 58;
        }
    }

    // Skip leading zeros (they represent leading zero bytes).
    while start < digits.len() && digits[start] == 0 {
        start += 1;
    }

    // Add '1' for each leading zero byte in input.
    let leading_ones = input.iter().take_while(|&&b| b == 0).count();
    let mut result = String::with_capacity(leading_ones + digits.len() - start);
    for _ in 0..leading_ones {
        result.push(ALPHABET[0] as char);
    }
    for &d in &digits[start..] {
        result.push(ALPHABET[d as usize] as char);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn load_valid_registry() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("keys.toml");
        std::fs::write(
            &path,
            r#"
[[keys]]
name = "team-alpha"
pubkey = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

[[keys]]
name = "team-beta"
pubkey = "DjPi1hDRXJLkZajm2VVxSCvnN6hBZNQMLcHREGfVDqTj"
"#,
        )
        .unwrap();

        let registry = KeyRegistry::load(&path).unwrap();
        assert_eq!(registry.len(), 2);
        assert!(registry.contains_str("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"));
        assert!(registry.contains_str("DjPi1hDRXJLkZajm2VVxSCvnN6hBZNQMLcHREGfVDqTj"));
        assert!(!registry.contains_str("3Kz8Abmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"));
    }

    #[test]
    fn load_missing_file() {
        let result = KeyRegistry::load("/nonexistent/keys.toml");
        assert!(result.is_err());
    }

    #[test]
    fn load_invalid_toml() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("keys.toml");
        std::fs::write(&path, "not valid toml {{{").unwrap();

        let result = KeyRegistry::load(&path);
        assert!(result.is_err());
    }

    #[test]
    fn reject_short_pubkey() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("keys.toml");
        std::fs::write(&path, "[[keys]]\nname = \"bad\"\npubkey = \"short\"\n").unwrap();

        let result = KeyRegistry::load(&path);
        assert!(result.is_err());
    }

    #[test]
    fn contains_works_with_real_key() {
        let key_b58 = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM";
        let dir = tempdir().unwrap();
        let path = dir.path().join("keys.toml");
        std::fs::write(
            &path,
            format!("[[keys]]\nname = \"test\"\npubkey = \"{key_b58}\"\n"),
        )
        .unwrap();

        let registry = KeyRegistry::load(&path).unwrap();
        assert!(registry.contains_str(key_b58));
        assert!(!registry.contains_str("7Xf9Bbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"));
    }

    #[test]
    fn base58_roundtrip() {
        let original = [0x42u8; 32];
        let encoded = base58_encode(&original);
        let decoded = base58_decode(&encoded).unwrap();
        assert_eq!(decoded, original);
    }

    #[test]
    fn base58_encode_leading_zeros() {
        let mut key = [0u8; 32];
        key[31] = 0x01;
        let encoded = base58_encode(&key);
        assert!(encoded.starts_with(&"1".repeat(31)));
    }

    #[test]
    fn validate_pubkey_accepts_short_base58() {
        let key = [0u8; 32];
        let encoded = base58_encode(&key);
        assert!(encoded.len() < 43);
        assert!(validate_pubkey(&encoded).is_ok());
    }
}
