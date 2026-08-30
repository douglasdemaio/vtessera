//! Marketplace-level MCP stdio transport.
//!
//! Serves a [`MarketplaceMcpServer`] over newline-delimited JSON-RPC 2.0 on
//! stdio, the standard MCP client wiring (Claude Desktop, etc.): one JSON-RPC
//! message per line on stdin, responses on stdout. All logging goes to stderr
//! so stdout stays machine-clean.
//!
//! This is the *registry* view an agent uses to discover compute, as opposed
//! to `vtessera-mcp`, which fronts a single node. It reads the public
//! marketplace manifest (`nodes.json`) — and, when given, a local offer-index
//! — and lets the agent list offers, pull a single signed offer, and hand a
//! job to a chosen node. Paid-node negotiation is preserved: an unpaid POST to
//! a paid node returns its x402 402 challenge, and the agent resolves it
//! out-of-band.
//!
//! Behind the `serve` feature so the default library build opens no sockets.
//!
//! Run:
//!
//!   cargo run -p vtessera-node-api --bin vtessera-marketplace-mcp \
//!     --features serve \
//!     -- --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
//!        [--index http://<lan-ip>:8403] [--agent-id <my-agent-id>]

use std::env;
use std::io::{BufRead, Write};
use std::process;

use vtessera_node_api::mcp::{MarketplaceMcpServer, MCP_PROTOCOL_VERSION};

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-marketplace-mcp \
        --marketplace <nodes.json-url> \
        [--index <offer-index-url>] [--agent-id <id>]"
    );
    process::exit(2);
}

struct Args {
    marketplace_url: String,
    index_url: Option<String>,
    agent_id: Option<String>,
}

fn parse_args() -> Args {
    let mut marketplace: Option<String> = None;
    let mut index_url: Option<String> = None;
    let mut agent_id: Option<String> = None;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--marketplace" => marketplace = it.next(),
            "--index" => index_url = it.next(),
            "--agent-id" => agent_id = it.next(),
            "--help" | "-h" => usage_and_exit(),
            _ => {
                eprintln!("unknown argument: {a}");
                usage_and_exit();
            }
        }
    }
    match marketplace {
        Some(m) => Args {
            marketplace_url: m,
            index_url,
            agent_id,
        },
        None => usage_and_exit(),
    }
}

fn main() {
    let args = parse_args();

    let mut server = MarketplaceMcpServer::new(args.marketplace_url, args.index_url);
    if let Some(aid) = args.agent_id {
        server = server.with_agent_id(aid);
    }

    eprintln!("vtessera-marketplace-mcp: serving MCP {MCP_PROTOCOL_VERSION} on stdio");

    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();
    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("vtessera-marketplace-mcp: stdin error: {e}");
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
                        eprintln!("vtessera-marketplace-mcp: stdout error: {e}");
                        break;
                    }
                }
                Err(e) => eprintln!("vtessera-marketplace-mcp: failed to serialize response: {e}"),
            }
        }
        if let Err(e) = out.flush() {
            eprintln!("vtessera-marketplace-mcp: flush error: {e}");
            break;
        }
    }
}
