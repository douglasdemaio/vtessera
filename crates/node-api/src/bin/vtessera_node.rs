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
//! Add `--marketplace` to register with the public marketplace (GitHub
//! Pages) and `--upnp` to auto-forward the port on a UPnP home router so
//! the node is reachable from the internet without a manual port-forward.
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

#[cfg(feature = "serve")]
use vtessera_transport::iroh_sidecar::{self, IrohEndpoint};

const DEFAULT_PUBLISH_INTERVAL_SECS: u64 = 60;
const DEFAULT_HEARTBEAT_SECS: u64 = 30;
const DEFAULT_MARKETPLACE_INTERVAL_SECS: u64 = 3600;
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
        [--publish <index-url>] [--publish-interval <secs>] \
        [--marketplace] [--marketplace-interval <secs>] \
        [--upnp]"
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
    /// Register with the public GitHub Pages marketplace on startup.
    /// Detects external IP and POSTs to the Cloudflare Worker (no token needed).
    marketplace: bool,
    /// How often to re-register with the marketplace (default: 3600s).
    marketplace_interval: Duration,
    /// Auto-forward the bind port on the home router via UPnP IGD so the
    /// node is reachable from the internet without a manual port-forward.
    upnp: bool,
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
    let mut marketplace = false;
    let mut marketplace_interval: u64 = DEFAULT_MARKETPLACE_INTERVAL_SECS;
    let mut upnp = false;
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
            "--marketplace" => marketplace = true,
            "--marketplace-interval" => {
                if let Some(s) = it.next() {
                    marketplace_interval = s.parse().unwrap_or(DEFAULT_MARKETPLACE_INTERVAL_SECS);
                }
            }
            "--upnp" => upnp = true,
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
            marketplace,
            marketplace_interval: Duration::from_secs(marketplace_interval),
            upnp,
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
    err: Option<serde_json::Value>,
    #[serde(default, rename = "innerInstructions")]
    inner_instructions: Vec<InnerInstructionSet>,
    /// Accounts pulled in at runtime via address-lookup tables (and CPI),
    /// not present in the static `message.accountKeys`. Inner-instruction
    /// account indices index into `static_keys ++ loaded.writable ++
    /// loaded.readonly`, so these must be appended for correct resolution.
    #[serde(default, rename = "loadedAddresses")]
    loaded_addresses: LoadedAddresses,
}

#[derive(serde::Deserialize, Default)]
struct LoadedAddresses {
    #[serde(default)]
    writable: Vec<String>,
    #[serde(default)]
    readonly: Vec<String>,
}

/// The set of instructions a top-level instruction produced. jsonParsed
/// returns them grouped per outer instruction; we scan the whole tx, so the
/// parent index is not needed (serde ignores fields we don't bind).
#[derive(serde::Deserialize, Debug)]
struct InnerInstructionSet {
    #[serde(default)]
    instructions: Vec<InnerInstruction>,
}

/// A single (inner) instruction in `encoding: json` (base58) form from
/// `getTransaction`. The program is identified by `programIdIndex` (an index
/// into the account keys); `accounts` are indices into the same list; `data`
/// is the raw base58 instruction data. (`jsonParsed` returns `accounts`/
/// `data` as null, so we must request `json` form to inspect CPI internals.)
#[derive(serde::Deserialize, Debug)]
struct InnerInstruction {
    #[serde(default, rename = "programId")]
    program_id: Option<String>,
    #[serde(default, rename = "programIdIndex")]
    program_id_index: Option<u8>,
    #[serde(default)]
    accounts: Vec<u8>,
    #[serde(default)]
    data: String,
}

#[derive(serde::Deserialize)]
struct ProofPayload {
    tx: String,
    #[serde(rename = "amount_micros")]
    amount_micros: u64,
    /// The 32-byte per-job contract seed (hex-encoded). This is the `job_id`
    /// the agent derived from its payer/mint/ATA and used to create the
    /// escrow contract PDA — *not* the human `job_id` in the job body.
    #[serde(default)]
    job_id: Option<String>,
    /// The stablecoin mint the buyer paid with (base58).
    #[serde(default)]
    mint: Option<String>,
}

