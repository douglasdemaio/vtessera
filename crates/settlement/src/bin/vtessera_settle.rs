//! `vtessera-settle` — the Module 3 settlement service (watch loop).
//!
//! Watches a state dir shared with `vtessera-node` and turns signed job
//! receipts into completion-fraction settlement records:
//!
//!   contracts/<job_id>.json      ← what the buyer agreed (JobContract)
//!   job-receipts/<job_id>.json   ← signed by the node that ran the job
//!   settlements/<job_id>.json    ← written here, once, atomically
//!
//! Args:
//!
//!   --state-dir <dir>   required; contracts/, job-receipts/, settlements/
//!   --interval <secs>   sweep period (default 60; ignored with --once)
//!   --once              single sweep, then exit (used by CI and tests)
//!
//! Exit codes with `--once`: 0 = swept cleanly (pending jobs are fine),
//! 1 = at least one job was permanently rejected (needs operator action).

use std::path::PathBuf;
use std::process;
use std::thread::sleep;
use std::time::Duration;

use vtessera_settlement::sweep;

fn main() {
    let args = parse_args();

    loop {
        let report = sweep(&args.state_dir).unwrap_or_else(|e| {
            eprintln!("vtessera-settle: sweep failed: {e}");
            process::exit(1);
        });
        for job in &report.settled {
            println!("settled {job}");
        }
        for (job, why) in &report.pending {
            println!("pending {job}: {why}");
        }
        for (job, why) in &report.rejected {
            eprintln!("REJECTED {job}: {why}");
        }
        if report.settled.is_empty() && report.pending.is_empty() && report.rejected.is_empty() {
            println!("no contracts to settle");
        }

        if args.once {
            // Pending is fine (retry later); a rejection is a real failure.
            if report.rejected.is_empty() {
                process::exit(0);
            }
            eprintln!(
                "vtessera-settle: {} job(s) rejected; operator intervention required",
                report.rejected.len()
            );
            process::exit(1);
        }

        sleep(Duration::from_secs(args.interval));
    }
}

struct Args {
    state_dir: PathBuf,
    interval: u64,
    once: bool,
}

fn parse_args() -> Args {
    let mut state_dir: Option<PathBuf> = None;
    let mut interval: u64 = 60;
    let mut once = false;
    let mut it = std::env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--state-dir" => {
                state_dir = it.next().map(PathBuf::from);
            }
            "--interval" => {
                let raw = it.next().unwrap_or_else(|| usage_and_exit());
                interval = raw.parse().unwrap_or_else(|_| usage_and_exit());
            }
            "--once" => once = true,
            "--help" | "-h" => usage_and_exit(),
            other => {
                eprintln!("unknown argument: {other}");
                usage_and_exit();
            }
        }
    }
    let state_dir = state_dir.unwrap_or_else(|| usage_and_exit());
    if !state_dir.as_path().exists() {
        eprintln!(
            "state-dir {} does not exist (start vtessera-node with the same --state-dir first)",
            state_dir.display()
        );
        process::exit(1);
    }
    Args {
        state_dir,
        interval,
        once,
    }
}

fn usage_and_exit() -> ! {
    eprintln!("usage: vtessera-settle --state-dir <dir> [--interval <secs>] [--once]");
    process::exit(2);
}
