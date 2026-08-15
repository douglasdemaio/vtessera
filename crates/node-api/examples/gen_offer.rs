//! Tiny helper that signs a Vtessera offer and prints the JSON to
//! stdout. Used by examples + integration scripts so they don't have to
//! hand-build canonical bytes.
//!
//!   cargo run -p vtessera-node-api --example gen_offer -- <free|paid>
//!   cargo run -p vtessera-node-api --example gen_offer -- free --key-out key.bin
//!   cargo run -p vtessera-node-api --example gen_offer -- free \
//!     --seed 1 --endpoint http://127.0.0.1:8402 --key-out key.bin

use ed25519_dalek::SigningKey;
use vtessera_offer::{
    derive_node_id, sign, to_json, AdvertisedDevice, Currency, OfferBody, PriceQuote,
    OFFER_SCHEMA_VER,
};

fn main() {
    let mut mode = None;
    let mut key_out: Option<std::path::PathBuf> = None;
    let mut seed: u8 = 42;
    let mut endpoint = "http://127.0.0.1:8402".to_string();
    let mut it = std::env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--key-out" => {
                if let Some(p) = it.next() {
                    key_out = Some(p.into());
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
            other if !other.starts_with("--") && mode.is_none() => mode = Some(other.to_string()),
            other => {
                eprintln!("unexpected argument: {other}");
                std::process::exit(2);
            }
        }
    }
    let mode = mode.unwrap_or_else(|| "free".into());
    // Deterministic key for reproducible examples (--seed derives distinct
    // node identities for multi-node demos) — never use in production.
    let key = SigningKey::from_bytes(&[seed; 32]);
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
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
        },
        other => {
            eprintln!("expected 'free' or 'paid', got {other}");
            std::process::exit(2);
        }
    };

    let body = OfferBody {
        schema_ver: OFFER_SCHEMA_VER,
        node_id,
        endpoint,
        device: AdvertisedDevice::Cpu {
            vcpus: 4,
            mem_mb: 16 * 1024,
        },
        price,
        issued_unix: 1_700_000_000,
        expires_unix: 2_000_000_000,
    };
    let signed = sign(body, &key);
    print!("{}", to_json(&signed));
}
