//! Shared `capacity.toml` schema + file-drop controller (issue #109).
//!
//! Two jobs live here, so the writer and reader of a capacity file can never
//! drift:
//!
//! 1. **The schema** — [`CapacityConfig`], [`load`] and [`file_stamp`] are
//!    the single source of truth for capacity files. The node's parse-only
//!    capacity module (`vtessera-node-api`, serve build) re-uses them; a file
//!    the controller writes is, byte-for-byte, what the node validates.
//! 2. **The apply** — [`apply_and_commit`] is the file-drop trigger. It
//!    validates an operator-dropped input file and atomically commits it over
//!    the node's watched `capacity.toml` (temp-write + rename in the same
//!    directory), then appends an audit record to an optional events log.
//!    The node's own watcher picks the file up on its next poll tick and
//!    reconfigures the queue / re-signs the offer from it — the controller
//!    needs nothing but a filesystem to drive a node.
//!
//! Everything here is dependency-light by design: no sockets, no GPU, no
//! payment surface. The schedule/autoscale engines (which also *write*
//! `capacity.toml`) build on the same [`apply_and_commit`] primitive.

use std::collections::BTreeMap;
use std::fs;
use std::io::Write;
use std::path::Path;
use std::time::SystemTime;

use serde::Deserialize;

/// Default poll interval for a controller watch loop (seconds).
pub const DEFAULT_WATCH_POLL_SECS: u64 = 2;

/// Live capacity controls read from `capacity.toml`.
///
/// Every field is optional — a file only overrides the knobs it mentions and
/// leaves the rest at their current value. Unknown keys are rejected at load
/// so a typo'ed control fails loudly instead of silently no-op'ing.
#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CapacityConfig {
    /// Open execution slots (queue concurrency cap).
    pub max_concurrent_jobs: Option<u32>,
    /// Jobs allowed to wait in the on-disk queue (0 disables waiting:
    /// a busy node always 503s).
    pub max_queue_len: Option<usize>,
    /// Advertised vCPUs for the offer's device.
    pub advertised_vcpus: Option<u32>,
    /// Advertised RAM (MiB) for the offer's device. As [`CapacityConfig::advertised_vcpus`].
    pub advertised_mem_mb: Option<u32>,
    /// Scale for `per_device_second_micros` on a paid offer.
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

/// `(mtime_nanos, size)` of `path`, or `None` when the file is absent. Both
/// are folded into the stamp so a same-second rewrite still registers.
///
/// Shared with the node's watcher so both sides watch with the same cheap
/// poll (no inotify dependency; the poll interval is seconds-scale anyway).
pub fn file_stamp(path: &Path) -> Option<(u128, u64)> {
    let md = fs::metadata(path).ok()?;
    let mt = md
        .modified()
        .ok()?
        .duration_since(SystemTime::UNIX_EPOCH)
        .ok()?
        .as_nanos();
    Some((mt, md.len()))
}

/// The knobs that differ between `prev` and `next`, rendered
/// `"name" => "old -> new"` for the event log. `None` renders as `None`.
pub fn diff_config(
    prev: Option<&CapacityConfig>,
    next: &CapacityConfig,
) -> BTreeMap<String, String> {
    fn delta<T>(name: &str, prev: Option<T>, next: Option<T>, out: &mut BTreeMap<String, String>)
    where
        T: PartialEq + std::fmt::Debug,
    {
        if prev != next {
            out.insert(name.to_string(), format!("{prev:?} -> {next:?}"));
        }
    }

    let mut out = BTreeMap::new();
    delta(
        "max_concurrent_jobs",
        prev.and_then(|c| c.max_concurrent_jobs),
        next.max_concurrent_jobs,
        &mut out,
    );
    delta(
        "max_queue_len",
        prev.and_then(|c| c.max_queue_len),
        next.max_queue_len,
        &mut out,
    );
    delta(
        "advertised_vcpus",
        prev.and_then(|c| c.advertised_vcpus),
        next.advertised_vcpus,
        &mut out,
    );
    delta(
        "advertised_mem_mb",
        prev.and_then(|c| c.advertised_mem_mb),
        next.advertised_mem_mb,
        &mut out,
    );
    delta(
        "price_multiplier",
        prev.and_then(|c| c.price_multiplier),
        next.price_multiplier,
        &mut out,
    );
    delta(
        "accept_workloads",
        prev.and_then(|c| c.accept_workloads),
        next.accept_workloads,
        &mut out,
    );
    out
}

