//! Vtessera node API — Module 2b/c (ROADMAP.md §2).
//!
//! This crate is the agent-facing HTTP surface of a Vtessera seller node.
//! It deliberately ships as **pure dispatch**: a request goes in, a
//! response comes out, no sockets are opened, no TLS is configured.
//!
//! Why pure dispatch:
//!
//! 1. v0's hard rule is that `vtesserad` opens no inbound sockets. The
//!    node API runs on a separate component, separately reviewable, and
//!    its threat model is decoupled from the meter's.
//! 2. The choice of web framework (hyper, axum, tiny_http, …) for the
//!    eventual binary is still open. Keeping dispatch testable without
//!    one means the framework can swap later without disturbing the
//!    contract.
//!
//! Three endpoints model the agent flow (ROADMAP.md §2):
//!
//! - `GET /offer` — returns the seller's signed [`SignedOffer`] as JSON.
//! - `GET /mcp/manifest` — returns an MCP-shaped resource manifest so an
//!   agent's tool catalog discovers this node automatically.
//! - `POST /jobs` — the work endpoint. For free offers, returns 200 and
//!   the job runs. For paid offers, returns 402 with x402 payment terms;
//!   on retry with a valid payment proof, returns 200.
//!
//! This crate does not verify payments — that's the escrow program's job
//! (Module 4). The node API only encodes the 402 challenge and threads
//! the proof to the verifier.

#![forbid(unsafe_code)]

use vtessera_offer::{PriceQuote, SignedOffer};

/// One inbound HTTP request, framework-agnostic.
#[derive(Debug, Clone)]
pub struct HttpRequest {
    pub method: HttpMethod,
    pub path: String,
    /// Headers, normalised to lowercase keys by the caller. The dispatcher
    /// expects `x-payment` (the x402 payment-proof header) and
    /// `accept` to be looked up case-insensitively.
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HttpMethod {
    Get,
    Post,
    Other,
}

/// One outbound HTTP response.
#[derive(Debug, Clone)]
pub struct HttpResponse {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

impl HttpResponse {
    fn json(status: u16, body: String) -> Self {
        let body_bytes = body.into_bytes();
        HttpResponse {
            status,
            headers: vec![
                ("content-type".into(), "application/json".into()),
                ("content-length".into(), body_bytes.len().to_string()),
            ],
            body: body_bytes,
        }
    }

    fn text(status: u16, body: &str) -> Self {
        let body_bytes = body.as_bytes().to_vec();
        HttpResponse {
            status,
            headers: vec![
                ("content-type".into(), "text/plain; charset=utf-8".into()),
                ("content-length".into(), body_bytes.len().to_string()),
            ],
            body: body_bytes,
        }
    }
}

/// State a request handler reads. Owned by the node binary and passed
/// into [`dispatch`] for each request.
pub struct NodeState {
    /// Currently published offer.
    pub offer: SignedOffer,
    /// On-chain account / PDA the escrow program holds funds under, for
    /// the 402 challenge body. The crate doesn't interpret this — the
    /// agent and the escrow program do.
    pub escrow_account: String,
    /// Network identifier the buyer is expected to pay on, e.g.
    /// "solana-mainnet-beta", "solana-devnet". Surfaced in the 402 body
    /// so the agent picks the right chain.
    pub network: String,
}

/// Outcome of handling a `/jobs` request when the offer is paid.
///
/// The handler doesn't itself talk to a chain. It returns one of these
/// and lets the caller (the binary) wire the verifier — that's where the
/// settlement crate plugs in.
#[derive(Debug)]
pub enum JobDecision<'a> {
    /// No payment header was supplied. Return the 402 challenge body to
    /// the agent so it can sign and retry.
    PaymentRequired(PaymentChallenge<'a>),
    /// A payment header was supplied. The binary should verify it (via
    /// the settlement / escrow path), then call the executor.
    VerifyAndRun {
        payment_proof: String,
        body: Vec<u8>,
    },
    /// The offer is free. The binary should just call the executor.
    RunFree { body: Vec<u8> },
}

/// The x402 challenge body. Serialised into the 402 response.
#[derive(Debug)]
pub struct PaymentChallenge<'a> {
    pub offer: &'a SignedOffer,
    pub escrow_account: &'a str,
    pub network: &'a str,
}

/// Dispatch a single request to the right handler. This is the function
/// every HTTP framework integration calls.
pub fn dispatch(state: &NodeState, req: HttpRequest) -> HttpResponse {
    match (req.method, req.path.as_str()) {
        (HttpMethod::Get, "/offer") => handle_offer(state),
        (HttpMethod::Get, "/mcp/manifest") => handle_mcp_manifest(state),
        (HttpMethod::Post, "/jobs") => handle_jobs(state, req),
        (HttpMethod::Get, "/healthz") => HttpResponse::text(200, "ok"),
        _ => HttpResponse::text(404, "not found"),
    }
}

fn handle_offer(state: &NodeState) -> HttpResponse {
    HttpResponse::json(200, vtessera_offer::to_json(&state.offer))
}

fn handle_mcp_manifest(state: &NodeState) -> HttpResponse {
    HttpResponse::json(200, mcp_manifest(state))
}

/// Classify an incoming `/jobs` request without running anything. The
/// caller binary handles the executor + verifier sides.
pub fn classify_job_request<'a>(state: &'a NodeState, req: &HttpRequest) -> JobDecision<'a> {
    if matches!(state.offer.body.price, PriceQuote::Free) {
        return JobDecision::RunFree {
            body: req.body.clone(),
        };
    }
    match header(&req.headers, "x-payment") {
        Some(proof) => JobDecision::VerifyAndRun {
            payment_proof: proof,
            body: req.body.clone(),
        },
        None => JobDecision::PaymentRequired(PaymentChallenge {
            offer: &state.offer,
            escrow_account: &state.escrow_account,
            network: &state.network,
        }),
    }
}

