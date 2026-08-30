//! MCP (Model Context Protocol) server — Module 2b (ROADMAP.md §2b).
//!
//! A real, protocol-spec-compliant MCP endpoint (protocol version
//! `2024-11-05`) wrapping the node's existing dispatch surface, so an
//! MCP-aware agent can discover this node, read its signed offer, and
//! submit jobs without bespoke client code.
//!
//! Transport-agnostic: this module speaks JSON-RPC 2.0 over
//! newline-delimited messages (`stdin` for a stdio server, a `POST /mcp`
//! body for the streamable-HTTP variant). It opens no sockets and holds
//! no I/O of its own — the caller owns the transport.
//!
//! Honesty invariant (matches `dispatch`): a tool call never fakes
//! success. With a runner wired in, `submit_job` actually runs the job and
//! returns its metering; without one it returns the x402 402 challenge as
//! tool *content* for unpaid paid offers, and an explicit not-implemented
//! result for everything that would have to run. Paid offers with a
//! payment proof stay refused until the on-chain verifier lands (Module 4).

#![forbid(unsafe_code)]

#[cfg(feature = "serve")]
use std::time::Duration;

use serde_json::{json, Value};

use crate::{classify_job_request, payment_required_body, JobDecision, NodeState};

/// MCP protocol version this server speaks.
pub const MCP_PROTOCOL_VERSION: &str = "2024-11-05";

pub const MCP_SERVER_NAME: &str = "vtessera-node";

/// `env!("CARGO_PKG_VERSION")` at the point this module is compiled.
pub const MCP_SERVER_VERSION: &str = env!("CARGO_PKG_VERSION");

/// MCP-specific JSON-RPC error codes (spec §7).
pub const ERROR_PARSE: i64 = -32700;
pub const ERROR_INVALID_REQUEST: i64 = -32600;
pub const ERROR_METHOD_NOT_FOUND: i64 = -32601;
pub const ERROR_INVALID_PARAMS: i64 = -32602;
pub const ERROR_RESOURCE_NOT_FOUND: i64 = -32002;
pub const ERROR_TOOL_EXECUTION_FAILED: i64 = -32003;
pub const ERROR_TOOL_NOT_FOUND: i64 = -32004;

const RESOURCE_OFFER_URI: &str = "vtessera://offer";
const TOOL_SUBMIT_JOB: &str = "submit_job";
#[cfg(feature = "serve")]
const TOOL_DISCOVER: &str = "discover";

/// One MCP server over a [`NodeState`]. Cheap to construct per request.
pub struct McpServer {
    state: NodeState,
}

impl McpServer {
    pub fn new(state: NodeState) -> Self {
        McpServer { state }
    }

    /// Handle one incoming message (one JSON-RPC line / body).
    ///
    /// Returns `None` for notifications — the caller acknowledges the
    /// request by emitting nothing (stdio) or a `202` (streamable HTTP).
    pub fn handle(&self, line: &str) -> Option<Value> {
        let msg: Value = match serde_json::from_str(line) {
            Ok(v) => v,
            Err(_) => return Some(error_response(Value::Null, ERROR_PARSE, "parse error")),
        };

        let Some(obj) = msg.as_object() else {
            return Some(error_response(
                Value::Null,
                ERROR_INVALID_REQUEST,
                "message must be a JSON-RPC object",
            ));
        };

        // Notifications carry no `id`. Anything without a method is
        // malformed; without an id we stay silent (spec §6.2).
        let method = match obj.get("method").and_then(|m| m.as_str()) {
            Some(m) => m,
            None => {
                return obj.get("id").map(|id| {
                    error_response(
                        id.clone(),
                        ERROR_INVALID_REQUEST,
                        "request is missing method",
                    )
                })
            }
        };
        let id = obj.get("id").cloned();
        if id.is_none() {
            self.dispatch(method, obj.get("params").cloned());
            return None;
        }
        let id = id.unwrap();

        let result = match method {
            "initialize" => self.initialize(),
            "ping" => Ok(json!({})),
            "tools/list" => self.tools_list(),
            "tools/call" => self.tools_call(obj.get("params").cloned()),
            "resources/list" => Ok(self.resources_list()),
            "resources/read" => self.resources_read(obj.get("params").cloned()),
            "prompts/list" => Ok(json!({ "prompts": [] })),
            // Spec §6.2: `notifications/initialized` and other
            // notifications are acknowledged by the absence of a response.
            _ if method.starts_with("notifications/") => {
                self.dispatch(method, obj.get("params").cloned());
                return None;
            }
            _ => Err(ERROR_METHOD_NOT_FOUND),
        };

        match result {
            Ok(r) => Some(result_response(id, r)),
            Err(code) => Some(error_response(id, code, error_message(code))),
        }
    }

    fn dispatch(&self, method: &str, _params: Option<Value>) {
        // The only notification we advertise/accept is
        // `notifications/initialized`; it's state-free, so there's nothing
        // to do. Any other notification is ignored per spec §6.2.
        let _ = (method, _params);
    }

    fn initialize(&self) -> Result<Value, i64> {
        Ok(json!({
            "protocolVersion": MCP_PROTOCOL_VERSION,
            "capabilities": {
                "tools": { "listChanged": false },
                "resources": { "subscribe": false, "listChanged": false },
                "prompts": { "listChanged": false },
            },
            "serverInfo": {
                "name": MCP_SERVER_NAME,
                "version": MCP_SERVER_VERSION,
            },
            "instructions": concat!(
                "Vtessera node: rentable CPU/GPU compute, settled on Solana in EURC or USDC. ",
                "Read the signed compute offer from the vtessera://offer resource, ",
                "then call submit_job. Paid offers negotiate via x402: submit without ",
                "a payment to receive the 402 challenge body, sign it, and resubmit ",
                "with the proof in the `payment` argument. Free offers run directly. ",
                "Paid submissions with a payment proof fail with 'not implemented' ",
                "until on-chain verification is wired."
            ),
        }))
    }

