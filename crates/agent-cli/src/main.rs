use clap::{Parser, Subcommand};
use std::path::{Path, PathBuf};
use std::process;

#[derive(Parser)]
#[command(
    name = "vtessera-agent",
    about = "CLI for AI agents to interact with vtessera nodes"
)]
struct Cli {
    #[command(subcommand)]
    command: Commands,

    /// Node HTTP endpoint (direct dial)
    #[arg(long, default_value = "http://127.0.0.1:8402", global = true)]
    node: String,

    /// Resolve and dial a node by its iroh EndpointId (hex, as published in
    /// the offer and offer-index) instead of an HTTP endpoint. Candidates
    /// come from the offer-index; the agent dials over iroh QUIC and speaks
    /// HTTP-over-QUIC to the node (design T2.1 resolver).
    #[arg(long, global = true)]
    node_id: Option<String>,

    /// Coordinator queue rendezvous: path to the coordinator's EndpointAddr
    /// JSON, or the string `endpoint=<endpoint-json>` / `queue=<path>`.
    /// When set, submit/health/offer rendezvous through the queue instead
    /// of direct HTTP dial (P1.7e).
    #[arg(long, global = true)]
    queue: Option<String>,

    /// Offer-index URL
    #[arg(long, default_value = "http://127.0.0.1:8403", global = true)]
    index: String,

    /// GitHub Pages marketplace URL (e.g. https://douglasdemaio.github.io/vtessera/marketplace/nodes.json)
    #[arg(long, global = true)]
    marketplace: Option<String>,

    /// Agent identity for claim gate
    #[arg(long, global = true)]
    agent_id: Option<String>,

    /// Output raw JSON
    #[arg(long, global = true)]
    json: bool,

    /// Auto-discover node from the local discovery file
    #[arg(long, global = true)]
    local: bool,

    /// x402 payment proof (JSON `{"tx":"<signature>","amount_micros":<n>}`)
    /// attached as the `x-payment` header on a paid job submit. Pay the
    /// escrow first (AGENTS.md x402 flow), then resubmit with `--payment`.
    #[arg(long, global = true)]
    payment: Option<String>,
}

#[derive(Subcommand)]
enum Commands {
    /// Query offer-index or marketplace for available free nodes
    Discover,
    /// Fetch a node's signed offer
    Offer,
    /// Submit a free job
    Submit {
        /// Path to JobSpec JSON file
        #[arg(short, long)]
        job: String,
    },
    /// Check if a node is up
    Health,
}

#[derive(serde::Deserialize)]
struct DiscoveryFile {
    endpoint: String,
    #[allow(dead_code)]
    node_id: Option<String>,
    index: Option<String>,
    pid: Option<u32>,
    /// Coordinator queue rendezvous for outbound-only nodes (P1.7e): a path
    /// to the coordinator's `EndpointAddr` JSON. When present, the node is
    /// reached through the queue rather than a direct HTTP dial.
    #[allow(dead_code)]
    queue: Option<String>,
}

fn discovery_file_path() -> PathBuf {
    let data_dir = std::env::var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            let home = std::env::var_os("HOME").unwrap_or_default();
            PathBuf::from(home).join(".local/share")
        });
    data_dir.join("vtessera/node-discovery.json")
}

fn read_discovery() -> Option<DiscoveryFile> {
    let path = discovery_file_path();
    let data = std::fs::read_to_string(&path).ok()?;
    let disc: DiscoveryFile = serde_json::from_str(&data).ok()?;

    // Check if the process is still alive.
    if let Some(pid) = disc.pid {
        // kill(pid, 0) checks process existence without sending a signal.
        let alive = unsafe { libc::kill(pid as i32, 0) == 0 };
        if !alive {
            return None;
        }
    }

    Some(disc)
}

