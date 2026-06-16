#![forbid(unsafe_code)]

mod config;
mod metrics;
mod receipt;
mod sign;
mod spool;

#[cfg(feature = "submit")]
mod submit;

use std::path::PathBuf;
use std::process;
use std::thread;
use std::time::Duration;

const VERSION: &str = env!("CARGO_PKG_VERSION");

/// Exit codes (documented in BUILD.md §4):
///   0 = success / --help / --version
///   1 = runtime error (config invalid, key error, IO)
///   2 = argument parsing error
const EXIT_OK: i32 = 0;
const EXIT_RUNTIME: i32 = 1;
const EXIT_USAGE: i32 = 2;

fn print_help(program: &str) {
    println!("vtesserad {VERSION} — Vtessera metering daemon");
    println!();
    println!("Usage: {program} --config <path> [--once]");
    println!("       {program} --version");
    println!("       {program} --help");
    println!();
    println!("Options:");
    println!("  --config <path>   Path to the TOML config file (required).");
    println!("  --once            Sample once and exit (does not finalize a window).");
    println!("  --version         Print version and exit.");
    println!("  -h, --help        Print this help and exit.");
}

fn usage_err(program: &str, msg: &str) -> ! {
    eprintln!("error: {msg}");
    eprintln!("Usage: {program} --config <path> [--once] [--version] [--help]");
    process::exit(EXIT_USAGE);
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let program = &args[0];

    let mut config_path: Option<PathBuf> = None;
    let mut once = false;

    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "--config" => {
                i += 1;
                let val = args.get(i).cloned().unwrap_or_else(|| {
                    usage_err(program, "--config requires a path argument");
                });
                config_path = Some(PathBuf::from(val));
            }
            "--once" => {
                once = true;
            }
            "--version" => {
                println!("vtesserad {VERSION}");
                process::exit(EXIT_OK);
            }
            "--help" | "-h" => {
                print_help(program);
                process::exit(EXIT_OK);
            }
            other => {
                usage_err(program, &format!("unknown argument '{other}'"));
            }
        }
        i += 1;
    }

    let config_path = config_path.unwrap_or_else(|| {
        usage_err(program, "--config is required");
    });

    let cfg = match config::Config::load(&config_path) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: failed to load config: {e}");
            process::exit(EXIT_RUNTIME);
        }
    };

    if let Err(e) = cfg.validate() {
        eprintln!("error: invalid config: {e}");
        process::exit(EXIT_RUNTIME);
    }

    let signing_key = match sign::load_or_generate(&cfg.key_path) {
        Ok(k) => k,
        Err(e) => {
            eprintln!("error: failed to load/generate key: {e}");
            process::exit(EXIT_RUNTIME);
        }
    };

    // node_id is derived from the signing key's public key — self-attesting
    // and stable across payout_id rotations. See BUILD.md §4 (receipt.rs).
    let node_id = receipt::derive_node_id(&signing_key.verifying_key().to_bytes());

    let interval = Duration::from_secs(cfg.sample_interval_secs);
    let window_size = cfg.window_size.unwrap_or(60);
    let state_dir = cfg.state_dir.clone();
    let state_dir_str = state_dir.to_string_lossy().to_string();

    eprintln!(
        "vtesserad started: sampling every {}s, window {}s, state_dir={}",
        cfg.sample_interval_secs, window_size, state_dir_str
    );

    let mut samples: Vec<metrics::ResourceSample> = Vec::new();
    let mut window_start: u64 = 0;

    loop {
        match metrics::sample(&state_dir_str) {
            Ok(s) => {
                if window_start == 0 {
                    window_start = s.ts_unix;
                }
                samples.push(s);
            }
            Err(e) => {
                eprintln!("error: failed to sample metrics: {e}");
            }
        }

        if !samples.is_empty() {
            // saturating_sub: tolerate backward NTP steps (see BUILD.md §4 / metrics.rs
            // clock-source note). If wall clock went backwards, elapsed becomes 0 and
            // the window simply doesn't close yet.
            let elapsed = samples.last().unwrap().ts_unix.saturating_sub(window_start);
            if elapsed >= window_size {
                if let Err(e) =
                    finalize_window(&cfg, &signing_key, &node_id, &samples, window_start)
                {
                    eprintln!("error: finalize window: {e}");
                }
                samples.clear();
                window_start = 0;
            }
        }

        if once {
            eprintln!("--once mode: exiting after single iteration");
            break;
        }

        thread::sleep(interval);
    }
}

fn finalize_window(
    cfg: &config::Config,
    signing_key: &ed25519_dalek::SigningKey,
    node_id: &str,
    samples: &[metrics::ResourceSample],
    window_start: u64,
) -> Result<(), Box<dyn std::error::Error>> {
    let window_end = samples.last().unwrap().ts_unix;
    let sample_count = samples.len() as u32;

    let cpu_sum: f64 = samples.iter().map(|s| s.cpu_pct).sum();
    let mem_sum: u64 = samples.iter().map(|s| s.mem_used_kb).sum();
    let disk_sum: u64 = samples.iter().map(|s| s.disk_free_kb).sum();
    let totals = receipt::Totals {
        cpu_pct_avg: cpu_sum / sample_count as f64,
        mem_used_kb_avg: mem_sum / sample_count as u64,
        disk_free_kb_avg: disk_sum / sample_count as u64,
        sample_count,
    };

    let mut sample_buf = Vec::new();
    for s in samples {
        sample_buf.extend_from_slice(&s.ts_unix.to_le_bytes());
        sample_buf.extend_from_slice(&s.cpu_pct.to_le_bytes());
        sample_buf.extend_from_slice(&s.mem_used_kb.to_le_bytes());
        sample_buf.extend_from_slice(&s.disk_free_kb.to_le_bytes());
    }
    let samples_digest = receipt::sample_digest(&sample_buf);

    let rec = receipt::Receipt {
        schema_ver: 1,
        node_id: node_id.to_string(),
        payout_id: cfg.payout_id.clone(),
        window_start,
        window_end,
        samples_digest,
        totals,
    };

    let signed = sign::sign(signing_key, &rec);
    spool::write_signed_receipt(&cfg.state_dir, &signed)?;

    eprintln!(
        "receipt written: window [{window_start}, {window_end}), {} samples, digest={}",
        sample_count,
        hex::encode(samples_digest)
    );

    #[cfg(feature = "submit")]
    if let Some(ref endpoint) = cfg.submit_endpoint {
        match submit::submit_receipt(endpoint, &signed) {
            Ok(_) => eprintln!("receipt submitted to {endpoint}"),
            Err(e) => eprintln!("warning: failed to submit receipt: {e}"),
        }
    }

    Ok(())
}
