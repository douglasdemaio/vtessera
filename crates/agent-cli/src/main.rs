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
        Commands::Offer => offer(&node, queue.as_deref(), json),
        Commands::Submit { job } => submit(&node, queue.as_deref(), &agent_id, job, json),
        Commands::Health => health(&node, queue.as_deref(), json),
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
    let mut offers: Vec<serde_json::Value> = Vec::new();
    let mut seen_endpoints: std::collections::HashSet<String> = std::collections::HashSet::new();

    // Process local index results.
    if let Some(resp) = &local_result {
        let arr = if resp.is_array() {
            resp.as_array().cloned().unwrap_or_default()
        } else {
            resp["offers"].as_array().cloned().unwrap_or_default()
        };
        for o in arr {
            if let Some(ep) = offer_endpoint(&o["offer"]["body"]) {
                if seen_endpoints.insert(ep.to_string()) {
                    offers.push(o);
                }
            }
        }
    }

    // Process marketplace results.
    if let Some(resp) = &market_result {
        if let Some(nodes) = resp["nodes"].as_array() {
            for node in nodes {
                if let Some(offer) = node.get("offer") {
                    if let Some(ep) = offer_endpoint(&offer["body"]) {
                        if seen_endpoints.insert(ep.to_string()) {
                            offers.push(offer.clone());
                        }
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

    println!("{:<20} {:<15} {:<40}", "NODE_ID", "DEVICE", "ENDPOINT");
    println!("{}", "-".repeat(75));
    for o in &offers {
        let body = if o.get("offer").is_some() {
            &o["offer"]["body"]
        } else {
            &o["body"]
        };
        let node_id = body["node_id"].as_str().unwrap_or("?");
        let device = body["device"]["kind"].as_str().unwrap_or("?");
        let endpoint = offer_endpoint(body).unwrap_or_else(|| "?".into());
        println!("{node_id:<20} {device:<15} {endpoint:<40}");
    }
    println!("\n{} node(s) found", offers.len());
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
) -> Result<(), String> {
    let job_json =
        std::fs::read_to_string(job_path).map_err(|e| format!("failed to read {job_path}: {e}"))?;
    let job: serde_json::Value = serde_json::from_str(&job_json)
        .map_err(|e| format!("invalid job JSON in {job_path}: {e}"))?;

    // Queue rendezvous: enqueue over iroh, the node pulls + runs + acks.
    if let Some(q) = queue {
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
    let resp: serde_json::Value = agent()
        .post(&url)
        .header("x-agent-id", agent_id)
        .send(&job_json)
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("failed to read response: {e}"))?;

    if json {
        println!("{}", serde_json::to_string_pretty(&resp).unwrap());
    } else {
        let status = resp["status"].as_str().unwrap_or("?");
        let job_id = resp["job_id"].as_str().unwrap_or("?");
        let backend = resp["backend"].as_str().unwrap_or("?");
        println!("status:  {status}");
        println!("job_id:  {job_id}");
        println!("backend: {backend}");
        if let Some(metering) = resp.get("metering") {
            let cpu = metering["cpu_seconds"].as_f64().unwrap_or(0.0);
            let exit = metering["exit_status"].as_str().unwrap_or("?");
            println!("cpu_seconds: {cpu:.2}");
            println!("exit_status: {exit}");
        }
    }
    Ok(())
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
/// by successfully dialing the coordinator; job/offer detail arrives when
/// the node pulls (P1.7e).
fn queue_render(cmd: &str, queue: &str, json: bool) -> Result<(), String> {
    let _client = queue_client(queue)?;
    let addr = load_coordinator_addr(queue)?;
    let coordinator = hex::encode(addr.id.as_bytes());
    if json {
        println!(
            "{}",
            serde_json::json!({
                "mode": "queue-rendezvous",
                "coordinator_id": coordinator,
                "reachability": "coordinator dialed ok",
                "note": "node is outbound-only; job/offer detail arrives after the node pulls"
            })
        );
    } else {
        println!("mode:          queue-rendezvous");
        println!("operation:     {cmd}");
        println!("coordinator:   {coordinator}");
        println!("reachability:  coordinator dialed ok");
        println!("note:          node is outbound-only; pull path delivers jobs/results");
    }
    Ok(())
}