fn handle_jobs(state: &NodeState, req: HttpRequest) -> HttpResponse {
    match classify_job_request(state, &req) {
        JobDecision::PaymentRequired(challenge) => {
            // x402: signal payment is required and surface the terms.
            // The body is JSON the agent parses to sign a payment.
            let mut resp = HttpResponse::json(402, payment_required_body(&challenge));
            resp.headers.push(("x-payment-required".into(), "1".into()));
            resp
        }
        JobDecision::VerifyAndRun { .. } | JobDecision::RunFree { .. } => {
            // The binary actually runs the job. From the perspective of
            // this pure-dispatch crate, the dispatch step itself is
            // "accept": real implementations replace this branch by
            // calling the executor and streaming results.
            HttpResponse::json(202, r#"{"status":"accepted"}"#.into())
        }
    }
}

fn header(headers: &[(String, String)], name: &str) -> Option<String> {
    headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case(name))
        .map(|(_, v)| v.clone())
}

/// The body of a 402 response. JSON shape matches the x402 challenge
/// pattern: enough information for an agent to construct a stablecoin
/// payment on the named chain, addressed to the escrow account, for the
/// offer's price.
pub fn payment_required_body(c: &PaymentChallenge<'_>) -> String {
    let mut s = String::with_capacity(256);
    s.push('{');
    s.push_str("\"scheme\":\"x402\",");
    s.push_str("\"network\":");
    json_string(c.network, &mut s);
    s.push(',');
    s.push_str("\"escrow_account\":");
    json_string(c.escrow_account, &mut s);
    s.push(',');
    s.push_str("\"offer\":");
    s.push_str(&vtessera_offer::to_json(c.offer));
    s.push('}');
    s
}

