//! Offer-index HTTP server — Module 2a (ROADMAP.md §2a).
//!
//! Serves the verified offer index over HTTP and, optionally, seeds it by
//! pulling signed offers from known nodes on an interval.
//!
//! Behind the `serve` feature so the default build of `vtessera-offer-index`
//! stays a socket-free library.
//!
//! Run:
//!
//!   # register-only mode:
//!   cargo run -p vtessera-offer-index --bin vtessera-offer-index --features serve \
//!     -- --bind 127.0.0.1:8403
//!
//!   # pull known nodes on an interval (push + pull coexist):
//!   cargo run -p vtessera-offer-index --bin vtessera-offer-index --features serve \
//!     -- --bind 127.0.0.1:8403 \
//!     --seed http://127.0.0.1:8402,https://node-b.example/vtessera \
//!     --seed-interval 60
//!
//! Routes:
//!
//!   GET    /healthz
//!   GET    /offers[?mode=free|paid&device=cpu|nvidia_gpu|nvidia_mig|amd_gpu]
//!   GET    /offers/{node_id}
//!   POST   /offers            (body: signed offer JSON; 201 | 400)
//!   DELETE /offers/{node_id}
//!
//! The pull poller keeps last-good entries on network failure (a node being
//! briefly unreachable must not evict a verified offer) and only evicts on a
//! verification failure (signature/tamper/expiry).

use std::env;
use std::net::TcpListener;
use std::process;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use vtessera_mini_http::serve;
use vtessera_offer::{verify, SignedOffer};
use vtessera_offer_index::{dispatch, IndexState};

const DEFAULT_SEED_INTERVAL_SECS: u64 = 60;

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-offer-index --bind <host:port> \
        [--seed <url1,url2,...>] [--seed-interval <secs>]"
    );
    process::exit(2);
}

struct Args {
    bind: String,
    seeds: Vec<String>,
    seed_interval: Duration,
}

fn parse_args() -> Args {
    let mut bind: Option<String> = None;
    let mut seeds: Vec<String> = Vec::new();
    let mut interval: u64 = DEFAULT_SEED_INTERVAL_SECS;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--bind" => bind = it.next(),
            "--seed" => {
                if let Some(list) = it.next() {
                    seeds.extend(
                        list.split(',')
                            .filter(|s| !s.is_empty())
                            .map(str::to_string),
                    );
                }
            }
            "--seed-interval" => {
                if let Some(s) = it.next() {
                    interval = s.parse().unwrap_or(DEFAULT_SEED_INTERVAL_SECS);
                }
            }
            "--help" | "-h" => usage_and_exit(),
            _ => {
                eprintln!("unknown argument: {a}");
                usage_and_exit();
            }
        }
    }
    match bind {
        Some(b) => Args {
            bind: b,
            seeds,
            seed_interval: Duration::from_secs(interval),
        },
        None => usage_and_exit(),
    }
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Pull a node's `/offer`, verify it, and return `(offer, source)`. Kept
/// out of `dispatch` so the lib stays socket-free.
fn fetch_offer(base: &str) -> Result<(SignedOffer, String), String> {
    let url = format!("{}/offer", base.trim_end_matches('/'));
    let response = ureq::get(&url).call().map_err(|e| e.to_string())?;
    let body = response
        .into_body()
        .read_to_string()
        .map_err(|e| format!("read body from {url}: {e}"))?;
    let offer: SignedOffer =
        serde_json::from_str(&body).map_err(|e| format!("bad offer JSON from {url}: {e}"))?;
    verify(&offer, Some(now_unix())).map_err(|e| format!("offer from {url} rejected: {e}"))?;
    Ok((offer, format!("pull:{base}")))
}

fn main() {
    let args = parse_args();

    let state = Arc::new(Mutex::new(IndexState::new()));

    if !args.seeds.is_empty() {
        let seeds = args.seeds.clone();
        let interval = args.seed_interval;
        let state = state.clone();
        thread::spawn(move || loop {
            for base in &seeds {
                match fetch_offer(base) {
                    Ok((offer, source)) => {
                        let node_id = state.lock().unwrap().register(offer, source, now_unix());
                        match node_id {
                            Ok(id) => eprintln!("seeded offer from {base} ({id})"),
                            Err(e) => eprintln!("offer from {base} rejected: {e}"),
                        }
                    }
                    Err(e) => eprintln!("seed {base} failed (keeping last good): {e}"),
                }
            }
            thread::sleep(interval);
        });
    }

    // Stale entry pruner — drops entries that haven't heartbeated in 3x
    // the heartbeat interval (90s default). Nodes that die or shut down
    // gracefully will be cleaned up automatically.
    {
        let state = state.clone();
        let stale_secs = 3 * vtessera_transport::DEFAULT_HEARTBEAT_SECS;
        thread::spawn(move || loop {
            thread::sleep(Duration::from_secs(
                vtessera_transport::DEFAULT_HEARTBEAT_SECS,
            ));
            let now = now_unix();
            let pruned = state.lock().unwrap().prune_stale(now, stale_secs);
            if pruned > 0 {
                eprintln!("pruned {pruned} stale entries");
            }
        });
    }

    let listener = TcpListener::bind(&args.bind).unwrap_or_else(|e| {
        eprintln!("bind {}: {e}", args.bind);
        process::exit(1);
    });
    eprintln!(
        "vtessera-offer-index: listening on {} ({} seed(s))",
        args.bind,
        args.seeds.len()
    );

    serve(
        listener,
        move |req| dispatch(&mut state.lock().unwrap(), req, now_unix()),
        32,
    );
}
