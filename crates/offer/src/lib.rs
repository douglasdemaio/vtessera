//! Vtessera offer — Module 2a (ROADMAP.md §2a).
//!
//! Each seller node publishes a **signed, machine-readable offer**
//! describing what its box sells: device class and specs (CPU/GPU model,
//! VRAM, MIG profile), availability, endpoint, price (in EURC or USDC) **or
//! `free`**, and — if paid — the seller's wallet. Offers are signed with
//! the v0 node identity (Ed25519, the same key behind `vtesserad`'s
//! receipts) so they can't be spoofed.
//!
//! Offers are intended to be served two ways:
//!
//! 1. As MCP (Model Context Protocol) resources — agents already speak MCP,
//!    so a Vtessera node appears in their tool/resource catalog.
//! 2. As plain JSON at a well-known endpoint, so a central offer index or
//!    a curious developer can fetch them with `curl`.
//!
//! Both shapes share the same canonical bytes (defined in [`canonical_bytes`])
//! — the signature is over those bytes, the JSON envelope just contains the
//! signature alongside.

#![forbid(unsafe_code)]

use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// Schema version for the offer wire format. Increment on any change to
/// [`canonical_bytes`]'s layout.
///
/// v1: `endpoint` was a single string.
/// v2: `endpoint` became a list (`Vec<String>`); `endpoint_id` carries the full
///     Ed25519 pubkey (the iroh `EndpointId`) instead of only the truncated
///     `node_id`. Consumers reject any schema_ver other than this.
pub const OFFER_SCHEMA_VER: u16 = 2;

/// Currencies a paid offer may quote in. Buyers pay in stablecoin and the
/// seller is paid out in that same stablecoin — whichever the node puts on
/// the offer. EURC is the default — ECB-anchored price stability matches
/// the European seller base most likely to value it.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Currency {
    Eurc,
    Usdc,
}

impl Currency {
    fn as_byte(self) -> u8 {
        match self {
            Currency::Eurc => 1,
            Currency::Usdc => 2,
        }
    }
}

/// The capability part of an offer. Mirrors the executor's `DeviceClass`
/// at the wire level — we duplicate the shape (rather than depending on
/// `vtessera-executor`) so the offer crate can be consumed without
/// pulling in the privileged executor surface.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum AdvertisedDevice {
    Cpu {
        vcpus: u32,
        mem_mb: u32,
    },
    NvidiaGpu {
        model: String,
        vram_mb: u32,
    },
    NvidiaMig {
        parent_model: String,
        profile: String,
        vram_mb: u32,
    },
    AmdGpu {
        model: String,
        vram_mb: u32,
    },
    /// NVIDIA vGPU instance (software-partitioned, licensed).
    NvidiaVgpu {
        parent_model: String,
        profile: String,
        vram_mb: u32,
    },
}

impl AdvertisedDevice {
    fn canonical_into(&self, buf: &mut Vec<u8>) {
        // Tag bytes are stable; do not renumber on additions, always append.
        match self {
            AdvertisedDevice::Cpu { vcpus, mem_mb } => {
                buf.push(1);
                buf.extend_from_slice(&vcpus.to_le_bytes());
                buf.extend_from_slice(&mem_mb.to_le_bytes());
            }
            AdvertisedDevice::NvidiaGpu { model, vram_mb } => {
                buf.push(2);
                push_str(buf, model);
                buf.extend_from_slice(&vram_mb.to_le_bytes());
            }
            AdvertisedDevice::NvidiaMig {
                parent_model,
                profile,
                vram_mb,
            } => {
                buf.push(3);
                push_str(buf, parent_model);
                push_str(buf, profile);
                buf.extend_from_slice(&vram_mb.to_le_bytes());
            }
            AdvertisedDevice::AmdGpu { model, vram_mb } => {
                buf.push(4);
                push_str(buf, model);
                buf.extend_from_slice(&vram_mb.to_le_bytes());
            }
            AdvertisedDevice::NvidiaVgpu {
                parent_model,
                profile,
                vram_mb,
            } => {
                buf.push(5);
                push_str(buf, parent_model);
                push_str(buf, profile);
                buf.extend_from_slice(&vram_mb.to_le_bytes());
            }
        }
    }
}

