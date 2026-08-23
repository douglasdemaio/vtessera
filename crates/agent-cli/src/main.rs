use clap::{Parser, Subcommand};
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

    /// Agent identity for claim gate
    #[arg(long, global = true)]
    agent_id: Option<String>,

    /// Output raw JSON
    #[arg(long, global = true)]
    json: bool,
}

#[derive(Subcommand)]
enum Commands {
    /// Query offer-index for available free nodes
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

    let result = match &cli.command {
        Commands::Discover => discover(&cli.index, cli.json),
        Commands::Offer => offer(&cli.node, cli.json),
        Commands::Submit { job } => submit(&cli.node, &agent_id, job, cli.json),
        Commands::Health => health(&cli.node, cli.json),
    };

    if let Err(e) = result {
        eprintln!("error: {e}");
        process::exit(1);
    }
}

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_defaults()
}

fn discover(index: &str, json: bool) -> Result<(), String> {
    let url = format!("{index}/offers?available=1&mode=free");
    let resp: serde_json::Value = agent()
        .get(&url)
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("failed to read response: {e}"))?;

    if json {
        println!("{}", serde_json::to_string_pretty(&resp).unwrap());
        return Ok(());
    }

    let offers = resp.as_array().ok_or("unexpected response: not an array")?;

    if offers.is_empty() {
        println!("no free nodes available");
        return Ok(());
    }

    println!("{:<20} {:<15} {:<40}", "NODE_ID", "DEVICE", "ENDPOINT");
    println!("{}", "-".repeat(75));
    for o in offers {
        let node_id = o["node_id"].as_str().unwrap_or("?");
        let device = o["device"]["kind"].as_str().unwrap_or("?");
        let endpoint = o["endpoint"].as_str().unwrap_or("?");
        println!("{node_id:<20} {device:<15} {endpoint:<40}");
    }
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
        let node_id = resp["pubkey_hex"].as_str().unwrap_or("?");
        let endpoint = body["endpoint"].as_str().unwrap_or("?");
        let device = body["device"]["kind"].as_str().unwrap_or("?");
        let price = if body["price"]["free"].as_bool().unwrap_or(false) {
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
