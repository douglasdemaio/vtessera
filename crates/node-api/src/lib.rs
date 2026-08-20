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
//!   agent's tool catalog discovers this node automatically. Kept for
//!   backward compatibility; the real MCP endpoint is `POST /mcp`.
//! - `POST /mcp` — a real MCP (Model Context Protocol) server
//!   (protocol `2024-11-05`) over JSON-RPC 2.0 ([`mcp`]); a request in,
//!   a JSON-RPC response out, `202` (empty) for notifications.
//! - `GET /.well-known/agent.json` — an A2A agent card so agent-to-agent
//!   frameworks can discover this node without bespoke client code.
//! - `POST /jobs` — the work endpoint. For paid offers, returns 402 with
//!   x402 payment terms. Free submissions (and, once an on-chain verifier
//!   exists, paid ones with a verified proof) run through a [`JobRunner`]
//!   the **binary** supplies — this crate stays executor-free by default.
//!   With no runner wired, submissions are refused with **501 Not
//!   Implemented** — never a fake 200/202.
//!
//! This crate does not verify payments — that's the escrow program's job
//! (Module 4). The node API only encodes the 402 challenge and threads
//! the proof to the verifier.

#![forbid(unsafe_code)]

use std::path::PathBuf;
use std::sync::Arc;

use vtessera_offer::{PriceQuote, SignedOffer};

#[cfg(feature = "serve")]
pub mod index;
pub mod mcp;

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

/// Error running a submitted job. Carries the HTTP status the lib should
/// respond with — 400 for a malformed spec or an admission rejection,
/// 500 for a backend failure. The message is human-readable and not part
/// of any wire contract.
#[derive(Debug, Clone)]
pub struct JobRunError {
    pub status: u16,
    pub message: String,
}

impl JobRunError {
    /// The submitted job spec was unparsable or failed admission.
    pub fn bad_request(message: impl Into<String>) -> Self {
        JobRunError {
            status: 400,
            message: message.into(),
        }
    }

    /// The executor backend failed while running the job.
    pub fn server(message: impl Into<String>) -> Self {
        JobRunError {
            status: 500,
            message: message.into(),
        }
    }
}

/// Runs a job on behalf of the node.
///
/// The lib is transport-only: it formats the response, it never executes.
/// The node/MCP binaries implement this by wrapping a `vtessera-executor`
/// backend (feature-gated, so the default library build links nothing
/// privileged). `None` in [`NodeState::runner`] keeps the honest 501
/// default.
pub trait JobRunner: Send + Sync {
    /// `body` is the raw `POST /jobs` request body (a JSON job spec, the
    /// executor's `JobSpec` shape). `Ok(json)` is the response body for a
    /// 200 (job accepted + metering); `Err` carries the status and a
    /// human-readable reason.
    fn run(&self, body: &[u8]) -> Result<String, JobRunError>;
}

/// Errors from off-chain payment verification (Solana RPC checks).
#[derive(Debug)]
pub enum PaymentVerifyError {
    /// Proof JSON is malformed or missing required fields.
    MalformedProof(String),
    /// Transaction not found or not confirmed.
    TransactionNotFound(String),
    /// Transaction doesn't involve the expected escrow account.
    EscrowMismatch {
        expected: String,
        found: Vec<String>,
    },
    /// Token transfer amount is less than the offer price.
    InsufficientAmount { expected: u64, found: u64 },
    /// The on-chain job_id doesn't match the submitted job_id.
    JobIdMismatch { expected: String, found: String },
    /// RPC endpoint unreachable.
    RpcUnavailable(String),
}

impl std::fmt::Display for PaymentVerifyError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::MalformedProof(e) => write!(f, "malformed payment proof: {e}"),
            Self::TransactionNotFound(e) => write!(f, "transaction not found: {e}"),
            Self::EscrowMismatch { expected, found } => {
                write!(f, "escrow mismatch: expected {expected}, found {found:?}")
            }
            Self::InsufficientAmount { expected, found } => {
                write!(f, "insufficient amount: expected {expected}, got {found}")
            }
            Self::JobIdMismatch { expected, found } => {
                write!(f, "job_id mismatch: expected {expected}, got {found}")
            }
            Self::RpcUnavailable(e) => write!(f, "RPC unavailable: {e}"),
        }
    }
}