fn main() {
    let cli = Cli::parse();
    let agent_id = cli.agent_id.unwrap_or_else(|| {
        use std::time::{SystemTime, UNIX_EPOCH};
        let t = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        format!("agent-{:x}", t)
    });

    let json = cli.json;
    let default_index = cli.index;

    // Resolve node and index: --local reads the discovery file, otherwise
    // use the explicit --node/--index flags (or their defaults).
    let (node, index) = if cli.local {
        match read_discovery() {
            Some(disc) => (disc.endpoint, disc.index.unwrap_or(default_index)),
            None => {
                eprintln!(
                    "error: no running node found (discovery file missing or stale: {})",
                    discovery_file_path().display()
                );
                process::exit(1);
            }
        }
    } else {
        (cli.node, default_index)
    };

    // P1.7e queue rendezvous: an explicit --queue wins; otherwise a
    // coordinator pin recorded in the discovery file.
    let queue = match &cli.queue {
        Some(q) if q.starts_with("queue=") => Some(q.trim_start_matches("queue=").to_owned()),
        Some(q) => Some(q.clone()),
        None if cli.local => {
            let disc = read_discovery();
            disc.and_then(|d| d.queue)
        }
        None => None,
    };

    let result = match &cli.command {
        Commands::Discover => discover(&index, cli.marketplace.as_deref(), json),
        // P2/T2.1 resolver: --node-id resolves the node from the offer-index
        // and dials over iroh QUIC; it wins over the queue and HTTP paths.
        Commands::Offer => {
            if let Some(nid) = &cli.node_id {
                quic_offer(&index, cli.marketplace.as_deref(), nid, json)
            } else {
                offer(&node, queue.as_deref(), json)
            }
        }
        Commands::Submit { job } => {
            if let Some(raw) = &cli.payment {
                let valid = serde_json::from_str::<serde_json::Value>(raw)
                    .map(|proof| {
                        proof["tx"].as_str().is_some() && proof["amount_micros"].as_u64().is_some()
                    })
                    .unwrap_or(false);
                if !valid {
                    eprintln!(
                        "error: --payment must be JSON {{\"tx\":\"<signature>\",\"amount_micros\":<amount>}}"
                    );
                    process::exit(1);
                }
            }

            if let Some(nid) = &cli.node_id {
                quic_submit(
                    &index,
                    cli.marketplace.as_deref(),
                    nid,
                    &agent_id,
                    job,
                    json,
                    cli.payment.as_deref(),
                )
            } else {
                submit(
                    &node,
                    queue.as_deref(),
                    &agent_id,
                    job,
                    json,
                    cli.payment.as_deref(),
                )
            }
        }
        Commands::Health => {
            if let Some(nid) = &cli.node_id {
                quic_health(&index, cli.marketplace.as_deref(), nid, json)
            } else {
                health(&node, queue.as_deref(), json)
            }
        }
    };

    if let Err(e) = result {
        eprintln!("error: {e}");
        process::exit(1);
    }
}

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_defaults()
}

/// Extract the primary reachability endpoint from an offer body.
///
/// The offer schema (v2) carries `endpoint` as a list; older offers
/// (v1, still in flight) used a bare string. Accept either and return the
/// first entry, mirroring the "ordered list" semantics of the new schema.
fn offer_endpoint(body: &serde_json::Value) -> Option<String> {
    if let Some(eps) = body["endpoint"].as_array() {
        return eps.first().and_then(|e| e.as_str()).map(String::from);
    }
    body["endpoint"].as_str().map(String::from)
}

