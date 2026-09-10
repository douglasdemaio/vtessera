//! Marketplace browsing — the "Browse" half of the Marketplace tab.
//!
//! Fetches the public marketplace `nodes.json` (the same source
//! `vtessera-agent discover --marketplace` reads) and turns each signed
//! offer into a small, UI-friendly [`Listing`]. All parsing is defensive:
//! a malformed or partial entry is skipped, never a crash.

use vtessera_offer::{AdvertisedDevice, Currency, OfferBody, PriceQuote};

/// The public marketplace's canonical URL. Nodes register with the
/// Cloudflare Worker (`.github/workflows/marketplace.yml`) which persists
/// entries here.
pub const DEFAULT_MARKETPLACE_URL: &str = "https://douglasdemaio.github.io/vtessera/nodes.json";

/// One browsable node, flattened from a marketplace entry.
#[derive(Debug, Clone)]
pub struct Listing {
    pub node_id: String,
    pub endpoint_id: String,
    /// Advertised HTTP(S) endpoints (first wins for reach); empty for
    /// iroh-only / outbound-only nodes.
    pub endpoint_host: Option<String>,
    pub device: AdvertisedDevice,
    pub price: PriceQuote,
    pub schema_ver: u16,
    pub issued_unix: u64,
    pub expires_unix: u64,
}

#[derive(Debug, Clone, Default)]
pub struct MarketplaceSnapshot {
    pub updated_at: u64,
    pub nodes: Vec<Listing>,
}

/// Fetch and parse the marketplace `nodes.json` at `url`. Never panics;
/// network/parse failures come back as `Err` strings for the status label.
pub fn fetch_marketplace(url: &str) -> Result<MarketplaceSnapshot, String> {
    let json = fetch_json(url)?;
    parse_marketplace(&json)
}

/// Fetch the raw JSON `nodes.json`. Split from parsing so tests exercise
/// `parse_marketplace` without a network.
fn fetch_json(url: &str) -> Result<serde_json::Value, String> {
    let agent = ureq::Agent::new_with_defaults();
    let resp = agent
        .get(url)
        .call()
        .map_err(|e| format!("marketplace unreachable ({url}): {e}"))?;
    resp.into_body()
        .read_json()
        .map_err(|e| format!("marketplace nodes.json unreadable: {e}"))
}

/// Parse a marketplace document. Entries that fail to yield a usable offer
/// are skipped rather than poisoning the whole list.
pub fn parse_marketplace(json: &serde_json::Value) -> Result<MarketplaceSnapshot, String> {
    let nodes = json
        .get("nodes")
        .and_then(serde_json::Value::as_array)
        .ok_or("marketplace nodes.json malformed (no nodes list)")?;
    let mut snapshot = MarketplaceSnapshot {
        updated_at: json
            .get("updated_at")
            .and_then(serde_json::Value::as_u64)
            .unwrap_or(0),
        nodes: Vec::new(),
    };
    for entry in nodes {
        if let Some(listing) = parse_entry(entry) {
            snapshot.nodes.push(listing);
        }
    }
    Ok(snapshot)
}

fn parse_entry(entry: &serde_json::Value) -> Option<Listing> {
    let body: OfferBody = serde_json::from_value(entry.get("offer")?.get("body")?.clone()).ok()?;
    let node_id = entry
        .get("node_id")
        .and_then(serde_json::Value::as_str)
        .unwrap_or(&body.node_id)
        .to_string();
    // Endpoint id wins at entry level; the offer body's full pubkey is the
    // fallback (older payloads carry it only in the body).
    let endpoint_id = entry
        .get("endpoint_id")
        .and_then(serde_json::Value::as_str)
        .unwrap_or(&body.endpoint_id)
        .to_string();
    let endpoint_host = body.endpoint.iter().find_map(|ep| host_of(ep));
    Some(Listing {
        node_id,
        endpoint_id,
        endpoint_host,
        schema_ver: body.schema_ver,
        device: body.device,
        price: body.price,
        issued_unix: body.issued_unix,
        expires_unix: body.expires_unix,
    })
}

/// Pull the `host[:port]` out of an http(s) URL; `None` for relayed/iroh
/// style endpoints the agent CLI can't use as a `--node` target.
fn host_of(url: &str) -> Option<String> {
    let rest = url
        .strip_prefix("http://")
        .or_else(|| url.strip_prefix("https://"))?;
    Some(rest.split(['/', '?', '#']).next()?.to_string())
}