impl std::error::Error for PaymentVerifyError {}

/// Off-chain payment verifier. The real implementation wraps a Solana RPC
/// client; the lib crate only depends on the trait.
pub trait PaymentVerifier: Send + Sync {
    /// Verify an x402 payment proof against the chain.
    ///
    /// `proof` — the raw `x-payment` header value (JSON string).
    /// `escrow_account` — the expected escrow PDA (from the 402 challenge).
    /// `network` — Solana network (e.g. "solana-devnet").
    ///
    /// Returns `Ok((mint_pubkey, amount_micros))` on success.
    fn verify(
        &self,
        proof: &str,
        escrow_account: &str,
        network: &str,
    ) -> Result<(String, u64), PaymentVerifyError>;
}

/// State a request handler reads. Owned by the node binary and passed
/// into [`dispatch`] for each request.
#[derive(Clone)]
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
    /// Optional job runner supplied by the binary. `None` means free
    /// submissions are refused with 501 (execution not wired).
    pub runner: Option<Arc<dyn JobRunner>>,
    /// Optional payment verifier. `Some` means paid jobs verify the x402
    /// proof on-chain before execution; `None` means paid jobs return 501
    /// (honest refusal — payment verification not wired).
    pub verifier: Option<Arc<dyn PaymentVerifier>>,
    /// State directory for contracts and receipts. `None` means contracts
    /// are not persisted (e.g. standalone mode).
    pub state_dir: Option<PathBuf>,
    /// Optional offer-index client (Module 2a wiring). `Some` means the
    /// node publishes its offer and enforces first-come-first-served claims
    /// via the index; `None` (and builds without the `serve` feature)
    /// behaves as a standalone node with no claim gate.
    #[cfg(feature = "serve")]
    pub index: Option<Arc<dyn index::IndexClient>>,
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
        (HttpMethod::Post, "/mcp") => handle_mcp(state, req),
        (HttpMethod::Get, "/.well-known/agent.json") => handle_agent_card(state),
        (HttpMethod::Post, "/jobs") => handle_jobs(state, req),
        (HttpMethod::Get, "/healthz") => HttpResponse::text(200, "ok"),
        _ => HttpResponse::text(404, "not found"),
    }
}

/// Parse a signed offer from the JSON produced by `vtessera_offer::to_json`
/// (or any serde-compatible rendering of it). Shared by the node and MCP
/// binaries so offer loading lives in exactly one audited place.
pub fn parse_signed_offer(raw: &str) -> Result<SignedOffer, String> {
    serde_json::from_str(raw).map_err(|e| format!("invalid signed offer JSON: {e}"))
}

fn handle_offer(state: &NodeState) -> HttpResponse {
    HttpResponse::json(200, vtessera_offer::to_json(&state.offer))
}

fn handle_mcp_manifest(state: &NodeState) -> HttpResponse {
    HttpResponse::json(200, mcp_manifest(state))
}

/// `POST /mcp`: one JSON-RPC message per request body. Notifications are
/// acknowledged with `202` and no body (streamable-HTTP protocol §6.2);
/// responses are JSON-RPC responses with `200`.
fn handle_mcp(state: &NodeState, req: HttpRequest) -> HttpResponse {
    let line = String::from_utf8_lossy(&req.body).to_string();
    let server = mcp::McpServer::new(state.clone());
    match server.handle(&line) {
        Some(resp) => {
            let body = serde_json::to_string(&resp).unwrap_or_else(|_| "{}".into());
            HttpResponse::json(200, body)
        }
        None => HttpResponse {
            status: 202,
            headers: vec![("content-length".into(), "0".into())],
            body: Vec::new(),
        },
    }
}