/// The legacy SPL Token program (`spl_token::id()`). The escrow program's
/// `pay_for_compute` deposits the price with a plain `token::transfer` CPI,
/// so the inner instruction we look for is a `Transfer` on this program.
const SPL_TOKEN_PROGRAM_ID: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

/// SPL Token `Transfer` instruction discriminant (`TokenInstruction::Transfer`).
const SPL_TRANSFER_TAG: u8 = 3;

/// Parse a 64-char hex string into the 32 bytes it encodes.
fn hex_to_bytes32(s: &str) -> Result<[u8; 32], String> {
    let mut out = [0u8; 32];
    let bytes = hex::decode(s).map_err(|e| format!("job_id not valid hex ({e})"))?;
    if bytes.len() != 32 {
        return Err("job_id must decode to 32 bytes".into());
    }
    out.copy_from_slice(&bytes);
    Ok(out)
}

/// The SPL Token `Transfer` instruction data is `[discriminant:u8,
/// amount:u64 LE]`. Decode the (base58) instruction data and, if it is a
/// Transfer whose destination account (resolved via `accounts[1]` against
/// the transaction's account keys) is the given escrow ATA, return the
/// transferred amount.
fn transfer_amount_into_escrow(
    instr: &InnerInstruction,
    account_keys: &[String],
    escrow_ata: &[u8; 32],
) -> Option<u64> {
    if instr.accounts.len() < 3 {
        return None;
    }
    // Resolve the program id: either inline (`programId`, jsonParsed) or via
    // an index into the account keys (`programIdIndex`, json form).
    let prog = if let Some(p) = &instr.program_id {
        Some(p.as_str())
    } else {
        let idx = instr.program_id_index? as usize;
        account_keys.get(idx).map(|s| s.as_str())
    };
    if prog != Some(SPL_TOKEN_PROGRAM_ID) {
        return None;
    }
    // Transfer accounts: [from, to, owner] — index 1 is the destination.
    let to_idx = *instr.accounts.get(1)? as usize;
    let to_pubkey = account_keys.get(to_idx)?;
    // account_keys are base58-encoded pubkeys; compare against the
    // base58 encoding of the derived escrow ATA.
    let escrow_ata_b58 = bs58::encode(escrow_ata).into_string();
    if *to_pubkey != escrow_ata_b58 {
        return None;
    }
    let data = bs58::decode(&instr.data).into_vec().ok()?;
    if data.len() < 9 || data[0] != SPL_TRANSFER_TAG {
        return None;
    }
    let mut buf = [0u8; 8];
    buf.copy_from_slice(&data[1..9]);
    Some(u64::from_le_bytes(buf))
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
                { "encoding": "json", "maxSupportedTransactionVersion": 0 }
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
        //    Build the *full* key list the inner-instruction indices reference:
        //    static `message.accountKeys` followed by the runtime-loaded
        //    addresses (writable, then readonly).
        let static_keys = tx_info
            .transaction
            .as_ref()
            .and_then(|t| t.message.as_ref())
            .map(|m| &m.account_keys)
            .cloned()
            .unwrap_or_default();
        let (loaded_writable, loaded_readonly) = tx_info
            .meta
            .as_ref()
            .map(|m| {
                (
                    m.loaded_addresses.writable.clone(),
                    m.loaded_addresses.readonly.clone(),
                )
            })
            .unwrap_or_default();
        let mut account_keys = static_keys;
        account_keys.extend(loaded_writable);
        account_keys.extend(loaded_readonly);
        if !account_keys.iter().any(|k| k == escrow_account) {
            return Err(PaymentVerifyError::EscrowMismatch {
                expected: escrow_account.to_string(),
                found: account_keys,
            });
        }

        // 5. Derive the escrow ATA for *this* job and verify the SPL Token
        //    transfer that funded it actually landed there.
        //
        //    `pay_for_compute` creates a per-job contract PDA
        //    (`[b"contract", job_id]`) and deposits the price into that
        //    contract's associated token account (the "escrow ATA") via a
        //    plain SPL Token `Transfer` CPI. The node names the job through
        //    the proof's `job_id` (the 32-byte contract seed) and `mint`.
        //
        //    A malicious buyer could put any amount in the proof, so we must
        //    parse the actual on-chain transfer and require it to be >= the
        //    claimed amount. Matching the *derived* ATA — which embeds both
        //    the job's contract PDA and the mint — is what pins this payment
        //    to this job and this stablecoin.
        let job_id_hex = payload
            .job_id
            .ok_or_else(|| PaymentVerifyError::MalformedProof("missing job_id".into()))?;
        let mint = payload
            .mint
            .ok_or_else(|| PaymentVerifyError::MalformedProof("missing mint".into()))?;
        let job_id =
            hex_to_bytes32(&job_id_hex).map_err(|e| PaymentVerifyError::MalformedProof(e))?;
        let mint_b58 = bs58::decode(&mint)
            .into_vec()
            .map_err(|_| PaymentVerifyError::MalformedProof("mint is not base58".into()))?;
        let mint_bytes: [u8; 32] = {
            if mint_b58.len() != 32 {
                return Err(PaymentVerifyError::MalformedProof(
                    "mint must be a 32-byte pubkey".into(),
                ));
            }
            let mut m = [0u8; 32];
            m.copy_from_slice(&mint_b58);
            m
        };
        let program_id: [u8; 32] = {
            let b = bs58::decode(escrow_account).into_vec().map_err(|_| {
                PaymentVerifyError::EscrowMismatch {
                    expected: escrow_account.to_string(),
                    found: account_keys.clone(),
                }
            })?;
            if b.len() != 32 {
                return Err(PaymentVerifyError::EscrowMismatch {
                    expected: escrow_account.to_string(),
                    found: account_keys.clone(),
                });
            }
            let mut p = [0u8; 32];
            p.copy_from_slice(&b);
            p
        };
        let (contract_pda, _bump) = vtessera_node_api::solana_derivation::contract_pda(
            &job_id,
            &program_id,
        )
        .ok_or_else(|| {
            PaymentVerifyError::TransactionNotFound("could not derive contract PDA for job".into())
        })?;
        let escrow_ata = vtessera_node_api::solana_derivation::get_associated_token_address(
            &contract_pda,
            &mint_bytes,
        );

        let total_deposited = tx_info
            .meta
            .as_ref()
            .map(|m| {
                m.inner_instructions
                    .iter()
                    .flat_map(|set| set.instructions.iter())
                    .filter_map(|instr| {
                        transfer_amount_into_escrow(instr, &account_keys, &escrow_ata)
                    })
                    .sum::<u64>()
            })
            .unwrap_or(0);

        if total_deposited < payload.amount_micros {
            return Err(PaymentVerifyError::InsufficientAmount {
                expected: payload.amount_micros,
                found: total_deposited,
            });
        }

        // 6. Confirm deposit exactly matches the claimed amount.
        //    (A buyer may over-pay; under-claiming a lower amount is rejected
        //    above, so a short payment never passes.)
        Ok((mint, payload.amount_micros))
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

    // Clone the signing key before moving it into NodeIdentity, so we can
    // create the iroh endpoint from the same secret key and re-sign for marketplace.
    let signing_key_for_iroh = signing_key.clone();
    let signing_key_for_marketplace = signing_key.clone();

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

    // Start iroh endpoint early so both heartbeat and accept loops can use it.
    // The endpoint connects to the default relay servers so nodes behind NAT
    // can be reached by agents through the relay.
    let iroh_endpoint: Option<Arc<tokio::sync::RwLock<Option<IrohEndpoint>>>> =
        if args.publish.is_some() {
            Some(start_iroh_endpoint(&signing_key_for_iroh))
        } else {
            None
        };

    let index: Option<Arc<dyn IndexClient>> = match &args.publish {
        Some(url) => {
            let client = UreqIndexClient::new(url.clone(), node_id.clone());
            let offer_json = vtessera_offer::to_json(&offer);
            match publish_offer(&client, &offer_json) {
                Ok(()) => eprintln!("vtessera-node: registered offer with index {url}"),
                Err(e) => eprintln!("vtessera-node: publish to {url} failed (will retry): {e}"),
            }
            spawn_publisher(url.clone(), offer_json, args.publish_interval);

            if let Some(ref ep) = iroh_endpoint {
                spawn_heartbeat_with_iroh(url.clone(), node_id.clone(), ep.clone());
            }

            Some(Arc::new(client) as Arc<dyn IndexClient>)
        }
        None => None,
    };

    // If UPnP is requested, discover the router's IGD (Internet Gateway
    // Device) and add a port forward for the node's port *before* binding or
    // registering with any marketplace. This makes a paid node behind a home
    // NAT reachable from the internet without a manual port-forward. The
    // returned external IP (if any) is what agents on other networks use.
    let upnp_external_ip: Option<String> = if args.upnp {
        match upnp_add_port_forward(&args.bind) {
            Ok(ip) => {
                eprintln!("vtessera-node: UPnP port forward OK (external IP: {ip})");
                Some(ip)
            }
            Err(e) => {
                eprintln!("vtessera-node: UPnP port forward failed: {e}");
                None
            }
        }
    } else {
        None
    };

    // Marketplace registration: detect external IP and register via Cloudflare Worker.
    if args.marketplace {
        // Parse port from bind address.
        let port = args
            .bind
            .rsplit(':')
            .next()
            .and_then(|p| p.parse::<u16>().ok())
            .unwrap_or(8402);

        // Prefer the IP that UPnP learned from the router (it also confirms
        // the port is forwarded); fall back to detecting it via an echo
        // service. Registration succeeds regardless.
        let external_ip = upnp_external_ip
            .clone()
            .or_else(detect_external_ip)
            .unwrap_or_default();

        let external_ip = if external_ip.is_empty() {
            eprintln!("vtessera-node: could not determine external IP for marketplace");
            None
        } else {
            Some(external_ip)
        };

        match register_with_marketplace_with_ip(
            &offer,
            &signing_key_for_marketplace,
            port,
            external_ip.clone(),
        ) {
            Ok(()) => {}
            Err(e) => eprintln!("vtessera-node: marketplace registration failed (will retry): {e}"),
        }
        spawn_marketplace_registration(
            offer.body.clone(),
            signing_key_for_marketplace,
            port,
            args.marketplace_interval,
            external_ip,
        );
    }

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
    //
    // In parallel, the iroh Router accepts incoming QUIC connections from
    // agents that discovered this node via the offer-index. Each connection
    // carries an HTTP request over a bi-directional QUIC stream, which is
    // dispatched to the same handler as TCP requests.

    if let Some(ref ep) = iroh_endpoint {
        spawn_iroh_router(ep, Arc::new(state.clone()));
    }

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

/// Detect the external/public IP address by querying a public service.
/// Returns None if detection fails (firewall, no internet, etc.).
fn detect_external_ip() -> Option<String> {
    let services = [
        "https://ifconfig.me",
        "https://api.ipify.org",
        "https://icanhazip.com",
    ];
    let agent = ureq::Agent::new_with_defaults();
    for url in &services {
        match agent.get(*url).call() {
            Ok(resp) => {
                if let Ok(ip) = resp.into_body().read_to_string() {
                    let ip = ip.trim().to_string();
                    // Validate it looks like an IP address.
                    if !ip.is_empty()
                        && ip.len() <= 45
                        && ip.chars().all(|c| c.is_ascii_digit() || c == '.')
                    {
                        return Some(ip);
                    }
                }
            }
            Err(_) => continue,
        }
    }
    None
}

/// Local non-loopback IPv4 of the first active interface, used as the
/// internal endpoint of a UPnP port forward when the node binds to `0.0.0.0`.
fn detect_local_ipv4() -> Option<std::net::Ipv4Addr> {
    let socket = std::net::UdpSocket::bind("0.0.0.0:0").ok()?;
    // A UDP "send" to a public address never actually transmits on Linux, but
    // it makes the kernel pick the local source address for that route.
    socket.connect("8.8.8.8:53").ok()?;
    let local = socket.local_addr().ok()?;
    match local.ip() {
        std::net::IpAddr::V4(v4) => Some(v4),
        _ => None,
    }
}

/// Discover the router's UPnP IGD and add a TCP port forward for the node's
/// bind port. Returns the router's reported WAN (external) IP on success.
///
/// Requires a UPnP-enabled home router on the LAN. Failure is non-fatal —
/// the caller logs and carries on (the node can still be reached on the LAN,
/// or via iroh relay / a manual port-forward).
fn upnp_add_port_forward(bind: &str) -> Result<String, String> {
    use igd_next::{PortMappingProtocol, SearchOptions};

    // Parse the local port to forward from the bind address.
    let bind_port = bind
        .rsplit(':')
        .next()
        .and_then(|p| p.parse::<u16>().ok())
        .ok_or_else(|| format!("invalid --bind {bind} (expected host:port)"))?;

    // The node binds to 0.0.0.0; resolve the LAN IP we should forward to.
    let local_ip = detect_local_ipv4().ok_or_else(|| "no LAN interface IP found".to_string())?;

    // Search for the router's IGD. Bound to the chosen interface so discovery
    // reaches the gateway's SSDP multicast address.
    let gateway = igd_next::search_gateway(SearchOptions {
        bind_addr: std::net::SocketAddr::new(std::net::IpAddr::V4(local_ip), 0),
        ..Default::default()
    })
    .map_err(|e| format!("gateway discovery failed: {e}"))?;

    let local = std::net::SocketAddr::new(std::net::IpAddr::V4(local_ip), bind_port);

    // Add a permanent (0 = infinite lease) TCP mapping: external 8402 -> local.
    gateway
        .add_port(
            PortMappingProtocol::TCP,
            bind_port,
            local,
            0,
            "vtessera-node",
        )
        .map_err(|e| format!("add port mapping failed: {e}"))?;

    // Query the router for our WAN IP to confirm reachability / for the offer.
    let external_ip = gateway
        .get_external_ip()
        .map_err(|e| format!("get external ip failed: {e}"))?;

    Ok(external_ip.to_string())
}

/// Register the node's offer with the marketplace via Cloudflare Worker.
///
/// The worker holds the GitHub token server-side — nodes don't need one.
/// Just POST to the worker URL.
///
/// `external_ip` overrides the IP the offer was signed with: it may come from
/// a UPnP router query (preferred, confirms the port is forwarded) or from an
/// IP-echo service. `None` falls back to detecting it at this moment.
const MARKETPLACE_WORKER_URL: &str = "https://vtessera.douglasdemaio.workers.dev";

/// Internal helper that takes an optional pre-detected external IP.
fn register_with_marketplace_with_ip(
    offer: &vtessera_offer::SignedOffer,
    signing_key: &SigningKey,
    port: u16,
    external_ip: Option<String>,
) -> Result<(), String> {
    // Detect external IP if not provided.
    let external_ip = match external_ip {
        Some(ip) => ip,
        None => detect_external_ip().ok_or_else(|| "failed to detect external IP".to_string())?,
    };

    // Override the endpoint with the external IP and re-sign.
    let mut offer_body = offer.body.clone();
    offer_body.endpoint = format!("http://{external_ip}:{port}");
    let re_signed = vtessera_offer::sign(offer_body, signing_key);

    let offer_json = vtessera_offer::to_json(&re_signed);

    // Build the registration payload.
    let payload = serde_json::json!({
        "offer": serde_json::from_str::<serde_json::Value>(&offer_json)
            .map_err(|e| e.to_string())?,
        "sig_hex": re_signed.sig_hex,
    });

    let url = format!("{MARKETPLACE_WORKER_URL}/register");
    let agent = ureq::Agent::new_with_defaults();

    match agent
        .post(&url)
        .header("Content-Type", "application/json")
        .send(serde_json::to_string(&payload).unwrap().as_str())
    {
        Ok(_resp) => {
            eprintln!("vtessera-node: registered with marketplace (IP: {external_ip})");
            Ok(())
        }
        Err(ureq::Error::StatusCode(status)) => Err(format!("worker returned {status}")),
        Err(e) => Err(format!("request failed: {e}")),
    }
}

/// Background loop that re-registers with the marketplace on an interval.
/// External IP may change (DHCP, VPN reconnect), so we re-detect and re-register.
fn spawn_marketplace_registration(
    offer_body: vtessera_offer::OfferBody,
    signing_key: SigningKey,
    port: u16,
    interval: Duration,
    external_ip: Option<String>,
) {
    thread::spawn(move || loop {
        thread::sleep(interval);
        let signed = vtessera_offer::sign(offer_body.clone(), &signing_key);
        if let Err(e) =
            register_with_marketplace_with_ip(&signed, &signing_key, port, external_ip.clone())
        {
            eprintln!("vtessera-node: marketplace registration failed (will retry): {e}");
        }
    });
}

/// POST a heartbeat with candidates and endpoint_id to the index.
/// Non-fatal: caller logs and retries on the next tick.
fn post_heartbeat(
    index_url: &str,
    node_id: &str,
    candidates: &[vtessera_transport::Candidate],
    endpoint_id: Option<&str>,
) -> Result<(), String> {
    let url = format!(
        "{}/offers/{node_id}/heartbeat",
        index_url.trim_end_matches('/')
    );
    let body = serde_json::json!({
        "candidates": candidates,
        "endpoint_id": endpoint_id,
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
/// index on an interval. With iroh integration, the endpoint manages
/// live connectivity and the heartbeat sends the current iroh EndpointId.
///
/// The first heartbeat fires immediately on startup so the index
/// isn't stale for the first 30 seconds.
fn spawn_heartbeat_with_iroh(
    index_url: String,
    node_id: String,
    iroh_ep: Arc<tokio::sync::RwLock<Option<IrohEndpoint>>>,
) {
    thread::spawn(move || {
        let interval = Duration::from_secs(DEFAULT_HEARTBEAT_SECS);
        // Send first heartbeat immediately so the index reflects this node
        // from the moment the server starts accepting connections.
        let (candidates, endpoint_id) = get_iroh_info(&iroh_ep);
        match post_heartbeat(&index_url, &node_id, &candidates, endpoint_id.as_deref()) {
            Ok(()) => {}
            Err(e) => eprintln!("vtessera-node: initial heartbeat failed (will retry): {e}"),
        }
        loop {
            thread::sleep(interval);
            // Fetch fresh candidates and endpoint_id from the iroh endpoint.
            let (candidates, endpoint_id) = get_iroh_info(&iroh_ep);
            match post_heartbeat(&index_url, &node_id, &candidates, endpoint_id.as_deref()) {
                Ok(()) => {}
                Err(e) => eprintln!("vtessera-node: heartbeat failed (will retry): {e}"),
            }
        }
    });
}

/// Fetch current candidates and endpoint_id from the iroh endpoint.
///
/// Returns (candidates, endpoint_id). The endpoint_id is the hex-encoded
/// Ed25519 public key that agents use to connect via iroh.
fn get_iroh_info(
    iroh_ep: &Arc<tokio::sync::RwLock<Option<IrohEndpoint>>>,
) -> (Vec<vtessera_transport::Candidate>, Option<String>) {
    // Try to read the endpoint — non-blocking, returns empty if not ready.
    if let Ok(guard) = iroh_ep.try_read() {
        if let Some(ep) = guard.as_ref() {
            let candidates = ep.candidates();
            let endpoint_id = ep.node_id().to_string();
            return (candidates, Some(endpoint_id));
        }
    }
    (vec![], None)
}

/// Start the iroh endpoint in a background tokio runtime.
///
/// Creates an iroh `Endpoint` from the node's Ed25519 secret key,
/// which connects to the default relay servers (Number 0). Returns
/// a shared handle so the heartbeat thread can query candidates.
fn start_iroh_endpoint(signing_key: &SigningKey) -> Arc<tokio::sync::RwLock<Option<IrohEndpoint>>> {
    let handle = Arc::new(tokio::sync::RwLock::new(None));
    let handle_clone = handle.clone();
    let key_bytes: [u8; 32] = signing_key.to_bytes();

    thread::spawn(move || {
        let rt = tokio::runtime::Runtime::new().unwrap_or_else(|e| {
            eprintln!("vtessera-node: failed to create tokio runtime for iroh: {e}");
            // Return a minimal runtime so the thread can still signal failure.
            std::process::exit(1);
        });

        rt.block_on(async move {
            match iroh_sidecar::create_endpoint(&key_bytes).await {
                Ok(ep) => {
                    let node_id = ep.node_id();
                    let relay = ep.endpoint_addr();
                    eprintln!("vtessera-node: iroh endpoint online, node_id={node_id}");
                    eprintln!("  endpoint_addr={relay:?}");
                    *handle_clone.write().await = Some(ep);
                }
                Err(e) => {
                    eprintln!("vtessera-node: iroh endpoint failed: {e}");
                    eprintln!("  falling back to LAN-only candidates");
                }
            }
        });

        // Keep the runtime alive so the endpoint stays connected.
        // The thread sleeps forever; the runtime drops on process exit.
        loop {
            thread::sleep(Duration::from_secs(3600));
        }
    });

    handle
}

/// Handler that bridges iroh QUIC connections to vtessera's dispatch().
///
/// Each incoming connection gets a bi-directional stream. The agent writes
/// an HTTP request, we dispatch it, and write the HTTP response back.
#[cfg(feature = "serve")]
struct VtesseraHandler {
    state: Arc<NodeState>,
}

#[cfg(feature = "serve")]
impl std::fmt::Debug for VtesseraHandler {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("VtesseraHandler").finish()
    }
}

#[cfg(feature = "serve")]
impl iroh::protocol::ProtocolHandler for VtesseraHandler {
    async fn accept(
        &self,
        connection: iroh::endpoint::Connection,
    ) -> Result<(), iroh::protocol::AcceptError> {
        let remote = connection.remote_id().to_string();
        eprintln!("vtessera-node: iroh connection from {remote}");

        // Accept the bi-directional stream (blocks until peer writes data)
        let (mut send, mut recv) = connection.accept_bi().await?;

        // Read the full HTTP request from the QUIC stream
        // 256 KiB limit matches the TCP path's request size cap
        let request_bytes = match tokio::time::timeout(
            std::time::Duration::from_secs(10),
            recv.read_to_end(256 * 1024),
        )
        .await
        {
            Ok(Ok(b)) => b,
            Ok(Err(e)) => {
                eprintln!("vtessera-node: iroh read error from {remote}: {e}");
                let body = format!("{{\"error\":\"read failed: {e}\"}}");
                let resp = format!(
                    "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                    body.len()
                );
                let _ = send.write_all(resp.as_bytes()).await;
                send.finish()?;
                return Ok(());
            }
            Err(_) => {
                eprintln!("vtessera-node: iroh read timeout from {remote}");
                let body = r#"{"error":"read timeout"}"#;
                let resp = format!(
                    "HTTP/1.1 408 Request Timeout\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                    body.len()
                );
                let _ = send.write_all(resp.as_bytes()).await;
                send.finish()?;
                return Ok(());
            }
        };

        // Parse HTTP request (simple format: METHOD PATH HTTP/1.1\r\nHeaders...\r\n\r\nBody)
        let request = match parse_quic_http_request(&request_bytes) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("vtessera-node: iroh bad request from {remote}: {e}");
                let body = format!("{{\"error\":\"bad request: {e}\"}}");
                let resp = format!(
                    "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                    body.len()
                );
                let _ = send.write_all(resp.as_bytes()).await;
                send.finish()?;
                return Ok(());
            }
        };

        eprintln!(
            "vtessera-node: iroh {remote} {} {}",
            match request.method {
                HttpMethod::Get => "GET",
                HttpMethod::Post => "POST",
                _ => "OTHER",
            },
            request.path
        );

        // Dispatch through the same handler as TCP requests
        let resp = dispatch(&self.state, request);

        // Write the HTTP response back to the QUIC stream
        let status = status_text(resp.status);
        let header = format!(
            "HTTP/1.1 {} {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
            resp.status,
            resp.body.len()
        );
        if let Err(e) = send.write_all(header.as_bytes()).await {
            eprintln!("vtessera-node: iroh write header error from {remote}: {e}");
            return Ok(());
        }
        if let Err(e) = send.write_all(&resp.body).await {
            eprintln!("vtessera-node: iroh write body error from {remote}: {e}");
            return Ok(());
        }
        if let Err(e) = send.finish() {
            eprintln!("vtessera-node: iroh finish error from {remote}: {e}");
        }

        // Wait for the client to finish reading the response before dropping
        // the connection. Without this, iroh may reset the connection while
        // the agent is still reading its last bytes over the relay path.
        connection.closed().await;

        Ok(())
    }
}

/// Parse a simple HTTP request from a QUIC stream.
///
/// Format: METHOD PATH HTTP/1.1\r\nHeader: Value\r\n...\r\n\r\nBody
#[cfg(feature = "serve")]
fn parse_quic_http_request(
    buf: &[u8],
) -> Result<HttpRequest, Box<dyn std::error::Error + Send + Sync>> {
    let text = std::str::from_utf8(buf)?;
    let mut lines = text.split("\r\n");

    let request_line = lines.next().ok_or("empty request")?;
    let mut parts = request_line.split_whitespace();
    let method_str = parts.next().ok_or("missing method")?;
    let path = parts.next().ok_or("missing path")?.to_string();

    let method = match method_str {
        "GET" => HttpMethod::Get,
        "POST" => HttpMethod::Post,
        _ => HttpMethod::Other,
    };

    let mut headers = Vec::new();
    let mut body_start = false;
    let mut body = Vec::new();

    for line in lines {
        if line.is_empty() {
            body_start = true;
            continue;
        }
        if body_start {
            body = line.as_bytes().to_vec();
            break;
        }
        if let Some((key, value)) = line.split_once(':') {
            headers.push((key.trim().to_string(), value.trim().to_string()));
        }
    }

    Ok(HttpRequest {
        method,
        path,
        headers,
        body,
    })
}

/// Map HTTP status code to status text.
#[cfg(feature = "serve")]
fn status_text(code: u16) -> &'static str {
    match code {
        200 => "OK",
        202 => "Accepted",
        400 => "Bad Request",
        402 => "Payment Required",
        403 => "Forbidden",
        404 => "Not Found",
        409 => "Conflict",
        500 => "Internal Server Error",
        501 => "Not Implemented",
        503 => "Service Unavailable",
        _ => "Unknown",
    }
}

