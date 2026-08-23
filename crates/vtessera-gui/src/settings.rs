//! Seller settings for the Vtessera GUI.
//!
//! Persisted as `settings.toml` in the user config dir. Distinct from the
//! daemon's `vtessera.toml` (which only holds the fields `vtesserad`
//! understands): the GUI keeps its own app-level settings (port, advertised
//! endpoint, escrow account, network, job backend) here and derives the
//! daemon config + signed offer from them on save.

use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

pub const DEFAULT_PORT: u16 = 8402;
pub const DEFAULT_ESCROW: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
pub const DEFAULT_NETWORK: &str = "solana-devnet";
/// Job executor the GUI passes to the spawned `vtessera-node` via `--backend`.
pub const DEFAULT_BACKEND: &str = "noop-cpu";
/// Bump when the consent copy or the consent contract changes: a stored
/// `consent_version` below this re-shows the first-run gate (§2 of
/// `docs/CONSENT.md`) so users re-read what they're consenting to.
pub const CURRENT_CONSENT_VERSION: u32 = 1;

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
    /// Dropdown preset: "devnet", "mainnet", or "custom".
    /// When "devnet" or "mainnet", `network` is auto-set.
    /// When "custom", the user types a free-text network string.
    #[serde(default = "default_network_preset")]
    pub network_preset: String,
    /// When true, operate in local/private network mode. The daemon switches
    /// to `network.mode = "private"` and uses `allowed_cidrs` for access
    /// control. The GUI hides marketplace publishing and internet discovery.
    #[serde(default)]
    pub local_network: bool,
    /// CIDR ranges allowed in local network mode (e.g. "192.168.1.0/24").
    /// Empty means all private ranges are allowed.
    #[serde(default)]
    pub allowed_cidrs: Vec<String>,
    /// Metering sample interval in seconds.
    pub sample_interval_secs: u64,
    /// Job executor backend passed to `vtessera-node --backend`:
    /// `"noop-cpu"` (synthetic metering, no execution) or `"local-cpu"`
    /// (runs the job's command on the host — NOT isolated). Defaults to
    /// `noop-cpu` so existing `settings.toml` files keep their behavior.
    #[serde(default = "default_backend")]
    pub backend: String,
    /// Explicit consent to metering (first-run gate, §2.1 of
    /// `docs/CONSENT.md`). `false` re-shows the gate; the user must press
    /// "Enable metering" before the app will start anything.
    #[serde(default)]
    pub metering_consent: bool,
    /// Second consent gate (§2.2): whether this machine accepts workloads
    /// from other agents. OFF by default and off until explicitly enabled.
    /// The local-cpu executor is not isolated, so this is deliberately a
    /// separate, explicit decision from metering consent.
    #[serde(default)]
    pub accept_workloads: bool,
    /// Version of the consent copy this `metering_consent` was given
    /// against. Bumped by `CURRENT_CONSENT_VERSION`; stored values below it
    /// re-shows the gate. `0` also marks "never consented" for pre-consent
    /// `settings.toml` files that predate this field.
    #[serde(default)]
    pub consent_version: u32,
    /// Marketplace index URL for offer publishing. Empty = no publishing.
    /// Defaults to `http://<lan-ip>:8443` when empty.
    #[serde(default)]
    pub marketplace_url: String,
}

fn default_backend() -> String {
    DEFAULT_BACKEND.into()
}

fn default_network_preset() -> String {
    "devnet".into()
}

impl Default for Settings {
    fn default() -> Self {
        Settings {
            mode: "free".into(),
            currency: "eurc".into(),
            price_per_cpu_hour: 0.05,
            payout_id: String::new(),
            port: DEFAULT_PORT,
            endpoint: String::new(), // auto-detected at start time
            escrow_account: DEFAULT_ESCROW.into(),
            network: DEFAULT_NETWORK.into(),
            network_preset: "devnet".into(),
            local_network: false,
            allowed_cidrs: Vec::new(),
            sample_interval_secs: 60,
            backend: DEFAULT_BACKEND.into(),
            metering_consent: false,
            accept_workloads: false,
            consent_version: 0,
            marketplace_url: String::new(),
        }
    }
}