fn discover(index: &str, marketplace: Option<&str>, json: bool) -> Result<(), String> {
    // Try the local offer-index first.
    let local_url = format!("{index}/offers?available=1&mode=free");
    let local_result = agent()
        .get(&local_url)
        .call()
        .ok()
        .and_then(|mut resp| resp.body_mut().read_json::<serde_json::Value>().ok());

    // Try the marketplace if provided.
    let market_result = marketplace.and_then(|url| {
        agent()
            .get(url)
            .call()
            .ok()
            .and_then(|mut resp| resp.body_mut().read_json::<serde_json::Value>().ok())
    });

    // Merge results: local index takes priority, marketplace fills in.
    // Identity is the iroh EndpointId when offered, else the HTTP endpoint —
    // outbound-only nodes (no HTTP endpoint; P1.7e) stay listed by their id.
    let mut offers: Vec<serde_json::Value> = Vec::new();
    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();

    // Process local index results.
    if let Some(resp) = &local_result {
        let arr = if resp.is_array() {
            resp.as_array().cloned().unwrap_or_default()
        } else {
            resp["offers"].as_array().cloned().unwrap_or_default()
        };
        for o in arr {
            let body = &o["offer"]["body"];
            let key = body["endpoint_id"]
                .as_str()
                .map(str::to_owned)
                .or_else(|| offer_endpoint(body))
                .unwrap_or_default();
            if !key.is_empty() && seen.insert(key) {
                offers.push(o);
            }
        }
    }

    // Process marketplace results.
    if let Some(resp) = &market_result {
        if let Some(nodes) = resp["nodes"].as_array() {
            for node in nodes {
                if let Some(offer) = node.get("offer") {
                    let body = &offer["body"];
                    let key = body["endpoint_id"]
                        .as_str()
                        .map(str::to_owned)
                        .or_else(|| offer_endpoint(body))
                        .unwrap_or_default();
                    if !key.is_empty() && seen.insert(key) {
                        offers.push(offer.clone());
                    }
                }
            }
        }
    }

    if json {
        let merged = serde_json::json!({
            "local_index": local_result,
            "marketplace": market_result,
            "merged_count": offers.len(),
        });
        println!("{}", serde_json::to_string_pretty(&merged).unwrap());
        return Ok(());
    }

    if offers.is_empty() {
        println!("no free nodes available");
        if marketplace.is_some() {
            println!("(checked local index and marketplace)");
        } else {
            println!("(tip: use --marketplace <url> to also search the public marketplace)");
        }
        return Ok(());
    }

    println!(
        "{:<20} {:<15} {:<40} {:<10}",
        "NODE_ID", "DEVICE", "REACH", "DIAL"
    );
    println!("{}", "-".repeat(90));
    for o in &offers {
        let body = if o.get("offer").is_some() {
            &o["offer"]["body"]
        } else {
            &o["body"]
        };
        let node_id = body["node_id"].as_str().unwrap_or("?");
        let device = body["device"]["kind"].as_str().unwrap_or("?");
        let endpoint_id = body["endpoint_id"].as_str().unwrap_or("");
        let reach = offer_endpoint(body).unwrap_or_else(|| {
            if !endpoint_id.is_empty() {
                format!("iroh:{endpoint_id}")
            } else {
                "?".into()
            }
        });
        let dial = if endpoint_id.is_empty() {
            "http"
        } else {
            "quic"
        };
        println!("{node_id:<20} {device:<15} {reach:<40} {dial:<10}");
    }
    println!("\n{} node(s) found", offers.len());
    println!("(resolve by id: vtessera-agent --node-id <endpoint_id> submit --job job.json)");
    Ok(())
}

/// Load a coordinator `EndpointAddr` from its JSON pin (a path, or the
/// literal JSON when the queue was passed inline).
fn load_coordinator_addr(queue: &str) -> Result<iroh::EndpointAddr, String> {
    let raw = if Path::new(queue).exists() {
        std::fs::read_to_string(queue).map_err(|e| format!("read {queue:?}: {e}"))?
    } else {
        queue.to_owned()
    };
    serde_json::from_str(&raw).map_err(|e| format!("parse coordinator EndpointAddr: {e}"))
}

/// Build a queue client for the pinned coordinator (fresh outbound iroh
/// endpoint, no listener — mirrors the node's outbound pull path).
fn queue_client(queue: &str) -> Result<vtessera_coordinator::iroh::QueueClient, String> {
    let addr = load_coordinator_addr(queue)?;
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| format!("tokio runtime: {e}"))?;
    rt.block_on(async move {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .bind()
            .await
            .map_err(|e| format!("bind iroh endpoint: {e}"))?;
        Ok(vtessera_coordinator::iroh::QueueClient::new(endpoint, addr))
    })
}

fn offer(node: &str, queue: Option<&str>, json: bool) -> Result<(), String> {
    if let Some(q) = queue {
        return queue_render("offer", q, json);
    }
    let url = format!("{node}/offer");
    let resp: serde_json::Value = agent()
        .get(&url)
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("failed to read response: {e}"))?;

    if json {
        println!("{}", serde_json::to_string_pretty(&resp).unwrap());
    } else {
        let body = &resp["body"];
        let node_id = body["node_id"].as_str().unwrap_or("?");
        let endpoint = offer_endpoint(body).unwrap_or_else(|| "?".into());
        let device = body["device"]["kind"].as_str().unwrap_or("?");
        let price = if body["price"]["mode"].as_str() == Some("free") {
            "free".to_string()
        } else {
            format!(
                "{}/s {}",
                body["price"]["per_device_second_micros"]
                    .as_f64()
                    .unwrap_or(0.0)
                    / 1_000_000.0,
                body["price"]["currency"].as_str().unwrap_or("?")
            )
        };
        println!("node:    {node_id}");
        println!("endpoint: {endpoint}");
        println!("device:  {device}");
        println!("price:   {price}");
    }
    Ok(())
}

