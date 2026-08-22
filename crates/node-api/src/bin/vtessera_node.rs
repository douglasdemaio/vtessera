//! Minimal HTTP server binding the node-api dispatcher to a TCP socket.
//!
//! The HTTP/1.1 parsing and connection handling live in `vtessera-mini-http`
//! — one audited parser shared by all agent-facing binaries (no tokio, no
//! hyper, no axum). For production deployments behind a real reverse proxy
//! this is fine; for serving direct internet traffic, front it with
//! something that does TLS termination and request size caps before this
//! process sees a byte.
//!
//! This binary is the **composition root**: it supplies the executor backend
//! (ROADMAP.md §1) that the node-api library — deliberately executor-free —
//! invokes through its `JobRunner` hook. Free-offer jobs run synchronously
//! here and the metering comes back in the response. Paid offers still
//! refuse until the on-chain payment verifier lands (Module 4).
//!
//! Behind the `serve` feature so `cargo build -p vtessera-node-api`
//! still produces a library that opens no sockets (matching v0's
//! no-inbound-network guarantee).
//!
//! Run:
//!
//!   cargo run -p vtessera-node-api --bin vtessera-node --features serve \
//!     -- --bind 127.0.0.1:8402 --offer offer.json --escrow <PDA> \
//!        --network solana-devnet [--backend noop-cpu|local-cpu]
//!
//! Where `offer.json` is the JSON output of `vtessera_offer::to_json`.
//!
//! `--backend` selects the executor:
//!   - `noop-cpu` (default) — returns synthetic metering; safe for CI and
//!     the devnet demo, never a production choice.
//!   - `local-cpu` — runs the job's command on the host. **Not isolated**
//!     (no cgroups/namespaces). Only choose this for trusted workloads.
//!   - `cloud-hypervisor` — production isolation (ROADMAP.md §1): each job
//!     boots a disposable microVM (host kernel + custom initramfs) with no
//!     guest network. Requires `/dev/kvm`, `cloud-hypervisor`, and an
//!     initramfs built by `scripts/build-initramfs.sh`.
//!
//! Routes (see `vtessera_node_api::dispatch`):
//!
//!   GET  /offer
//!   GET  /mcp/manifest            (legacy MCP manifest)
//!   POST /mcp                     (MCP 2024-11-05 JSON-RPC)
//!   GET  /.well-known/agent.json  (A2A agent card)
//!   POST /jobs                    (x402 challenge / free-job execution)
//!   GET  /healthz
//!
//! Offer-index wiring (Module 2a): with `--publish <index-url>` the node
//! registers its signed offer with the index on startup and refreshes it
//! every `--publish-interval` seconds, and enforces first-come-first-served
//! claims through the index: a free job only runs if the submitting agent
//! (the `X-Agent-Id` header) is the node's current claimant — or the node is
//! unclaimed and this submit claims it. Without `--publish` the node is
//! standalone and anonymous free jobs run as before.

use std::env;
use std::fs;
use std::io::BufReader;
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use vtessera_executor::{Backend, Executor, ExecutorError, JobMetering, JobSpec};
use vtessera_mini_http::{serve, Method as MiniMethod, Request as MiniRequest, Response};
use vtessera_node_api::index::{AdmitError, IndexClient, IndexQuery};
use vtessera_node_api::{
    dispatch, parse_signed_offer, HttpMethod, HttpRequest, JobRunError, JobRunner, NodeState,
    PaymentVerifier, PaymentVerifyError,
};
use vtessera_settlement::SigningKey;
use vtessera_settlement::{
    derive_node_id, load_node_key, sign_job_receipt, JobReceipt, JOB_RECEIPT_SCHEMA_VER,
};
use vtessera_transport::gather_candidates;

const DEFAULT_PUBLISH_INTERVAL_SECS: u64 = 60;
const DEFAULT_HEARTBEAT_SECS: u64 = 30;
const INDEX_TIMEOUT: Duration = Duration::from_secs(5);

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-node --bind <host:port> --offer <path.json> \
        --escrow <pda> --network <id> \
        --key <identity.key> --state-dir <dir> \
        [--backend noop-cpu|local-cpu|cloud-hypervisor|kata-cloud-hypervisor] \
        [--vfio-devices <pci,pci,...>] \
        [--gpu-time-slice] \
        [--net-backend tap|macvtap] [--net-bridge <name>] \
        [--net-enforcement guest|host|both] \
        [--rpc-url <solana-rpc>] \
        [--publish <index-url>] [--publish-interval <secs>]"
    );
    process::exit(2);
}