/// The price half of an offer. A node either charges in stablecoin or
/// donates compute. Donating skips every on-chain step (ROADMAP.md §2b).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(tag = "mode", rename_all = "snake_case")]
pub enum PriceQuote {
    /// `unit_micros` is the price per device-second in micro-units of the
    /// chosen currency (1 EURC = 1_000_000 micros). Settlement is denominated
    /// in micros end-to-end so floating-point doesn't leak into payment.
    Paid {
        currency: Currency,
        per_device_second_micros: u64,
        /// Seller wallet whose stablecoin ATA (in the offer's currency)
        /// receives the earned slice at release. The escrow pays directly;
        /// no swap, no conversion.
        payout_id: String,
    },
    /// The seller donates the compute. No 402, no escrow, no fee, no swap.
    Free,
}

impl PriceQuote {
    fn canonical_into(&self, buf: &mut Vec<u8>) {
        match self {
            PriceQuote::Paid {
                currency,
                per_device_second_micros,
                payout_id,
            } => {
                buf.push(1);
                buf.push(currency.as_byte());
                buf.extend_from_slice(&per_device_second_micros.to_le_bytes());
                push_str(buf, payout_id);
            }
            PriceQuote::Free => {
                buf.push(0);
            }
        }
    }
}

/// The signed body of an offer. Everything a buyer needs to decide
/// whether to engage with this node.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct OfferBody {
    pub schema_ver: u16,
    /// Stable node identity (`vtesserad`'s `node_id`). Buyers cross-reference
    /// the receipts they later get against this; it is the truncated
    /// `SHA-256(pubkey)[..16]` form.
    pub node_id: String,
    /// Full Ed25519 public key (64 hex chars, the iroh `EndpointId`). This is
    /// what a dialer needs to reach the node by identity; the truncated
    /// `node_id` alone is insufficient.
    pub endpoint_id: String,
    /// Endpoint(s) where the buyer connects. Ordered list: direct IP:port
    /// first, then queue/coordinator URLs and relayed endpoints. Free offers
    /// serve directly; paid offers return 402 until paid (Module 2b). Empty is
    /// acceptable only when the only reachability key is `endpoint_id`.
    pub endpoint: Vec<String>,
    /// What's on offer.
    pub device: AdvertisedDevice,
    /// Price or `free`.
    pub price: PriceQuote,
    /// UNIX epoch seconds the offer was issued. Stale offers can be ignored
    /// by the index without re-fetching the node.
    pub issued_unix: u64,
    /// UNIX epoch seconds the offer is no longer valid.
    pub expires_unix: u64,
}

/// A signed offer ready to be served over HTTP or shipped to an index.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SignedOffer {
    pub body: OfferBody,
    /// Hex-encoded Ed25519 public key (32 bytes / 64 hex chars). Same key
    /// that signs v0 receipts.
    pub pubkey_hex: String,
    /// Hex-encoded Ed25519 signature over [`canonical_bytes`].
    pub sig_hex: String,
}

/// Canonical signing bytes for an offer.
///
/// Length-prefix every variable-length field so concatenation can't be
/// confused (`"ab"+"cdef"` versus `"abc"+"def"`). Little-endian everywhere,
/// matching v0's receipt layout. This layout is frozen per [`OFFER_SCHEMA_VER`]
/// and must not change without bumping it.
pub fn canonical_bytes(body: &OfferBody) -> Vec<u8> {
    let mut buf = Vec::with_capacity(256);
    buf.extend_from_slice(&body.schema_ver.to_le_bytes());
    push_str(&mut buf, &body.node_id);
    push_str(&mut buf, &body.endpoint_id);
    buf.push(u8::try_from(body.endpoint.len()).unwrap_or(u8::MAX));
    for ep in &body.endpoint {
        push_str(&mut buf, ep);
    }
    body.device.canonical_into(&mut buf);
    body.price.canonical_into(&mut buf);
    buf.extend_from_slice(&body.issued_unix.to_le_bytes());
    buf.extend_from_slice(&body.expires_unix.to_le_bytes());
    buf
}