fn submit(
    node: &str,
    queue: Option<&str>,
    agent_id: &str,
    job_path: &str,
    json: bool,
    payment: Option<&str>,
) -> Result<(), String> {
    let job_json =
        std::fs::read_to_string(job_path).map_err(|e| format!("failed to read {job_path}: {e}"))?;
    let job: serde_json::Value = serde_json::from_str(&job_json)
        .map_err(|e| format!("invalid job JSON in {job_path}: {e}"))?;

    // Queue rendezvous: enqueue over iroh, the node pulls + runs + acks.
    if let Some(q) = queue {
        if payment.is_some() {
            return Err("x402 payment is not supported over queue rendezvous".into());
        }
        let client = queue_client(q)?;
        let coordinator = client.coordinator_id_hex();
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .map_err(|e| format!("tokio runtime: {e}"))?;
        let scoped_id = rt
            .block_on(client.enqueue(job))
            .map_err(|e| format!("enqueue failed: {e}"))?;
        if json {
            println!(
                "{}",
                serde_json::json!({
                    "status": "queued",
                    "coordinator_id": coordinator,
                    "job_id": format!("{}", scoped_id.display()),
                    "mode": "queue-rendezvous"
                })
            );
        } else {
            println!("status:  queued");
            println!("coordinator: {coordinator}");
            println!("job_id:  {}", scoped_id.display());
            println!("mode:    queue-rendezvous (node pulls from queue)");
        }
        return Ok(());
    }

    let url = format!("{node}/jobs");
    let (status, v) = post_job(&url, agent_id, payment, &job_json)?;
    render_submit_outcome(status, &v, payment.is_some(), json)?;
    Ok(())
}

/// POST a job over HTTP and return `(status, parsed JSON body)`.
///
/// Uses `http_status_as_error(false)` so the x402 challenge (HTTP 402) is
/// surfaced as a body instead of being dropped by ureq's status-as-error
/// behaviour. Unparseable bodies (non-JSON proxies etc.) are preserved as
/// `{"raw": "<text>"}` so the message survives.
fn post_job(
    url: &str,
    agent_id: &str,
    payment: Option<&str>,
    job_json: &str,
) -> Result<(u16, serde_json::Value), String> {
    use std::io::Read;
    let agent = ureq::config::Config::builder()
        .http_status_as_error(false)
        .build()
        .new_agent();
    let mut req = agent.post(url).header("x-agent-id", agent_id);
    if let Some(p) = payment {
        req = req.header("x-payment", p);
    }
    let mut resp = req
        .send(job_json)
        .map_err(|e| format!("request failed: {e}"))?;
    let status = resp.status().as_u16();
    let mut bytes: Vec<u8> = Vec::new();
    resp.body_mut()
        .as_reader()
        .read_to_end(&mut bytes)
        .map_err(|e| format!("failed to read response body: {e}"))?;
    match serde_json::from_slice::<serde_json::Value>(&bytes) {
        Ok(v) => Ok((status, v)),
        Err(_) => Ok((
            status,
            serde_json::json!({ "raw": String::from_utf8_lossy(&bytes).trim_end_matches('\n') }),
        )),
    }
}

/// Classify + render the outcome of a job submission, shared by the HTTP
/// and QUIC (wire-parsed) transports.
///
/// - HTTP 200 -> accepted, with metering when the node reports it.
/// - HTTP 402 without a payment proof -> the x402 challenge + how to pay.
/// - HTTP 402 with a proof -> challenge + error (the proof was rejected).
/// - Anything else -> the node's error message.
fn render_submit_outcome(
    status: u16,
    v: &serde_json::Value,
    had_payment: bool,
    json: bool,
) -> Result<(), String> {
    if status == 200 {
        if json {
            println!("{}", serde_json::to_string_pretty(v).unwrap());
            return Ok(());
        }
        let job_id = v["job_id"].as_str().unwrap_or("?");
        let backend = v["backend"].as_str().unwrap_or("?");
        println!("status:   accepted");
        println!("job_id:   {job_id}");
        println!("backend:  {backend}");
        if let Some(metering) = v.get("metering") {
            let cpu = metering["cpu_seconds"].as_f64().unwrap_or(0.0);
            let exit = metering["exit_status"].as_str().unwrap_or("?");
            println!("cpu_seconds: {cpu:.2}");
            println!("exit_status: {exit}");
        }
        return Ok(());
    }
    if status == 402 {
        print_x402_challenge(v, json);
        if had_payment {
            return Err("payment proof not accepted (still 402) - see terms above".into());
        }
        return Ok(());
    }
    let detail = match v["error"].as_str() {
        Some(s) => s.trim_end_matches('\n').to_string(),
        None => v["raw"].as_str().unwrap_or("").to_string(),
    };
    let detail = if detail.is_empty() {
        v.to_string()
    } else {
        detail
    };
    Err(format!("job rejected (HTTP {status}): {detail}"))
}