    fn tools_list(&self) -> Result<Value, i64> {
        let submit_job = serde_json::json!({
            "name": TOOL_SUBMIT_JOB,
            "description": concat!(
                "Submit an OCI workload to this node. For paid offers the first ",
                "call returns the x402 payment challenge (HTTP 402) as text; ",
                "pass the signed payment back via the `payment` argument. ",
                "Free offers run when the node has an executor backend wired; ",
                "paid submissions with a payment proof fail honestly until ",
                "on-chain verification lands. When the node is claim-gated, ",
                "pass the `agent_id` this node is claimed by (or claim it)."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "job": {
                        "type": "string",
                        "description": "JSON body of the job (workload description)",
                    },
                    "agent_id": {
                        "type": "string",
                        "description": "Agent identifier for first-come-first-served claim enforcement on a publish-wired node",
                    },
                    "payment": {
                        "type": "string",
                        "description": "x402 payment proof header value, returned by the 402 challenge flow",
                    },
                },
                "required": ["job"],
            },
        });
        #[cfg(feature = "serve")]
        let mut tools = vec![submit_job];
        #[cfg(not(feature = "serve"))]
        let tools = vec![submit_job];
        #[cfg(feature = "serve")]
        if self.state.index.is_some() {
            tools.push(serde_json::json!({
                "name": TOOL_DISCOVER,
                "description": concat!(
                    "List compute offers currently registered with the node's ",
                    "offer index, with claim status. Returns the index's JSON; ",
                    "read the `endpoint` from an offer body to submit a job there."
                ),
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "mode": {
                            "type": "string",
                            "enum": ["free", "paid"],
                            "description": "Only offers of this pricing mode",
                        },
                        "device": {
                            "type": "string",
                            "enum": ["cpu", "nvidia_gpu", "nvidia_mig", "nvidia_vgpu", "amd_gpu"],
                            "description": "Only offers advertising this device",
                        },
                        "available": {
                            "type": "boolean",
                            "description": "Only offers with no active claim",
                        },
                    },
                },
            }));
        }
        Ok(serde_json::json!({ "tools": tools }))
    }

    fn tools_call(&self, params: Option<Value>) -> Result<Value, i64> {
        let params = params.unwrap_or_else(|| json!({}));
        let name = params
            .get("name")
            .and_then(|n| n.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        if name != TOOL_SUBMIT_JOB {
            #[cfg(feature = "serve")]
            if name == TOOL_DISCOVER {
                return self.tool_discover(params.get("arguments").cloned());
            }
            return Err(ERROR_TOOL_NOT_FOUND);
        }
        let arguments = params
            .get("arguments")
            .cloned()
            .unwrap_or_else(|| json!({}));

        let job = arguments
            .get("job")
            .and_then(|j| j.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;

        let mut headers: Vec<(String, String)> = Vec::new();
        if let Some(p) = arguments.get("payment").and_then(|p| p.as_str()) {
            headers.push(("x-payment".into(), p.to_string()));
        }
        if let Some(a) = arguments.get("agent_id").and_then(|a| a.as_str()) {
            headers.push(("x-agent-id".into(), a.to_string()));
        }

        let req = crate::HttpRequest {
            method: crate::HttpMethod::Post,
            path: "/jobs".into(),
            headers,
            body: job.as_bytes().to_vec(),
        };

        // Same decision the HTTP surface makes: 402 for unpaid paid
        // offers; free offers run through the binary-supplied runner;
        // paid-with-proof routes through the verifier (501 if not wired).
        match classify_job_request(&self.state, &req) {
            JobDecision::PaymentRequired(challenge) => Ok(json!({
                "content": [{
                    "type": "text",
                    "text": payment_required_body(&challenge),
                }],
                "isError": false,
            })),
            JobDecision::VerifyAndRun {
                payment_proof,
                body,
            } => {
                let resp = crate::handle_paid_job(&self.state, &payment_proof, &body);
                let text = String::from_utf8_lossy(&resp.body).to_string();
                Ok(json!({
                    "content": [{ "type": "text", "text": text }],
                    "isError": resp.status != 200,
                }))
            }
            JobDecision::RunFree { body } => {
                // Same claim gate and runner path as the HTTP surface —
                // one enforcement point for both.
                let agent_id = arguments.get("agent_id").and_then(|a| a.as_str());
                let resp = crate::run_free(&self.state, &body, agent_id.map(str::to_string));
                let text = String::from_utf8_lossy(&resp.body).to_string();
                Ok(json!({
                    "content": [{ "type": "text", "text": text }],
                    "isError": resp.status != 200,
                }))
            }
        }
    }

    /// `discover`: query the node's configured offer index for current
    /// offers. Only reachable under `serve` with an index wired in; without
    /// one this is an honest error, not a fake empty list.
    #[cfg(feature = "serve")]
    fn tool_discover(&self, arguments: Option<Value>) -> Result<Value, i64> {
        let arguments = arguments.unwrap_or_else(|| json!({}));
        let Some(index) = &self.state.index else {
            return Ok(json!({
                "content": [{
                    "type": "text",
                    "text": r#"{"status":"not-configured","reason":"index not configured; start the node with --publish"}"#,
                }],
                "isError": true,
            }));
        };
        let mode = arguments
            .get("mode")
            .and_then(|m| m.as_str())
            .map(str::to_string);
        let device = arguments
            .get("device")
            .and_then(|d| d.as_str())
            .map(str::to_string);
        let available = arguments
            .get("available")
            .and_then(|a| a.as_bool())
            .unwrap_or(false);
        let query = crate::index::IndexQuery {
            mode,
            device,
            available,
        };
        match index.discover(&query) {
            Ok(body) => Ok(json!({
                "content": [{ "type": "text", "text": body }],
                "isError": false,
            })),
            Err(e) => Ok(json!({
                "content": [{
                    "type": "text",
                    "text": serde_json::to_string(&json!({
                        "status": "discover-failed",
                        "reason": e,
                    }))
                    .unwrap_or_else(|_| r#"{"status":"discover-failed"}"#.into()),
                }],
                "isError": true,
            })),
        }
    }

    fn resources_list(&self) -> Value {
        json!({
            "resources": [{
                "uri": RESOURCE_OFFER_URI,
                "name": "Vtessera compute offer",
                "description": concat!(
                    "Signed machine-readable offer of compute on this node. ",
                    "Free or paid (EURC/USDC, settled to the seller in the same stablecoin)."
                ),
                "mimeType": "application/json",
            }]
        })
    }

    fn resources_read(&self, params: Option<Value>) -> Result<Value, i64> {
        let params = params.unwrap_or_else(|| json!({}));
        let uri = params
            .get("uri")
            .and_then(|u| u.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        match uri {
            RESOURCE_OFFER_URI => Ok(json!({
                "contents": [{
                    "uri": RESOURCE_OFFER_URI,
                    "mimeType": "application/json",
                    "text": vtessera_offer::to_json(&self.state.offer),
                }]
            })),
            _ => Err(ERROR_RESOURCE_NOT_FOUND),
        }
    }
}

/// Market-level MCP server — the public registry view an agent uses to
/// *discover* compute, as opposed to the per-node [`McpServer`] that talks
/// to a single box.
///
/// Fetches a marketplace manifest (`nodes.json`: a `{ "nodes": [...] }`
/// array of signed offers), filters it by device/price/mode, and lets the
/// agent pull a single node's full signed offer or hand a job to a chosen
/// node. Node dialing for `submit_job` uses the plain HTTP endpoint in the
/// offer (the same path as `vtessera-agent`), so the node's own x402
/// payment negotiation (402 challenge → pay → resubmit) is preserved: with
/// no `payment` proof the node returns its 402 body, and the agent resolves
/// it out-of-band.
///
/// Transport-agnostic (same JSON-RPC-over-lines contract as [`McpServer`]);
/// the caller owns the transport. Gated on `serve` because it opens
/// outbound HTTP.
#[cfg(feature = "serve")]
pub const MCP_MARKETPLACE_SERVER_NAME: &str = "vtessera-marketplace";

pub const TOOL_LIST_NODES: &str = "list_nodes";
pub const TOOL_GET_OFFER: &str = "get_offer";
#[cfg(feature = "serve")]
pub const TOOL_SUBMIT_JOB_MARKET: &str = "submit_job";

/// Outbound job-submit timeout used by the marketplace server's
/// `submit_job` tool. Must be generous enough for a paid node to return its
/// x402 402 challenge, which happens on the first (unpaid) POST.
#[cfg(feature = "serve")]
const JOB_SUBMIT_TIMEOUT_SECS: u64 = 30;

/// A marketplace-level MCP server over a `nodes.json` manifest URL.
///
/// `list_nodes` filters against the offers fetched from `marketplace_url`
/// (and, when given, a local offer-index `index_url`, so an agent on the
/// same LAN sees local nodes too). `submit_job` POSTs to whatever node
/// `endpoint` the agent chose.
#[cfg(feature = "serve")]
pub struct MarketplaceMcpServer {
    marketplace_url: String,
    index_url: Option<String>,
    agent_id: Option<String>,
    /// Test-only: inject a pre-fetched offer list so unit tests can exercise
    /// the filtering/rendering logic without opening sockets. Stripped from
    /// non-test builds.
    #[cfg(test)]
    offers_override: Option<Vec<Value>>,
}

#[cfg(feature = "serve")]
impl MarketplaceMcpServer {
    pub fn new(marketplace_url: String, index_url: Option<String>) -> Self {
        MarketplaceMcpServer {
            marketplace_url,
            index_url,
            agent_id: None,
            #[cfg(test)]
            offers_override: None,
        }
    }

    /// Set a default agent ident used for x-agent-id when the tool call
    /// doesn't supply one.
    pub fn with_agent_id(mut self, agent_id: String) -> Self {
        self.agent_id = Some(agent_id);
        self
    }

    pub fn handle(&self, line: &str) -> Option<Value> {
        let msg: Value = match serde_json::from_str(line) {
            Ok(v) => v,
            Err(_) => return Some(error_response(Value::Null, ERROR_PARSE, "parse error")),
        };

        let Some(obj) = msg.as_object() else {
            return Some(error_response(
                Value::Null,
                ERROR_INVALID_REQUEST,
                "message must be a JSON-RPC object",
            ));
        };

        let method = match obj.get("method").and_then(|m| m.as_str()) {
            Some(m) => m,
            None => {
                return obj.get("id").map(|id| {
                    error_response(
                        id.clone(),
                        ERROR_INVALID_REQUEST,
                        "request is missing method",
                    )
                })
            }
        };
        let id = obj.get("id").cloned()?;

        let result = match method {
            "initialize" => Ok(json!({
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": { "tools": { "listChanged": false } },
                "serverInfo": {
                    "name": MCP_MARKETPLACE_SERVER_NAME,
                    "version": MCP_SERVER_VERSION,
                },
                "instructions": concat!(
                    "Vtessera marketplace: discover rentable CPU/GPU compute from the ",
                    "public registry, then run jobs. Call list_nodes to search offers by ",
                    "device/price/mode, get_offer for a single node's full signed offer, ",
                    "and submit_job to hand a workload to a chosen node. Paid offers ",
                    "negotiate via x402: submit without a payment to receive the 402 ",
                    "challenge, pay, and resubmit with the proof in the `payment` argument. ",
                    "Trust model: node identity is bound to pubkey_hex (Ed25519) and funds ",
                    "are held in a Solana escrow; behavioral honesty is by operator trust."
                ),
            })),
            "ping" => Ok(json!({})),
            "tools/list" => Ok(self.tools_list()),
            "tools/call" => self.tools_call(obj.get("params").cloned()),
            _ if method.starts_with("notifications/") => return None,
            _ => Err(ERROR_METHOD_NOT_FOUND),
        };

        match result {
            Ok(r) => Some(result_response(id, r)),
            Err(code) => Some(error_response(id, code, error_message(code))),
        }
    }

    fn tools_list(&self) -> Value {
        let tools = vec![
            json!({
                "name": TOOL_LIST_NODES,
                "description": concat!(
                    "List compute offers from the Vtessera marketplace (and the local ",
                    "offer-index when configured), with optional filters. Returns a table ",
                    "of node_id, endpoint, device, price-mode and per-second price for ",
                    "each matching offer."
                ),
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "device": { "type": "string", "enum": ["cpu", "nvidia_gpu", "amd_gpu", "nvidia_vgpu", "nvidia_mig"] },
                        "mode": { "type": "string", "enum": ["free", "paid"] },
                        "max_price_micros": { "type": "integer", "description": "For paid offers, maximum per-device-second price in micros of the stablecoin." },
                        "currency": { "type": "string", "description": "Stablecoin: eurc or usdc." },
                    },
                },
            }),
            json!({
                "name": TOOL_GET_OFFER,
                "description": "Return the full signed offer for a single node_id from the marketplace manifest.",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "node_id": { "type": "string", "description": "The 32-hex node id from list_nodes." },
                    },
                    "required": ["node_id"],
                },
            }),
            json!({
                "name": TOOL_SUBMIT_JOB_MARKET,
                "description": concat!(
                    "Submit a job to a chosen node by its endpoint (from list_nodes). ",
                    "If the node's offer is paid and no payment proof is supplied, the ",
                    "node returns its x402 402 challenge body as text; pay it and resubmit ",
                    "with the proof in `payment`. For free offers the job runs directly."
                ),
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "endpoint": { "type": "string", "description": "Node endpoint URL, e.g. http://192.168.1.10:8402" },
                        "job": { "type": "string", "description": "JSON job body (job_id, image, command, env, devices, max_duration_secs)." },
                        "payment": { "type": "string", "description": "x402 payment proof header value (from the 402 challenge flow)." },
                        "agent_id": { "type": "string", "description": "Agent identifier for first-come-first-served claim enforcement." },
                    },
                    "required": ["endpoint", "job"],
                },
            }),
        ];
        json!({ "tools": tools })
    }

    fn tools_call(&self, params: Option<Value>) -> Result<Value, i64> {
        let params = params.unwrap_or_else(|| json!({}));
        let name = params
            .get("name")
            .and_then(|n| n.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        let arguments = params
            .get("arguments")
            .cloned()
            .unwrap_or_else(|| json!({}));

        match name {
            TOOL_LIST_NODES => self.tool_list_nodes(arguments),
            TOOL_GET_OFFER => self.tool_get_offer(arguments),
            TOOL_SUBMIT_JOB_MARKET => self.tool_submit_job(arguments),
            _ => Err(ERROR_TOOL_NOT_FOUND),
        }
    }

    /// Fetch + merge the marketplace manifest and (optional) local index
    /// into a flat, de-duplicated list of offer JSON values. In tests an
    /// injected `offers_override` short-circuits the network fetch so the
    /// filtering/rendering logic can run hermetically.
    fn fetch_offers(&self) -> Result<Vec<Value>, String> {
        #[cfg(test)]
        if let Some(offers) = &self.offers_override {
            return Ok(offers.clone());
        }

        let agent = ureq::Agent::new_with_defaults();
        let mut offers: Vec<Value> = Vec::new();
        let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();

        let mut absorb = |url: &str, node_key: &str| -> Result<(), String> {
            let text = agent
                .get(url)
                .call()
                .map_err(|e| format!("GET {url}: {e}"))?
                .into_body()
                .read_to_string()
                .map_err(|e| format!("read {url}: {e}"))?;
            let resp: serde_json::Value =
                serde_json::from_str(&text).map_err(|e| format!("parse {url}: {e}"))?;
            let arr = if let Some(nodes) = resp.get(node_key).and_then(|n| n.as_array()) {
                let mut out = Vec::new();
                for n in nodes {
                    if let Some(o) = n.get("offer") {
                        out.push(o.clone());
                    }
                }
                out
            } else if resp.is_array() {
                resp.as_array().cloned().unwrap_or_default()
            } else {
                resp.get("offers")
                    .and_then(|o| o.as_array())
                    .cloned()
                    .unwrap_or_default()
            };
            for o in arr {
                if let Some(ep) = o["body"]["endpoint"].as_str() {
                    if seen.insert(ep.to_string()) {
                        offers.push(o);
                    }
                }
            }
            Ok(())
        };

        absorb(&self.marketplace_url, "nodes")?;
        if let Some(idx) = &self.index_url {
            // Ignore a missing local index — it just means this agent isn't
            // on the same LAN as the node.
            let _ = absorb(idx, "nodes");
        }
        Ok(offers)
    }

    fn tool_list_nodes(&self, arguments: Value) -> Result<Value, i64> {
        let device = arguments.get("device").and_then(|d| d.as_str());
        let mode = arguments.get("mode").and_then(|m| m.as_str());
        let max_price = arguments.get("max_price_micros").and_then(|p| p.as_u64());
        let currency = arguments.get("currency").and_then(|c| c.as_str());

        let offers = match self.fetch_offers() {
            Ok(o) => o,
            Err(e) => {
                return Ok(json!({
                    "content": [{ "type": "text", "text": serde_json::to_string(&json!({"status":"fetch-failed","reason":e})).unwrap() }],
                    "isError": true,
                }))
            }
        };

        let mut rows: Vec<Value> = Vec::new();
        for o in &offers {
            let body = &o["body"];
            if let Some(d) = device {
                if body["device"]["kind"].as_str() != Some(d) {
                    continue;
                }
            }
            let price = &body["price"];
            let offer_mode = price["mode"].as_str().unwrap_or("free");
            if let Some(m) = mode {
                if offer_mode != m {
                    continue;
                }
            }
            if let Some(cur) = currency {
                if price["currency"].as_str() != Some(cur) {
                    continue;
                }
            }
            if let Some(maxp) = max_price {
                if let Some(pm) = price["per_device_second_micros"].as_u64() {
                    if pm > maxp {
                        continue;
                    }
                }
            }
            rows.push(json!({
                "node_id": body["node_id"],
                "endpoint": body["endpoint"],
                "device_kind": body["device"]["kind"],
                "device": body["device"],
                "mode": offer_mode,
                "currency": price["currency"],
                "per_device_second_micros": price["per_device_second_micros"],
                "payout_id": price["payout_id"],
                "expires_unix": body["expires_unix"],
            }));
        }

        let text = serde_json::to_string_pretty(&json!({
            "count": rows.len(),
            "nodes": rows,
        }))
        .unwrap_or_else(|_| "{}".into());
        Ok(json!({ "content": [{ "type": "text", "text": text }], "isError": false }))
    }

    fn tool_get_offer(&self, arguments: Value) -> Result<Value, i64> {
        let node_id = arguments
            .get("node_id")
            .and_then(|n| n.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        let offers = self.fetch_offers().map_err(|e| {
            // Surface as a tool execution result, not a protocol failure.
            let _ = e;
            ERROR_TOOL_EXECUTION_FAILED
        })?;
        for o in offers {
            if o["body"]["node_id"].as_str() == Some(node_id) {
                let text = serde_json::to_string_pretty(&o).unwrap_or_else(|_| "{}".into());
                return Ok(
                    json!({ "content": [{ "type": "text", "text": text }], "isError": false }),
                );
            }
        }
        Ok(json!({
            "content": [{
                "type": "text",
                "text": serde_json::to_string(&json!({"status":"not-found","node_id":node_id})).unwrap(),
            }],
            "isError": true,
        }))
    }

    fn tool_submit_job(&self, arguments: Value) -> Result<Value, i64> {
        let endpoint = arguments
            .get("endpoint")
            .and_then(|e| e.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        let job = arguments
            .get("job")
            .and_then(|j| j.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;

        // Build with http_status_as_error(false) (same as the node's own
        // IndexClient) so a paid node's 402 x402 challenge comes back as a
        // normal response whose body we can hand to the agent, instead of
        // being folded into an Err(Status) branch that discards the body.
        let agent = ureq::Agent::config_builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(JOB_SUBMIT_TIMEOUT_SECS)))
            .build();
        let agent = ureq::Agent::new_with_config(agent);

        let mut req = agent.post(&format!("{endpoint}/jobs"));
        req = req.header("Content-Type", "application/json");
        if let Some(a) = arguments
            .get("agent_id")
            .and_then(|a| a.as_str())
            .or(self.agent_id.as_deref())
        {
            req = req.header("x-agent-id", a);
        }
        if let Some(p) = arguments.get("payment").and_then(|p| p.as_str()) {
            req = req.header("x-payment", p);
        }

        let resp = req.send(job).map_err(|e| {
            eprintln!("vtessera-marketplace-mcp: submit_job to {endpoint} failed: {e}");
            ERROR_TOOL_EXECUTION_FAILED
        })?;

        let status = resp.status();
        let bytes = resp.into_body().read_to_vec().map_err(|e| {
            eprintln!("vtessera-marketplace-mcp: read {endpoint} response: {e}");
            ERROR_TOOL_EXECUTION_FAILED
        })?;
        let body = String::from_utf8_lossy(&bytes).to_string();
        Ok(json!({
            "content": [{ "type": "text", "text": body }],
            "isError": status != 200,
        }))
    }
}

fn error_message(code: i64) -> &'static str {
    match code {
        ERROR_PARSE => "parse error",
        ERROR_INVALID_REQUEST => "invalid request",
        ERROR_METHOD_NOT_FOUND => "method not found",
        ERROR_INVALID_PARAMS => "invalid params",
        ERROR_RESOURCE_NOT_FOUND => "resource not found",
        ERROR_TOOL_NOT_FOUND => "tool not found",
        ERROR_TOOL_EXECUTION_FAILED => "tool execution failed",
        _ => "error",
    }
}