/// Result of one apply pass.
#[derive(Debug, Clone, PartialEq)]
pub struct CommitSummary {
    /// True when the input was already equivalent to the committed file (no
    /// write performed). The controller does not bump the node's watcher when
    /// nothing would change.
    pub unchanged: bool,
    /// The knobs this apply changed, `"name" => "old -> new"`.
    pub changed: BTreeMap<String, String>,
}

/// One file-drop apply pass ([`CapacityConfig`] is the schema).
///
/// - validate `in_path` (rejects unknown keys, unreadable files);
/// - diff it against the already-committed `out_path` — an equivalent input
///   is a no-op (no write, no event);
/// - otherwise commit the input byte-for-byte to `out_path` with a
///   temp-write + rename in the same directory (atomic: a node watching the
///   file sees either the old content or the new, never a partial write);
/// - append a JSON-lines audit record to `events_path` (when given).
pub fn apply_and_commit(
    in_path: &Path,
    out_path: &Path,
    events_path: Option<&Path>,
) -> Result<CommitSummary, CapacityError> {
    let next = load(in_path)?;
    let prev = fs::read_to_string(out_path)
        .ok()
        .and_then(|raw| toml::from_str::<CapacityConfig>(&raw).ok());
    let changed = diff_config(prev.as_ref(), &next);
    if changed.is_empty() {
        return Ok(CommitSummary {
            unchanged: true,
            changed,
        });
    }

    // Commit the operator's file verbatim (comments and ordering preserved) —
    // the node parses the same schema, so what left the controller is exactly
    // what the node validates.
    let raw = fs::read_to_string(in_path)
        .map_err(|e| CapacityError(format!("read {}: {e}", in_path.display())))?;
    write_atomically(out_path, &raw)?;
    if let Some(events) = events_path {
        append_event(events, in_path, &changed)?;
    }
    Ok(CommitSummary {
        unchanged: false,
        changed,
    })
}

/// Atomically replace `path` with `body`: write a temp file in the same
/// directory, then rename over the target. `fs::rename` replaces atomically
/// on POSIX, so a concurrent reader (the node's watcher) never observes a
/// torn write.
fn write_atomically(path: &Path, body: &str) -> Result<(), CapacityError> {
    let dir = path.parent().unwrap_or_else(|| Path::new("."));
    let base = path
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| "capacity.toml".into());
    let tmp = dir.join(format!(".{base}.tmp{}", std::process::id()));
    {
        let mut f = fs::File::create(&tmp)
            .map_err(|e| CapacityError(format!("create {}: {e}", tmp.display())))?;
        f.write_all(body.as_bytes())
            .and_then(|_| f.sync_all())
            .map_err(|e| CapacityError(format!("write {}: {e}", tmp.display())))?;
    }
    fs::rename(&tmp, path).map_err(|e| CapacityError(format!("rename {}: {e}", path.display())))
}