fn push_str(buf: &mut Vec<u8>, s: &str) {
    let bytes = s.as_bytes();
    debug_assert!(
        bytes.len() <= u16::MAX as usize,
        "string too long for u16 prefix"
    );
    buf.extend_from_slice(&(bytes.len() as u16).to_le_bytes());
    buf.extend_from_slice(bytes);
}

/// Derive the v0 `node_id` from an Ed25519 public key. Mirrors
/// `vtesserad::receipt::derive_node_id` — duplicated to avoid pulling the
/// daemon binary in as a library dep.
pub fn derive_node_id(pubkey: &[u8; 32]) -> String {
    let mut h = Sha256::new();
    h.update(pubkey);
    let digest = h.finalize();
    hex::encode(&digest[..16])
}

/// Sign an offer body with the seller's identity key.
pub fn sign(body: OfferBody, key: &SigningKey) -> SignedOffer {
    let bytes = canonical_bytes(&body);
    let sig: Signature = key.sign(&bytes);
    SignedOffer {
        body,
        pubkey_hex: hex::encode(key.verifying_key().to_bytes()),
        sig_hex: hex::encode(sig.to_bytes()),
    }
}

/// Verification errors with enough detail to debug an offer-index reject.
#[derive(Debug)]
pub enum VerifyError {
    BadPubkeyHex,
    BadPubkey,
    BadSigHex,
    BadSig,
    /// Signature didn't verify against the body's canonical bytes.
    SignatureMismatch,
    /// `node_id` in the body doesn't match `derive_node_id(pubkey)`.
    NodeIdMismatch,
    /// `endpoint_id` in the body doesn't match the pubkey that signed.
    EndpointIdMismatch,
    /// Offer's `expires_unix` is in the past relative to a caller-supplied
    /// clock. Callers do this check separately because the offer crate has
    /// no clock of its own.
    Expired,
    /// Schema version not understood.
    UnsupportedSchema,
}

impl std::fmt::Display for VerifyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            VerifyError::BadPubkeyHex => write!(f, "pubkey is not 64 hex chars"),
            VerifyError::BadPubkey => write!(f, "pubkey is not a valid Ed25519 key"),
            VerifyError::BadSigHex => write!(f, "sig is not 128 hex chars"),
            VerifyError::BadSig => write!(f, "sig is not a valid Ed25519 signature"),
            VerifyError::SignatureMismatch => write!(f, "signature does not verify"),
            VerifyError::NodeIdMismatch => write!(f, "node_id does not match pubkey"),
            VerifyError::EndpointIdMismatch => write!(f, "endpoint_id does not match pubkey"),
            VerifyError::Expired => write!(f, "offer expired"),
            VerifyError::UnsupportedSchema => write!(f, "offer schema_ver not supported"),
        }
    }
}

impl std::error::Error for VerifyError {}