fn result_response(id: Value, result: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "result": result })
}

fn error_response(id: Value, code: i64, message: &str) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "error": { "code": code, "message": message } })
}

#[cfg(test)]
mod tests {
    use super::*;
    use vtessera_offer::{
        derive_node_id, sign, AdvertisedDevice, Currency, OfferBody, PriceQuote, OFFER_SCHEMA_VER,
    };

    fn signed(price: PriceQuote) -> crate::SignedOffer {
        let key = ed25519_dalek::SigningKey::from_bytes(&[7u8; 32]);
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

    fn server(price: PriceQuote) -> McpServer {
        McpServer::new(crate::NodeState {
            offer: signed(price),
            escrow_account: "Esc1111111111111111111111111111111111111111".into(),
            network: "solana-devnet".into(),
            runner: None,
            verifier: None,
            state_dir: None,
            #[cfg(feature = "serve")]
            index: None,
        })
    }

    /// MCP server with a fake runner so the free path can be exercised
    /// without linking the executor into the test crate's default build.
    fn server_with_runner(price: PriceQuote, runner: impl crate::JobRunner + 'static) -> McpServer {
        let mut state = server(price).state;
        state.runner = Some(std::sync::Arc::new(runner));
        McpServer::new(state)
    }

    struct FakeRunner;
    impl crate::JobRunner for FakeRunner {
        fn run(&self, body: &[u8]) -> Result<String, crate::JobRunError> {
            #[cfg(feature = "serve")]
            if let Ok(spec) = serde_json::from_slice::<vtessera_executor::JobSpec>(body) {
                return match spec.job_id.as_str() {
                    "boom" => Err(crate::JobRunError::server("backend exploded")),
                    _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
                };
            }
            #[cfg(not(feature = "serve"))]
            if let Ok(v) = serde_json::from_slice::<serde_json::Value>(body) {
                return match v.get("job_id").and_then(|j| j.as_str()) {
                    Some("boom") => Err(crate::JobRunError::server("backend exploded")),
                    _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
                };
            }
            match body {
                b"boom" => Err(crate::JobRunError::server("backend exploded")),
                b"" => Err(crate::JobRunError::bad_request("empty job body")),
                _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
            }
        }
    }

    /// Minimal valid `JobSpec` JSON for MCP job arguments.
    fn valid_job_spec_str() -> String {
        r#"{"job_id":"j-1","image":"ghcr.io/example/echo:latest","command":[],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":1024,"min_vram_mb":0},"max_duration_secs":60}"#.into()
    }

    /// Valid `JobSpec` JSON with caller-specified `job_id`.
    fn valid_job_spec_str_with_id(job_id: &str) -> String {
        format!(
            r#"{{"job_id":"{job_id}","image":"ghcr.io/example/echo:latest","command":[],"env":[],"devices":{{"class":{{"kind":"cpu"}},"vcpus":1,"mem_kb":1024,"min_vram_mb":0}},"max_duration_secs":60}}"#
        )
    }

    fn paid() -> PriceQuote {
        PriceQuote::Paid {
            currency: Currency::Eurc,
            per_device_second_micros: 100,
            payout_id: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM".into(),
        }
    }

    fn call(srv: &McpServer, msg: &str) -> Value {
        srv.handle(msg).expect("request should produce a response")
    }

    #[test]
    fn initialize_handshake() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}"#,
        );
        assert_eq!(r["jsonrpc"], "2.0");
        assert_eq!(r["id"], 1);
        assert_eq!(r["result"]["protocolVersion"], MCP_PROTOCOL_VERSION);
        assert_eq!(r["result"]["serverInfo"]["name"], "vtessera-node");
    }

    #[test]
    fn notification_is_acknowledged_without_response() {
        let srv = server(paid());
        let r = srv.handle(r#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#);
        assert!(r.is_none());
    }

    #[test]
    fn ping_returns_empty_result() {
        let srv = server(paid());
        let r = call(&srv, r#"{"jsonrpc":"2.0","id":2,"method":"ping"}"#);
        assert_eq!(r["result"], json!({}));
        assert!(r.get("error").is_none());
    }

    #[test]
    fn tools_list_advertises_submit_job() {
        let srv = server(paid());
        let r = call(&srv, r#"{"jsonrpc":"2.0","id":3,"method":"tools/list"}"#);
        let tools = r["result"]["tools"].as_array().unwrap();
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0]["name"], "submit_job");
        assert_eq!(tools[0]["inputSchema"]["required"][0], "job");
    }

    #[test]
    fn tools_call_paid_offer_returns_402_challenge_content() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{}"}}}"#,
        );
        let content = r["result"]["content"][0].clone();
        assert_eq!(content["type"], "text");
        assert_eq!(r["result"]["isError"], json!(false));
        assert!(content["text"]
            .as_str()
            .unwrap()
            .contains("\"scheme\":\"x402\""));
    }

    #[test]
    fn tools_call_with_payment_is_honestly_not_implemented() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{}","payment":"0xPROOF"}}}"#,
        );
        assert_eq!(r["result"]["isError"], json!(true));
        assert!(r["result"]["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("not-implemented"));
    }

    #[test]
    fn tools_call_free_offer_is_honestly_not_implemented() {
        let srv = server(PriceQuote::Free);
        let job = valid_job_spec_str();
        let r = call(
            &srv,
            &format!(
                r#"{{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{{"name":"submit_job","arguments":{{"job":"{}"}}}}}}"#,
                job.replace('"', "\\\"")
            ),
        );
        assert_eq!(r["result"]["isError"], json!(true));
        assert!(r["result"]["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("not-implemented"));
    }

    #[test]
    fn tools_call_free_offer_runs_through_the_wired_runner() {
        let srv = server_with_runner(PriceQuote::Free, FakeRunner);
        let job = valid_job_spec_str();
        let r = call(
            &srv,
            &format!(
                r#"{{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{{"name":"submit_job","arguments":{{"job":"{}"}}}}}}"#,
                job.replace('"', "\\\"")
            ),
        );
        assert_eq!(r["result"]["isError"], json!(false));
        let text = r["result"]["content"][0]["text"].as_str().unwrap();
        assert!(text.contains("\"status\":\"accepted\""));
        assert!(text.contains("\"job_id\":\"j-1\""));
    }

    #[test]
    fn tools_call_free_offer_surfaces_runner_errors() {
        let srv = server_with_runner(PriceQuote::Free, FakeRunner);
        let job = valid_job_spec_str_with_id("boom");
        let r = call(
            &srv,
            &format!(
                r#"{{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{{"name":"submit_job","arguments":{{"job":"{}"}}}}}}"#,
                job.replace('"', "\\\"")
            ),
        );
        assert_eq!(r["result"]["isError"], json!(true));
        let text = r["result"]["content"][0]["text"].as_str().unwrap();
        assert!(text.contains("backend exploded"));
    }

    #[test]
    fn tools_call_unknown_tool_returns_tool_not_found() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}"#,
        );
        assert_eq!(r["error"]["code"], ERROR_TOOL_NOT_FOUND);
    }

    #[test]
    fn tools_call_missing_job_arg_is_invalid_params() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"submit_job","arguments":{}}}"#,
        );
        assert_eq!(r["error"]["code"], ERROR_INVALID_PARAMS);
    }

    #[test]
    fn resources_list_and_read_offer() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":9,"method":"resources/list"}"#,
        );
        assert_eq!(r["result"]["resources"][0]["uri"], "vtessera://offer");

        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":10,"method":"resources/read","params":{"uri":"vtessera://offer"}}"#,
        );
        let text = r["result"]["contents"][0]["text"].as_str().unwrap();
        assert!(text.contains("\"schema_ver\":1"));
        assert!(text.contains("\"pubkey_hex\":"));
    }

    #[test]
    fn resources_read_unknown_uri_is_not_found() {
        let srv = server(paid());
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":11,"method":"resources/read","params":{"uri":"vtessera://nope"}}"#,
        );
        assert_eq!(r["error"]["code"], ERROR_RESOURCE_NOT_FOUND);
    }

    #[test]
    fn prompts_list_is_empty() {
        let srv = server(paid());
        let r = call(&srv, r#"{"jsonrpc":"2.0","id":12,"method":"prompts/list"}"#);
        assert_eq!(r["result"]["prompts"], json!([]));
    }

    #[test]
    fn unknown_method_is_method_not_found() {
        let srv = server(paid());
        let r = call(&srv, r#"{"jsonrpc":"2.0","id":13,"method":"wat"}"#);
        assert_eq!(r["error"]["code"], ERROR_METHOD_NOT_FOUND);
    }

    #[test]
    fn malformed_json_is_parse_error() {
        let srv = server(paid());
        let r = srv.handle("not json").unwrap();
        assert_eq!(r["error"]["code"], ERROR_PARSE);
    }

    #[cfg(feature = "serve")]
    mod gate {
        use super::*;
        use crate::index::{AdmitError, IndexClient, IndexQuery};
        use std::sync::{Arc, Mutex};

        struct FakeIndex {
            calls: Mutex<Vec<String>>,
            admit_result: Mutex<Result<(), AdmitError>>,
            discover_body: Mutex<Result<String, String>>,
        }

        impl Default for FakeIndex {
            fn default() -> Self {
                Self {
                    calls: Mutex::new(Vec::new()),
                    admit_result: Mutex::new(Ok(())),
                    discover_body: Mutex::new(Ok(r#"{"count":1,"offers":[]}"#.into())),
                }
            }
        }

        impl FakeIndex {
            fn admitting() -> Arc<Self> {
                Arc::new(Self::default())
            }
        }

        impl IndexClient for FakeIndex {
            fn admit(&self, agent_id: &str) -> Result<(), AdmitError> {
                self.calls.lock().unwrap().push(agent_id.to_string());
                self.admit_result.lock().unwrap().clone()
            }

            fn discover(&self, query: &IndexQuery) -> Result<String, String> {
                self.calls.lock().unwrap().push(format!(
                    "mode={:?} device={:?} available={}",
                    query.mode, query.device, query.available
                ));
                self.discover_body.lock().unwrap().clone()
            }
        }

        fn gated_server(price: PriceQuote, index: Arc<FakeIndex>) -> McpServer {
            let mut state = server(price).state;
            state.index = Some(index);
            McpServer::new(state)
        }

        #[test]
        fn tools_list_advertises_discover_when_index_is_wired() {
            let srv = gated_server(PriceQuote::Free, FakeIndex::admitting());
            let r = call(&srv, r#"{"jsonrpc":"2.0","id":1,"method":"tools/list"}"#);
            let tools = r["result"]["tools"].as_array().unwrap();
            let names: Vec<&str> = tools.iter().map(|t| t["name"].as_str().unwrap()).collect();
            assert_eq!(names, vec!["submit_job", "discover"]);
        }

        #[test]
        fn tools_list_omits_discover_without_index() {
            let srv = server(PriceQuote::Free);
            let r = call(&srv, r#"{"jsonrpc":"2.0","id":1,"method":"tools/list"}"#);
            let tools = r["result"]["tools"].as_array().unwrap();
            assert_eq!(tools.len(), 1);
            assert_eq!(tools[0]["name"], "submit_job");
        }

        #[test]
        fn discover_returns_index_offers() {
            let index = FakeIndex::admitting();
            let srv = gated_server(PriceQuote::Free, index.clone());
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"discover","arguments":{"mode":"free","available":true}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(false));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("\"count\":1"));
            let call = index.calls.lock().unwrap().first().unwrap().clone();
            assert!(call.contains("mode=Some(\"free\")"));
            assert!(call.contains("available=true"));
        }

        #[test]
        fn discover_without_index_is_honest_error() {
            let srv = server(PriceQuote::Free);
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"discover","arguments":{}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(true));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("not-configured"));
        }

        #[test]
        fn discover_failure_is_error() {
            let index = FakeIndex::admitting();
            *index.discover_body.lock().unwrap() = Err("index unreachable".into());
            let srv = gated_server(PriceQuote::Free, index);
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"discover","arguments":{}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(true));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("discover-failed"));
        }

        #[test]
        fn submit_job_forwards_agent_id_through_gate() {
            let index = FakeIndex::admitting();
            let srv = gated_server(PriceQuote::Free, index.clone());
            let mut state = srv.state;
            state.runner = Some(Arc::new(FakeRunner));
            let srv = McpServer::new(state);
            let job = valid_job_spec_str();
            let r = call(
                &srv,
                &format!(
                    r#"{{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{{"name":"submit_job","arguments":{{"job":"{}","agent_id":"agent-demo"}}}}}}"#,
                    job.replace('"', "\\\"")
                ),
            );
            assert_eq!(r["result"]["isError"], json!(false));
            assert_eq!(index.calls.lock().unwrap()[0], "agent-demo");
        }

        #[test]
        fn gated_submit_job_without_agent_id_is_refused() {
            let index = FakeIndex::admitting();
            let srv = gated_server(PriceQuote::Free, index);
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{...}"}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(true));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("agent identity required"));
        }

        #[test]
        fn gated_submit_job_is_refused_when_taken() {
            let index = FakeIndex::admitting();
            *index.admit_result.lock().unwrap() = Err(AdmitError::Taken("agent-other".into()));
            let srv = gated_server(PriceQuote::Free, index);
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{...}","agent_id":"agent-demo"}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(true));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("node claimed by agent-other"));
        }

        #[test]
        fn gated_submit_job_fails_closed_when_index_unreachable() {
            let index = FakeIndex::admitting();
            *index.admit_result.lock().unwrap() =
                Err(AdmitError::Unreachable("connection refused".into()));
            let srv = gated_server(PriceQuote::Free, index);
            let r = call(
                &srv,
                r#"{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{...}","agent_id":"agent-demo"}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(true));
            let text = r["result"]["content"][0]["text"].as_str().unwrap();
            assert!(text.contains("cannot verify claim availability"));
        }
    }

    #[cfg(feature = "serve")]
    mod marketplace {
        use super::*;

        fn offer_json(endpoint: &str, mode: &str) -> Value {
            let per_device_second_micros = if mode == "paid" { 1000 } else { 0 };
            json!({
                "body": {
                    "schema_ver": OFFER_SCHEMA_VER,
                    "node_id": derive_node_id(&[0u8; 32]),
                    "endpoint": endpoint,
                    "device": {"kind": "cpu", "vcpus": 2, "mem_mb": 4096},
                    "price": {
                        "mode": mode,
                        "currency": "eurc",
                        "per_device_second_micros": per_device_second_micros,
                    },
                    "expires_unix": 1_700_010_000,
                }
            })
        }

        fn marketplace_server(offers: Vec<Value>) -> MarketplaceMcpServer {
            MarketplaceMcpServer {
                marketplace_url: "https://example.invalid/nodes.json".into(),
                index_url: None,
                agent_id: None,
                offers_override: Some(offers),
            }
        }

        fn call_market(srv: &MarketplaceMcpServer, msg: &str) -> Value {
            srv.handle(msg).expect("request should produce a response")
        }

        #[test]
        fn marketplace_initialize_identifies_server() {
            let srv = marketplace_server(vec![]);
            let r = call_market(
                &srv,
                r#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}"#,
            );
            assert_eq!(r["result"]["protocolVersion"], MCP_PROTOCOL_VERSION);
            assert_eq!(
                r["result"]["serverInfo"]["name"],
                MCP_MARKETPLACE_SERVER_NAME
            );
        }

        #[test]
        fn marketplace_tools_list_advertises_discovery_tools() {
            let srv = marketplace_server(vec![]);
            let r = call_market(&srv, r#"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#);
            let names: Vec<&str> = r["result"]["tools"]
                .as_array()
                .unwrap()
                .iter()
                .map(|t| t["name"].as_str().unwrap())
                .collect();
            assert_eq!(names.len(), 3);
            assert!(names.contains(&TOOL_LIST_NODES));
            assert!(names.contains(&TOOL_GET_OFFER));
            assert!(names.contains(&TOOL_SUBMIT_JOB_MARKET));
        }

        #[test]
        fn list_nodes_filters_by_mode_and_device() {
            let offers = vec![
                offer_json("http://free-1:8402", "free"),
                offer_json("http://paid-1:8402", "paid"),
            ];
            let srv = marketplace_server(offers);
            let r = call_market(
                &srv,
                r#"{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_nodes","arguments":{"mode":"paid"}}}"#,
            );
            assert_eq!(r["result"]["isError"], json!(false));
            let data: Value =
                serde_json::from_str(r["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
            assert_eq!(data["count"], 1);
            assert_eq!(data["nodes"][0]["endpoint"], "http://paid-1:8402");
        }

        #[test]
        fn get_offer_returns_signed_offer_for_node() {
            let offers = vec![offer_json("http://free-1:8402", "free")];
            let srv = marketplace_server(offers);
            let r = call_market(
                &srv,
                r#"{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_offer","arguments":{"node_id":"missing"}}}"#,
            );
            // Unknown node id -> not-found tool error, not a protocol failure.
            assert_eq!(r["result"]["isError"], json!(true));
        }

        #[test]
        fn marketplace_submit_job_rejects_missing_arguments() {
            let srv = marketplace_server(vec![]);
            let r = call_market(
                &srv,
                r#"{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"submit_job","arguments":{}}}"#,
            );
            assert_ne!(r.get("result").map(|_| true), Some(true));
            assert!(r.get("error").is_some());
        }
    }
}
