#![cfg(feature = "serve")]

//! Minimal coordinator daemon — an opt-in queue/rendezvous for outbound-only
//! nodes (P1.5). Serves the work queue over iroh QUIC on `vtessera/0`,
//! addressed by its Ed25519 pubkey.
//!
//! Usage:
//!   vtessera-coordinator --key <path-to-32-byte-seed>    # deterministic id
//!   vtessera-coordinator                                 # generated id, printed
//!   vtessera-coordinator submit --addr <endpoint.json> --job <job.json>
//!                                                       # enqueue a job (agent/operator)
//!
//! Trust is opt-in: nodes pin a coordinator by pubkey (§7c); nothing uses
//! this binary unless an operator explicitly wires it up.

use std::path::PathBuf;
use std::process::exit;

use ed25519_dalek::SigningKey;
use iroh::protocol::Router;
use vtessera_coordinator::iroh::{CoordinatorQueueHandler, QueueClient};
use vtessera_coordinator::CoordinatorId;
use vtessera_transport::iroh_sidecar::VTESSERA_ALPN;

fn main() {
    let mut args = std::env::args_os().skip(1);
    let mut key_path: Option<PathBuf> = None;
    let mut addr_out: Option<PathBuf> = None;
    while let Some(arg) = args.next() {
        match arg.to_str() {
            Some("--key") => {
                key_path = args.next().map(PathBuf::from);
                if key_path.is_none() {
                    eprintln!("vtessera-coordinator: --key requires a path");
                    exit(2);
                }
            }
            Some("--addr-out") => {
                addr_out = args.next().map(PathBuf::from);
                if addr_out.is_none() {
                    eprintln!("vtessera-coordinator: --addr-out requires a path");
                    exit(2);
                }
            }
            Some("submit") => {
                let rest: Vec<String> = args.map(|a| a.to_string_lossy().into_owned()).collect();
                return cmd_submit(&rest);
            }
            Some("--help") | Some("-h") => {
                println!(
                    "vtessera-coordinator — opt-in queue/rendezvous for outbound-only nodes\n\
                     \n\
                     USAGE:\n\
                     \x20 vtessera-coordinator [--key <path>] [--addr-out <path>]\n\
                     \x20 vtessera-coordinator submit --addr <endpoint.json> --job <job.json>\n\n\
                     Prints its EndpointId (Ed25519 pubkey). Nodes/agents pin it explicitly.\n\
                     `--addr-out <path>` also writes this coordinator's serialized\n\
                     `EndpointAddr` (JSON) so nodes/agents can dial it directly.\n\
                     `submit` enqueues a job at a pinned coordinator (agent/operator client);\n\
                     it does not serve anything."
                );
                return;
            }
            _ => {
                eprintln!(
                    "vtessera-coordinator: unknown argument {:?} (try --help)",
                    arg.to_string_lossy()
                );
                exit(2);
            }
        }
    }

    let (seed, fresh) = match &key_path {
        Some(path) => (read_seed(path), false),
        None => {
            use rand::RngCore;
            let mut seed = [0u8; 32];
            rand::thread_rng().fill_bytes(&mut seed);
            (seed, true)
        }
    };

    let signing_key = SigningKey::from_bytes(&seed);
    let id = CoordinatorId::from_seed(&seed);

    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async move {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .secret_key(iroh::SecretKey::from_bytes(&seed))
            .bind()
            .await
            .unwrap_or_else(|e| {
                eprintln!("vtessera-coordinator: bind failed: {e}");
                exit(1);
            });

        let handler = CoordinatorQueueHandler::new(signing_key);
        let _router = Router::builder(endpoint.clone())
            .accept(VTESSERA_ALPN, handler)
            .spawn();

        // Let iroh discover its local (loopback/LAN) addresses, then — if
        // asked — persist the serialized EndpointAddr for the node/agent to
        // pin (their `--coordinator-addr` reads exactly this JSON).
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        if let Some(path) = addr_out {
            let loopback_addrs = endpoint
                .addr()
                .addrs
                .iter()
                .filter(|a| matches!(a, iroh::TransportAddr::Ip(_)))
                .cloned()
                .collect::<std::collections::BTreeSet<_>>();
            let addr = iroh::EndpointAddr {
                id: endpoint.id(),
                addrs: loopback_addrs,
            };
            if let Err(e) = std::fs::write(&path, serde_json::to_vec(&addr).unwrap()) {
                eprintln!("vtessera-coordinator: failed to write --addr-out {path:?}: {e}");
            } else {
                println!("vtessera-coordinator: addr pinned at {path:?}");
            }
        }

        println!("vtessera-coordinator: endpoint_id={}", id.0);
        if fresh {
            eprintln!("(no --key given; identity is ephemeral and will change on restart)");
        }
        println!("vtessera-coordinator: serving queue over iroh (vtessera/0)");
        loop {
            tokio::time::sleep(std::time::Duration::from_secs(3600)).await;
        }
    });
}