/// Symbol for the currency ticker — "€" for EURC, "$" for USDC.
pub fn currency_symbol(currency: &Currency) -> &'static str {
    match currency {
        Currency::Eurc => "€",
        Currency::Usdc => "$",
    }
}

/// "CPU 8 vCPU · 16 GB" — the short spec line shown in Browse rows.
pub fn node_label(device: &AdvertisedDevice) -> String {
    match device {
        AdvertisedDevice::Cpu { vcpus, mem_mb } => {
            format!("CPU {vcpus} vCPU · {} GB", mb_to_gb(*mem_mb))
        }
        AdvertisedDevice::NvidiaGpu { model, vram_mb } => {
            format!("NVIDIA {model} · {} GB", mb_to_gb(*vram_mb))
        }
        AdvertisedDevice::NvidiaMig {
            parent_model,
            profile,
            vram_mb,
        } => format!(
            "NVIDIA MIG {parent_model} · {profile} · {} GB",
            mb_to_gb(*vram_mb)
        ),
        AdvertisedDevice::AmdGpu { model, vram_mb } => {
            format!("AMD {model} · {} GB", mb_to_gb(*vram_mb))
        }
        AdvertisedDevice::NvidiaVgpu {
            parent_model,
            profile,
            vram_mb,
        } => format!(
            "NVIDIA vGPU {parent_model} · {profile} · {} GB",
            mb_to_gb(*vram_mb)
        ),
    }
}

fn mb_to_gb(mb: u32) -> f64 {
    (mb as f64 / 1024.0 * 10.0).round() / 10.0
}

/// Human "price per hour" in micro-units — `11560 micros/s` →
/// `0.0416/h`. The design prices an hour, not a second, for browsing.
pub fn price_per_hour(per_device_second_micros: u64) -> f64 {
    per_device_second_micros as f64 * 3600.0 / 1_000_000.0
}

/// "€0.05/h", "0.1000 USDC/h", or "free".
pub fn price_label(price: &PriceQuote) -> String {
    match price {
        PriceQuote::Free => "free".into(),
        PriceQuote::Paid {
            currency,
            per_device_second_micros,
            ..
        } => format!(
            "{}{}/h",
            currency_symbol(currency),
            format_per_hour(price_per_hour(*per_device_second_micros))
        ),
    }
}

fn format_per_hour(v: f64) -> String {
    if v >= 100.0 {
        format!("{v:.0}")
    } else {
        let s = format!("{v:.4}");
        let s = s.trim_end_matches('0').trim_end_matches('.');
        if s.is_empty() {
            "0".into()
        } else {
            s.to_string()
        }
    }
}

/// The copyable one-liner an agent would use to submit work to this node:
/// `vtessera-agent --node <endpoint-host> submit --job job.json`, falling
/// back to the iroh `endpoint_id` for nodes with no HTTP endpoint.
pub fn agent_command(listing: &Listing) -> String {
    match &listing.endpoint_host {
        Some(host) => format!("vtessera-agent --node {host} submit --job job.json"),
        None => format!(
            "vtessera-agent --node {} submit --job job.json",
            listing.endpoint_id
        ),
    }
}

/// Truncated id for column space — `"9f2a…c441"` style.
pub fn short_id(id: &str) -> String {
    let chars: Vec<char> = id.chars().collect();
    if chars.len() <= 8 {
        return id.to_string();
    }
    let head: String = chars[..4].iter().collect();
    let tail: String = chars[chars.len() - 4..].iter().collect();
    format!("{head}…{tail}")
}

