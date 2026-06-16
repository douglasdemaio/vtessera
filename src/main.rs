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

fn usage(program: &str) -> ! {
    eprintln!("Usage: {program} --config <path> [--once] [--version]");
    process::exit(1);
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
                config_path = Some(PathBuf::from(args.get(i).cloned().unwrap_or_else(|| {
                    eprintln!("error: --config requires a path argument");
                    process::exit(1);
                })));
            }
            "--once" => {
                once = true;
            }
            "--version" => {
                println!("vtesserad {VERSION}");
                process::exit(0);
            }
            other => {
                eprintln!("error: unknown argument '{other}'");
                usage(program);
            }
        }
        i += 1;
    }

    let config_path = config_path.unwrap_or_else(|| {
        eprintln!("error: --config is required");
        usage(program);
    });

    let cfg = match config::Config::load(&config_path) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: failed to load config: {e}");
            process::exit(1);
        }
    };

    if let Err(e) = cfg.validate() {
        eprintln!("error: invalid config: {e}");
        process::exit(1);
    }

    let signing_key = match sign::load_or_generate(&cfg.key_path) {
        Ok(k) => k,
        Err(e) => {
            eprintln!("error: failed to load/generate key: {e}");
            process::exit(1);
        }
    };

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
            let elapsed = samples.last().unwrap().ts_unix - window_start;
            if elapsed >= window_size {
                if let Err(e) = finalize_window(&cfg, &signing_key, &samples, window_start) {
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
        node_id: cfg.payout_id.clone(),
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