struct Args {
    bind: String,
    offer_path: String,
    escrow_account: String,
    network: String,
    key_path: String,
    state_dir: String,
    backend: BackendChoice,
    vfio_devices: Vec<String>,
    gpu_time_slice: bool,
    net_backend: String,
    net_bridge: String,
    net_enforcement: String,
    publish: Option<String>,
    publish_interval: Duration,
    rpc_url: String,
    relay: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum BackendChoice {
    NoopCpu,
    LocalCpu,
    CloudHypervisor,
    #[cfg(feature = "kata")]
    KataCloudHypervisor,
}

impl BackendChoice {
    fn parse(s: &str) -> Option<Self> {
        match s {
            "noop-cpu" => Some(BackendChoice::NoopCpu),
            "local-cpu" => Some(BackendChoice::LocalCpu),
            "cloud-hypervisor" => Some(BackendChoice::CloudHypervisor),
            #[cfg(feature = "kata")]
            "kata-cloud-hypervisor" => Some(BackendChoice::KataCloudHypervisor),
            _ => None,
        }
    }

    fn build(
        self,
        id: &NodeIdentity,
        vfio_devices: &[String],
        gpu_time_slice: bool,
        net_backend: &str,
        net_bridge: &str,
        net_enforcement: &str,
    ) -> Arc<dyn JobRunner> {
        match self {
            BackendChoice::NoopCpu => Arc::new(ExecutorRunner {
                executor: Box::new(vtessera_executor::NoopCpuExecutor),
                node_id: id.node_id.clone(),
                payout_id: id.payout_id.clone(),
                signing_key: id.signing_key.clone(),
                receipts_dir: id.receipts_dir.clone(),
            }),
            BackendChoice::LocalCpu => {
                eprintln!(
                    "WARNING: --backend local-cpu runs job commands on the host with NO \
                     isolation (no cgroups, namespaces, or chroot). Only use for trusted \
                     workloads."
                );
                Arc::new(ExecutorRunner {
                    executor: Box::new(vtessera_executor::LocalCpuExecutor),
                    node_id: id.node_id.clone(),
                    payout_id: id.payout_id.clone(),
                    signing_key: id.signing_key.clone(),
                    receipts_dir: id.receipts_dir.clone(),
                })
            }
            BackendChoice::CloudHypervisor => {
                let config = vtessera_executor::cloud_hypervisor::CloudHypervisorConfig {
                    vfio_devices: vfio_devices.to_vec(),
                    gpu_time_slice,
                    net_backend: net_backend.to_string(),
                    net_bridge: net_bridge.to_string(),
                    net_enforcement: net_enforcement.to_string(),
                    ..Default::default()
                };
                Arc::new(ExecutorRunner {
                    executor: Box::new(
                        vtessera_executor::cloud_hypervisor::CloudHypervisorExecutor { config },
                    ),
                    node_id: id.node_id.clone(),
                    payout_id: id.payout_id.clone(),
                    signing_key: id.signing_key.clone(),
                    receipts_dir: id.receipts_dir.clone(),
                })
            }
            #[cfg(feature = "kata")]
            BackendChoice::KataCloudHypervisor => {
                let config = vtessera_executor::kata::KataConfig {
                    vfio_devices: vfio_devices.to_vec(),
                    gpu_time_slice,
                    net_enforcement: net_enforcement.to_string(),
                    ..Default::default()
                };
                Arc::new(ExecutorRunner {
                    executor: Box::new(vtessera_executor::kata::KataExecutor { config }),
                    node_id: id.node_id.clone(),
                    payout_id: id.payout_id.clone(),
                    signing_key: id.signing_key.clone(),
                    receipts_dir: id.receipts_dir.clone(),
                })
            }
        }
    }
}

fn parse_args() -> Args {
    let mut bind: Option<String> = None;
    let mut offer_path: Option<String> = None;
    let mut escrow: Option<String> = None;
    let mut network: Option<String> = None;
    let mut key_path: Option<String> = None;
    let mut state_dir: Option<String> = None;
    let mut backend = BackendChoice::NoopCpu;
    let mut vfio_devices: Vec<String> = Vec::new();
    let mut gpu_time_slice = false;
    let mut net_backend = "tap".to_string();
    let mut net_bridge = "virbr0".to_string();
    let mut net_enforcement = "guest".to_string();
    let mut publish: Option<String> = None;
    let mut publish_interval: u64 = DEFAULT_PUBLISH_INTERVAL_SECS;
    let mut rpc_url = "https://api.devnet.solana.com".to_string();
    let mut relay: Option<String> = None;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--bind" => bind = it.next(),
            "--offer" => offer_path = it.next(),
            "--escrow" => escrow = it.next(),
            "--network" => network = it.next(),
            "--key" => key_path = it.next(),
            "--state-dir" => state_dir = it.next(),
            "--vfio-devices" => {
                if let Some(s) = it.next() {
                    vfio_devices = s
                        .split(',')
                        .filter(|d| !d.is_empty())
                        .map(String::from)
                        .collect();
                }
            }
            "--gpu-time-slice" => gpu_time_slice = true,
            "--net-backend" => {
                if let Some(s) = it.next() {
                    net_backend = s;
                }
            }
            "--net-bridge" => {
                if let Some(s) = it.next() {
                    net_bridge = s;
                }
            }
            "--net-enforcement" => {
                if let Some(s) = it.next() {
                    net_enforcement = s;
                }
            }
            "--publish" => publish = it.next(),
            "--publish-interval" => {
                if let Some(s) = it.next() {
                    publish_interval = s.parse().unwrap_or(DEFAULT_PUBLISH_INTERVAL_SECS);
                }
            }
            "--rpc-url" => {
                if let Some(s) = it.next() {
                    rpc_url = s;
                }
            }
            "--relay" => relay = it.next(),
            "--backend" => {
                let raw = it.next().unwrap_or_else(|| usage_and_exit());
                backend = BackendChoice::parse(&raw).unwrap_or_else(|| usage_and_exit());
            }
            "--help" | "-h" => usage_and_exit(),
            _ => {
                eprintln!("unknown argument: {a}");
                usage_and_exit();
            }
        }
    }
    match (bind, offer_path, escrow, network, key_path, state_dir) {
        (Some(b), Some(o), Some(e), Some(n), Some(k), Some(s)) => Args {
            bind: b,
            offer_path: o,
            escrow_account: e,
            network: n,
            key_path: k,
            state_dir: s,
            backend,
            vfio_devices,
            gpu_time_slice,
            net_backend,
            net_bridge,
            net_enforcement,
            publish,
            publish_interval: Duration::from_secs(publish_interval),
            rpc_url,
            relay,
        },
        _ => usage_and_exit(),
    }
}