/// Build a [`Listing`] straight from an offer body (the My Listing preview
/// path — the GUI re-derives its signed offer, then renders it like any
/// other market listing).
pub fn listing_from_body(body: &OfferBody) -> Listing {
    Listing {
        node_id: body.node_id.clone(),
        endpoint_id: body.endpoint_id.clone(),
        endpoint_host: body.endpoint.iter().find_map(|ep| host_of(ep)),
        schema_ver: body.schema_ver,
        device: body.device.clone(),
        price: body.price.clone(),
        issued_unix: body.issued_unix,
        expires_unix: body.expires_unix,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> serde_json::Value {
        serde_json::json!({
            "version": 1,
            "updated_at": 1_720_000_000,
            "nodes": [
                {
                    "node_id": "cpu-node",
                    "offer": {
                        "body": {
                            "schema_ver": 2,
                            "node_id": "cpu-node",
                            "endpoint_id": "9f2a0000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac441",
                            "endpoint": ["http://192.168.1.100:8402"],
                            "device": {"kind": "cpu", "vcpus": 8, "mem_mb": 16384},
                            "price": {"mode": "free"},
                            "issued_unix": 1_720_000_000,
                            "expires_unix": 1_720_000_100
                        }
                    }
                },
                {
                    "node_id": "paid-node",
                    "offer": {
                        "body": {
                            "schema_ver": 2,
                            "node_id": "paid-node",
                            "endpoint_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                            "endpoint": ["http://10.0.0.5:8402"],
                            "device": {"kind": "nvidia_gpu", "model": "RTX 4090", "vram_mb": 24576},
                            "price": {"mode": "paid", "currency": "eurc",
                                      "per_device_second_micros": 139, "payout_id": "x"},
                            "issued_unix": 1_720_000_000,
                            "expires_unix": 1_720_000_100
                        }
                    }
                },
                {
                    "node_id": "bad-node",
                    "offer": {"body": {"schema_ver": "not-a-number"}}
                }
            ]
        })
    }

    #[test]
    fn parse_drops_malformed_entries() {
        let snap = parse_marketplace(&fixture()).unwrap();
        assert_eq!(snap.updated_at, 1_720_000_000);
        assert_eq!(snap.nodes.len(), 2);
        assert_eq!(snap.nodes[0].node_id, "cpu-node");
        assert_eq!(snap.nodes[1].node_id, "paid-node");
    }

    #[test]
    fn missing_nodes_list_is_an_error() {
        assert!(parse_marketplace(&serde_json::json!({})).is_err());
    }

    #[test]
    fn node_label_covers_device_variants() {
        let cpu = AdvertisedDevice::Cpu {
            vcpus: 8,
            mem_mb: 16384,
        };
        assert_eq!(node_label(&cpu), "CPU 8 vCPU · 16 GB");
        let gpu = AdvertisedDevice::NvidiaGpu {
            model: "RTX 4090".into(),
            vram_mb: 24576,
        };
        assert_eq!(node_label(&gpu), "NVIDIA RTX 4090 · 24 GB");
        let mig = AdvertisedDevice::NvidiaMig {
            parent_model: "A100".into(),
            profile: "1g.10gb".into(),
            vram_mb: 10240,
        };
        assert_eq!(node_label(&mig), "NVIDIA MIG A100 · 1g.10gb · 10 GB");
        let amd = AdvertisedDevice::AmdGpu {
            model: "RX 7900 XTX".into(),
            vram_mb: 24576,
        };
        assert_eq!(node_label(&amd), "AMD RX 7900 XTX · 24 GB");
        let vgpu = AdvertisedDevice::NvidiaVgpu {
            parent_model: "H100".into(),
            profile: "H100-80GB-5C".into(),
            vram_mb: 16384,
        };
        assert_eq!(node_label(&vgpu), "NVIDIA vGPU H100 · H100-80GB-5C · 16 GB");
    }

    #[test]
    fn price_per_hour_conversion() {
        // 0.05/h ⇒ 13.888… micros/s; 100 micros/s ⇒ 0.36/h.
        assert!((price_per_hour(14) - 0.0504).abs() < 1e-6);
        assert!((price_per_hour(100) - 0.36).abs() < 1e-6);
    }

    #[test]
    fn price_label_free_and_paid() {
        assert_eq!(price_label(&PriceQuote::Free), "free");
        let paid = PriceQuote::Paid {
            currency: Currency::Eurc,
            per_device_second_micros: 100,
            payout_id: "x".into(),
        };
        assert_eq!(price_label(&paid), "€0.36/h");
        let usdc = PriceQuote::Paid {
            currency: Currency::Usdc,
            per_device_second_micros: 100,
            payout_id: "x".into(),
        };
        assert_eq!(price_label(&usdc), "$0.36/h");
    }

    #[test]
    fn agent_command_http_vs_iroh_only() {
        let snap = parse_marketplace(&fixture()).unwrap();
        assert_eq!(
            agent_command(&snap.nodes[0]),
            "vtessera-agent --node 192.168.1.100:8402 submit --job job.json"
        );
        // Force an iroh-only node (no HTTP endpoint).
        let mut n = snap.nodes[0].clone();
        n.endpoint_host = None;
        assert_eq!(
            agent_command(&n),
            "vtessera-agent --node 9f2a0000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac441 submit --job job.json"
        );
    }

    #[test]
    fn short_id_truncates() {
        let long = "9f2a0000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac441";
        assert_eq!(short_id(long), "9f2a…c441");
        assert_eq!(short_id("abc"), "abc");
    }
}
