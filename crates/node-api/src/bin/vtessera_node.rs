//! Minimal HTTP server binding the node-api dispatcher to a TCP socket.
//!
//! Hand-rolled HTTP/1.1 parser, **no external HTTP framework**. The goal
//! is to keep the inbound surface auditable: one screen of parsing, no
//! tokio, no hyper, no axum. For production deployments behind a real
//! reverse proxy this is fine; for serving direct internet traffic,
//! front it with something that does TLS termination and request size
//! caps before this process sees a byte.
//!
//! Behind the `serve` feature so `cargo build -p vtessera-node-api`
//! still produces a library that opens no sockets (matching v0's
//! no-inbound-network guarantee).
//!
//! Run:
//!
//!   cargo run -p vtessera-node-api --bin vtessera-node --features serve \
//!     -- --bind 127.0.0.1:8402 --offer offer.json --escrow <PDA> --network solana-devnet
//!
//! Where `offer.json` is the JSON output of `vtessera_offer::to_json`.

use std::env;
use std::fs;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::process;
use std::time::Duration;

use vtessera_node_api::{dispatch, HttpMethod, HttpRequest, NodeState};
use vtessera_offer::SignedOffer;

const MAX_HEADER_BYTES: usize = 16 * 1024;
const MAX_BODY_BYTES: usize = 1024 * 1024;
const READ_TIMEOUT: Duration = Duration::from_secs(15);

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-node --bind <host:port> --offer <path.json> \
        --escrow <pda> --network <id>"
    );
    process::exit(2);
}

struct Args {
    bind: String,
    offer_path: String,
    escrow_account: String,
    network: String,
}

fn parse_args() -> Args {
    let mut bind: Option<String> = None;
    let mut offer_path: Option<String> = None;
    let mut escrow: Option<String> = None;
    let mut network: Option<String> = None;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--bind" => bind = it.next(),
            "--offer" => offer_path = it.next(),
            "--escrow" => escrow = it.next(),
            "--network" => network = it.next(),
            "--help" | "-h" => usage_and_exit(),
            _ => {
                eprintln!("unknown argument: {a}");
                usage_and_exit();
            }
        }
    }
    match (bind, offer_path, escrow, network) {
        (Some(b), Some(o), Some(e), Some(n)) => Args {
            bind: b,
            offer_path: o,
            escrow_account: e,
            network: n,
        },
        _ => usage_and_exit(),
    }
}

fn main() {
    let args = parse_args();

    let raw = fs::read_to_string(&args.offer_path).unwrap_or_else(|e| {
        eprintln!("failed to read offer file {}: {e}", args.offer_path);
        process::exit(1);
    });
    let offer: SignedOffer = parse_offer_json(&raw).unwrap_or_else(|e| {
        eprintln!("failed to parse offer JSON: {e}");
        process::exit(1);
    });

    let state = NodeState {
        offer,
        escrow_account: args.escrow_account,
        network: args.network,
    };

    let listener = TcpListener::bind(&args.bind).unwrap_or_else(|e| {
        eprintln!("bind {}: {e}", args.bind);
        process::exit(1);
    });
    eprintln!("vtessera-node: listening on {}", args.bind);

    for incoming in listener.incoming() {
        match incoming {
            Ok(stream) => {
                if let Err(e) = handle_connection(stream, &state) {
                    eprintln!("connection error: {e}");
                }
            }
            Err(e) => eprintln!("accept error: {e}"),
        }
    }
}

fn handle_connection(mut stream: TcpStream, state: &NodeState) -> std::io::Result<()> {
    stream.set_read_timeout(Some(READ_TIMEOUT))?;
    stream.set_write_timeout(Some(READ_TIMEOUT))?;

    let request = match read_request(&mut stream) {
        Ok(r) => r,
        Err(why) => {
            write_status(&mut stream, 400, &why)?;
            return Ok(());
        }
    };

    let response = dispatch(state, request);
    let mut head = format!(
        "HTTP/1.1 {} {}\r\n",
        response.status,
        status_text(response.status)
    );
    for (k, v) in &response.headers {
        head.push_str(&format!("{k}: {v}\r\n"));
    }
    head.push_str("connection: close\r\n\r\n");
    stream.write_all(head.as_bytes())?;
    stream.write_all(&response.body)?;
    Ok(())
}