/// Print an x402 challenge — the HTTP 402 `/jobs` response. JSON mode echoes
/// the raw body for machine parsing; human mode renders the terms and the
/// pay-then-resubmit flow.
fn print_x402_challenge(v: &serde_json::Value, json: bool) {
    if json {
        println!("{}", serde_json::to_string_pretty(v).unwrap());
        return;
    }
    println!("status:   402 payment_required");
    if let Some(s) = v["scheme"].as_str() {
        println!("scheme:   {s}");
    }
    if let Some(n) = v["network"].as_str() {
        println!("network:  {n}");
    }
    if let Some(e) = v["escrow_account"].as_str() {
        println!("escrow:   {e}");
    }
    if let Some(price) = v["offer"]["body"]["price"].as_object() {
        let mode = price.get("mode").and_then(|m| m.as_str()).unwrap_or("?");
        let cur = price
            .get("currency")
            .and_then(|c| c.as_str())
            .unwrap_or("?");
        let per_sec = price
            .get("per_device_second_micros")
            .and_then(|p| p.as_u64())
            .unwrap_or(0);
        println!("price:    {mode} {cur} {per_sec} micros/s/device");
        if let Some(payout) = price.get("payout_id").and_then(|p| p.as_str()) {
            println!("payout_id: {payout}");
        }
    }
    println!("pay:      spl-token transfer <TOKEN_MINT> <amount> <escrow> \\");
    println!("          --with-memo <job_id>  (AGENTS.md x402 flow), then resubmit:");
    println!("vtessera-agent ... submit \\");
    println!("    --payment '{{\"tx\":\"<signature>\",\"amount_micros\":<amount>}}'");
}

fn health(node: &str, queue: Option<&str>, json: bool) -> Result<(), String> {
    if let Some(q) = queue {
        return queue_render("health", q, json);
    }
    let url = format!("{node}/healthz");
    let body = agent()
        .get(&url)
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_to_string()
        .map_err(|e| format!("failed to read response: {e}"))?;

    if json {
        println!("{}", serde_json::json!({"status": body.trim()}));
    } else {
        println!("{}", body.trim());
    }
    Ok(())
}

/// Render offer/health through the queue rendezvous. Reachability is proven
/// by actually dialing the coordinator (§4b-7) — a QUIC handshake either
/// completes against a live coordinator or fails, and the reported
/// "reachability" always reflects the outcome. Job/offer detail arrives
/// when the node pulls (P1.7e).
fn queue_render(cmd: &str, queue: &str, json: bool) -> Result<(), String> {
    let addr = load_coordinator_addr(queue)?;
    let coordinator = hex::encode(addr.id.as_bytes());
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| format!("tokio runtime: {e}"))?;
    let dial = rt.block_on(async move {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .bind()
            .await
            .map_err(|e| format!("bind iroh endpoint: {e}"))?;
        let client = vtessera_coordinator::iroh::QueueClient::new(endpoint, addr);
        client.probe(std::time::Duration::from_secs(10)).await
    });
    match dial {
        Ok(()) => {
            if json {
                println!(
                    "{}",
                    serde_json::json!({
                        "mode": "queue-rendezvous",
                        "operation": cmd,
                        "coordinator_id": coordinator,
                        "reachability": "coordinator dialed ok",
                        "note": "node is outbound-only; job/offer detail arrives after the node pulls"
                    })
                );
            } else {
                println!("mode:          queue-rendezvous");
                println!("operation:     {cmd}");
                println!("coordinator:   {coordinator}");
                println!("reachability:  coordinator dialed ok (QUIC handshake)");
                println!("note:          node is outbound-only; pull path delivers jobs/results");
            }
            Ok(())
        }
        Err(e) => {
            if json {
                println!(
                    "{}",
                    serde_json::json!({
                        "mode": "queue-rendezvous",
                        "coordinator_id": coordinator,
                        "reachability": "unreachable",
                        "detail": e
                    })
                );
            } else {
                println!("mode:          queue-rendezvous");
                println!("coordinator:   {coordinator}");
                println!("reachability:  unreachable");
                println!("detail:        {e}");
            }
            Err(format!("coordinator unreachable: {e}"))
        }
    }
}