/// MCP-shaped resource manifest. The shape is deliberately small — a
/// real MCP server can wrap this and surface tools that map onto the
/// `/jobs` endpoint. The goal here is "an MCP-aware agent can discover
/// this node without bespoke client code."
pub fn mcp_manifest(state: &NodeState) -> String {
    let mut s = String::with_capacity(512);
    s.push('{');
    s.push_str("\"protocolVersion\":\"2024-11-05\",");
    s.push_str("\"serverInfo\":{");
    s.push_str("\"name\":\"vtessera-node\",\"version\":\"0.1.0\"},");
    s.push_str("\"resources\":[{");
    s.push_str("\"uri\":\"vtessera://offer\",");
    s.push_str("\"name\":\"Vtessera compute offer\",");
    s.push_str("\"description\":\"Signed machine-readable offer of compute on this node. ");
    s.push_str("Free or paid (EURC/USDC, settled to seller in HNT).\",");
    s.push_str("\"mimeType\":\"application/json\"");
    s.push_str("}],");
    s.push_str("\"tools\":[{");
    s.push_str("\"name\":\"submit_job\",");
    s.push_str("\"description\":\"Submit an OCI workload to this node. ");
    s.push_str("Returns 200 for free offers, 402 (x402 challenge) for paid offers.\",");
    s.push_str("\"endpoint\":");
    json_string(&state.offer.body.endpoint, &mut s);
    s.push_str("}]}");
    s
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
    use vtessera_offer::{
        derive_node_id, sign, AdvertisedDevice, Currency, OfferBody, PriceQuote, OFFER_SCHEMA_VER,
    };

    fn signed(price: PriceQuote) -> SignedOffer {
        // Deterministic key for tests so we don't pull rand into the crate's
        // dep surface for unit testing.
        let seed = [7u8; 32];
        let key = ed25519_dalek::SigningKey::from_bytes(&seed);
        let node_id = derive_node_id(&key.verifying_key().to_bytes());
        let body = OfferBody {
            schema_ver: OFFER_SCHEMA_VER,
            node_id,
            endpoint: "https://node.example/v1".into(),
            device: AdvertisedDevice::Cpu {
                vcpus: 4,
                mem_mb: 16 * 1024,
            },
            price,
            issued_unix: 1_700_000_000,
            expires_unix: 1_700_010_000,
        };
        sign(body, &key)
    }

    fn state(price: PriceQuote) -> NodeState {
        NodeState {
            offer: signed(price),
            escrow_account: "Esc1111111111111111111111111111111111111111".into(),
            network: "solana-devnet".into(),
        }
    }

    fn req(method: HttpMethod, path: &str, headers: Vec<(&str, &str)>) -> HttpRequest {
        HttpRequest {
            method,
            path: path.into(),
            headers: headers
                .into_iter()
                .map(|(k, v)| (k.to_ascii_lowercase(), v.into()))
                .collect(),
            body: Vec::new(),
        }
    }

    fn paid() -> PriceQuote {
        PriceQuote::Paid {
            currency: Currency::Eurc,
            per_device_second_micros: 100,
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
        }
    }

    #[test]
    fn unknown_path_404() {
        let s = state(PriceQuote::Free);
        let r = dispatch(&s, req(HttpMethod::Get, "/nope", vec![]));
        assert_eq!(r.status, 404);
    }

    #[test]
    fn healthz_200() {
        let s = state(PriceQuote::Free);
        let r = dispatch(&s, req(HttpMethod::Get, "/healthz", vec![]));
        assert_eq!(r.status, 200);
    }

    #[test]
    fn offer_endpoint_returns_signed_offer_json() {
        let s = state(PriceQuote::Free);
        let r = dispatch(&s, req(HttpMethod::Get, "/offer", vec![]));
        assert_eq!(r.status, 200);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("\"mode\":\"free\""));
        assert!(body.contains("\"sig_hex\":"));
    }

    #[test]
    fn free_jobs_post_accepts_immediately() {
        let s = state(PriceQuote::Free);
        let r = dispatch(&s, req(HttpMethod::Post, "/jobs", vec![]));
        assert_eq!(r.status, 202);
    }

    #[test]
    fn paid_jobs_post_without_proof_returns_402_with_x402_challenge() {
        let s = state(paid());
        let r = dispatch(&s, req(HttpMethod::Post, "/jobs", vec![]));
        assert_eq!(r.status, 402);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("\"scheme\":\"x402\""));
        assert!(body.contains("\"network\":\"solana-devnet\""));
        assert!(body.contains("\"escrow_account\":\"Esc"));
    }

    #[test]
    fn paid_jobs_post_with_proof_classifies_to_verify_and_run() {
        let s = state(paid());
        let r = req(HttpMethod::Post, "/jobs", vec![("x-payment", "0xPROOF")]);
        match classify_job_request(&s, &r) {
            JobDecision::VerifyAndRun { payment_proof, .. } => {
                assert_eq!(payment_proof, "0xPROOF");
            }
            other => panic!("expected VerifyAndRun, got {other:?}"),
        }
    }

    #[test]
    fn mcp_manifest_advertises_submit_job_tool() {
        let s = state(paid());
        let r = dispatch(&s, req(HttpMethod::Get, "/mcp/manifest", vec![]));
        assert_eq!(r.status, 200);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("\"name\":\"submit_job\""));
        assert!(body.contains("\"resources\":["));
        assert!(body.contains("vtessera://offer"));
    }
}
