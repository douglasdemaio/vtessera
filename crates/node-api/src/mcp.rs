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
        Ok(json!({
            "tools": [{
                "name": TOOL_SUBMIT_JOB,
                "description": concat!(
                    "Submit an OCI workload to this node. For paid offers the first ",
                    "call returns the x402 payment challenge (HTTP 402) as text; ",
                    "pass the signed payment back via the `payment` argument. Job ",
                    "execution is not yet wired in v0, so submissions currently ",
                    "fail with 'not implemented'."
                ),
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "job": {
                            "type": "string",
                            "description": "JSON body of the job (workload description)",
                        },
                        "payment": {
                            "type": "string",
                            "description": "x402 payment proof header value, returned by the 402 challenge flow",
                        },
                    },
                    "required": ["job"],
                },
            }]
        }))
    }

    fn tools_call(&self, params: Option<Value>) -> Result<Value, i64> {
        let params = params.unwrap_or_else(|| json!({}));
        let name = params
            .get("name")
            .and_then(|n| n.as_str())
            .ok_or(ERROR_INVALID_PARAMS)?;
        if name != TOOL_SUBMIT_JOB {
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

        let req = crate::HttpRequest {
            method: crate::HttpMethod::Post,
            path: "/jobs".into(),
            headers,
            body: job.as_bytes().to_vec(),
        };

        // Same decision the HTTP surface makes: 402 for unpaid paid
        // offers; free offers run through the binary-supplied runner;
        // paid-with-proof stays an honest refusal until the verifier lands.
        match classify_job_request(&self.state, &req) {
            JobDecision::PaymentRequired(challenge) => Ok(json!({
                "content": [{
                    "type": "text",
                    "text": payment_required_body(&challenge),
                }],
                "isError": false,
            })),
            JobDecision::VerifyAndRun { .. } => Ok(json!({
                "content": [{
                    "type": "text",
                    "text": r#"{"status":"not-implemented","reason":"payment proof was not verified; on-chain verification is not wired"}"#,
                }],
                "isError": true,
            })),
            JobDecision::RunFree { body } => match &self.state.runner {
                Some(runner) => match runner.run(&body) {
                    Ok(json) => Ok(json!({
                        "content": [{ "type": "text", "text": json }],
                        "isError": false,
                    })),
                    Err(e) => Ok(json!({
                        "content": [{ "type": "text", "text": e.message }],
                        "isError": true,
                    })),
                },
                None => Ok(json!({
                    "content": [{
                        "type": "text",
                        "text": r#"{"status":"not-implemented","reason":"job execution is not wired; start the node with an executor backend"}"#,
                    }],
                    "isError": true,
                })),
            },
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
            match body {
                b"boom" => Err(crate::JobRunError::server("backend exploded")),
                b"" => Err(crate::JobRunError::bad_request("empty job body")),
                _ => Ok(r#"{"status":"accepted","job_id":"j-1"}"#.into()),
            }
        }
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
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{}"}}}"#,
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
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"{...}"}}}"#,
        );
        assert_eq!(r["result"]["isError"], json!(false));
        let text = r["result"]["content"][0]["text"].as_str().unwrap();
        assert!(text.contains("\"status\":\"accepted\""));
        assert!(text.contains("\"job_id\":\"j-1\""));
    }

    #[test]
    fn tools_call_free_offer_surfaces_runner_errors() {
        let srv = server_with_runner(PriceQuote::Free, FakeRunner);
        let r = call(
            &srv,
            r#"{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"submit_job","arguments":{"job":"boom"}}}"#,
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
}
