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
//!
//! Routes (see `vtessera_node_api::dispatch`):
//!
//!   GET  /offer
//!   GET  /mcp/manifest            (legacy MCP manifest)
//!   POST /mcp                     (MCP 2024-11-05 JSON-RPC)
//!   GET  /.well-known/agent.json  (A2A agent card)
//!   POST /jobs                    (x402 challenge / free-job execution)
//!   GET  /healthz

use std::env;
use std::fs;
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process;
use std::sync::Arc;

use vtessera_executor::{Backend, Executor, ExecutorError, JobMetering, JobSpec};
use vtessera_mini_http::{serve, Method as MiniMethod, Request as MiniRequest, Response};
use vtessera_node_api::{
    dispatch, parse_signed_offer, HttpMethod, HttpRequest, JobRunError, JobRunner, NodeState,
};
use vtessera_settlement::SigningKey;
use vtessera_settlement::{
    derive_node_id, load_node_key, sign_job_receipt, JobReceipt, JOB_RECEIPT_SCHEMA_VER,
};

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-node --bind <host:port> --offer <path.json> \
        --escrow <pda> --network <id> \
        --key <identity.key> --state-dir <dir> \
        [--backend noop-cpu|local-cpu]"
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
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum BackendChoice {
    NoopCpu,
    LocalCpu,
}

impl BackendChoice {
    fn parse(s: &str) -> Option<Self> {
        match s {
            "noop-cpu" => Some(BackendChoice::NoopCpu),
            "local-cpu" => Some(BackendChoice::LocalCpu),
            _ => None,
        }
    }

    fn build(self, id: &NodeIdentity) -> Arc<dyn JobRunner> {
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
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--bind" => bind = it.next(),
            "--offer" => offer_path = it.next(),
            "--escrow" => escrow = it.next(),
            "--network" => network = it.next(),
            "--key" => key_path = it.next(),
            "--state-dir" => state_dir = it.next(),
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
    let runner = args.backend.build(&identity);

    let state = NodeState {
        offer,
        escrow_account: args.escrow_account,
        network: args.network,
        runner: Some(runner),
    };

    let listener = TcpListener::bind(&args.bind).unwrap_or_else(|e| {
        eprintln!("bind {}: {e}", args.bind);
        process::exit(1);
    });
    eprintln!(
        "vtessera-node: listening on {} (backend {:?})",
        args.bind, args.backend
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
