//! Derive the daemon-facing `vtessera.toml` from the GUI settings.
//!
//! The daemon config uses `deny_unknown_fields`, so the GUI must write only
//! the exact fields `vtesserad::config::Config` understands (config.rs) —
//! GUI-only settings live in `settings.toml` instead.

use serde::Serialize;
use std::path::Path;

use crate::settings::Settings;

/// Exact field set of `vtesserad`'s `Config` (see vtesserad/src/config.rs).
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct DaemonToml {
    sample_interval_secs: u64,
    state_dir: String,
    key_path: String,
    payout_id: String,
    mode: String,
    currency: String,
    price_per_cpu_hour: f64,
    submit_endpoint: Option<String>,
    window_size: Option<u64>,
    max_spool_files: Option<usize>,
    resource_caps: Caps,
}

#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct Caps {
    max_cpus: Option<u64>,
    max_mem_kb: Option<u64>,
    max_disk_kb: Option<u64>,
}

/// Write `vtessera.toml` at `path` for the given settings + paths.
pub fn write_daemon_config(
    settings: &Settings,
    config_path: &Path,
    key_path: &Path,
    state_dir: &Path,
) -> Result<(), String> {
    let doc = DaemonToml {
        sample_interval_secs: settings.sample_interval_secs,
        state_dir: state_dir.to_string_lossy().into_owned(),
        key_path: key_path.to_string_lossy().into_owned(),
        payout_id: settings.payout_id.trim().to_string(),
        mode: settings.mode.clone(),
        currency: settings.currency.clone(),
        price_per_cpu_hour: settings.price_per_cpu_hour,
        submit_endpoint: None,
        window_size: None,
        max_spool_files: Some(1000),
        resource_caps: Caps {
            max_cpus: None,
            max_mem_kb: None,
            max_disk_kb: None,
        },
    };
    let raw = toml::to_string(&doc).map_err(|e| format!("serialize daemon config: {e}"))?;
    if let Some(parent) = config_path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    std::fs::write(config_path, raw).map_err(|e| format!("write {}: {e}", config_path.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn settings() -> Settings {
        Settings {
            mode: "paid".into(),
            currency: "eurc".into(),
            price_per_cpu_hour: 0.05,
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
            port: 8402,
            endpoint: "http://127.0.0.1:8402".into(),
            escrow_account: crate::settings::DEFAULT_ESCROW.into(),
            network: "solana-devnet".into(),
            sample_interval_secs: 60,
            backend: crate::settings::DEFAULT_BACKEND.into(),
            metering_consent: true,
            accept_workloads: false,
            consent_version: crate::settings::CURRENT_CONSENT_VERSION,
            marketplace_url: String::new(),
        }
    }

    #[test]
    fn written_config_roundtrips_through_daemon_rules() {
        // We can't depend on the vtesserad binary crate here, but the written
        // TOML must parse into exactly the daemon's field set. Re-assert the
        // contract by checking the keys we emit. Optional fields (`None`) are
        // omitted by the toml serializer — the daemon defaults those.
        let dir = std::env::temp_dir().join("vtessera_gui_daemon_cfg");
        let _ = std::fs::remove_dir_all(&dir);
        let path = dir.join("vtessera.toml");
        write_daemon_config(
            &settings(),
            &path,
            &dir.join("identity.key"),
            &dir.join("state"),
        )
        .expect("write");
        let raw = std::fs::read_to_string(&path).unwrap();
        for key in [
            "sample_interval_secs",
            "state_dir",
            "key_path",
            "payout_id",
            "mode",
            "currency",
            "price_per_cpu_hour",
            "max_spool_files",
            "resource_caps",
        ] {
            assert!(raw.contains(key), "missing key {key}:\n{raw}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }
}