/// Whether the stored settings still need the first-run consent gate.
/// True when consent was never recorded, or when the copy has been updated
/// since it was (see `CURRENT_CONSENT_VERSION`).
pub fn needs_consent(settings: &Settings) -> bool {
    !settings.metering_consent || settings.consent_version < CURRENT_CONSENT_VERSION
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
            std::fs::create_dir_all(parent)
                .map_err(|e| format!("create {}: {e}", parent.display()))?;
        }
        std::fs::write(path, raw).map_err(|e| format!("write {}: {e}", path.display()))
    }

    pub fn is_free(&self) -> bool {
        self.mode == "free"
    }

    pub fn validate(&self) -> Result<(), String> {
        if !matches!(self.mode.as_str(), "paid" | "free") {
            return Err(format!(
                "mode must be \"paid\" or \"free\", got \"{}\"",
                self.mode
            ));
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
        if !matches!(self.backend.as_str(), "noop-cpu" | "local-cpu") {
            return Err(format!(
                "backend must be \"noop-cpu\" or \"local-cpu\", got \"{}\"",
                self.backend
            ));
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
        if !matches!(
            self.network_preset.as_str(),
            "devnet" | "mainnet" | "custom"
        ) {
            return Err(format!(
                "network_preset must be \"devnet\", \"mainnet\", or \"custom\", got \"{}\"",
                self.network_preset
            ));
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
            return Err(format!(
                "payout address contains invalid character {:?}",
                c as char
            ));
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
            network_preset: "devnet".into(),
            sample_interval_secs: 60,
            backend: DEFAULT_BACKEND.into(),
            metering_consent: true,
            accept_workloads: false,
            consent_version: CURRENT_CONSENT_VERSION,
            marketplace_url: String::new(),
            local_network: false,
            allowed_cidrs: Vec::new(),
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
        assert_eq!(s.mode, "free");
        assert_eq!(s.port, DEFAULT_PORT);
        assert_eq!(s.backend, DEFAULT_BACKEND);
        // Fresh installs start without consent and with workloads off.
        assert!(!s.metering_consent);
        assert!(!s.accept_workloads);
        assert_eq!(s.consent_version, 0);
        assert!(needs_consent(&s));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn backend_validation_rejects_unknown_backends() {
        let mut s = valid();
        s.backend = "docker".into();
        assert!(s.validate().is_err());
        for ok in ["noop-cpu", "local-cpu"] {
            let mut s = valid();
            s.backend = ok.into();
            assert!(s.validate().is_ok());
        }
    }

    #[test]
    fn old_settings_without_backend_default_to_noop_cpu() {
        // An existing settings.toml (no `backend` key) must load with the
        // safe default rather than failing the whole read.
        let dir = std::env::temp_dir().join("vtessera_gui_settings_backend_default");
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("settings.toml");
        std::fs::create_dir_all(&dir).unwrap();
        let old = "mode = \"free\"\ncurrency = \"eurc\"\nprice_per_cpu_hour = 0.0\n\
                   payout_id = \"\"\nport = 8402\nendpoint = \"http://127.0.0.1:8402\"\n\
                   escrow_account = \"6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma\"\n\
                   network = \"solana-devnet\"\nsample_interval_secs = 60\n";
        std::fs::write(&path, old).unwrap();
        let s = Settings::load_or_default(&path);
        assert_eq!(s.mode, "free");
        assert_eq!(s.backend, DEFAULT_BACKEND);
        // Pre-consent settings.toml files re-show the gate once.
        assert!(needs_consent(&s));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn consent_version_gating() {
        let mut s = valid();
        assert!(!needs_consent(&s));

        // Stale copy → re-show.
        s.consent_version = CURRENT_CONSENT_VERSION - 1;
        assert!(needs_consent(&s));
        s.consent_version = CURRENT_CONSENT_VERSION;
        assert!(!needs_consent(&s));

        // Consent revoked → re-show regardless of version.
        s.metering_consent = false;
        assert!(needs_consent(&s));
    }

    #[test]
    fn accept_workloads_defaults_off() {
        let mut s = valid();
        assert!(!s.accept_workloads);
        s.accept_workloads = true;
        s.save(&std::env::temp_dir().join("vtessera_gui_accept_workloads_test.toml"))
            .ok();
        let loaded = Settings::load_or_default(
            &std::env::temp_dir().join("vtessera_gui_accept_workloads_test.toml"),
        );
        assert!(loaded.accept_workloads);
        let _ = std::fs::remove_file(
            std::env::temp_dir().join("vtessera_gui_accept_workloads_test.toml"),
        );
    }
}