/// `GET /.well-known/agent.json`: an A2A (agent-to-agent) card so discovery
/// frameworks can index this node. Skills map one-to-one onto the MCP
/// tool catalog.
fn handle_agent_card(state: &NodeState) -> HttpResponse {
    let card = serde_json::json!({
        "name": mcp::MCP_SERVER_NAME,
        "description": "Vtessera compute seller node: signed compute offers over MCP + x402; paid offers settle in EURC/USDC.",
        "url": state.offer.body.endpoint,
        "version": env!("CARGO_PKG_VERSION"),
        "capabilities": { "streaming": false, "pushNotifications": false },
        "authentication": { "schemes": ["none"], "credentials": false },
        "skills": [{
            "id": "submit_job",
            "name": "Submit compute job",
            "description": "Submit an OCI workload to this node. Free offers execute directly; paid offers return 402 (x402) until a signed payment is attached.",
            "tags": ["compute"],
        }],
    });
    let body = serde_json::to_string(&card).unwrap_or_else(|_| "{}".into());
    HttpResponse::json(200, body)
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
        JobDecision::VerifyAndRun {
            payment_proof,
            body,
        } => handle_paid_job(state, &payment_proof, &body),
        JobDecision::RunFree { body } => run_free(state, &body, header(&req.headers, "x-agent-id")),
    }
}

/// Handle a paid job submission with an x402 payment proof.
///
/// Without a verifier this is the honest 501 — the lib never claims
/// a payment was verified when it wasn't.
#[cfg(feature = "serve")]
pub fn handle_paid_job(state: &NodeState, payment_proof: &str, body: &[u8]) -> HttpResponse {
    let Some(verifier) = &state.verifier else {
        return HttpResponse::json(
            501,
            r#"{"status":"not-implemented","reason":"payment verification not wired"}"#.into(),
        );
    };

    // Parse the job spec to get job_id.
    let spec: vtessera_executor::JobSpec = match serde_json::from_slice(body) {
        Ok(s) => s,
        Err(e) => return HttpResponse::json(400, format!("bad job spec: {e}")),
    };

    // Verify payment on-chain.
    match verifier.verify(payment_proof, &state.escrow_account, &state.network) {
        Ok((_mint, _amount)) => { /* proceed */ }
        Err(PaymentVerifyError::MalformedProof(e)) => {
            return HttpResponse::json(400, format!("bad payment proof: {e}"));
        }
        Err(PaymentVerifyError::JobIdMismatch { expected, found }) => {
            return HttpResponse::json(
                400,
                format!("payment job_id mismatch: expected {expected}, got {found}"),
            );
        }
        Err(PaymentVerifyError::RpcUnavailable(e)) => {
            return HttpResponse::json(503, format!("RPC unavailable: {e}"));
        }
        Err(e) => {
            // Re-challenge with a fresh 402 so the agent can retry.
            let challenge = PaymentChallenge {
                offer: &state.offer,
                escrow_account: &state.escrow_account,
                network: &state.network,
            };
            let mut resp = HttpResponse::json(402, payment_required_body(&challenge));
            resp.headers.push(("x-payment-required".into(), "1".into()));
            resp.headers.push((
                "x-payment-error".into(),
                format!("verification failed: {e}"),
            ));
            return resp;
        }
    }

    // Create contract and write to disk.
    create_and_write_contract(state, &spec);

    // Run through executor.
    match &state.runner {
        Some(runner) => match runner.run(body) {
            Ok(json) => HttpResponse::json(200, json),
            Err(e) => HttpResponse::json(e.status, e.message),
        },
        None => HttpResponse::json(
            501,
            r#"{"status":"not-implemented","reason":"job execution not wired"}"#.into(),
        ),
    }
}

#[cfg(not(feature = "serve"))]
fn handle_paid_job(_state: &NodeState, _payment_proof: &str, _body: &[u8]) -> HttpResponse {
    HttpResponse::json(
        501,
        r#"{"status":"not-implemented","reason":"payment verification not wired"}"#.into(),
    )
}