fn read_seed(path: &std::path::Path) -> [u8; 32] {
    let bytes = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) => {
            eprintln!("vtessera-coordinator: cannot read key {path:?}: {e}");
            exit(1);
        }
    };
    let seed: Result<[u8; 32], Vec<u8>> = bytes.try_into();
    match seed {
        Ok(seed) => seed,
        Err(v) => {
            eprintln!(
                "vtessera-coordinator: key {path:?} must be exactly 32 bytes (got {})",
                v.len()
            );
            exit(1);
        }
    }
}

/// `submit` — enqueue a job at a pinned coordinator over iroh, then print
/// the coordinator-scoped job id. Agent/operator client mode; no serving.
fn cmd_submit(args: &[String]) {
    let mut addr_path: Option<PathBuf> = None;
    let mut job_path: Option<PathBuf> = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--addr" => {
                addr_path = Some(PathBuf::from(args.get(i + 1).unwrap_or_else(|| {
                    eprintln!("vtessera-coordinator submit: --addr requires a path");
                    exit(2);
                })));
                i += 2;
            }
            "--job" => {
                job_path = Some(PathBuf::from(args.get(i + 1).unwrap_or_else(|| {
                    eprintln!("vtessera-coordinator submit: --job requires a path");
                    exit(2);
                })));
                i += 2;
            }
            "--help" | "-h" => {
                println!(
                    "vtessera-coordinator submit — enqueue a job at a pinned coordinator\n\n\
                     USAGE:\n  vtessera-coordinator submit --addr <endpoint.json> --job <job.json>\n\n\
                     `--addr` is the coordinator's serialized `EndpointAddr` (JSON),\n\
                     `--job` the job body to run. Prints the coordinator-scoped job id."
                );
                return;
            }
            other => {
                eprintln!("vtessera-coordinator submit: unknown argument {other:?} (--addr/--job)");
                exit(2);
            }
        }
    }
    let (Some(addr_path), Some(job_path)) = (addr_path, job_path) else {
        eprintln!(
            "vtessera-coordinator submit: need both --addr <endpoint.json> and --job <job.json>"
        );
        exit(2);
    };

    let coordinator_addr: iroh::EndpointAddr = {
        let raw = match std::fs::read_to_string(&addr_path) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("vtessera-coordinator submit: cannot read {addr_path:?}: {e}");
                exit(1);
            }
        };
        match serde_json::from_str(&raw) {
            Ok(a) => a,
            Err(e) => {
                eprintln!("vtessera-coordinator submit: bad EndpointAddr in {addr_path:?}: {e}");
                exit(1);
            }
        }
    };

    let job: serde_json::Value = {
        let raw = match std::fs::read_to_string(&job_path) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("vtessera-coordinator submit: cannot read {job_path:?}: {e}");
                exit(1);
            }
        };
        match serde_json::from_str(&raw) {
            Ok(j) => j,
            Err(e) => {
                eprintln!("vtessera-coordinator submit: bad job JSON in {job_path:?}: {e}");
                exit(1);
            }
        }
    };

    let rt = tokio::runtime::Runtime::new().unwrap();
    rt.block_on(async move {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .bind()
            .await
            .unwrap_or_else(|e| {
                eprintln!("vtessera-coordinator submit: bind failed: {e}");
                exit(1);
            });
        let client = QueueClient::new(endpoint, coordinator_addr);
        match client.enqueue(job).await {
            Ok(job_id) => println!("vtessera-coordinator submit: enqueued {}", job_id.display()),
            Err(e) => {
                eprintln!("vtessera-coordinator submit: enqueue failed: {e}");
                exit(1);
            }
        }
    });
}
