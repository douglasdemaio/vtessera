//! Signed-offer building for the GUI.
//!
//! Reuses the `vtessera-offer` crate so the wire format and canonical-byte
//! signing stay identical to the rest of the protocol. The seller's device
//! is advertised as CPU-class using the number of host cores and total host
//! memory (read from `/proc`), mirroring the daemon's `metrics` module's
//! approach — no extra dependencies.

use std::fs;
use std::path::Path;

use ed25519_dalek::SigningKey;
use vtessera_offer::{
    derive_node_id, sign, to_json, AdvertisedDevice, Currency, OfferBody, PriceQuote,
    OFFER_SCHEMA_VER,
};

use crate::settings::Settings;

const OFFER_TTL_SECS: u64 = 30 * 24 * 3600; // 30 days

/// Load (or generate, mode 0600) the Ed25519 identity key at `key_path`.
/// Same format `vtesserad` uses: the raw 32-byte secret key.
pub fn load_or_generate_key(key_path: &Path) -> Result<SigningKey, String> {
    if key_path.exists() {
        let raw = fs::read(key_path).map_err(|e| format!("read {}: {e}", key_path.display()))?;
        if raw.len() != ed25519_dalek::SECRET_KEY_LENGTH {
            return Err(format!(
                "identity key {} has wrong length: expected {}, got {}",
                key_path.display(),
                ed25519_dalek::SECRET_KEY_LENGTH,
                raw.len()
            ));
        }
        let mut arr = [0u8; ed25519_dalek::SECRET_KEY_LENGTH];
        arr.copy_from_slice(&raw);
        return Ok(SigningKey::from_bytes(&arr));
    }

    let key = SigningKey::generate(&mut rand::rngs::OsRng);
    if let Some(parent) = key_path.parent() {
        fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    fs::write(key_path, key.to_bytes())
        .map_err(|e| format!("write {}: {e}", key_path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(key_path, fs::Permissions::from_mode(0o600))
            .map_err(|e| format!("chmod {}: {e}", key_path.display()))?;
    }
    Ok(key)
}

/// Read the number of logical CPUs from `/proc/cpuinfo` (0 if unavailable).
pub fn host_vcpus() -> u32 {
    let Ok(raw) = fs::read_to_string("/proc/cpuinfo") else {
        return 0;
    };
    raw.lines().filter(|l| l.starts_with("processor")).count() as u32
}

/// Read total host memory in MB from `/proc/meminfo` (0 if unavailable).
pub fn host_mem_mb() -> u32 {
    let Ok(raw) = fs::read_to_string("/proc/meminfo") else {
        return 0;
    };
    for line in raw.lines() {
        if let Some(rest) = line.strip_prefix("MemTotal:") {
            let kb: u64 = rest
                .split_whitespace()
                .next()
                .and_then(|n| n.parse().ok())
                .unwrap_or(0);
            return (kb / 1024) as u32;
        }
    }
    0
}

/// Convert a human "price per CPU-hour" (e.g. `0.05` EURC) into the offer's
/// `per_device_second_micros` unit (micro-units of currency per second).
pub fn price_per_cpu_second_micros(price_per_cpu_hour: f64) -> u64 {
    let micros_per_hour = (price_per_cpu_hour * 1_000_000.0).round();
    (micros_per_hour / 3600.0).round() as u64
}

/// Build and sign an offer JSON string for the current settings.
pub fn build_offer_json(settings: &Settings, key: &SigningKey) -> String {
    let node_id = derive_node_id(&key.verifying_key().to_bytes());

    let currency = match settings.currency.as_str() {
        "usdc" => Currency::Usdc,
        _ => Currency::Eurc,
    };

    let price = if settings.is_free() {
        PriceQuote::Free
    } else {
        PriceQuote::Paid {
            currency,
            per_device_second_micros: price_per_cpu_second_micros(settings.price_per_cpu_hour),
            payout_id: settings.payout_id.clone(),
        }
    };

    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();

    let body = OfferBody {
        schema_ver: OFFER_SCHEMA_VER,
        node_id,
        endpoint: settings.endpoint.clone(),
        device: AdvertisedDevice::Cpu {
            vcpus: host_vcpus(),
            mem_mb: host_mem_mb(),
        },
        price,
        issued_unix: now,
        expires_unix: now + OFFER_TTL_SECS,
    };

    let signed = sign(body, key);
    to_json(&signed)
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
    fn price_conversion_is_sane() {
        // 0.05 / hour = 50_000 micros / hour ≈ 13.89 micros / sec.
        let micros = price_per_cpu_second_micros(0.05);
        assert_eq!(micros, 14);
        // 0.36 / hour = 360_000 / 3600 = 100 micros / sec exactly.
        assert_eq!(price_per_cpu_second_micros(0.36), 100);
        assert_eq!(price_per_cpu_second_micros(0.0), 0);
    }

    #[test]
    fn offer_roundtrips_paid() {
        let dir = std::env::temp_dir().join("vtessera_gui_offer_paid");
        let _ = std::fs::remove_dir_all(&dir);
        let key = load_or_generate_key(&dir.join("identity.key")).expect("key");
        let json = build_offer_json(&settings(), &key);
        assert!(json.contains("\"mode\":\"paid\""));
        assert!(json.contains("\"payout_id\":"));
        assert!(json.contains("\"per_device_second_micros\":14"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn offer_roundtrips_free() {
        let dir = std::env::temp_dir().join("vtessera_gui_offer_free");
        let _ = std::fs::remove_dir_all(&dir);
        let key = load_or_generate_key(&dir.join("identity.key")).expect("key");
        let mut s = settings();
        s.mode = "free".into();
        let json = build_offer_json(&s, &key);
        assert!(json.contains("\"mode\":\"free\""));
        assert!(!json.contains("\"payout_id\""));
        let _ = std::fs::remove_dir_all(&dir);
    }
}