/// Run a free job through the binary-supplied runner.
///
/// Without a runner this is the honest 501 — the lib never fakes
/// acceptance of a job nothing is executing.
#[cfg(feature = "serve")]
pub fn run_free(state: &NodeState, body: &[u8], agent_id: Option<String>) -> HttpResponse {
    if let Err(resp) = check_claim_gate(state, agent_id.as_deref()) {
        return resp;
    }
    // Parse the job spec to create a contract before execution.
    let spec: vtessera_executor::JobSpec = match serde_json::from_slice(body) {
        Ok(s) => s,
        Err(e) => return HttpResponse::json(400, format!("bad job spec: {e}")),
    };
    create_and_write_contract(state, &spec);
    match &state.runner {
        Some(runner) => match runner.run(body) {
            Ok(json) => HttpResponse::json(200, json),
            Err(e) => HttpResponse::json(e.status, e.message),
        },
        None => HttpResponse::json(
            501,
            r#"{"status":"not-implemented","reason":"job execution is not wired; start the node with an executor backend"}"#.into(),
        ),
    }
}

#[cfg(not(feature = "serve"))]
fn run_free(state: &NodeState, body: &[u8], agent_id: Option<String>) -> HttpResponse {
    if let Err(resp) = check_claim_gate(state, agent_id.as_deref()) {
        return resp;
    }
    match &state.runner {
        Some(runner) => match runner.run(body) {
            Ok(json) => HttpResponse::json(200, json),
            Err(e) => HttpResponse::json(e.status, e.message),
        },
        None => HttpResponse::json(
            501,
            r#"{"status":"not-implemented","reason":"job execution is not wired; start the node with an executor backend"}"#.into(),
        ),
    }
}

/// Create a `JobContract` from the offer and job spec, and write it to disk.
/// Logs but does not fail on I/O errors — the job proceeds regardless.
#[cfg(feature = "serve")]
fn create_and_write_contract(state: &NodeState, spec: &vtessera_executor::JobSpec) {
    let device_class = device_class_from_offer(&state.offer.body.device);
    let contract = vtessera_settlement::create_contract(
        spec.job_id.clone(),
        state.offer.body.node_id.clone(),
        device_class,
        spec.max_duration_secs,
    );
    if let Some(dir) = &state.state_dir {
        if let Err(e) = vtessera_settlement::write_contract(&contract, dir) {
            eprintln!("failed to write contract for {}: {e}", spec.job_id);
        }
    }
}

/// Derive a [`DeviceClass`] from the offer's advertised device.
#[cfg(feature = "serve")]
fn device_class_from_offer(
    device: &vtessera_offer::AdvertisedDevice,
) -> vtessera_executor::DeviceClass {
    use vtessera_executor::DeviceClass;
    use vtessera_offer::AdvertisedDevice;
    match device {
        AdvertisedDevice::Cpu { .. } => DeviceClass::Cpu,
        AdvertisedDevice::NvidiaGpu { model, .. } => DeviceClass::NvidiaGpu {
            model: model.clone(),
        },
        AdvertisedDevice::NvidiaMig {
            parent_model,
            profile,
            ..
        } => DeviceClass::NvidiaMig {
            parent_model: parent_model.clone(),
            profile: profile.clone(),
        },
        AdvertisedDevice::NvidiaVgpu {
            parent_model,
            profile,
            ..
        } => DeviceClass::NvidiaVgpu {
            parent_model: parent_model.clone(),
            profile: profile.clone(),
        },
        AdvertisedDevice::AmdGpu { model, .. } => DeviceClass::AmdGpu {
            model: model.clone(),
        },
    }
}