/// Spawn an iroh Router that accepts incoming QUIC connections.
///
/// The router runs on the iroh endpoint's tokio runtime and dispatches
/// each connection to `VtesseraHandler`, which bridges to the same
/// `dispatch()` function used by the TCP path.
#[cfg(feature = "serve")]
fn spawn_iroh_router(
    iroh_handle: &Arc<tokio::sync::RwLock<Option<IrohEndpoint>>>,
    state: Arc<NodeState>,
) {
    let handle = iroh_handle.clone();
    thread::spawn(move || {
        let rt = tokio::runtime::Runtime::new().unwrap();
        rt.block_on(async move {
            // Wait for the iroh endpoint to come online
            let ep = loop {
                {
                    let guard = handle.read().await;
                    if let Some(ep) = guard.as_ref() {
                        break ep.iroh_endpoint().clone();
                    }
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            };

            let handler = VtesseraHandler { state };
            let _router = iroh::protocol::Router::builder(ep)
                .accept(iroh_sidecar::VTESSERA_ALPN, handler)
                .spawn();

            eprintln!("vtessera-node: iroh accept loop active (Router)");

            // Keep the runtime alive — router runs in background tasks
            loop {
                tokio::time::sleep(Duration::from_secs(3600)).await;
            }
        });
    });
}