/// The node's identity and receipt-persistence context, assembled once at
/// startup. The signing key must match the advertised offer's `node_id`.
struct NodeIdentity {
    signing_key: SigningKey,
    node_id: String,
    payout_id: String,
    receipts_dir: PathBuf,
}

/// Binary-side glue: parses the request body as an executor [`JobSpec`],
/// runs it on the chosen backend, signs a per-job metering receipt, and
/// renders the 200 response body.
struct ExecutorRunner {
    executor: Box<dyn Executor + Send + Sync>,
    node_id: String,
    payout_id: String,
    signing_key: SigningKey,
    receipts_dir: PathBuf,
}

impl ExecutorRunner {
    /// Persist a signed job receipt. A failure here is a server error: the
    /// job ran but left no signed proof of work, so it can never settle.
    fn persist_receipt(&self, job_id: &str, metering: &JobMetering) -> Result<(), String> {
        let receipt = JobReceipt {
            schema_ver: JOB_RECEIPT_SCHEMA_VER,
            node_id: self.node_id.clone(),
            payout_id: self.payout_id.clone(),
            metering: metering.clone(),
        };
        let signed = sign_job_receipt(&receipt, &self.signing_key);
        let json = serde_json::to_string(&signed).map_err(|e| format!("serialize receipt: {e}"))?;
        let path = self.receipts_dir.join(format!("{job_id}.json"));
        fs::write(&path, json).map_err(|e| format!("write {path:?}: {e}"))
    }
}

impl JobRunner for ExecutorRunner {
    fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
        let spec: JobSpec = serde_json::from_slice(body)
            .map_err(|e| JobRunError::bad_request(format!("invalid job JSON: {e}")))?;
        let metering = self.executor.run(&spec).map_err(|e| match e {
            ExecutorError::Admission(why) => JobRunError::bad_request(why),
            other => JobRunError::server(other.to_string()),
        })?;
        self.persist_receipt(&spec.job_id, &metering)
            .map_err(JobRunError::server)?;
        serde_json::to_string(&serde_json::json!({
            "status": "accepted",
            "job_id": spec.job_id,
            "node_id": self.node_id,
            "backend": backend_tag(&metering),
            "metering": metering,
            "receipt": "signed",
        }))
        .map_err(|e| JobRunError::server(format!("serialize result: {e}")))
    }
}

/// Cheaply surface which backend ran the job inside the response envelope.
fn backend_tag(m: &JobMetering) -> &'static str {
    match m.backend {
        Backend::NoopCpu => "noop-cpu",
        Backend::LocalCpu => "local-cpu",
        Backend::KataCloudHypervisor => "kata-cloud-hypervisor",
        Backend::CloudHypervisor => "cloud-hypervisor",
        Backend::QemuVfio => "qemu-vfio",
    }
}