/// First-come-first-served admission gate: with a claim gate configured, a
/// free job only runs if the submitting agent is the node's current claimant
/// (or the node is unclaimed and this submit claims it). Refusals are
/// `Err(HttpResponse)` so callers just return them.
///
/// Without `serve`, or with no index configured, this always admits — a
/// standalone node behaves as before.
#[cfg(feature = "serve")]
fn check_claim_gate(state: &NodeState, agent_id: Option<&str>) -> Result<(), HttpResponse> {
    let Some(index) = &state.index else {
        return Ok(());
    };
    let Some(agent) = agent_id else {
        return Err(HttpResponse::json(
            409,
            r#"{"status":"refused","reason":"agent identity required"}"#.into(),
        ));
    };
    match index.admit(agent) {
        Ok(()) => Ok(()),
        Err(index::AdmitError::Taken(owner)) => Err(HttpResponse::json(
            409,
            serde_json::to_string(&serde_json::json!({
                "status": "refused",
                "reason": format!("node claimed by {owner}"),
            }))
            .unwrap_or_else(|_| r#"{"status":"refused"}"#.into()),
        )),
        Err(index::AdmitError::Unreachable(reason)) => Err(HttpResponse::json(
            503,
            serde_json::to_string(&serde_json::json!({
                "status": "refused",
                "reason": "cannot verify claim availability",
                "detail": reason,
            }))
            .unwrap_or_else(|_| r#"{"status":"refused"}"#.into()),
        )),
    }
}

#[cfg(not(feature = "serve"))]
fn check_claim_gate(_state: &NodeState, _agent_id: Option<&str>) -> Result<(), HttpResponse> {
    Ok(())
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
    s.push_str("Free or paid (EURC/USDC, settled to the seller in the same stablecoin).\",");
    s.push_str("\"mimeType\":\"application/json\"");
    s.push_str("}],");
    s.push_str("\"tools\":[{");
    s.push_str("\"name\":\"submit_job\",");
    s.push_str("\"description\":\"Submit an OCI workload to this node. ");
    s.push_str("Free offers execute directly; paid offers return 402 (x402) ");
    s.push_str("until a signed payment is attached.\",");
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
            runner: None,
            verifier: None,
            state_dir: None,
            #[cfg(feature = "serve")]
            index: None,
        }
    }

    fn state_with_runner(price: PriceQuote, runner: impl JobRunner + 'static) -> NodeState {
        NodeState {
            runner: Some(Arc::new(runner)),
            ..state(price)
        }
    }

    /// Minimal valid `JobSpec` JSON body.
    fn valid_job_spec() -> Vec<u8> {
        br#"{"job_id":"j-1","image":"ghcr.io/example/echo:latest","command":[],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":1024,"min_vram_mb":0},"max_duration_secs":60}"#.to_vec()
    }

    /// Valid `JobSpec` with a caller-specified `job_id`.
    fn valid_job_spec_with_id(job_id: &str) -> Vec<u8> {
        format!(
            r#"{{"job_id":"{job_id}","image":"ghcr.io/example/echo:latest","command":[],"env":[],"devices":{{"class":{{"kind":"cpu"}},"vcpus":1,"mem_kb":1024,"min_vram_mb":0}},"max_duration_secs":60}}"#
        )
        .into_bytes()
    }

    /// Test runner: parses body as `JobSpec` and routes on `job_id`
    /// (when the `serve` feature is active), falling back to raw byte
    /// matching for non-serve paths.
    struct FakeRunner;
    impl JobRunner for FakeRunner {
        fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
            #[cfg(feature = "serve")]
            if let Ok(spec) = serde_json::from_slice::<vtessera_executor::JobSpec>(body) {
                return match spec.job_id.as_str() {
                    "boom" => Err(JobRunError::server("backend exploded")),
                    _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
                };
            }
            // Non-serve fallback: try a lightweight JSON check for "boom"
            // job_id (tests send valid JobSpec JSON in both feature modes).
            #[cfg(not(feature = "serve"))]
            if let Ok(v) = serde_json::from_slice::<serde_json::Value>(body) {
                return match v.get("job_id").and_then(|j| j.as_str()) {
                    Some("boom") => Err(JobRunError::server("backend exploded")),
                    _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
                };
            }
            match body {
                b"boom" => Err(JobRunError::server("backend exploded")),
                b"" => Err(JobRunError::bad_request("empty job body")),
                _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
            }
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
    fn free_jobs_post_is_refused_with_501_until_execution_is_wired() {
        let s = state(PriceQuote::Free);
        let mut r = req(HttpMethod::Post, "/jobs", vec![]);
        r.body = valid_job_spec();
        let r = dispatch(&s, r);
        assert_eq!(r.status, 501);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("\"status\":\"not-implemented\""));
        assert!(!body.contains("\"status\":\"accepted\""));
    }

    #[test]
    fn free_jobs_post_runs_through_the_wired_runner() {
        let s = state_with_runner(PriceQuote::Free, FakeRunner);
        let mut r = req(HttpMethod::Post, "/jobs", vec![]);
        r.body = valid_job_spec();
        let resp = dispatch(&s, r);
        assert_eq!(resp.status, 200);
        let body = String::from_utf8(resp.body).unwrap();
        assert!(body.contains("\"status\":\"accepted\""));
        assert!(body.contains("\"job_id\":\"j-1\""));
    }

    #[test]
    fn free_jobs_post_with_bad_body_returns_400() {
        let s = state_with_runner(PriceQuote::Free, FakeRunner);
        let resp = dispatch(&s, req(HttpMethod::Post, "/jobs", vec![]));
        assert_eq!(resp.status, 400);
        assert!(!String::from_utf8(resp.body)
            .unwrap()
            .contains("\"status\":\"accepted\""));
    }

    #[test]
    fn free_jobs_post_with_backend_failure_returns_500_from_runner() {
        let s = state_with_runner(PriceQuote::Free, FakeRunner);
        let mut r = req(HttpMethod::Post, "/jobs", vec![]);
        r.body = valid_job_spec_with_id("boom");
        let resp = dispatch(&s, r);
        assert_eq!(resp.status, 500);
        let body = String::from_utf8(resp.body).unwrap();
        assert!(body.contains("backend exploded"));
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
    fn paid_jobs_post_with_proof_is_refused_with_501_not_verified() {
        let s = state(paid());
        let r = dispatch(
            &s,
            req(HttpMethod::Post, "/jobs", vec![("x-payment", "0xPROOF")]),
        );
        assert_eq!(r.status, 501);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("payment verification not wired"));
        assert!(!body.contains("\"status\":\"accepted\""));
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

    #[test]
    fn parse_signed_offer_roundtrips_to_json() {
        let s = state(paid());
        let r = dispatch(&s, req(HttpMethod::Get, "/offer", vec![]));
        let body = String::from_utf8(r.body).unwrap();
        let parsed = parse_signed_offer(&body).expect("offer JSON should parse");
        assert_eq!(parsed.body.node_id, s.offer.body.node_id);
        assert_eq!(parsed.pubkey_hex, s.offer.pubkey_hex);
    }

    #[test]
    fn parse_signed_offer_rejects_garbage() {
        assert!(parse_signed_offer("not json").is_err());
    }

    #[test]
    fn mcp_post_over_http_handles_initialize() {
        let s = state(paid());
        let body = br#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}"#;
        let r = HttpRequest {
            method: HttpMethod::Post,
            path: "/mcp".into(),
            headers: vec![("content-type".into(), "application/json".into())],
            body: body.to_vec(),
        };
        let resp = dispatch(&s, r);
        assert_eq!(resp.status, 200);
        let text = String::from_utf8(resp.body).unwrap();
        assert!(text.contains("\"jsonrpc\":\"2.0\""));
        assert!(text.contains("\"serverInfo\""));
    }

    #[test]
    fn mcp_post_notification_returns_202_empty() {
        let s = state(paid());
        let body = br#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#;
        let r = HttpRequest {
            method: HttpMethod::Post,
            path: "/mcp".into(),
            headers: vec![],
            body: body.to_vec(),
        };
        let resp = dispatch(&s, r);
        assert_eq!(resp.status, 202);
        assert!(resp.body.is_empty());
    }

    #[test]
    fn agent_card_surfaces_skills() {
        let s = state(paid());
        let r = dispatch(&s, req(HttpMethod::Get, "/.well-known/agent.json", vec![]));
        assert_eq!(r.status, 200);
        let body = String::from_utf8(r.body).unwrap();
        assert!(body.contains("\"id\":\"submit_job\""));
        assert!(body.contains("\"tags\":[\"compute\"]"));
        assert!(body.contains("\"capabilities\""));
    }

    #[cfg(feature = "serve")]
    mod claim_gate {
        use super::*;
        use crate::index::{AdmitError, IndexClient, IndexQuery};
        use std::sync::{Arc, Mutex};

        struct FakeIndex {
            admit_result: Mutex<Result<(), AdmitError>>,
        }

        impl Default for FakeIndex {
            fn default() -> Self {
                Self {
                    admit_result: Mutex::new(Ok(())),
                }
            }
        }

        impl FakeIndex {
            fn with(result: Result<(), AdmitError>) -> Arc<Self> {
                Arc::new(Self {
                    admit_result: Mutex::new(result),
                })
            }
        }

        impl IndexClient for FakeIndex {
            fn admit(&self, _agent_id: &str) -> Result<(), AdmitError> {
                self.admit_result.lock().unwrap().clone()
            }

            fn discover(&self, _query: &IndexQuery) -> Result<String, String> {
                Ok(r#"{"count":0,"offers":[]}"#.into())
            }
        }

        fn job_req(headers: Vec<(&str, &str)>) -> HttpRequest {
            let mut r = req(HttpMethod::Post, "/jobs", headers);
            r.body = valid_job_spec();
            r
        }

        #[test]
        fn gated_job_runs_when_unclaimed_admits() {
            let mut s = state_with_runner(PriceQuote::Free, FakeRunner);
            s.index = Some(FakeIndex::with(Ok(())));
            let r = dispatch(&s, job_req(vec![("x-agent-id", "agent-a")]));
            assert_eq!(r.status, 200);
            assert!(String::from_utf8(r.body)
                .unwrap()
                .contains("\"status\":\"accepted\""));
        }

        #[test]
        fn gated_job_is_refused_when_taken() {
            let mut s = state_with_runner(PriceQuote::Free, FakeRunner);
            s.index = Some(FakeIndex::with(Err(AdmitError::Taken(
                "agent-other".into(),
            ))));
            let r = dispatch(&s, job_req(vec![("x-agent-id", "agent-a")]));
            assert_eq!(r.status, 409);
            assert!(String::from_utf8(r.body)
                .unwrap()
                .contains("node claimed by agent-other"));
        }

        #[test]
        fn gated_job_without_agent_id_is_refused() {
            let mut s = state_with_runner(PriceQuote::Free, FakeRunner);
            s.index = Some(FakeIndex::with(Ok(())));
            let r = dispatch(&s, job_req(vec![]));
            assert_eq!(r.status, 409);
            assert!(String::from_utf8(r.body)
                .unwrap()
                .contains("agent identity required"));
        }

        #[test]
        fn gated_job_fails_closed_when_index_unreachable() {
            let mut s = state_with_runner(PriceQuote::Free, FakeRunner);
            s.index = Some(FakeIndex::with(Err(AdmitError::Unreachable(
                "conn refused".into(),
            ))));
            let r = dispatch(&s, job_req(vec![("x-agent-id", "agent-a")]));
            assert_eq!(r.status, 503);
            assert!(String::from_utf8(r.body)
                .unwrap()
                .contains("cannot verify claim availability"));
        }

        #[test]
        fn no_index_behaves_as_standalone_node() {
            let s = state_with_runner(PriceQuote::Free, FakeRunner);
            let r = dispatch(&s, job_req(vec![]));
            assert_eq!(r.status, 200);
        }
    }
}
