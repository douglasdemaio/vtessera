//! Tiny helper that signs a Vtessera offer and prints the JSON to
//! stdout. Used by examples + integration scripts so they don't have to
//! hand-build canonical bytes.
//!
//!   cargo run -p vtessera-node-api --example gen_offer -- <free|paid>
//!   cargo run -p vtessera-node-api --example gen_offer -- free --key-out key.bin
//!   cargo run -p vtessera-node-api --example gen_offer -- free \
//!     --seed 1 --endpoint http://127.0.0.1:8402 --key-out key.bin
//!   cargo run -p vtessera-node-api --example gen_offer -- free \
//!     --key /path/to/identity.key --endpoint http://192.168.1.5:8402

use ed25519_dalek::SigningKey;
use std::time::{SystemTime, UNIX_EPOCH};
use vtessera_offer::{
    derive_node_id, sign, to_json, AdvertisedDevice, Currency, OfferBody, PriceQuote,
    OFFER_SCHEMA_VER,
};

fn main() {
    let mut mode = None;
    let mut key_out: Option<std::path::PathBuf> = None;
    let mut key_in: Option<std::path::PathBuf> = None;
    let mut seed: u8 = 42;
    let mut endpoint = "http://127.0.0.1:8402".to_string();
    let mut vcpus: u32 = 4;
    let mut mem_mb: u32 = 16 * 1024;
    let mut payout_id = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".to_string();
    let mut it = std::env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--key-out" => {
                if let Some(p) = it.next() {
                    key_out = Some(p.into());
                }
            }
            "--key" => {
                if let Some(p) = it.next() {
                    key_in = Some(p.into());
                }
            }
            "--seed" => {
                if let Some(s) = it.next() {
                    seed = s.parse().unwrap_or(42);
                }
            }
            "--endpoint" => {
                if let Some(e) = it.next() {
                    endpoint = e;
                }
            }
            "--vcpus" => {
                if let Some(v) = it.next() {
                    vcpus = v.parse().unwrap_or(4);
                }
            }
            "--mem-mb" => {
                if let Some(m) = it.next() {
                    mem_mb = m.parse().unwrap_or(16384);
                }
            }
            "--payout" => {
                if let Some(p) = it.next() {
                    payout_id = p;
                }
            }
            other if !other.starts_with("--") && mode.is_none() => mode = Some(other.to_string()),
            other => {
                eprintln!("unexpected argument: {other}");
                std::process::exit(2);
            }
        }
    }
    let mode = mode.unwrap_or_else(|| "free".into());

    // Load or generate the signing key.
    let key = if let Some(path) = &key_in {
        let raw = std::fs::read(path).unwrap_or_else(|e| {
            eprintln!("failed to read key {}: {e}", path.display());
            std::process::exit(1);
        });
        if raw.len() != 32 {
            eprintln!(
                "key {} has wrong length: expected 32, got {}",
                path.display(),
                raw.len()
            );
            std::process::exit(1);
        }
        let mut arr = [0u8; 32];
        arr.copy_from_slice(&raw);
        SigningKey::from_bytes(&arr)
    } else {
        // Deterministic key for reproducible examples (--seed derives distinct
        // node identities for multi-node demos) — never use in production.
        SigningKey::from_bytes(&[seed; 32])
    };

    let node_id = derive_node_id(&key.verifying_key().to_bytes());

    if let Some(path) = key_out {
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        // 0600: vtessera-node refuses identity keys with group/world bits.
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .truncate(true)
            .write(true)
            .mode(0o600)
            .open(&path)
            .unwrap_or_else(|e| {
                eprintln!("failed to create key {}: {e}", path.display());
                std::process::exit(1);
            });
        f.write_all(&key.to_bytes()).unwrap_or_else(|e| {
            eprintln!("failed to write key {}: {e}", path.display());
            std::process::exit(1);
        });
    }

    let price = match mode.as_str() {
        "free" => PriceQuote::Free,
        "paid" => PriceQuote::Paid {
            currency: Currency::Usdc,
            per_device_second_micros: 100,
            payout_id,
        },
        other => {
            eprintln!("expected 'free' or 'paid', got {other}");
            std::process::exit(2);
        }
    };

    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();

    let body = OfferBody {
        schema_ver: OFFER_SCHEMA_VER,
        node_id,
        endpoint,
        device: AdvertisedDevice::Cpu { vcpus, mem_mb },
        price,
        issued_unix: now,
        expires_unix: now + 30 * 24 * 3600,
    };
    let signed = sign(body, &key);
    print!("{}", to_json(&signed));
}