/// Verify a signed offer. Optionally enforce expiry against a caller-
/// supplied UNIX epoch second.
pub fn verify(offer: &SignedOffer, now_unix: Option<u64>) -> Result<(), VerifyError> {
    if offer.body.schema_ver != OFFER_SCHEMA_VER {
        return Err(VerifyError::UnsupportedSchema);
    }

    let pk_bytes = decode_fixed_hex::<32>(&offer.pubkey_hex).ok_or(VerifyError::BadPubkeyHex)?;
    let vk = VerifyingKey::from_bytes(&pk_bytes).map_err(|_| VerifyError::BadPubkey)?;

    if derive_node_id(&pk_bytes) != offer.body.node_id {
        return Err(VerifyError::NodeIdMismatch);
    }

    // The advertised `endpoint_id` must be the same full pubkey that signed.
    let ep_id_bytes =
        decode_fixed_hex::<32>(&offer.body.endpoint_id).ok_or(VerifyError::BadPubkeyHex)?;
    if ep_id_bytes != pk_bytes {
        return Err(VerifyError::EndpointIdMismatch);
    }

    let sig_bytes = decode_fixed_hex::<64>(&offer.sig_hex).ok_or(VerifyError::BadSigHex)?;
    let sig = Signature::from_bytes(&sig_bytes);

    vk.verify(&canonical_bytes(&offer.body), &sig)
        .map_err(|_| VerifyError::SignatureMismatch)?;

    if let Some(now) = now_unix {
        if now > offer.body.expires_unix {
            return Err(VerifyError::Expired);
        }
    }
    Ok(())
}

fn decode_fixed_hex<const N: usize>(s: &str) -> Option<[u8; N]> {
    if s.len() != N * 2 {
        return None;
    }
    let mut out = [0u8; N];
    hex::decode_to_slice(s, &mut out).ok()?;
    Some(out)
}

/// Message covered by a signed heartbeat, per `docs/INTERNET-CONNECTIVITY.md`
/// §2.2: `X-Signature: <ed25519-sign("heartbeat:{node_id}:{timestamp}")>`.
pub fn heartbeat_message(node_id: &str, timestamp: u64) -> Vec<u8> {
    format!("heartbeat:{node_id}:{timestamp}").into_bytes()
}

/// Sign a heartbeat for the node's identity key. Returns the hex-encoded
/// Ed25519 signature over [`heartbeat_message`].
pub fn sign_heartbeat(node_id: &str, timestamp: u64, key: &SigningKey) -> String {
    let sig: Signature = key.sign(&heartbeat_message(node_id, timestamp));
    hex::encode(sig.to_bytes())
}

/// Verify a heartbeat signature against the node's pubkey (the one bound to
/// the registered offer). Lets an offer-index reject forged heartbeats.
pub fn verify_heartbeat(
    node_id: &str,
    timestamp: u64,
    sig_hex: &str,
    pubkey_hex: &str,
) -> Result<(), VerifyError> {
    let pk_bytes = decode_fixed_hex::<32>(pubkey_hex).ok_or(VerifyError::BadPubkeyHex)?;
    let vk = VerifyingKey::from_bytes(&pk_bytes).map_err(|_| VerifyError::BadPubkey)?;
    let sig_bytes = decode_fixed_hex::<64>(sig_hex).ok_or(VerifyError::BadSigHex)?;
    let sig = Signature::from_bytes(&sig_bytes);
    vk.verify(&heartbeat_message(node_id, timestamp), &sig)
        .map_err(|_| VerifyError::SignatureMismatch)
}

/// Render a signed offer to JSON (manual writer, no serde_json dep).
///
/// Field order is fixed so two implementations of the spec produce
/// byte-identical JSON for a given offer.
pub fn to_json(offer: &SignedOffer) -> String {
    let mut s = String::with_capacity(512);
    s.push('{');
    s.push_str("\"body\":");
    body_to_json(&offer.body, &mut s);
    s.push_str(",\"pubkey_hex\":");
    json_string(&offer.pubkey_hex, &mut s);
    s.push_str(",\"sig_hex\":");
    json_string(&offer.sig_hex, &mut s);
    s.push('}');
    s
}