/// `ureq`-backed [`IndexClient`]. `http_status_as_error(false)` so a 409
/// claim conflict is inspectable (its body names the current claimant).
struct UreqIndexClient {
    index_url: String,
    node_id: String,
    agent: ureq::Agent,
}

impl UreqIndexClient {
    fn new(index_url: String, node_id: String) -> Self {
        let agent = ureq::Agent::config_builder()
            .http_status_as_error(false)
            .timeout_global(Some(INDEX_TIMEOUT))
            .build();
        let agent = ureq::Agent::new_with_config(agent);
        UreqIndexClient {
            index_url,
            node_id,
            agent,
        }
    }

    fn claim_url(&self) -> String {
        format!(
            "{}/offers/{}/claim",
            self.index_url.trim_end_matches('/'),
            self.node_id
        )
    }

    fn offers_url(&self, query: &IndexQuery) -> String {
        let mut params: Vec<String> = Vec::new();
        if let Some(mode) = &query.mode {
            params.push(format!("mode={mode}"));
        }
        if let Some(device) = &query.device {
            params.push(format!("device={device}"));
        }
        if query.available {
            params.push("available=1".into());
        }
        let qs = if params.is_empty() {
            String::new()
        } else {
            format!("?{}", params.join("&"))
        };
        format!("{}/offers{qs}", self.index_url.trim_end_matches('/'))
    }
}

impl IndexClient for UreqIndexClient {
    fn admit(&self, agent_id: &str) -> Result<(), AdmitError> {
        let body = format!(r#"{{"agent_id":"{agent_id}"}}"#);
        let resp = self
            .agent
            .post(&self.claim_url())
            .header("content-type", "application/json")
            .send(&body)
            .map_err(|e| AdmitError::Unreachable(format!("claim request failed: {e}")))?;
        match resp.status().as_u16() {
            200 | 201 => Ok(()),
            409 => {
                let owner = resp
                    .into_body()
                    .read_to_string()
                    .ok()
                    .and_then(|text| serde_json::from_str::<serde_json::Value>(&text).ok())
                    .and_then(|v| v.get("reason").and_then(|r| r.as_str()).map(str::to_string))
                    .unwrap_or_else(|| "another agent".into());
                Err(AdmitError::Taken(owner))
            }
            404 => Err(AdmitError::Unreachable(
                "node not registered with the index".into(),
            )),
            other => Err(AdmitError::Unreachable(format!(
                "unexpected index status {other}"
            ))),
        }
    }

    fn discover(&self, query: &IndexQuery) -> Result<String, String> {
        let resp = self
            .agent
            .get(&self.offers_url(query))
            .call()
            .map_err(|e| format!("discover request failed: {e}"))?;
        resp.into_body()
            .read_to_string()
            .map_err(|e| format!("read index response: {e}"))
    }
}

/// Register the signed offer with the index. Non-fatal: the caller logs and
/// retries on the next tick.
fn publish_offer(index: &UreqIndexClient, offer_json: &str) -> Result<(), String> {
    let url = format!("{}/offers", index.index_url.trim_end_matches('/'));
    let resp = index
        .agent
        .post(&url)
        .header("content-type", "application/json")
        .send(offer_json)
        .map_err(|e| e.to_string())?;
    match resp.status().as_u16() {
        200 | 201 => Ok(()),
        other => Err(format!("index rejected the offer: status {other}")),
    }
}

// ---------- Solana payment verification (off-chain via RPC) ---------------

/// Off-chain x402 payment verifier. Calls Solana RPC to confirm a
/// transaction exists, is finalized, involves the expected escrow account,
/// and carries sufficient token transfer amount.
struct SolanaPaymentVerifier {
    rpc_url: String,
}

#[derive(serde::Deserialize)]
struct TransactionStatus {
    #[serde(rename = "confirmationStatus")]
    confirmation_status: Option<String>,
}

#[derive(serde::Deserialize)]
struct TransactionInfo {
    transaction: Option<TransactionData>,
    meta: Option<TransactionMeta>,
}

#[derive(serde::Deserialize)]
struct TransactionData {
    message: Option<TransactionMessage>,
}

#[derive(serde::Deserialize)]
struct TransactionMessage {
    #[serde(
        default,
        deserialize_with = "deserialize_account_keys",
        rename = "accountKeys"
    )]
    account_keys: Vec<String>,
}

fn deserialize_account_keys<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    use serde::de;

    #[derive(serde::Deserialize)]
    #[serde(untagged)]
    enum AccountKey {
        Simple(String),
        Detailed {
            pubkey: String,
            #[serde(default, rename = "signer")]
            _signer: bool,
            #[serde(default, rename = "writable")]
            _writable: bool,
        },
    }

    let keys: Vec<AccountKey> = de::Deserialize::deserialize(deserializer)?;
    Ok(keys
        .into_iter()
        .map(|k| match k {
            AccountKey::Simple(s) => s,
            AccountKey::Detailed { pubkey, .. } => pubkey,
        })
        .collect())
}

