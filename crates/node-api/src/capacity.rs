//! Live capacity reconfiguration for a node (issue #109).
//!
//! A `capacity.toml` lets an operator re-tune a running node's admission
//! and advertising knobs without a restart:
//!
//! ```toml
//! max_concurrent_jobs = 4    # open execution slots
//! max_queue_len = 32         # jobs allowed to wait in the on-disk queue
//! advertised_vcpus = 8       # offer device (applied by offer re-signing)
//! advertised_mem_mb = 16384  # offer device (applied by offer re-signing)
//! price_multiplier = 1.5     # paid-offer price scale (offer re-signing)
//! accept_workloads = true    # GUI consent-toggle mirror (admission policy)
//! ```
//!
//! Every field is optional — a file only overrides the knobs it mentions
//! and leaves the rest at their current value. Unknown keys are rejected at
//! load so a typo'ed control fails loudly instead of silently no-op'ing.
//!
//! The queue gate ([`JobQueue::reconfigure`]) is applied immediately. The
//! offer-side knobs (`advertised_*`, `price_multiplier`,
//! `accept_workloads`) are parsed and validated here because they change
//! the signed offer / admission contract, which this module does not own;
//! they are applied by the live offer-reconfiguration follow-up.
//!
//! Watching is a cheap mtime+size poll (no inotify dependency): the node's
//! process stays self-contained and the poll interval is seconds-scale
//! anyway.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::thread;
use std::time::{Duration, SystemTime};

use serde::Deserialize;

use crate::queue::JobQueue;

/// Default poll interval for the capacity watch loop.
pub const DEFAULT_CAPACITY_POLL_SECS: u64 = 2;

/// Live capacity controls read from `capacity.toml`.
#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CapacityConfig {
    /// Open execution slots (queue concurrency cap).
    pub max_concurrent_jobs: Option<u32>,
    /// Jobs allowed to wait in the on-disk queue (0 disables waiting:
    /// a busy node always 503s).
    pub max_queue_len: Option<usize>,
    /// Advertised vCPUs for the offer's device. Parsed for validation now;
    /// re-signed into the published offer by the offer-reconfig follow-up.
    pub advertised_vcpus: Option<u32>,
    /// Advertised RAM (MiB) for the offer's device. As [`CapacityConfig::advertised_vcpus`].
    pub advertised_mem_mb: Option<u32>,
    /// Scale for `per_device_second_micros` on a paid offer. Parsed now;
    /// applied by the offer-reconfig follow-up.
    pub price_multiplier: Option<f64>,
    /// Mirrors the GUI's consent toggle (only accept approved workloads).
    pub accept_workloads: Option<bool>,
}

/// A rejected capacity file. The message is safe to show to an operator.
#[derive(Debug, Clone, PartialEq)]
pub struct CapacityError(pub String);

impl std::fmt::Display for CapacityError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.0)
    }
}

impl std::error::Error for CapacityError {}

/// Load and validate the capacity file at `path`.
pub fn load(path: &Path) -> Result<CapacityConfig, CapacityError> {
    let raw = fs::read_to_string(path)
        .map_err(|e| CapacityError(format!("read {}: {e}", path.display())))?;
    toml::from_str(&raw).map_err(|e| CapacityError(format!("{}: {e}", path.display())))
}

/// Apply the capacity file to a queue once: reconfigure the admission gate
/// from the file's non-`None` queue knobs, leaving everything else at its
/// current value. The parsed config is returned so callers can log which
/// offer-side knobs are validated-but-deferred.
pub fn apply_config(path: &Path, queue: &JobQueue) -> Result<CapacityConfig, CapacityError> {
    let cfg = load(path)?;
    queue.reconfigure(
        cfg.max_concurrent_jobs
            .unwrap_or_else(|| queue.max_concurrent()),
        cfg.max_queue_len.unwrap_or_else(|| queue.max_queue_len()),
    );
    Ok(cfg)
}

/// One apply pass with operator-facing logging. Shared by the startup
/// one-shot and the watch loop so every reload reports the same way.
pub fn apply_and_log(path: &Path, queue: &Arc<JobQueue>) {
    match apply_config(path, queue) {
        Ok(cfg) => {
            eprintln!("vtessera-node: applied capacity file {}", path.display());
            if offer_knobs_present(&cfg) {
                eprintln!(
                    "vtessera-node: warning: advertised_vcpus/advertised_mem_mb/price_multiplier/\
                     accept_workloads are validated but applied by live offer re-signing (follow-up)"
                );
            }
        }
        Err(e) => eprintln!("vtessera-node: capacity apply failed: {e}"),
    }
}

fn offer_knobs_present(cfg: &CapacityConfig) -> bool {
    cfg.advertised_vcpus.is_some()
        || cfg.advertised_mem_mb.is_some()
        || cfg.price_multiplier.is_some()
        || cfg.accept_workloads.is_some()
}