fn body_to_json(b: &OfferBody, s: &mut String) {
    use std::fmt::Write;
    s.push('{');
    write!(s, "\"schema_ver\":{}", b.schema_ver).unwrap();
    s.push_str(",\"node_id\":");
    json_string(&b.node_id, s);
    s.push_str(",\"endpoint_id\":");
    json_string(&b.endpoint_id, s);
    s.push_str(",\"endpoint\":[");
    for (i, ep) in b.endpoint.iter().enumerate() {
        if i > 0 {
            s.push(',');
        }
        json_string(ep, s);
    }
    s.push(']');
    s.push_str(",\"device\":");
    device_to_json(&b.device, s);
    s.push_str(",\"price\":");
    price_to_json(&b.price, s);
    write!(s, ",\"issued_unix\":{}", b.issued_unix).unwrap();
    write!(s, ",\"expires_unix\":{}", b.expires_unix).unwrap();
    s.push('}');
}

fn device_to_json(d: &AdvertisedDevice, s: &mut String) {
    use std::fmt::Write;
    s.push('{');
    match d {
        AdvertisedDevice::Cpu { vcpus, mem_mb } => {
            write!(s, "\"kind\":\"cpu\",\"vcpus\":{vcpus},\"mem_mb\":{mem_mb}").unwrap();
        }
        AdvertisedDevice::NvidiaGpu { model, vram_mb } => {
            s.push_str("\"kind\":\"nvidia_gpu\",\"model\":");
            json_string(model, s);
            write!(s, ",\"vram_mb\":{vram_mb}").unwrap();
        }
        AdvertisedDevice::NvidiaMig {
            parent_model,
            profile,
            vram_mb,
        } => {
            s.push_str("\"kind\":\"nvidia_mig\",\"parent_model\":");
            json_string(parent_model, s);
            s.push_str(",\"profile\":");
            json_string(profile, s);
            write!(s, ",\"vram_mb\":{vram_mb}").unwrap();
        }
        AdvertisedDevice::AmdGpu { model, vram_mb } => {
            s.push_str("\"kind\":\"amd_gpu\",\"model\":");
            json_string(model, s);
            write!(s, ",\"vram_mb\":{vram_mb}").unwrap();
        }
        AdvertisedDevice::NvidiaVgpu {
            parent_model,
            profile,
            vram_mb,
        } => {
            s.push_str("\"kind\":\"nvidia_vgpu\",\"parent_model\":");
            json_string(parent_model, s);
            s.push_str(",\"profile\":");
            json_string(profile, s);
            write!(s, ",\"vram_mb\":{vram_mb}").unwrap();
        }
    }
    s.push('}');
}

fn price_to_json(p: &PriceQuote, s: &mut String) {
    use std::fmt::Write;
    s.push('{');
    match p {
        PriceQuote::Paid {
            currency,
            per_device_second_micros,
            payout_id,
        } => {
            let cur = match currency {
                Currency::Eurc => "eurc",
                Currency::Usdc => "usdc",
            };
            write!(s, "\"mode\":\"paid\",\"currency\":\"{cur}\"").unwrap();
            write!(
                s,
                ",\"per_device_second_micros\":{per_device_second_micros}"
            )
            .unwrap();
            s.push_str(",\"payout_id\":");
            json_string(payout_id, s);
        }
        PriceQuote::Free => {
            s.push_str("\"mode\":\"free\"");
        }
    }
    s.push('}');
}