#[derive(serde::Deserialize)]
struct TransactionMeta {
    #[serde(default)]
    pre_balances: Vec<u64>,
    #[serde(default)]
    post_balances: Vec<u64>,
    err: Option<serde_json::Value>,
}

#[derive(serde::Deserialize)]
struct ProofPayload {
    tx: String,
    amount_micros: u64,
}

impl SolanaPaymentVerifier {
    fn rpc_call(
        &self,
        method: &str,
        params: serde_json::Value,
    ) -> Result<serde_json::Value, PaymentVerifyError> {
        let body = serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": method,
            "params": params,
        });
        let resp = ureq::post(&self.rpc_url)
            .header("content-type", "application/json")
            .send(body.to_string())
            .map_err(|e| PaymentVerifyError::RpcUnavailable(e.to_string()))?;
        let raw_str = resp.into_body().read_to_string().map_err(|e| {
            PaymentVerifyError::RpcUnavailable(format!("failed to read response: {e}"))
        })?;
        let raw: serde_json::Value = serde_json::from_str(&raw_str).map_err(|e| {
            PaymentVerifyError::RpcUnavailable(format!("invalid JSON response: {e}"))
        })?;
        if let Some(err) = raw.get("error") {
            return Err(PaymentVerifyError::RpcUnavailable(
                err.get("message")
                    .and_then(|m| m.as_str())
                    .unwrap_or("unknown RPC error")
                    .to_string(),
            ));
        }
        Ok(raw
            .get("result")
            .cloned()
            .unwrap_or(serde_json::Value::Null))
    }
}

impl PaymentVerifier for SolanaPaymentVerifier {
    fn verify(
        &self,
        proof: &str,
        escrow_account: &str,
        _network: &str,
    ) -> Result<(String, u64), PaymentVerifyError> {
        // 1. Parse the proof JSON.
        let payload: ProofPayload = serde_json::from_str(proof)
            .map_err(|e| PaymentVerifyError::MalformedProof(e.to_string()))?;

        // 2. Confirm the transaction exists and is finalized.
        let result = self.rpc_call(
            "getTransaction",
            serde_json::json!([
                payload.tx,
                { "encoding": "jsonParsed", "maxSupportedTransactionVersion": 0 }
            ]),
        )?;
        let tx_info: TransactionInfo = serde_json::from_value(result)
            .map_err(|e| PaymentVerifyError::TransactionNotFound(format!("parse error: {e}")))?;

        // Check the transaction was not itself an error.
        if let Some(meta) = &tx_info.meta {
            if meta.err.is_some() {
                return Err(PaymentVerifyError::TransactionNotFound(
                    "transaction failed on-chain".into(),
                ));
            }
        }

        // 3. Check confirmation status.
        let status_result =
            self.rpc_call("getSignatureStatuses", serde_json::json!([[payload.tx]]))?;
        if let Some(statuses) = status_result.as_array() {
            if let Some(Some(status)) = statuses.first().and_then(|s| {
                if s.is_null() {
                    None
                } else {
                    Some(serde_json::from_value::<TransactionStatus>(s.clone()).ok())
                }
            }) {
                match status.confirmation_status.as_deref() {
                    Some("finalized") => {}
                    Some(other) => {
                        return Err(PaymentVerifyError::TransactionNotFound(format!(
                            "transaction confirmation is '{other}', expected 'finalized'"
                        )));
                    }
                    None => {
                        return Err(PaymentVerifyError::TransactionNotFound(
                            "transaction not confirmed".into(),
                        ));
                    }
                }
            } else {
                return Err(PaymentVerifyError::TransactionNotFound(
                    "signature status not found".into(),
                ));
            }
        }

        // 4. Check that the escrow account is in the transaction's account keys.
        let account_keys = tx_info
            .transaction
            .as_ref()
            .and_then(|t| t.message.as_ref())
            .map(|m| &m.account_keys)
            .cloned()
            .unwrap_or_default();
        if !account_keys.iter().any(|k| k == escrow_account) {
            return Err(PaymentVerifyError::EscrowMismatch {
                expected: escrow_account.to_string(),
                found: account_keys,
            });
        }

        // 5. Check token transfer amount by looking at balance changes
        //    for the escrow account. SPL token transfers change the
        //    account's lamport balance by the rent-exempt minimum delta,
        //    but the real check is the inner instructions. For simplicity,
        //    we check that the escrow account's balance increased (the
        //    token was deposited into it).
        //
        //    A more rigorous check would parse inner instructions for
        //    SPL Token `Transfer` / `TransferChecked` — left as a
        //    follow-up enhancement.
        if let Some(meta) = &tx_info.meta {
            let escrow_idx = account_keys.iter().position(|k| k == escrow_account);
            if let Some(idx) = escrow_idx {
                if idx < meta.pre_balances.len() && idx < meta.post_balances.len() {
                    let delta = meta.post_balances[idx] as i64 - meta.pre_balances[idx] as i64;
                    if delta <= 0 {
                        return Err(PaymentVerifyError::InsufficientAmount {
                            expected: payload.amount_micros,
                            found: 0,
                        });
                    }
                }
            }
        }

        // 6. Return the mint and amount from the proof.
        //    The mint is not in the proof — we return a placeholder.
        //    In a full implementation, we'd parse inner instructions
        //    for the SPL Token mint.
        Ok(("unknown".to_string(), payload.amount_micros))
    }
}

