//! `vtessera-capacity` — capacity controller (issue #109).
//!
//! Three modes pile on the same primitive (validate -> atomic commit to the
//! node's watched `capacity.toml` -> audit event):
//!
//! ```text
//! vtessera-capacity --apply <in.toml> --out <capacity.toml> [--events <events.jsonl>]
//! vtessera-capacity --watch <in.toml> --out <capacity.toml> [--events <events.jsonl>] [--poll-secs <n>]
//! vtessera-capacity --autoscale <autoscale.toml>
//! ```
//!
//! `--apply` is the one-shot (CI/test-friendly) file-drop trigger;
//! `--watch` mirrors every change to `<in.toml>` onto `<out>` until the
//! process stops. `--autoscale` (built with the `autoscale` feature) polls a
//! node's `/metrics` demand gauges and scales `advertised_vcpus` /
//! `max_concurrent_jobs` between the configured floor and ceiling. Either
//! way the node's own watcher picks the committed file up on its next poll
//! and reconfigures its queue / re-signs its offer. The file-drop modes have
//! no sockets — in v1 the orchestrator and the controller talk over the
//! filesystem; the autoscale mode's only socket is an outbound `/metrics`
//! poll.

use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::thread;
use std::time::Duration;

use vtessera_capacity::DEFAULT_WATCH_POLL_SECS;

enum Mode {
    Apply,
    Watch { poll: Duration },
    Autoscale(PathBuf),
}

fn usage() -> ! {
    eprintln!(
        "usage:\n  vtessera-capacity --apply <in.toml> --out <capacity.toml> [--events <events.jsonl>]\n  vtessera-capacity --watch <in.toml> --out <capacity.toml> [--events <events.jsonl>] [--poll-secs <n>]\n  vtessera-capacity --autoscale <autoscale.toml>"
    );
    std::process::exit(2);
}

fn parse(raw: &[String]) -> (Mode, Option<PathBuf>, Option<PathBuf>, Option<PathBuf>) {
    let mut mode = None;
    let mut in_path = None;
    let mut out = None;
    let mut events = None;
    let mut i = 0;
    while i < raw.len() {
        let key = raw[i].as_str();
        let val = raw
            .get(i + 1)
            .and_then(|v| (!v.starts_with("--")).then(|| v.clone()));
        let take_value = |name: &str| -> String {
            match val {
                Some(v) => v,
                None => {
                    eprintln!("{name} requires a value");
                    usage();
                }
            }
        };
        match key {
            "--apply" => {
                in_path = Some(take_value("--apply"));
            }
            "--watch" => {
                in_path = Some(take_value("--watch"));
                mode = Some(Mode::Watch {
                    poll: Duration::from_secs(DEFAULT_WATCH_POLL_SECS),
                });
            }
            "--autoscale" => {
                mode = Some(Mode::Autoscale(PathBuf::from(take_value("--autoscale"))));
            }
            "--poll-secs" => {
                let secs: u64 = take_value("--poll-secs").parse().unwrap_or_else(|_| {
                    eprintln!("--poll-secs must be a number");
                    usage();
                });
                mode = Some(Mode::Watch {
                    poll: Duration::from_secs(secs),
                });
            }
            "--out" => {
                out = Some(take_value("--out"));
            }
            "--events" => {
                events = Some(PathBuf::from(take_value("--events")));
            }
            _ => usage(),
        }
        i += 2;
    }
    let mode = match mode {
        Some(m) => m,
        None => {
            if in_path.is_some() {
                // Neither --watch nor --poll-secs: a bare `--apply <in>` is
                // the one-shot mode.
                Mode::Apply
            } else {
                usage();
            }
        }
    };
    let in_path = in_path.map(PathBuf::from);
    (mode, in_path, out.map(PathBuf::from), events)
}

fn apply_and_report(in_path: &Path, out: &Path, events: Option<&Path>) -> ExitCode {
    match vtessera_capacity::apply_and_commit(in_path, out, events) {
        Ok(summary) if summary.unchanged => {
            println!(
                "no change: {} already matches {}",
                out.display(),
                in_path.display()
            );
            ExitCode::SUCCESS
        }
        Ok(summary) => {
            let changed = summary
                .changed
                .iter()
                .map(|(k, v)| format!("{k} = {v}"))
                .collect::<Vec<_>>()
                .join(", ");
            println!(
                "applied {} -> {} (changed: {changed})",
                in_path.display(),
                out.display()
            );
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("vtessera-capacity: error: {e}");
            ExitCode::FAILURE
        }
    }
}

fn run_autoscale(cfg_path: &Path) -> ExitCode {
    #[cfg(feature = "autoscale")]
    {
        match vtessera_capacity::autoscale::load_config(cfg_path) {
            Ok(cfg) => {
                vtessera_capacity::autoscale::run(cfg);
                ExitCode::SUCCESS
            }
            Err(e) => {
                eprintln!("vtessera-capacity: {e}");
                ExitCode::FAILURE
            }
        }
    }
    #[cfg(not(feature = "autoscale"))]
    {
        let _ = cfg_path;
        eprintln!(
            "vtessera-capacity: built without the autoscale feature; \
             rebuild with `cargo build -p vtessera-capacity --features autoscale`"
        );
        ExitCode::FAILURE
    }
}

fn main() -> ExitCode {
    let raw: Vec<String> = std::env::args().skip(1).collect();
    if raw.is_empty() {
        usage();
    }
    let (mode, in_path, out, events) = parse(&raw);
    match mode {
        Mode::Apply => {
            let in_path = in_path.unwrap_or_else(|| usage());
            let out = out.unwrap_or_else(|| usage());
            apply_and_report(&in_path, &out, events.as_deref())
        }
        Mode::Watch { poll } => {
            let in_path = in_path.unwrap_or_else(|| usage());
            let out = out.unwrap_or_else(|| usage());
            // Apply whatever is present at startup, then mirror changes.
            let startup = apply_and_report(&in_path, &out, events.as_deref());
            if startup != ExitCode::SUCCESS {
                return startup;
            }
            println!(
                "watching {} every {:?} -> {}",
                in_path.display(),
                poll,
                out.display()
            );
            let mut last = vtessera_capacity::file_stamp(&in_path);
            loop {
                thread::sleep(poll);
                let stamp = vtessera_capacity::file_stamp(&in_path);
                if stamp != last {
                    let _ = apply_and_report(&in_path, &out, events.as_deref());
                    last = stamp;
                }
            }
        }
        Mode::Autoscale(cfg_path) => run_autoscale(&cfg_path),
    }
}