// ---------------------------------------------------------------- Resolver
// Design P2 / T2.1: agents may dial a node by its iroh EndpointId using the
// candidate list served by the offer-index, with no HTTP endpoint required.
// The node's `VtesseraHandler` (vtessera_node.rs) already speaks HTTP over a
// QUIC bi-stream on `vtessera/0`, so the agent reuses the plain wire format.

/// Find the offer-index entry for `node_id` and hand back the candidates the
/// node heartbeats there. Returns (endpoint_id, candidates).
fn resolve_node_candidates(
    index: &str,
    node_id: &str,
) -> Result<(String, Vec<vtessera_transport::Candidate>), String> {
    let resp: serde_json::Value = agent()
        .get(&format!("{index}/offers"))
        .call()
        .map_err(|e| format!("offer-index unreachable ({index}): {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("offer-index response unreadable: {e}"))?;

    let entries = resp["offers"]
        .as_array()
        .ok_or("offer-index returned no list")?;
    let node_id = node_id.to_owned();
    index_entry_for(entries, &node_id)
        .map(|entry| {
            let candidates: Vec<vtessera_transport::Candidate> =
                serde_json::from_value(entry["candidates"].clone()).unwrap_or_default();
            let id = entry["endpoint_id"]
                .as_str()
                .unwrap_or(&node_id)
                .to_string();
            (id, candidates)
        })
        .ok_or_else(|| {
            format!("no offer-index entry for endpoint_id {node_id} (is the node registered at {index}?)")
        })
}

/// Locate the index entry identifying `node_id` — by the entry-level
/// `endpoint_id` (heartbeated) or the signed offer body's `endpoint_id`.
fn index_entry_for<'a>(
    entries: &'a [serde_json::Value],
    node_id: &str,
) -> Option<&'a serde_json::Value> {
    entries.iter().find(|entry| {
        entry["endpoint_id"].as_str() == Some(node_id)
            || entry["offer"]["body"]["endpoint_id"].as_str() == Some(node_id)
    })
}

/// Resolve `node_id` to a dialable `iroh::EndpointAddr` via the offer-index.
fn resolve_addr(index: &str, node_id: &str) -> Result<iroh::EndpointAddr, String> {
    let (id, candidates) = resolve_node_candidates(index, node_id)?;
    if candidates.is_empty() {
        return Err(format!(
            "{node_id} has no dial candidates yet (node registered but no heartbeat arrived; try again shortly)"
        ));
    }
    vtessera_transport::iroh_sidecar::endpoint_addr_from_candidates(&id, &candidates)
}

/// Parse the node's HTTP-over-QUIC response into (status, body bytes).
fn parse_quic_http_response(buf: &[u8]) -> Result<(u16, Vec<u8>), String> {
    let sep = buf
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .ok_or("response had no header terminator")?;
    let header =
        std::str::from_utf8(&buf[..sep]).map_err(|e| format!("bad response header: {e}"))?;
    let mut lines = header.lines();
    let status_line = lines.next().ok_or("empty status line")?;
    let mut parts = status_line.splitn(3, ' ');
    let _proto = parts.next();
    let status: u16 = parts
        .next()
        .and_then(|s| s.parse().ok())
        .ok_or("unparseable status line")?;
    let mut content_length: Option<usize> = None;
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            if k.trim().eq_ignore_ascii_case("content-length") {
                content_length = v.trim().parse().ok();
            }
        }
    }
    let body = &buf[sep + 4..];
    let body = match content_length {
        Some(n) => body.get(..n.min(body.len())).unwrap_or(body).to_vec(),
        None => body.to_vec(),
    };
    Ok((status, body))
}