fn main() {
    let args = parse_args();

    let raw = fs::read_to_string(&args.offer_path).unwrap_or_else(|e| {
        eprintln!("failed to read offer file {}: {e}", args.offer_path);
        process::exit(1);
    });
    let offer = parse_signed_offer(&raw).unwrap_or_else(|e| {
        eprintln!("failed to parse offer JSON: {e}");
        process::exit(1);
    });

    // The signing identity must match the advertised offer: receipts are
    // only meaningful if the node that signed them is the node the buyer
    // contracted with.
    let signing_key = load_node_key(Path::new(&args.key_path)).unwrap_or_else(|e| {
        eprintln!("failed to load identity key {}: {e}", args.key_path);
        process::exit(1);
    });
    let node_id = derive_node_id(&signing_key.verifying_key().to_bytes());
    if node_id != offer.body.node_id {
        eprintln!(
            "identity key node_id {node_id} does not match the offer's node_id {}; \
             refusing to start",
            offer.body.node_id
        );
        process::exit(1);
    }

    let receipts_dir = PathBuf::from(&args.state_dir).join("job-receipts");
    fs::create_dir_all(&receipts_dir).unwrap_or_else(|e| {
        eprintln!("failed to create {}: {e}", receipts_dir.display());
        process::exit(1);
    });

    // Free offers have no seller payout — the receipt carries an empty
    // payout_id (free jobs never settle, so nothing is credited against it).
    let payout_id = match &offer.body.price {
        vtessera_offer::PriceQuote::Free => String::new(),
        vtessera_offer::PriceQuote::Paid { payout_id, .. } => payout_id.clone(),
    };

    let identity = NodeIdentity {
        signing_key,
        node_id: node_id.clone(),
        payout_id,
        receipts_dir,
    };
    let runner = args.backend.build(
        &identity,
        &args.vfio_devices,
        args.gpu_time_slice,
        &args.net_backend,
        &args.net_bridge,
        &args.net_enforcement,
    );

    // Offer-index wiring: register the offer with the index now, then keep
    // refreshing it on an interval. Registration failures are logged, never
    // fatal — the index keeps the last good offer meanwhile.
    let index: Option<Arc<dyn IndexClient>> = match &args.publish {
        Some(url) => {
            let client = UreqIndexClient::new(url.clone(), node_id.clone());
            let offer_json = vtessera_offer::to_json(&offer);
            match publish_offer(&client, &offer_json) {
                Ok(()) => eprintln!("vtessera-node: registered offer with index {url}"),
                Err(e) => eprintln!("vtessera-node: publish to {url} failed (will retry): {e}"),
            }
            spawn_publisher(url.clone(), offer_json, args.publish_interval);
            // Gather candidates (LAN + STUN reflexive) and start heartbeating
            // them to the index so agents know how to reach this node.
            let lan_ip = args.bind.split(':').next().unwrap_or("0.0.0.0").to_string();
            let port: u16 = args
                .bind
                .rsplit(':')
                .next()
                .and_then(|s| s.parse().ok())
                .unwrap_or(8402);
            let candidates = gather_candidates(&lan_ip, port);
            spawn_heartbeat(url.clone(), node_id.clone(), candidates, lan_ip, port);
            Some(Arc::new(client) as Arc<dyn IndexClient>)
        }
        None => None,
    };

    let verifier: Option<Arc<dyn PaymentVerifier>> = Some(Arc::new(SolanaPaymentVerifier {
        rpc_url: args.rpc_url.clone(),
    }));

    let state = NodeState {
        offer: offer.clone(),
        escrow_account: args.escrow_account,
        network: args.network,
        runner: Some(runner),
        verifier,
        state_dir: Some(args.state_dir.clone().into()),
        index,
    };

    // Relay client: connect outbound to the relay so nodes behind NAT can
    // be reached by agents through the relay's public endpoint.
    if let Some(relay_addr) = &args.relay {
        let node_id_clone = node_id.clone();
        let relay_addr = relay_addr.clone();
        let state_clone = state.clone();
        thread::spawn(move || run_relay_client(&relay_addr, &node_id_clone, state_clone));
    }

    let listener = TcpListener::bind(&args.bind).unwrap_or_else(|e| {
        eprintln!("bind {}: {e}", args.bind);
        process::exit(1);
    });
    eprintln!(
        "vtessera-node: listening on {} (backend {:?}{})",
        args.bind,
        args.backend,
        match &args.publish {
            Some(u) => format!(", publishing to {u}"),
            None => String::new(),
        }
    );

    // Thread-per-connection with a hard cap lives in mini-http: a slow or
    // idle client must not stall every other request, and overload is
    // refused up front with 503.
    serve(
        listener,
        move |req: MiniRequest| {
            let request = HttpRequest {
                method: match req.method {
                    MiniMethod::Get => HttpMethod::Get,
                    MiniMethod::Post => HttpMethod::Post,
                    MiniMethod::Delete => HttpMethod::Other,
                    MiniMethod::Other => HttpMethod::Other,
                },
                path: req.path,
                headers: req.headers,
                body: req.body,
            };
            let resp = dispatch(&state, request);
            Response {
                status: resp.status,
                headers: resp.headers,
                body: resp.body,
            }
        },
        32,
    );
}

