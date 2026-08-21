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
    // Ed25519 public keys are 32 bytes. Base58 encoding of 32 bytes
    // produces 43-44 characters.
    if s.len() < 43 || s.len() > 44 {
        return Err(format!(
            "public key must be 43-44 base58 chars (32 bytes), got {}",
            s.len()
        ));
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
    Ok(())
}

/// Minimal base58 encoder for 32-byte public keys. No external deps.
fn base58_encode(input: &[u8]) -> String {
    const ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    let mut digits = vec![0u8; input.len() * 2]; // upper bound
    let mut carry: u16;
    let mut start = 0;

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

    #[test]
    fn load_valid_registry() {
        let dir = std::env::temp_dir().join("vtessera_key_registry_test");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("keys.toml");
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

        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn load_missing_file() {
        let result = KeyRegistry::load("/nonexistent/keys.toml");
        assert!(result.is_err());
    }

    #[test]
    fn load_invalid_toml() {
        let dir = std::env::temp_dir().join("vtessera_key_registry_test_invalid");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("keys.toml");
        std::fs::write(&path, "not valid toml {{{").unwrap();

        let result = KeyRegistry::load(&path);
        assert!(result.is_err());

        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn reject_short_pubkey() {
        let dir = std::env::temp_dir().join("vtessera_key_registry_test_short");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("keys.toml");
        std::fs::write(&path, "[[keys]]\nname = \"bad\"\npubkey = \"short\"\n").unwrap();

        let result = KeyRegistry::load(&path);
        assert!(result.is_err());

        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn contains_works_with_real_key() {
        // A known Solana pubkey shape (33 chars base58 = 32 bytes).
        let key_b58 = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM";
        let dir = std::env::temp_dir().join("vtessera_key_registry_test_real");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("keys.toml");
        std::fs::write(
            &path,
            format!("[[keys]]\nname = \"test\"\npubkey = \"{key_b58}\"\n"),
        )
        .unwrap();

        let registry = KeyRegistry::load(&path).unwrap();
        assert!(registry.contains_str(key_b58));
        assert!(!registry.contains_str("7Xf9Bbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"));

        std::fs::remove_dir_all(&dir).unwrap();
    }
}
