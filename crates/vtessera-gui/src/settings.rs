//! Seller settings for the Vtessera GUI.
//!
//! Persisted as `settings.toml` in the user config dir. Distinct from the
//! daemon's `vtessera.toml` (which only holds the fields `vtesserad`
//! understands): the GUI keeps its own app-level settings (port, advertised
//! endpoint, escrow account, network) here and derives the daemon config +
//! signed offer from them on save.

use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

pub const DEFAULT_PORT: u16 = 8402;
pub const DEFAULT_ESCROW: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
pub const DEFAULT_NETWORK: &str = "solana-devnet";

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    /// `"paid"` (sell compute) or `"free"` (donate it).
    pub mode: String,
    /// `"eurc"` or `"usdc"`.
    pub currency: String,
    /// Price per CPU-hour as a decimal, e.g. `0.05`.
    pub price_per_cpu_hour: f64,
    /// Solana base58 payout address. Required only in paid mode.
    pub payout_id: String,
    /// Port the agent-facing node API binds.
    pub port: u16,
    /// Endpoint advertised in the signed offer (what agents connect to).
    pub endpoint: String,
    /// Escrow account surfaced in the x402 payment challenge.
    pub escrow_account: String,
    /// Chain identifier surfaced in the x402 payment challenge.
    pub network: String,
    /// Metering sample interval in seconds.
    pub sample_interval_secs: u64,
}

impl Default for Settings {
    fn default() -> Self {
        Settings {
            mode: "paid".into(),
            currency: "eurc".into(),
            price_per_cpu_hour: 0.05,
            payout_id: String::new(),
            port: DEFAULT_PORT,
            endpoint: format!("http://127.0.0.1:{DEFAULT_PORT}"),
            escrow_account: DEFAULT_ESCROW.into(),
            network: DEFAULT_NETWORK.into(),
            sample_interval_secs: 60,
        }
    }
}

impl Settings {
    pub fn load_or_default(path: &Path) -> Settings {
        match std::fs::read_to_string(path) {
            Ok(raw) => toml::from_str(&raw).unwrap_or_else(|e| {
                eprintln!("vtessera-gui: ignoring unreadable settings ({e}); using defaults");
                Settings::default()
            }),
            Err(_) => Settings::default(),
        }
    }

    pub fn save(&self, path: &Path) -> Result<(), String> {
        let raw = toml::to_string(self).map_err(|e| format!("serialize settings: {e}"))?;
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
        }
        std::fs::write(path, raw).map_err(|e| format!("write {}: {e}", path.display()))
    }

    pub fn is_free(&self) -> bool {
        self.mode == "free"
    }

    pub fn validate(&self) -> Result<(), String> {
        if !matches!(self.mode.as_str(), "paid" | "free") {
            return Err(format!("mode must be \"paid\" or \"free\", got \"{}\"", self.mode));
        }
        if !matches!(self.currency.as_str(), "eurc" | "usdc") {
            return Err(format!(
                "currency must be \"eurc\" or \"usdc\", got \"{}\"",
                self.currency
            ));
        }
        if self.sample_interval_secs == 0 || self.sample_interval_secs > 3600 {
            return Err("sample interval must be between 1 and 3600 seconds".into());
        }
        if self.port == 0 {
            return Err("port must not be 0".into());
        }
        if !(self.endpoint.starts_with("http://") || self.endpoint.starts_with("https://")) {
            return Err("endpoint must start with http:// or https://".into());
        }
        if self.escrow_account.trim().is_empty() {
            return Err("escrow account must not be empty".into());
        }
        if self.network.trim().is_empty() {
            return Err("network must not be empty".into());
        }
        if self.is_free() {
            // Donate mode: no address and no price required.
            return Ok(());
        }
        validate_payout_id(&self.payout_id)?;
        if self.price_per_cpu_hour <= 0.0 {
            return Err("paid mode requires a price per CPU-hour greater than 0".into());
        }
        if self.price_per_cpu_hour > 1_000_000.0 {
            return Err("price per CPU-hour looks unreasonable (max 1,000,000)".into());
        }
        Ok(())
    }
}

/// `payout_id` must be a Solana Ed25519 base58 address: 32–44 chars of the
/// base58 alphabet. Same rule the daemon enforces.
fn validate_payout_id(s: &str) -> Result<(), String> {
    if s.is_empty() {
        return Err("a Solana payout address is required in paid mode".into());
    }
    if !(32..=44).contains(&s.len()) {
        return Err(format!(
            "payout address must be 32–44 base58 characters, got {}",
            s.len()
        ));
    }
    const BASE58_ALPHABET: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
    for c in s.bytes() {
        if !BASE58_ALPHABET.contains(&c) {
            return Err(format!("payout address contains invalid character {:?}", c as char));
        }
    }
    Ok(())
}

/// Standard paths under the Flatpak's writable dirs.
pub fn config_dir() -> PathBuf {
    gtk4::glib::user_config_dir().join("vtessera")
}

pub fn data_dir() -> PathBuf {
    gtk4::glib::user_data_dir().join("vtessera")
}

pub fn settings_path() -> PathBuf {
    config_dir().join("settings.toml")
}

pub fn daemon_config_path() -> PathBuf {
    config_dir().join("vtessera.toml")
}

pub fn key_path() -> PathBuf {
    config_dir().join("identity.key")
}

pub fn offer_path() -> PathBuf {
    config_dir().join("offer.json")
}

pub fn state_dir() -> PathBuf {
    data_dir()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> Settings {
        Settings {
            mode: "paid".into(),
            currency: "eurc".into(),
            price_per_cpu_hour: 0.05,
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
            port: 8402,
            endpoint: "http://127.0.0.1:8402".into(),
            escrow_account: DEFAULT_ESCROW.into(),
            network: "solana-devnet".into(),
            sample_interval_secs: 60,
        }
    }

    #[test]
    fn paid_mode_requires_address_and_price() {
        assert!(valid().validate().is_ok());
        let mut s = valid();
        s.payout_id = String::new();
        assert!(s.validate().is_err());
        let mut s = valid();
        s.price_per_cpu_hour = 0.0;
        assert!(s.validate().is_err());
    }

    #[test]
    fn free_mode_neither_address_nor_price() {
        let mut s = valid();
        s.mode = "free".into();
        s.payout_id = String::new();
        s.price_per_cpu_hour = 0.0;
        assert!(s.validate().is_ok());
    }

    #[test]
    fn bad_address_rejected() {
        let mut s = valid();
        s.payout_id = "not base58 0".into();
        assert!(s.validate().is_err());
    }

    #[test]
    fn settings_roundtrip() {
        let dir = std::env::temp_dir().join("vtessera_gui_settings_test");
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("settings.toml");
        let s = valid();
        s.save(&path).expect("save");
        let loaded = Settings::load_or_default(&path);
        assert_eq!(loaded.payout_id, s.payout_id);
        assert_eq!(loaded.port, s.port);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn load_defaults_when_missing() {
        let dir = std::env::temp_dir().join("vtessera_gui_settings_missing");
        let _ = std::fs::remove_dir_all(&dir);
        let s = Settings::load_or_default(&dir.join("nope.toml"));
        assert_eq!(s.mode, "paid");
        assert_eq!(s.port, DEFAULT_PORT);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