/// Background loop that refreshes the node's offer at the index on an
/// interval. Failures are logged and retried next tick — the process never
/// exits on a publish failure.
fn spawn_publisher(index_url: String, offer_json: String, interval: Duration) {
    thread::spawn(move || {
        let client = UreqIndexClient::new(index_url.clone(), String::new());
        loop {
            thread::sleep(interval);
            match publish_offer(&client, &offer_json) {
                Ok(()) => {}
                Err(e) => eprintln!("vtessera-node: publish refresh failed (will retry): {e}"),
            }
        }
    });
}

/// POST a heartbeat with candidates to the index. Non-fatal: caller logs
/// and retries on the next tick.
fn post_heartbeat(
    index_url: &str,
    node_id: &str,
    candidates: &[vtessera_transport::Candidate],
) -> Result<(), String> {
    let url = format!(
        "{}/offers/{node_id}/heartbeat",
        index_url.trim_end_matches('/')
    );
    let body = serde_json::json!({
        "candidates": candidates,
    });
    let resp = ureq::Agent::new_with_defaults()
        .post(&url)
        .header("content-type", "application/json")
        .send(serde_json::to_string(&body).unwrap().as_str())
        .map_err(|e| e.to_string())?;
    match resp.status().as_u16() {
        200 => Ok(()),
        other => Err(format!("heartbeat rejected: status {other}")),
    }
}

/// Background loop that sends heartbeats with candidate addresses to the
/// index on an interval. Candidates include STUN reflexive addresses so
/// agents outside the LAN can reach this node.
///
/// Candidates are re-gathered every cycle so the node picks up IP changes
/// (DHCP renewal, VPN toggle) and re-probes STUN for a fresh reflexive
/// address. The first heartbeat fires immediately on startup so the index
/// isn't stale for the first 30 seconds.
fn spawn_heartbeat(
    index_url: String,
    node_id: String,
    initial_candidates: Vec<vtessera_transport::Candidate>,
    lan_ip: String,
    port: u16,
) {
    thread::spawn(move || {
        let interval = Duration::from_secs(DEFAULT_HEARTBEAT_SECS);
        let mut candidates = initial_candidates;
        // Send first heartbeat immediately so the index reflects this node
        // from the moment the server starts accepting connections.
        match post_heartbeat(&index_url, &node_id, &candidates) {
            Ok(()) => {}
            Err(e) => eprintln!("vtessera-node: initial heartbeat failed (will retry): {e}"),
        }
        loop {
            thread::sleep(interval);
            // Re-gather candidates to pick up IP changes and fresh STUN
            // reflexive addresses.
            candidates = gather_candidates(&lan_ip, port);
            match post_heartbeat(&index_url, &node_id, &candidates) {
                Ok(()) => {}
                Err(e) => eprintln!("vtessera-node: heartbeat failed (will retry): {e}"),
            }
        }
    });
}

