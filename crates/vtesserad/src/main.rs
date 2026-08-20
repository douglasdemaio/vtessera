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
    let mut window_start: Option<u64> = None;
    let mut cpu_state = metrics::CpuState::default();

    loop {
        match metrics::sample(&state_dir_str, &mut cpu_state) {
            Ok(s) => {
                if window_start.is_none() {
                    window_start = Some(s.ts_unix);
                }
                samples.push(s);
            }
            Err(e) => {
                eprintln!("error: failed to sample metrics: {e}");
            }
        }

        if let Some(start) = window_start {
            // close every completed window (see advance_windows: windows are
            // epoch-aligned and contiguous, so the closing-sample gap in the
            // old code is gone).
            window_start = Some(advance_windows(
                &mut samples,
                start,
                window_size,
                |win, s, e| {
                    if let Err(err) = finalize_window(&cfg, &signing_key, &node_id, win, s, e) {
                        eprintln!("error: finalize window: {err}");
                    }
                },
            ));
        }

        if once {
            eprintln!("--once mode: exiting after single iteration");
            break;
        }

        thread::sleep(interval);
    }
}

/// Close out every full window boundary covered by the samples so far.
///
/// Boundaries are fixed at `start`, `start + window_size`, `start + 2*window_size`,
/// … and keyed to wall clock rather than to sample arrival. Consecutive windows
/// are therefore contiguous and gapless: even when `sample_interval_secs ==
/// window_size` (or a sampling gap occurs), no wall-clock interval falls
/// outside a receipt. `finalize` receives each completed window's samples plus
/// its `[window_start, window_end)` range. Returns the new `window_start`.
fn advance_windows(
    samples: &mut Vec<metrics::ResourceSample>,
    window_start: u64,
    window_size: u64,
    mut finalize: impl FnMut(&[metrics::ResourceSample], u64, u64),
) -> u64 {
    let mut start = window_start;
    while let Some(last) = samples.last() {
        let boundary = start.saturating_add(window_size);
        if last.ts_unix < boundary {
            break;
        }
        let idx = samples.partition_point(|s| s.ts_unix < boundary);
        let win: Vec<metrics::ResourceSample> = samples.drain(..idx).collect();
        // A window with no samples (sampling gap) is skipped: there is no
        // data to report, and finalizing it would yield a 0/0 receipt.
        if !win.is_empty() {
            finalize(&win, start, boundary);
        }
        start = boundary;
    }
    start
}

fn finalize_window(
    cfg: &config::Config,
    signing_key: &ed25519_dalek::SigningKey,
    node_id: &str,
    samples: &[metrics::ResourceSample],
    window_start: u64,
    window_end: u64,
) -> Result<(), Box<dyn std::error::Error>> {
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

    // Spool rotation (ROADMAP.md §5). v0 grew receipts forever; production
    // deployments cap retention via `max_spool_files`. Rotation failures
    // are logged but never abort the run — the daemon's primary job is to
    // keep producing receipts.
    if let Some(keep) = cfg.max_spool_files {
        match spool::rotate(&cfg.state_dir, keep) {
            Ok(removed) if removed > 0 => eprintln!("spool: pruned {removed} old receipt(s)"),
            Ok(_) => {}
            Err(e) => eprintln!("warning: spool rotation failed: {e}"),
        }
    }

    #[cfg(feature = "submit")]
    if let Some(ref endpoint) = cfg.submit_endpoint {
        match submit::submit_receipt(endpoint, &signed) {
            Ok(_) => eprintln!("receipt submitted to {endpoint}"),
            Err(e) => eprintln!("warning: failed to submit receipt: {e}"),
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_at(ts_unix: u64) -> metrics::ResourceSample {
        metrics::ResourceSample {
            ts_unix,
            cpu_pct: 0.0,
            mem_used_kb: 0,
            disk_free_kb: 0,
        }
    }

    /// Regression test for the window-gap bug: when `sample_interval_secs ==
    /// window_size` (60/60), every wall-clock second must fall inside a
    /// receipt. The old code reset `window_start` to zero on close, so
    /// windows alternated [covered][gap][covered] and half the time was
    /// unbilled.
    #[test]
    fn windows_are_contiguous_when_interval_equals_window() {
        let mut samples = Vec::new();
        let mut start = 0u64;
        let mut wins: Vec<(u64, u64, usize)> = Vec::new();

        let mut tick = |ts: u64| {
            samples.push(sample_at(ts));
            start = advance_windows(&mut samples, start, 60, |win, s, e| {
                wins.push((s, e, win.len()));
            });
        };

        for ts in [0u64, 60, 120, 180, 240] {
            tick(ts);
        }

        // Gapless consecutive windows, one sample each; the t=240 sample is
        // still pending in the not-yet-closed [240,300) window.
        assert_eq!(
            wins,
            vec![(0, 60, 1), (60, 120, 1), (120, 180, 1), (180, 240, 1)]
        );
        assert_eq!(samples.len(), 1);
        assert_eq!(samples[0].ts_unix, 240);
    }

    #[test]
    fn windows_cover_a_sampling_gap() {
        // No samples arrive between t=0 and t=130: the t=0 sample's window
        // still closes on its boundary, the [60,120) window is empty (no
        // data exists — skipped), and the late sample lands in [120,180).
        let mut samples = Vec::new();
        let mut wins: Vec<(u64, u64, usize)> = Vec::new();
        samples.push(sample_at(0));
        let mut start = advance_windows(&mut samples, 0, 60, |win, s, e| {
            wins.push((s, e, win.len()));
        });
        samples.push(sample_at(130));
        start = advance_windows(&mut samples, start, 60, |win, s, e| {
            wins.push((s, e, win.len()));
        });

        assert_eq!(wins, vec![(0, 60, 1)]);
        assert_eq!(samples.len(), 1); // the t=130 sample, still in [120,180)
        assert_eq!(samples[0].ts_unix, 130);
        assert_eq!(start, 120);
    }

    #[test]
    fn multiple_windows_close_on_one_tick() {
        // A long stall then a burst: 4 windows close in a single advance.
        let mut samples = Vec::new();
        let mut wins: Vec<(u64, u64, usize)> = Vec::new();
        for ts in [0u64, 60, 120, 180] {
            samples.push(sample_at(ts));
        }
        let start = advance_windows(&mut samples, 0, 60, |win, s, e| {
            wins.push((s, e, win.len()));
        });
        assert_eq!(wins, vec![(0, 60, 1), (60, 120, 1), (120, 180, 1)]);
        assert_eq!(start, 180);
        // The t=180 sample is pending in the open [180,240) window.
        assert_eq!(samples.len(), 1);
        assert_eq!(samples[0].ts_unix, 180);
    }
}