/// Append one JSON-lines audit record for an applied transition.
fn append_event(
    path: &Path,
    source: &Path,
    changed: &BTreeMap<String, String>,
) -> Result<(), CapacityError> {
    let ts = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let rec = serde_json::json!({
        "ts": ts,
        "source": source.display().to_string(),
        "changed": changed,
    });
    let mut line =
        serde_json::to_string(&rec).map_err(|e| CapacityError(format!("encode event: {e}")))?;
    line.push('\n');
    let mut f = fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .map_err(|e| CapacityError(format!("open {}: {e}", path.display())))?;
    f.write_all(line.as_bytes())
        .map_err(|e| CapacityError(format!("append {}: {e}", path.display())))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::path::PathBuf;

    // Tagged per-crate temp dirs so the controller suite and the node's
    // capacity tests never race on the same directory in unit-test runs.
    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("vtcc-{tag}"));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).expect("create temp dir");
        dir
    }

    fn write(dir: &Path, name: &str, body: &str) -> PathBuf {
        let p = dir.join(name);
        fs::write(&p, body).expect("write capacity file");
        p
    }

    /// A full six-knob config, matching the schema fixture used by the node.
    fn full_body() -> &'static str {
        "max_concurrent_jobs = 4\n\
         max_queue_len = 32\n\
         advertised_vcpus = 8\n\
         advertised_mem_mb = 16384\n\
         price_multiplier = 1.5\n\
         accept_workloads = true\n"
    }

    fn no_knobs() -> CapacityConfig {
        CapacityConfig {
            max_concurrent_jobs: None,
            max_queue_len: None,
            advertised_vcpus: None,
            advertised_mem_mb: None,
            price_multiplier: None,
            accept_workloads: None,
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
        let p = write(&temp_dir("full"), "capacity.toml", full_body());
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

    #[test]
    fn diff_config_reports_only_changed_knobs() {
        let prev = CapacityConfig {
            max_concurrent_jobs: Some(4),
            max_queue_len: Some(32),
            advertised_vcpus: Some(8),
            advertised_mem_mb: Some(16384),
            price_multiplier: Some(1.0),
            accept_workloads: Some(true),
        };
        let next = CapacityConfig {
            max_concurrent_jobs: Some(8),
            advertised_mem_mb: Some(24576),
            ..prev.clone()
        };
        let d = diff_config(Some(&prev), &next);
        assert_eq!(
            d,
            BTreeMap::from([
                ("max_concurrent_jobs".into(), "Some(4) -> Some(8)".into()),
                (
                    "advertised_mem_mb".into(),
                    "Some(16384) -> Some(24576)".into()
                ),
            ])
        );
        // No prev → every knob is reported as a change so the event log still
        // shows what the first commit established.
        let first = diff_config(None, &next);
        assert_eq!(first.len(), 6);
        assert_eq!(diff_config(Some(&prev), &prev), BTreeMap::new());
        assert!(diff_config(None, &no_knobs()).is_empty());
    }

    #[test]
    fn apply_and_commit_writes_atomically() {
        let dir = temp_dir("apply");
        let src = write(&dir, "capacity.in.toml", "max_concurrent_jobs = 8\n");
        let out = dir.join("capacity.toml");
        let events = dir.join("events.jsonl");

        let r = apply_and_commit(&src, &out, Some(&events)).unwrap();
        assert!(!r.unchanged);
        assert!(r.changed.contains_key("max_concurrent_jobs"));
        // Verbatim commit — the node gets exactly what the operator dropped.
        assert_eq!(
            fs::read_to_string(&out).unwrap(),
            "max_concurrent_jobs = 8\n"
        );
        // No temp file left behind of the atomic write.
        assert_eq!(
            fs::read_dir(&dir).unwrap().count(),
            3,
            "src, out, events — no .tmp stragglers"
        );

        // Second identical commit is a no-op: no rewrite, no new event.
        let r2 = apply_and_commit(&src, &out, Some(&events)).unwrap();
        assert!(r2.unchanged);
        let events = fs::read_to_string(&events).unwrap();
        assert_eq!(events.lines().count(), 1);
        assert!(events.contains("\"changed\":{\"max_concurrent_jobs\":\"None -> Some(8)\"}"));
        assert!(events.contains("\"source\":\""));
    }

    #[test]
    fn apply_and_commit_records_transition_event() {
        let dir = temp_dir("events");
        let lvl1 = write(&dir, "l1.in.toml", "max_concurrent_jobs = 8\n");
        let out = dir.join("capacity.toml");
        let events = dir.join("events.jsonl");

        apply_and_commit(&lvl1, &out, Some(&events)).unwrap();
        // Scale down later: the event shows the old -> new knobs.
        let lvl2 = write(&dir, "l2.in.toml", "max_concurrent_jobs = 2\n");
        let r = apply_and_commit(&lvl2, &out, Some(&events)).unwrap();
        assert!(!r.unchanged);
        assert_eq!(r.changed["max_concurrent_jobs"], "Some(8) -> Some(2)");

        let lines: Vec<_> = fs::read_to_string(&events)
            .unwrap()
            .lines()
            .map(|l| serde_json::from_str::<serde_json::Value>(l).unwrap())
            .collect();
        assert_eq!(lines.len(), 2);
        assert_eq!(
            lines[0]["changed"]["max_concurrent_jobs"],
            "None -> Some(8)"
        );
        assert_eq!(
            lines[1]["changed"]["max_concurrent_jobs"],
            "Some(8) -> Some(2)"
        );
    }

    #[test]
    fn apply_and_commit_rejects_invalid_input() {
        let dir = temp_dir("invalid");
        let src = write(&dir, "bad.in.toml", "bogus_knob = 1\n");
        let out = dir.join("capacity.toml");
        let err = apply_and_commit(&src, &out, None).unwrap_err();
        assert!(err.to_string().contains("unknown field"), "err: {err}");
        assert!(!out.exists(), "nothing committed for an invalid file");
    }
}