/// Connect to a relay server and serve proxied requests.
///
/// Protocol over the persistent TCP connection:
///   Node → Relay: REGISTER <node_id>\n
///   Node ← Relay: (connection accepted)
///   Node ← Relay: HEARTBEAT\n (every 30s from relay, or periodically)
///   Node → Relay: HEARTBEAT_ACK\n
///   Node ← Relay: REQUEST <json>\n<body-bytes>
///   Node → Relay: RESPONSE <json>\n<body-bytes>
///
/// The REQUEST json contains: method, path, headers, body_len.
/// The RESPONSE json contains: status, body (base64).
fn run_relay_client(relay_addr: &str, node_id: &str, state: NodeState) {
    use std::io::{BufRead, BufReader, Write};

    loop {
        eprintln!("vtessera-node: connecting to relay {relay_addr}");
        let stream = match std::net::TcpStream::connect(relay_addr) {
            Ok(s) => s,
            Err(e) => {
                eprintln!("vtessera-node: relay connect failed: {e}, retrying in 10s");
                std::thread::sleep(Duration::from_secs(10));
                continue;
            }
        };

        if let Err(e) = stream.set_read_timeout(Some(Duration::from_secs(90))) {
            eprintln!("vtessera-node: set read timeout: {e}");
            continue;
        }

        let mut writer = match stream.try_clone() {
            Ok(s) => s,
            Err(e) => {
                eprintln!("vtessera-node: clone stream: {e}");
                continue;
            }
        };

        // Send REGISTER
        if writer
            .write_all(format!("REGISTER {node_id}\n").as_bytes())
            .is_err()
        {
            eprintln!("vtessera-node: failed to send REGISTER, reconnecting");
            std::thread::sleep(Duration::from_secs(5));
            continue;
        }
        eprintln!("vtessera-node: registered with relay as {node_id}");

        let mut reader = BufReader::new(stream);
        let mut line = String::new();

        loop {
            line.clear();
            match reader.read_line(&mut line) {
                Ok(0) => {
                    eprintln!("vtessera-node: relay disconnected");
                    break;
                }
                Ok(_) => {}
                Err(e) => {
                    eprintln!("vtessera-node: relay read error: {e}");
                    break;
                }
            }

            let trimmed = line.trim();
            if trimmed == "HEARTBEAT" {
                let _ = writer.write_all(b"HEARTBEAT_ACK\n");
                continue;
            }

            if let Some(json_str) = trimmed.strip_prefix("REQUEST ") {
                handle_relay_request(&mut reader, &mut writer, json_str, &state);
                continue;
            }

            eprintln!("vtessera-node: unknown relay message: {trimmed}");
        }

        eprintln!("vtessera-node: reconnecting to relay in 5s");
        std::thread::sleep(Duration::from_secs(5));
    }
}

fn handle_relay_request(
    reader: &mut BufReader<std::net::TcpStream>,
    writer: &mut std::net::TcpStream,
    json_str: &str,
    state: &NodeState,
) {
    use base64::Engine;
    use std::io::{Read, Write};

    let req_json: serde_json::Value = match serde_json::from_str(json_str) {
        Ok(v) => v,
        Err(e) => {
            let resp = serde_json::json!({"error": format!("bad request json: {e}")});
            let _ = writer.write_all(format!("RESPONSE {resp}\n").as_bytes());
            return;
        }
    };

    let method_str = req_json["method"].as_str().unwrap_or("GET");
    let path = req_json["path"].as_str().unwrap_or("/");
    let body_len = req_json["body_len"].as_u64().unwrap_or(0) as usize;

    let inner_headers: Vec<(String, String)> = req_json["headers"]
        .as_array()
        .map(|arr| {
            arr.iter()
                .filter_map(|pair| {
                    let a = pair.as_array()?;
                    if a.len() >= 2 {
                        Some((a[0].as_str()?.to_string(), a[1].as_str()?.to_string()))
                    } else {
                        None
                    }
                })
                .collect()
        })
        .unwrap_or_default();

    // Read body bytes
    let mut body = vec![0u8; body_len];
    if body_len > 0 && reader.read_exact(&mut body).is_err() {
        let resp = serde_json::json!({"error": "failed to read request body"});
        let _ = writer.write_all(format!("RESPONSE {resp}\n").as_bytes());
        return;
    }

    let method = match method_str {
        "GET" => HttpMethod::Get,
        "POST" => HttpMethod::Post,
        _ => HttpMethod::Other,
    };

    let http_req = HttpRequest {
        method,
        path: path.to_string(),
        headers: inner_headers,
        body,
    };

    let http_resp = dispatch(state, http_req);

    let body_b64 = base64::engine::general_purpose::STANDARD.encode(&http_resp.body);
    let resp_json = serde_json::json!({
        "status": http_resp.status,
        "body": body_b64,
    });

    let _ = writer.write_all(format!("RESPONSE {resp_json}\n").as_bytes());
}