/// Poll `path` every `poll` and re-apply it whenever it changes (mtime or
/// size). Runs until the process exits. A broken file is reported and
/// retried on the next change — the node keeps its last good values.
pub fn spawn_watcher(path: PathBuf, queue: Arc<JobQueue>, poll: Duration) {
    thread::spawn(move || {
        // Don't re-apply the state the binary already applied at startup;
        // only actual changes (or a file appearing later) trigger a pass.
        let mut last = file_stamp(&path);
        loop {
            thread::sleep(poll);
            let stamp = file_stamp(&path);
            if stamp != last {
                apply_and_log(&path, &queue);
                last = stamp;
            }
        }
    });
}

/// `(mtime_nanos, size)` of `path`, or `None` when the file is absent. Both
/// are folded into the stamp so a same-second rewrite still registers.
fn file_stamp(path: &Path) -> Option<(u128, u64)> {
    let md = fs::metadata(path).ok()?;
    let mt = md
        .modified()
        .ok()?
        .duration_since(SystemTime::UNIX_EPOCH)
        .ok()?
        .as_nanos();
    Some((mt, md.len()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::queue::{JobQueue, QueuedJob};
    use crate::{JobRunError, JobRunner};

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("vtcap-{tag}"));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("create temp dir");
        dir
    }

    fn write(dir: &Path, name: &str, body: &str) -> PathBuf {
        let p = dir.join(name);
        fs::write(&p, body).expect("write capacity file");
        p
    }

    struct Echo;
    impl JobRunner for Echo {
        fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
            let _ = QueuedJob {
                seq: 0,
                job_id: "x".into(),
                priority: 0,
                body: body.to_vec(),
                created_unix: 0,
            };
            Ok(r#"{"status":"accepted","job_id":"x"}"#.into())
        }
    }

    #[test]
    fn load_rejects_unknown_fields() {
        let p = write(&temp_dir("unknown"), "capacity.toml", "bogus_knob = 1\n");
        let err = load(&p).unwrap_err();
        assert!(err.to_string().contains("unknown field"), "err: {err}");
    }

    #[test]
    fn load_parses_full_config() {
        let p = write(
            &temp_dir("full"),
            "capacity.toml",
            "max_concurrent_jobs = 4\n\
             max_queue_len = 32\n\
             advertised_vcpus = 8\n\
             advertised_mem_mb = 16384\n\
             price_multiplier = 1.5\n\
             accept_workloads = true\n",
        );
        let cfg = load(&p).unwrap();
        assert_eq!(
            cfg,
            CapacityConfig {
                max_concurrent_jobs: Some(4),
                max_queue_len: Some(32),
                advertised_vcpus: Some(8),
                advertised_mem_mb: Some(16384),
                price_multiplier: Some(1.5),
                accept_workloads: Some(true),
            }
        );
    }

    #[test]
    fn load_missing_file_errors() {
        let err = load(&temp_dir("missing").join("nope.toml")).unwrap_err();
        assert!(err.to_string().contains("read"), "err: {err}");
    }

    #[test]
    fn apply_config_reconfigures_the_queue() {
        let p = write(
            &temp_dir("apply"),
            "capacity.toml",
            "max_concurrent_jobs = 3\nmax_queue_len = 8\n",
        );
        let q = JobQueue::new(temp_dir("apply-q"), Arc::new(Echo), 1, 16);
        apply_config(&p, &q).unwrap();
        assert_eq!(q.max_concurrent(), 3);
        assert_eq!(q.max_queue_len(), 8);
    }

    #[test]
    fn apply_config_keeps_unmentioned_knobs() {
        let p = write(
            &temp_dir("partial"),
            "capacity.toml",
            "max_concurrent_jobs = 2\n",
        );
        let q = JobQueue::new(temp_dir("partial-q"), Arc::new(Echo), 1, 16);
        apply_config(&p, &q).unwrap();
        assert_eq!(q.max_concurrent(), 2);
        assert_eq!(q.max_queue_len(), 16, "unmentioned knob is untouched");
    }

    #[test]
    fn file_stamp_tracks_rewrites() {
        let dir = temp_dir("stamp");
        let p = write(&dir, "capacity.toml", "max_concurrent_jobs = 1\n");
        let before = file_stamp(&p).expect("stamp");
        // Same-second rewrite with different content: the size must flip the stamp.
        fs::write(&p, "max_concurrent_jobs = 77\nmax_queue_len = 3\n").unwrap();
        let after = file_stamp(&p).expect("stamp");
        assert_ne!(before, after);
        assert_eq!(file_stamp(&dir.join("absent.toml")), None);
    }
}