fn json_string(value: &str, s: &mut String) {
    s.push('"');
    for c in value.chars() {
        match c {
            '"' => s.push_str("\\\""),
            '\\' => s.push_str("\\\\"),
            '\n' => s.push_str("\\n"),
            '\r' => s.push_str("\\r"),
            '\t' => s.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                use std::fmt::Write;
                write!(s, "\\u{:04x}", c as u32).unwrap();
            }
            c => s.push(c),
        }
    }
    s.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::SigningKey;
    use rand::rngs::OsRng;

    fn sample_body_for_key(key: &SigningKey) -> OfferBody {
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let endpoint_id = hex::encode(key.verifying_key().to_bytes());
        OfferBody {
            schema_ver: OFFER_SCHEMA_VER,
            node_id,
            endpoint_id,
            endpoint: vec!["https://node-1.example/vtessera".into()],
            device: AdvertisedDevice::NvidiaGpu {
                model: "H100-80GB".into(),
                vram_mb: 80 * 1024,
            },
            price: PriceQuote::Paid {
                currency: Currency::Eurc,
                per_device_second_micros: 250,
                payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
            },
            issued_unix: 1_700_000_000,
            expires_unix: 1_700_010_000,
        }
    }

    #[test]
    fn sign_then_verify_roundtrips() {
        let key = SigningKey::generate(&mut OsRng);
        let signed = sign(sample_body_for_key(&key), &key);
        verify(&signed, None).expect("signature should verify");
    }

    #[test]
    fn verify_rejects_tamper() {
        let key = SigningKey::generate(&mut OsRng);
        let mut signed = sign(sample_body_for_key(&key), &key);
        signed.body.endpoint = vec!["https://imposter.example".into()];
        assert!(matches!(
            verify(&signed, None),
            Err(VerifyError::SignatureMismatch)
        ));
    }

    #[test]
    fn verify_rejects_node_id_lie() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        // Tamper with node_id so it no longer matches derive_node_id(pubkey).
        body.node_id = "0".repeat(32);
        let signed = sign(body, &key);
        assert!(matches!(
            verify(&signed, None),
            Err(VerifyError::NodeIdMismatch)
        ));
    }

    #[test]
    fn verify_rejects_endpoint_id_mismatch() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        // Set a valid 64-hex endpoint_id that does NOT match the signing key.
        let other_key = SigningKey::generate(&mut OsRng);
        body.endpoint_id = hex::encode(other_key.verifying_key().to_bytes());
        let signed = sign(body, &key);
        assert!(matches!(
            verify(&signed, None),
            Err(VerifyError::EndpointIdMismatch)
        ));
    }

    #[test]
    fn verify_rejects_expired_when_clock_supplied() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        body.expires_unix = 1_000;
        let signed = sign(body, &key);
        assert!(matches!(
            verify(&signed, Some(2_000)),
            Err(VerifyError::Expired)
        ));
    }

    #[test]
    fn free_offer_signs_and_verifies() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        body.price = PriceQuote::Free;
        let signed = sign(body, &key);
        verify(&signed, None).expect("free offer should verify");
        let json = to_json(&signed);
        assert!(json.contains("\"mode\":\"free\""));
        assert!(!json.contains("payout_id"));
    }

    #[test]
    fn canonical_bytes_distinguishes_paid_from_free() {
        let key = SigningKey::generate(&mut OsRng);
        let paid = sample_body_for_key(&key);
        let mut free = paid.clone();
        free.price = PriceQuote::Free;
        assert_ne!(canonical_bytes(&paid), canonical_bytes(&free));
    }

    #[test]
    fn to_json_uses_stable_field_order() {
        let key = SigningKey::generate(&mut OsRng);
        let signed = sign(sample_body_for_key(&key), &key);
        let j = to_json(&signed);
        let body_idx = j.find("\"body\":").unwrap();
        let pk_idx = j.find("\"pubkey_hex\":").unwrap();
        let sig_idx = j.find("\"sig_hex\":").unwrap();
        assert!(body_idx < pk_idx && pk_idx < sig_idx);
        // Inside body: schema_ver before node_id, endpoint_id before endpoint list.
        let body_section = &j[body_idx..];
        let schema_idx = body_section.find("\"schema_ver\":").unwrap();
        let node_idx = body_section.find("\"node_id\":").unwrap();
        let ep_id_idx = body_section.find("\"endpoint_id\":").unwrap();
        let ep_idx = body_section.find("\"endpoint\":").unwrap();
        assert!(schema_idx < node_idx && node_idx < ep_id_idx && ep_id_idx < ep_idx);
    }

    #[test]
    fn endpoint_list_json_is_array() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        body.endpoint = vec![
            "http://1.2.3.4:8402".into(),
            "wss://relay.example:443".into(),
        ];
        let signed = sign(body, &key);
        let j = to_json(&signed);
        // The endpoint key should be a JSON array, not a bare string.
        let ep_idx = j.find("\"endpoint\":[").unwrap();
        assert!(ep_idx > 0, "endpoint must be serialized as a JSON array");
    }

    #[test]
    fn vgpu_canonical_bytes_differ_from_gpu() {
        let key = SigningKey::generate(&mut OsRng);
        let gpu = sample_body_for_key(&key);
        let mut vgpu = gpu.clone();
        vgpu.device = AdvertisedDevice::NvidiaVgpu {
            parent_model: "H100-80GB".into(),
            profile: "A100-80GB-5C".into(),
            vram_mb: 16 * 1024,
        };
        assert_ne!(canonical_bytes(&gpu), canonical_bytes(&vgpu));
    }

    #[test]
    fn vgpu_json_contains_nvidia_vgpu_kind() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        body.device = AdvertisedDevice::NvidiaVgpu {
            parent_model: "A100".into(),
            profile: "A100-80GB-5C".into(),
            vram_mb: 16 * 1024,
        };
        let signed = sign(body, &key);
        let j = to_json(&signed);
        assert!(j.contains("\"kind\":\"nvidia_vgpu\""));
        assert!(j.contains("\"parent_model\""));
        assert!(j.contains("\"profile\""));
    }

    #[test]
    fn unsupported_schema_ver_rejected() {
        let key = SigningKey::generate(&mut OsRng);
        let mut body = sample_body_for_key(&key);
        body.schema_ver = OFFER_SCHEMA_VER - 1;
        let signed = sign(body, &key);
        assert!(matches!(
            verify(&signed, None),
            Err(VerifyError::UnsupportedSchema)
        ));
    }

    #[test]
    fn heartbeat_signed_and_verified_for_node() {
        let key = SigningKey::from_bytes(&[9u8; 32]);
        let pk = hex::encode(key.verifying_key().to_bytes());
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let ts = 1_700_000_042;
        let sig = sign_heartbeat(&node_id, ts, &key);
        assert_eq!(sig.len(), 128);
        assert!(verify_heartbeat(&node_id, ts, &sig, &pk).is_ok());
    }

    #[test]
    fn heartbeat_rejects_forged_or_mutated_signature() {
        let key = SigningKey::from_bytes(&[9u8; 32]);
        let pk = hex::encode(key.verifying_key().to_bytes());
        let node_id = derive_node_id(&key.verifying_key().to_bytes());

        // Signature from a different key (forged by an impostor).
        let impostor = SigningKey::from_bytes(&[11u8; 32]);
        let forged = sign_heartbeat(&node_id, 100, &impostor);
        assert!(matches!(
            verify_heartbeat(&node_id, 100, &forged, &pk),
            Err(VerifyError::SignatureMismatch)
        ));

        // Signature over a different node_id (replayed from another node).
        let sig_other = sign_heartbeat("other-node", 100, &key);
        assert!(matches!(
            verify_heartbeat(&node_id, 100, &sig_other, &pk),
            Err(VerifyError::SignatureMismatch)
        ));

        // Signature over a different timestamp (mutation of the body).
        let sig_other_ts = sign_heartbeat(&node_id, 101, &key);
        assert!(matches!(
            verify_heartbeat(&node_id, 100, &sig_other_ts, &pk),
            Err(VerifyError::SignatureMismatch)
        ));
    }

    #[test]
    fn heartbeat_rejects_bad_hex() {
        let key = SigningKey::from_bytes(&[9u8; 32]);
        let pk = hex::encode(key.verifying_key().to_bytes());
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        assert!(matches!(
            verify_heartbeat(&node_id, 100, "nope", &pk),
            Err(VerifyError::BadSigHex)
        ));
        assert!(matches!(
            verify_heartbeat(&node_id, 100, "nope", "also-not-hex"),
            Err(VerifyError::BadPubkeyHex)
        ));
    }
}