/// One HTTP request to a node dialed by EndpointId over iroh QUIC. Returns
/// (status, response body).
fn quic_round_trip(
    index: &str,
    node_id: &str,
    method: &str,
    path: &str,
    headers: &[(String, String)],
    body: &[u8],
) -> Result<(u16, Vec<u8>), String> {
    let addr = resolve_addr(index, node_id)?;
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| format!("tokio runtime: {e}"))?;
    rt.block_on(async move {
        let endpoint = iroh::Endpoint::builder(iroh::endpoint::presets::N0)
            .bind()
            .await
            .map_err(|e| format!("bind iroh endpoint: {e}"))?;
        let conn = tokio::time::timeout(
            std::time::Duration::from_secs(15),
            endpoint.connect(addr, vtessera_transport::iroh_sidecar::VTESSERA_ALPN),
        )
        .await
        .map_err(|_| "dial {node_id}: timed out (no relay/DIRECT path)".to_string())?
        .map_err(|e| format!("dial {node_id}: {e}"))?;
        let (mut send, mut recv) = conn.open_bi().await.map_err(|e| format!("open_bi: {e}"))?;

        let mut head = format!("{method} {path} HTTP/1.1\r\n");
        for (k, v) in headers {
            head.push_str(&format!("{k}: {v}\r\n"));
        }
        head.push_str(&format!("content-length: {}\r\n\r\n", body.len()));
        send.write_all(head.as_bytes())
            .await
            .map_err(|e| format!("send request head: {e}"))?;
        send.write_all(body)
            .await
            .map_err(|e| format!("send request body: {e}"))?;
        send.finish().map_err(|e| format!("finish request: {e}"))?;
        let response = tokio::time::timeout(
            std::time::Duration::from_secs(15),
            recv.read_to_end(256 * 1024),
        )
        .await
        .map_err(|_| "read response: timed out".to_string())?
        .map_err(|e| format!("read response: {e}"))?;
        conn.close(0u32.into(), b"done");
        parse_quic_http_response(&response)
    })
}

/// `offer` over the resolver (dial by EndpointId).
fn quic_offer(
    index: &str,
    _marketplace: Option<&str>,
    node_id: &str,
    json: bool,
) -> Result<(), String> {
    let (status, body) = quic_round_trip(index, node_id, "GET", "/offer", &[], b"")?;
    if status != 200 {
        return Err(format!(
            "offer failed (HTTP {status}): {}",
            String::from_utf8_lossy(&body)
        ));
    }
    let v: serde_json::Value =
        serde_json::from_slice(&body).map_err(|e| format!("offer response unparseable: {e}"))?;
    if json {
        println!("{}", serde_json::to_string_pretty(&v).unwrap());
        return Ok(());
    }
    let b = &v["body"];
    let nid = b["node_id"].as_str().unwrap_or("?");
    let device = b["device"]["kind"].as_str().unwrap_or("?");
    let endpoint_id = b["endpoint_id"].as_str().unwrap_or("");
    let price = if b["price"]["mode"].as_str() == Some("free") {
        "free".to_string()
    } else {
        serde_json::to_string(&b["price"]).unwrap_or_else(|_| "?".into())
    };
    println!("node_id:      {nid}");
    println!("endpoint_id:  {endpoint_id}");
    println!("device:       {device}");
    println!("price:        {price}");
    Ok(())
}

/// `submit` over the resolver (dial by EndpointId).
fn quic_submit(
    index: &str,
    _marketplace: Option<&str>,
    node_id: &str,
    agent_id: &str,
    job_path: &str,
    json: bool,
    payment: Option<&str>,
) -> Result<(), String> {
    let job_json = std::fs::read_to_string(job_path)
        .map_err(|e| format!("read job file {job_path:?}: {e}"))?;
    let mut headers = vec![("x-agent-id".to_string(), agent_id.to_string())];
    if let Some(p) = payment {
        headers.push(("x-payment".to_string(), p.to_string()));
    }
    let (status, body) = quic_round_trip(
        index,
        node_id,
        "POST",
        "/jobs",
        &headers,
        job_json.as_bytes(),
    )?;
    let v: serde_json::Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(_) => {
            serde_json::json!({ "raw": String::from_utf8_lossy(&body).trim_end_matches('\n') })
        }
    };
    render_submit_outcome(status, &v, payment.is_some(), json)?;
    Ok(())
}