fn read_request(stream: &mut TcpStream) -> Result<HttpRequest, String> {
    let mut reader = BufReader::new(stream.try_clone().map_err(|e| format!("clone: {e}"))?);
    let mut request_line = String::new();
    reader
        .read_line(&mut request_line)
        .map_err(|e| format!("read request line: {e}"))?;
    if request_line.is_empty() {
        return Err("empty request".into());
    }
    let mut parts = request_line.split_whitespace();
    let method = match parts.next() {
        Some("GET") => HttpMethod::Get,
        Some("POST") => HttpMethod::Post,
        Some(_) => HttpMethod::Other,
        None => return Err("missing method".into()),
    };
    let path = parts
        .next()
        .ok_or_else(|| "missing path".to_string())?
        .to_string();

    let mut headers: Vec<(String, String)> = Vec::new();
    let mut header_bytes = 0usize;
    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        let n = reader
            .read_line(&mut line)
            .map_err(|e| format!("read header line: {e}"))?;
        if n == 0 {
            break;
        }
        header_bytes += n;
        if header_bytes > MAX_HEADER_BYTES {
            return Err("header section too large".into());
        }
        if line == "\r\n" || line == "\n" {
            break;
        }
        let trimmed = line.trim_end();
        if let Some(idx) = trimmed.find(':') {
            let (k, v) = trimmed.split_at(idx);
            let key = k.trim().to_ascii_lowercase();
            let val = v[1..].trim().to_string();
            if key == "content-length" {
                content_length = val.parse().unwrap_or(0);
            }
            headers.push((key, val));
        }
    }
    if content_length > MAX_BODY_BYTES {
        return Err("body too large".into());
    }
    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader
            .read_exact(&mut body)
            .map_err(|e| format!("read body: {e}"))?;
    }
    Ok(HttpRequest {
        method,
        path,
        headers,
        body,
    })
}

fn write_status(stream: &mut TcpStream, code: u16, msg: &str) -> std::io::Result<()> {
    let body = msg.as_bytes();
    let resp = format!(
        "HTTP/1.1 {code} {text}\r\ncontent-type: text/plain; charset=utf-8\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
        body.len(),
        text = status_text(code),
    );
    stream.write_all(resp.as_bytes())?;
    stream.write_all(body)?;
    Ok(())
}

fn status_text(code: u16) -> &'static str {
    match code {
        200 => "OK",
        202 => "Accepted",
        400 => "Bad Request",
        402 => "Payment Required",
        404 => "Not Found",
        _ => "Status",
    }
}

/// Tiny JSON reader for the offer envelope. The offer crate writes JSON
/// in a fixed field order via `to_json`, so a real JSON parser is more
/// than needed here. We extract the strings/numbers we care about with
/// string scanning; if the file isn't an offer produced by `to_json`
/// the load fails fast and the server exits.
fn parse_offer_json(s: &str) -> Result<SignedOffer, String> {
    // Required top-level keys.
    let body_str = extract_object(s, "\"body\":").ok_or("missing body")?;
    let pubkey_hex = extract_string(s, "\"pubkey_hex\":").ok_or("missing pubkey_hex")?;
    let sig_hex = extract_string(s, "\"sig_hex\":").ok_or("missing sig_hex")?;

    let schema_ver: u16 = extract_number(&body_str, "\"schema_ver\":")
        .ok_or("missing schema_ver")?
        .parse()
        .map_err(|e: std::num::ParseIntError| e.to_string())?;
    let node_id = extract_string(&body_str, "\"node_id\":").ok_or("missing node_id")?;
    let endpoint = extract_string(&body_str, "\"endpoint\":").ok_or("missing endpoint")?;
    let device_str = extract_object(&body_str, "\"device\":").ok_or("missing device")?;
    let price_str = extract_object(&body_str, "\"price\":").ok_or("missing price")?;
    let issued_unix: u64 = extract_number(&body_str, "\"issued_unix\":")
        .ok_or("missing issued_unix")?
        .parse()
        .map_err(|e: std::num::ParseIntError| e.to_string())?;
    let expires_unix: u64 = extract_number(&body_str, "\"expires_unix\":")
        .ok_or("missing expires_unix")?
        .parse()
        .map_err(|e: std::num::ParseIntError| e.to_string())?;

    let device = parse_device(&device_str)?;
    let price = parse_price(&price_str)?;

    Ok(SignedOffer {
        body: vtessera_offer::OfferBody {
            schema_ver,
            node_id,
            endpoint,
            device,
            price,
            issued_unix,
            expires_unix,
        },
        pubkey_hex,
        sig_hex,
    })
}

