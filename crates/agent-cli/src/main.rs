use clap::{Parser, Subcommand};
use std::path::PathBuf;
use std::process;

#[derive(Parser)]
#[command(
    name = "vtessera-agent",
    about = "CLI for AI agents to interact with vtessera nodes"
)]
struct Cli {
    #[command(subcommand)]
    command: Commands,

    /// Node HTTP endpoint
    #[arg(long, default_value = "http://127.0.0.1:8402", global = true)]
    node: String,

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

    let result = match &cli.command {
        Commands::Discover => discover(&index, cli.marketplace.as_deref(), json),
        Commands::Offer => offer(&node, json),
        Commands::Submit { job } => submit(&node, &agent_id, job, json),
        Commands::Health => health(&node, json),
    };

    if let Err(e) = result {
        eprintln!("error: {e}");
        process::exit(1);
    }
}

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_defaults()
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
            if let Some(ep) = o["offer"]["body"]["endpoint"].as_str() {
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
                    if let Some(ep) = offer["body"]["endpoint"].as_str() {
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
        let endpoint = body["endpoint"].as_str().unwrap_or("?");
        println!("{node_id:<20} {device:<15} {endpoint:<40}");
    }
    println!("\n{} node(s) found", offers.len());
    Ok(())
}

fn offer(node: &str, json: bool) -> Result<(), String> {
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
        let endpoint = body["endpoint"].as_str().unwrap_or("?");
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

fn submit(node: &str, agent_id: &str, job_path: &str, json: bool) -> Result<(), String> {
    let job_json =
        std::fs::read_to_string(job_path).map_err(|e| format!("failed to read {job_path}: {e}"))?;

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

fn health(node: &str, json: bool) -> Result<(), String> {
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
