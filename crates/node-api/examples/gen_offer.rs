//! Tiny helper that signs a Vtessera offer and prints the JSON to
//! stdout. Used by examples + integration scripts so they don't have to
//! hand-build canonical bytes.
//!
//!   cargo run -p vtessera-node-api --example gen_offer -- <free|paid>

use ed25519_dalek::SigningKey;
use vtessera_offer::{
    derive_node_id, sign, to_json, AdvertisedDevice, Currency, OfferBody, PriceQuote,
    OFFER_SCHEMA_VER,
};

fn main() {
    let mode = std::env::args().nth(1).unwrap_or_else(|| "free".into());
    // Deterministic key for reproducible examples — never use in production.
    let key = SigningKey::from_bytes(&[42u8; 32]);
    let node_id = derive_node_id(&key.verifying_key().to_bytes());

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
        endpoint: "http://127.0.0.1:8402".into(),
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
