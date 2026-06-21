use serde::Deserialize;
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(dead_code)]
pub struct Caps {
    pub max_cpus: Option<u64>,
    pub max_mem_kb: Option<u64>,
    pub max_disk_kb: Option<u64>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(dead_code)]
pub struct Config {
    pub sample_interval_secs: u64,
    pub state_dir: PathBuf,
    pub key_path: PathBuf,
    pub resource_caps: Caps,
    pub payout_id: String,
    #[serde(default)]
    pub submit_endpoint: Option<String>,
    #[serde(default)]
    pub window_size: Option<u64>,
    /// Cap on the number of `receipt_*.json` files kept in `state_dir`.
    /// When set, the daemon prunes the oldest files after each write so
    /// the spool can't grow unbounded. `None` (default) means no cap.
    /// See ROADMAP.md §5 — long-running deployments should always set
    /// this.
    #[serde(default)]
    pub max_spool_files: Option<usize>,
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        let raw = fs::read_to_string(path.as_ref()).map_err(ConfigError::Io)?;
        toml::from_str(&raw).map_err(ConfigError::Parse)
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.sample_interval_secs == 0 || self.sample_interval_secs > 3600 {
            return Err(ConfigError::Validation(
                "sample_interval_secs must be between 1 and 3600".into(),
            ));
        }
        validate_payout_id(&self.payout_id)?;
        if self.state_dir.as_os_str().is_empty() {
            return Err(ConfigError::Validation(
                "state_dir must not be empty".into(),
            ));
        }
        if self.key_path.as_os_str().is_empty() {
            return Err(ConfigError::Validation("key_path must not be empty".into()));
        }
        Ok(())
    }
}

/// payout_id must be a Solana Ed25519 base58 address: 32–44 chars of the
/// Bitcoin/Solana base58 alphabet. Length + charset check only; we don't
/// pull a full base58 decoder into the default-build dep tree (BUILD.md §1.3).
/// This catches typos and obvious wrong-network strings; the settlement
/// enclave does the full decode + on-curve check.
fn validate_payout_id(s: &str) -> Result<(), ConfigError> {
    if s.is_empty() {
        return Err(ConfigError::Validation(
            "payout_id must not be empty".into(),
        ));
    }
    if s.len() < 32 || s.len() > 44 {
        return Err(ConfigError::Validation(format!(
            "payout_id must be 32–44 base58 chars (Solana address), got {}",
            s.len()
        )));
    }
    const BASE58_ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    for c in s.bytes() {
        if !BASE58_ALPHABET.contains(&c) {
            return Err(ConfigError::Validation(format!(
                "payout_id contains non-base58 character {:?}",
                c as char
            )));
        }
    }
    Ok(())
}

#[derive(Debug)]
pub enum ConfigError {
    Io(std::io::Error),
    Parse(toml::de::Error),
    Validation(String),
}

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "IO error reading config: {e}"),
            ConfigError::Parse(e) => write!(f, "TOML parse error: {e}"),
            ConfigError::Validation(e) => write!(f, "Config validation error: {e}"),
        }
    }
}

impl std::error::Error for ConfigError {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn payout_id_accepts_solana_address() {
        // A real-shape Solana base58 pubkey (33 chars, all valid alphabet).
        assert!(validate_payout_id("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM").is_ok());
    }

    #[test]
    fn payout_id_rejects_empty() {
        assert!(validate_payout_id("").is_err());
    }

    #[test]
    fn payout_id_rejects_too_short() {
        assert!(validate_payout_id("9WzDXwBbmkg8").is_err());
    }

    #[test]
    fn payout_id_rejects_too_long() {
        assert!(validate_payout_id(&"9".repeat(45)).is_err());
    }

    #[test]
    fn payout_id_rejects_non_base58_char() {
        // '0', 'O', 'I', 'l' are not in base58.
        assert!(validate_payout_id("0WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM").is_err());
        assert!(validate_payout_id("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWW0").is_err());
    }
}