/// `health` over the resolver (dial by EndpointId).
fn quic_health(
    index: &str,
    _marketplace: Option<&str>,
    node_id: &str,
    json: bool,
) -> Result<(), String> {
    let (status, body) = quic_round_trip(index, node_id, "GET", "/healthz", &[], b"")?;
    if json {
        println!(
            "{}",
            serde_json::json!({"status": status, "body": String::from_utf8_lossy(&body).trim()})
        );
        return Ok(());
    }
    if status == 200 {
        println!("{}", String::from_utf8_lossy(&body).trim());
        Ok(())
    } else {
        Err(format!(
            "health failed (HTTP {status}): {}",
            String::from_utf8_lossy(&body).trim()
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn index_entry_matches_entry_level_and_offer_body_endpoint_id() {
        let entries = serde_json::from_str::<Vec<serde_json::Value>>(
            r#"[
              {
                "offer": {"body": {"endpoint_id": "aaa1"}},
                "endpoint_id": null,
                "candidates": []
              },
              {
                "offer": {"body": {"endpoint_id": "bbb2"}},
                "endpoint_id": "bbb2",
                "candidates": [{"kind": "host", "transport": "iroh_quic", "addr": "192.0.2.9:8402", "priority": 200}]
              },
              {
                "offer": {"body": {"endpoint_id": "ccc3"}},
                "endpoint_id": "ccc3",
                "candidates": []
              }
            ]"#,
        )
        .unwrap();

        // Entry-level (heartbeated) id matches first.
        let found = index_entry_for(&entries, "bbb2").expect("found bbb2");
        assert_eq!(found["endpoint_id"].as_str(), Some("bbb2"));
        // An id that only exists in the signed offer body is found too.
        assert!(index_entry_for(&entries, "aaa1").is_some());
        // Unknown id -> none.
        assert!(index_entry_for(&entries, "zzz9").is_none());
    }

    /// Validate the `--payment` x402 proof shape (JSON with `tx` + numeric
    /// `amount_micros`), as required by the `x-payment` header.
    fn x402_proof_valid(raw: &str) -> bool {
        serde_json::from_str::<serde_json::Value>(raw)
            .map(|proof| {
                proof["tx"].as_str().is_some() && proof["amount_micros"].as_u64().is_some()
            })
            .unwrap_or(false)
    }

    #[test]
    fn x402_challenge_and_outcomes_are_classified() {
        let ch = serde_json::json!({
            "scheme": "x402",
            "network": "solana-devnet",
            "escrow_account": "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma",
            "offer": {"body": {
                "price": {"mode": "paid", "currency": "eurc",
                          "per_device_second_micros": 2792,
                          "payout_id": "5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs"}
            }}
        });

        // 402 without a proof -> Ok, the challenge is rendered (step 1 of the
        // x402 flow; scripts detect "payment_required" in the output).
        assert!(render_submit_outcome(402, &ch, false, true).is_ok());
        // 402 with a proof -> the proof was rejected: hard error.
        assert!(render_submit_outcome(402, &ch, true, true).is_err());
        // 200 accepted -> Ok.
        let acc =
            serde_json::json!({"status": "accepted", "job_id": "job-1", "backend": "noop-cpu"});
        assert!(render_submit_outcome(200, &acc, false, true).is_ok());
        assert!(render_submit_outcome(200, &acc, true, true).is_ok());
        // Other codes -> Err carrying the node's message.
        let rejected = serde_json::json!({"error": "no capacity"});
        let r = render_submit_outcome(503, &rejected, false, true);
        assert!(r.is_err());
        assert!(r.unwrap_err().contains("no capacity"));
        // Non-JSON body fallback keeps the raw text.
        let r = render_submit_outcome(502, &serde_json::json!({"raw": "bad gateway"}), false, true);
        assert!(r.unwrap_err().contains("bad gateway"));
    }

    #[test]
    fn x402_challenge_rejects_payment_without_tx_or_amount() {
        // A proof needs both "tx" and a numeric "amount_micros".
        assert!(x402_proof_valid(r#"{"tx":"sig","amount_micros":10000}"#));
        assert!(!x402_proof_valid(r#"{"tx":"sig"}"#));
        assert!(!x402_proof_valid(r#"{"amount_micros":10000}"#));
        assert!(!x402_proof_valid("not json"));
    }

    #[test]
    fn quic_http_response_parses_status_and_bounded_body() {
        let resp =
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 5\r\n\r\nhello";
        let (status, body) = parse_quic_http_response(resp).unwrap();
        assert_eq!(status, 200);
        assert_eq!(body, b"hello");

        let resp = b"HTTP/1.1 402 Payment Required\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}";
        let (status, body) = parse_quic_http_response(resp).unwrap();
        assert_eq!(status, 402);
        assert_eq!(body, b"{}");

        assert!(parse_quic_http_response(b"garbage").is_err());
        assert!(parse_quic_http_response(b"HTTP/1.1 XXX\r\n\r\n").is_err());
    }
}
