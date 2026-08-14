//! MCP stdio transport — Module 2b (ROADMAP.md §2b).
//!
//! Spawns an MCP server (protocol `2024-11-05`) over newline-delimited
//! JSON-RPC 2.0 on stdio, the standard MCP client wiring (Claude Desktop,
//! etc.): one JSON-RPC message per line on stdin, responses on stdout.
//! All logging goes to stderr so stdout stays machine-clean.
//!
//! Behind the `serve` feature so the default library build opens no
//! sockets and spawns no processes.
//!
//! Run:
//!
//!   cargo run -p vtessera-node-api --bin vtessera-mcp --features serve \
//!     -- --offer offer.json --escrow <PDA> --network solana-devnet
//!
//! Where `offer.json` is the JSON output of `vtessera_offer::to_json`.

use std::env;
use std::fs;
use std::io::{BufRead, Write};
use std::process;

use vtessera_node_api::{mcp::McpServer, parse_signed_offer, NodeState};

fn usage_and_exit() -> ! {
    eprintln!("usage: vtessera-mcp --offer <path.json> --escrow <pda> --network <id>");
    process::exit(2);
}

struct Args {
    offer_path: String,
    escrow_account: String,
    network: String,
}

fn parse_args() -> Args {
    let mut offer_path: Option<String> = None;
    let mut escrow: Option<String> = None;
    let mut network: Option<String> = None;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
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
    match (offer_path, escrow, network) {
        (Some(o), Some(e), Some(n)) => Args {
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
    let offer = parse_signed_offer(&raw).unwrap_or_else(|e| {
        eprintln!("failed to parse offer JSON: {e}");
        process::exit(1);
    });

    let server = McpServer::new(NodeState {
        offer,
        escrow_account: args.escrow_account,
        network: args.network,
    });

    eprintln!(
        "vtessera-mcp: serving MCP {} on stdio",
        vtessera_node_api::mcp::MCP_PROTOCOL_VERSION
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