fn parse_device(s: &str) -> Result<vtessera_offer::AdvertisedDevice, String> {
    let kind = extract_string(s, "\"kind\":").ok_or("device.kind missing")?;
    match kind.as_str() {
        "cpu" => {
            let vcpus: u32 = extract_number(s, "\"vcpus\":")
                .ok_or("vcpus")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            let mem_mb: u32 = extract_number(s, "\"mem_mb\":")
                .ok_or("mem_mb")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            Ok(vtessera_offer::AdvertisedDevice::Cpu { vcpus, mem_mb })
        }
        "nvidia_gpu" => {
            let model = extract_string(s, "\"model\":").ok_or("model")?;
            let vram_mb: u32 = extract_number(s, "\"vram_mb\":")
                .ok_or("vram_mb")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            Ok(vtessera_offer::AdvertisedDevice::NvidiaGpu { model, vram_mb })
        }
        "nvidia_mig" => {
            let parent_model = extract_string(s, "\"parent_model\":").ok_or("parent_model")?;
            let profile = extract_string(s, "\"profile\":").ok_or("profile")?;
            let vram_mb: u32 = extract_number(s, "\"vram_mb\":")
                .ok_or("vram_mb")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            Ok(vtessera_offer::AdvertisedDevice::NvidiaMig {
                parent_model,
                profile,
                vram_mb,
            })
        }
        "amd_gpu" => {
            let model = extract_string(s, "\"model\":").ok_or("model")?;
            let vram_mb: u32 = extract_number(s, "\"vram_mb\":")
                .ok_or("vram_mb")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            Ok(vtessera_offer::AdvertisedDevice::AmdGpu { model, vram_mb })
        }
        other => Err(format!("unknown device kind {other}")),
    }
}

fn parse_price(s: &str) -> Result<vtessera_offer::PriceQuote, String> {
    let mode = extract_string(s, "\"mode\":").ok_or("price.mode missing")?;
    match mode.as_str() {
        "free" => Ok(vtessera_offer::PriceQuote::Free),
        "paid" => {
            let currency = extract_string(s, "\"currency\":").ok_or("currency")?;
            let currency = match currency.as_str() {
                "eurc" => vtessera_offer::Currency::Eurc,
                "usdc" => vtessera_offer::Currency::Usdc,
                other => return Err(format!("unknown currency {other}")),
            };
            let per: u64 = extract_number(s, "\"per_device_second_micros\":")
                .ok_or("per_device_second_micros")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?;
            let payout_id = extract_string(s, "\"payout_id\":").ok_or("payout_id")?;
            Ok(vtessera_offer::PriceQuote::Paid {
                currency,
                per_device_second_micros: per,
                payout_id,
            })
        }
        other => Err(format!("unknown price mode {other}")),
    }
}

fn extract_string(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = s[start..].trim_start();
    if !rest.starts_with('"') {
        return None;
    }
    let body = &rest[1..];
    let end = body.find('"')?;
    Some(body[..end].to_string())
}

fn extract_number(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = s[start..].trim_start();
    let end = rest
        .find(|c: char| !(c.is_ascii_digit() || c == '-'))
        .unwrap_or(rest.len());
    if end == 0 {
        None
    } else {
        Some(rest[..end].to_string())
    }
}

/// Extract a balanced `{...}` object value following `key` in `s`.
fn extract_object(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = &s[start..];
    let open = rest.find('{')?;
    let bytes = &rest.as_bytes()[open..];
    let mut depth = 0i32;
    let mut in_str = false;
    let mut esc = false;
    for (i, &b) in bytes.iter().enumerate() {
        if esc {
            esc = false;
            continue;
        }
        if in_str {
            if b == b'\\' {
                esc = true;
            } else if b == b'"' {
                in_str = false;
            }
            continue;
        }
        match b {
            b'"' => in_str = true,
            b'{' => depth += 1,
            b'}' => {
                depth -= 1;
                if depth == 0 {
                    return Some(std::str::from_utf8(&bytes[..=i]).ok()?.to_string());
                }
            }
            _ => {}
        }
    }
    None
}
