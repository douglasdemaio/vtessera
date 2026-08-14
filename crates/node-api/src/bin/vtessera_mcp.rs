//! MCP stdio transport — Module 2b (ROADMAP.md §2b).
//!
//! Spawns an MCP server (protocol `2024-11-05`) over newline-delimited
//! JSON-RPC 2.0 on stdio, the standard MCP client wiring (Claude Desktop,
//! etc.): one JSON-RPC message per line on stdin, responses on stdout.
//! All logging goes to stderr so stdout stays machine-clean.
//!
//! Like `vtessera-node`, this binary is the composition root: it supplies
//! the executor backend (ROADMAP.md §1) the MCP server invokes through its
//! `JobRunner` hook. Free-offer `submit_job` calls run synchronously here
//! and return the metering; paid offers still refuse until the on-chain
//! verifier lands (Module 4).
//!
//! Behind the `serve` feature so the default library build opens no
//! sockets and spawns no processes.
//!
//! Run:
//!
//!   cargo run -p vtessera-node-api --bin vtessera-mcp --features serve \
//!     -- --offer offer.json --escrow <PDA> --network solana-devnet \
//!        [--backend noop-cpu|local-cpu]
//!
//! Where `offer.json` is the JSON output of `vtessera_offer::to_json`.

use std::env;
use std::fs;
use std::io::{BufRead, Write};
use std::process;
use std::sync::Arc;

use vtessera_executor::{Backend, Executor, ExecutorError, JobMetering, JobSpec};
use vtessera_node_api::{mcp::McpServer, parse_signed_offer, JobRunError, JobRunner, NodeState};

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-mcp --offer <path.json> --escrow <pda> --network <id> \
        [--backend noop-cpu|local-cpu]"
    );
    process::exit(2);
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

    fn build(self, node_id: &str) -> Arc<dyn JobRunner> {
        match self {
            BackendChoice::NoopCpu => Arc::new(ExecutorRunner {
                executor: Box::new(vtessera_executor::NoopCpuExecutor),
                node_id: node_id.to_string(),
            }),
            BackendChoice::LocalCpu => {
                eprintln!(
                    "WARNING: --backend local-cpu runs job commands on the host with NO \
                     isolation. Only use for trusted workloads."
                );
                Arc::new(ExecutorRunner {
                    executor: Box::new(vtessera_executor::LocalCpuExecutor),
                    node_id: node_id.to_string(),
                })
            }
        }
    }
}

struct Args {
    offer_path: String,
    escrow_account: String,
    network: String,
    backend: BackendChoice,
}

fn parse_args() -> Args {
    let mut offer_path: Option<String> = None;
    let mut escrow: Option<String> = None;
    let mut network: Option<String> = None;
    let mut backend = BackendChoice::NoopCpu;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--offer" => offer_path = it.next(),
            "--escrow" => escrow = it.next(),
            "--network" => network = it.next(),
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
    match (offer_path, escrow, network) {
        (Some(o), Some(e), Some(n)) => Args {
            offer_path: o,
            escrow_account: e,
            network: n,
            backend,
        },
        _ => usage_and_exit(),
    }
}

/// Binary-side glue: parses the request body as an executor [`JobSpec`]
/// and runs it on the chosen backend. Mirrors `vtessera-node`'s runner.
struct ExecutorRunner {
    executor: Box<dyn Executor + Send + Sync>,
    node_id: String,
}

impl JobRunner for ExecutorRunner {
    fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
        let spec: JobSpec = serde_json::from_slice(body)
            .map_err(|e| JobRunError::bad_request(format!("invalid job JSON: {e}")))?;
        let metering = self.executor.run(&spec).map_err(|e| match e {
            ExecutorError::Admission(why) => JobRunError::bad_request(why),
            other => JobRunError::server(other.to_string()),
        })?;
        serde_json::to_string(&serde_json::json!({
            "status": "accepted",
            "job_id": spec.job_id,
            "node_id": self.node_id,
            "backend": backend_tag(&metering),
            "metering": metering,
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

    let runner = args.backend.build(&offer.body.node_id);

    let server = McpServer::new(NodeState {
        offer,
        escrow_account: args.escrow_account,
        network: args.network,
        runner: Some(runner),
    });

    eprintln!(
        "vtessera-mcp: serving MCP {} on stdio (backend {:?})",
        vtessera_node_api::mcp::MCP_PROTOCOL_VERSION,
        args.backend
    );

    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();
    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("vtessera-mcp: stdin error: {e}");
                break;
            }
        };
        if line.trim().is_empty() {
            continue;
        }
        if let Some(resp) = server.handle(&line) {
            match serde_json::to_string(&resp) {
                Ok(text) => {
                    if let Err(e) = writeln!(out, "{text}") {
                        eprintln!("vtessera-mcp: stdout error: {e}");
                        break;
                    }
                }
                Err(e) => eprintln!("vtessera-mcp: failed to serialize response: {e}"),
            }
        }
        // Notifications produce no output by design; flush so the client
        // sees responses immediately.
        if let Err(e) = out.flush() {
            eprintln!("vtessera-mcp: flush error: {e}");
            break;
        }
    }
}
