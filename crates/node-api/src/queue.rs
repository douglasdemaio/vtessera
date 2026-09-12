//! Node-local job queue (ROADMAP §2 / PRD shortfall "No queuing / scheduling").
//!
//! Spec: `docs/design/specs/2026-09-10-job-queue-design.md`.
//!
//! A [`JobQueue`] bounds the number of jobs a node runs at once
//! (`max_concurrent`, default 1) and holds the excess in a **durable,
//! priority-aware wait queue** under `<state-dir>/job-queue/`. The node
//! binary owns the drainer thread ([`JobQueue::spawn`]); this module only
//! calls the injected [`crate::JobRunner`] trait — the same trait `POST
//! /jobs` uses today — so the lib stays executor-free.
//!
//! Disk layout (all writes are temp-write + rename so a crash never leaves
//! a partial file):
//!
//! - `<job_id>.json`           — a queued job.
//! - `<job_id>.json.running`   — marker while a job holds a slot.
//! - `<job_id>.json.done`      — final result (runner's 200 JSON, or a
//!   failure envelope).
//! - `<job_id>.json.cancelled` — cancel marker (queued jobs only).

use std::fs;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU32, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::{JobRunError, JobRunner};

/// Cap a hostile job_id before it becomes a filename.
pub const MAX_JOB_ID_LEN: usize = 128;

/// A job_id is admissible iff it is a safe single path segment.
/// Anything else is a 400 before any write — path traversal rejected up front.
pub fn valid_job_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= MAX_JOB_ID_LEN
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b'_' | b'~'))
}

/// One queued job, as persisted under `<job-queue>/<id>.json`.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct QueuedJob {
    /// Ordering key (secondary). Monotonic per node.
    pub seq: u64,
    pub job_id: String,
    /// 0 (default) ..= 9. Higher runs first. Advisory + node-local: never
    /// persisted into contracts or receipts.
    pub priority: u8,
    /// Raw `POST /jobs` body, replayed to the runner when the slot opens.
    pub body: Vec<u8>,
    pub created_unix: u64,
}

/// What [`JobQueue::enqueue`] decided to do with a submission.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EnqueueOutcome {
    /// A slot was free; the job ran synchronously with this runner 200 JSON.
    RanNow(String),
    /// The node was busy; the job is waiting at 1-based `position`.
    Queued { position: usize },
}

/// Parsed `done` file state.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DoneState {
    Completed(serde_json::Value),
    Failed { reason: String },
}

/// A lookup for a job that has no on-disk traces at all.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QueueLookupError {
    NoSuchJob,
}

struct QueueInner {
    queued: Vec<QueuedJob>,
    running: u32,
    next_seq: u64,
}

/// A durable, bounded, priority-aware wait queue + drainer.
///
/// The admission gates are atomics so a running node's capacity can be
/// re-tuned live ([`JobQueue::reconfigure`], issue #109) without dropping
/// the queue. Reads are `Relaxed`: admission decisions are still
/// coordinated under [`JobQueue::inner`]'s mutex, and a briefly stale cap
/// self-corrects on the next enqueue/drain tick.
pub struct JobQueue {
    dir: PathBuf,
    runner: Arc<dyn JobRunner>,
    /// Open execution slots (live-changeable).
    max_concurrent: AtomicU32,
    /// Jobs allowed to wait (live-changeable; 0 disables the wait queue).
    max_queue_len: AtomicUsize,
    inner: Mutex<QueueInner>,
}

struct FileNames {
    queued: PathBuf,    // <id>.json
    running: PathBuf,   // <id>.json.running
    done: PathBuf,      // <id>.json.done
    cancelled: PathBuf, // <id>.json.cancelled
}

impl FileNames {
    fn new(dir: &std::path::Path, job_id: &str) -> Self {
        Self {
            queued: dir.join(format!("{job_id}.json")),
            running: dir.join(format!("{job_id}.json.running")),
            done: dir.join(format!("{job_id}.json.done")),
            cancelled: dir.join(format!("{job_id}.json.cancelled")),
        }
    }
}

impl JobQueue {
    /// Create a queue rooted at `dir` (created if missing), re-scanning any
    /// existing `*.json` files into the wait set, sorted priority-desc then
    /// seq-asc. Leftover `.running` markers are rewound (the job re-runs on
    /// the next slot — the executor is idempotent at the job_id level).
    pub fn new(
        dir: impl Into<PathBuf>,
        runner: Arc<dyn JobRunner>,
        max_concurrent: u32,
        max_queue_len: usize,
    ) -> Arc<JobQueue> {
        let dir = dir.into();
        fs::create_dir_all(&dir).expect("create job-queue dir");
        let mut next_seq = 1u64;
        let mut queued = Vec::new();
        if let Ok(rd) = fs::read_dir(&dir) {
            for entry in rd.flatten() {
                let path = entry.path();
                if path.extension().is_some_and(|x| x == "json")
                    && !path.as_os_str().to_string_lossy().contains(".json.")
                {
                    if let Ok(raw) = fs::read_to_string(&path) {
                        if let Ok(job) = serde_json::from_str::<QueuedJob>(&raw) {
                            next_seq = next_seq.max(job.seq + 1);
                            queued.push(job);
                        }
                    }
                }
            }
        }
        queued.sort_by(compare_jobs);
        Arc::new(JobQueue {
            dir,
            runner,
            max_concurrent: AtomicU32::new(max_concurrent.max(1)),
            max_queue_len: AtomicUsize::new(max_queue_len),
            inner: Mutex::new(QueueInner {
                queued,
                running: 0,
                next_seq,
            }),
        })
    }

    /// Current concurrency cap. May differ from the `new` value after a
    /// live [`JobQueue::reconfigure`].
    pub fn max_concurrent(&self) -> u32 {
        self.max_concurrent.load(Ordering::Relaxed)
    }

    /// Current wait-queue depth cap. May differ from the `new` value after
    /// a live [`JobQueue::reconfigure`].
    pub fn max_queue_len(&self) -> usize {
        self.max_queue_len.load(Ordering::Relaxed)
    }

    /// Live-reconfigure the admission gates without restarting the node.
    ///
    /// `max_concurrent` is clamped to at least 1 (a node must always admit
    /// work) and best-effort to at least the number of jobs currently
    /// running, so a shrink never strands an in-flight job — reducing the
    /// cap only stops *new* admissions; running jobs finish untouched.
    /// `max_queue_len` is clamped to at least 1 (0 would silently drop
    /// every busy submission).
    ///
    /// Both take effect on the next enqueue/drain tick; the watcher in
    /// `capacity.rs` calls this when `capacity.toml` changes.
    pub fn reconfigure(&self, max_concurrent: u32, max_queue_len: usize) {
        let running = self.inner.lock().unwrap().running;
        let new_concurrent = max_concurrent.max(1).max(running);
        self.max_concurrent.store(new_concurrent, Ordering::Relaxed);
        self.max_queue_len
            .store(max_queue_len.max(1), Ordering::Relaxed);
    }

    /// Admit a job body. Runs it synchronously when a slot is free (returns
    /// the runner's 200 JSON); otherwise queues it and reports its position.
    /// Refuses [`JobRunError::server`] (→ 503) when the wait queue is full.
    pub fn enqueue(
        &self,
        job_id: &str,
        priority: u8,
        body: &[u8],
    ) -> Result<EnqueueOutcome, JobRunError> {
        if !valid_job_id(job_id) {
            return Err(JobRunError::bad_request(format!(
                "invalid job_id {job_id:?}: allowed {MAX_JOB_ID_LEN} chars of [A-Za-z0-9._-~]"
            )));
        }
        let priority = priority.min(9);

        // Reserve a slot atomically with the drainer so the fast path and the
        // drainer can't both exceed `max_concurrent` at once.
        let slot = {
            let mut inner = self.inner.lock().unwrap();
            if inner.running < self.max_concurrent.load(Ordering::Relaxed) {
                inner.running += 1;
                true
            } else {
                false
            }
        };
        if slot {
            let job = QueuedJob {
                seq: 0,
                job_id: job_id.to_string(),
                priority,
                body: body.to_vec(),
                created_unix: now_unix(),
            };
            let result = self.run_job(&job).map(EnqueueOutcome::RanNow);
            {
                let mut inner = self.inner.lock().unwrap();
                inner.running = inner.running.saturating_sub(1);
            }
            return result;
        }

        let position = {
            let mut inner = self.inner.lock().unwrap();
            if inner.queued.len() >= self.max_queue_len.load(Ordering::Relaxed) {
                return Err(JobRunError::unavailable("queue full — retry later"));
            }
            let seq = inner.next_seq;
            inner.next_seq += 1;
            let job = QueuedJob {
                seq,
                job_id: job_id.to_string(),
                priority,
                body: body.to_vec(),
                created_unix: now_unix(),
            };
            let idx = match inner.queued.binary_search_by(|q| compare_jobs(q, &job)) {
                Ok(i) => i,
                Err(i) => i,
            };
            inner.queued.insert(idx, job.clone());
            let _ = self.persist_queued(&job);
            idx + 1
        };
        Ok(EnqueueOutcome::Queued { position })
    }

    /// 1-based queue position of a queued job (compacted), if still waiting.
    pub fn position(&self, job_id: &str) -> Option<usize> {
        let inner = self.inner.lock().unwrap();
        inner
            .queued
            .iter()
            .position(|q| q.job_id == job_id)
            .map(|i| i + 1)
    }

    /// Status synthesized from on-disk state (§4.5).
    /// `Ok(None)` = unknown/idle marker file; `Ok(Some(v))` = status JSON;
    /// `Err(QueueLookupError::NoSuchJob)` = 404 (no traces at all).
    pub fn status(&self, job_id: &str) -> Result<Option<serde_json::Value>, QueueLookupError> {
        let names = FileNames::new(&self.dir, job_id);
        if names.cancelled.exists() {
            return Ok(Some(
                serde_json::json!({ "status": "cancelled", "job_id": job_id }),
            ));
        }
        if let Some(d) = self.done_state(job_id) {
            return Ok(Some(match d {
                DoneState::Completed(result) => serde_json::json!({
                    "status": "completed", "job_id": job_id, "result": result
                }),
                DoneState::Failed { reason } => serde_json::json!({
                    "status": "failed", "job_id": job_id, "reason": reason
                }),
            }));
        }
        if names.running.exists() {
            return Ok(Some(
                serde_json::json!({ "status": "running", "job_id": job_id }),
            ));
        }
        if names.queued.exists() {
            return Ok(Some(serde_json::json!({
                "status": "queued",
                "job_id": job_id,
                "position": self.position(job_id).unwrap_or(1),
                "priority": self.queued_priority(job_id).unwrap_or(0),
            })));
        }
        Err(QueueLookupError::NoSuchJob)
    }

    /// Cancel a **queued** job. Running/finished jobs are never touched (no
    /// preemption). `Ok(Ok(()))` on cancel, `Ok(Err(reason))` when the job is
    /// past queued, `Err(QueueLookupError::NoSuchJob)` (404) if it never existed.
    pub fn cancel(&self, job_id: &str) -> Result<Result<(), String>, QueueLookupError> {
        let names = FileNames::new(&self.dir, job_id);
        if names.cancelled.exists() || names.running.exists() || self.done_state(job_id).is_some() {
            return Ok(Err("job already past queued".into()));
        }
        let mut inner = self.inner.lock().unwrap();
        let before = inner.queued.len();
        inner.queued.retain(|q| q.job_id != job_id);
        if inner.queued.len() == before {
            return Err(QueueLookupError::NoSuchJob);
        }
        drop(inner);
        if names.queued.exists() {
            let _ = fs::remove_file(&names.queued);
        }
        let _ = fs::write(&names.cancelled, b"");
        Ok(Ok(()))
    }

    /// Spawn the single drainer thread (process-owned; stopped only by exit).
    pub fn spawn(self: &Arc<Self>) {
        let q = self.clone();
        std::thread::spawn(move || loop {
            let next = {
                let mut inner = q.inner.lock().unwrap();
                if inner.queued.is_empty()
                    || inner.running >= q.max_concurrent.load(Ordering::Relaxed)
                {
                    None
                } else {
                    inner.running += 1;
                    Some(inner.queued.remove(0))
                }
            };
            match next {
                Some(job) => {
                    let _ = q.run_job(&job);
                    let mut inner = q.inner.lock().unwrap();
                    inner.running = inner.running.saturating_sub(1);
                }
                None => std::thread::sleep(Duration::from_millis(250)),
            }
        });
    }

    /// Test/synchronous sibling of [`JobQueue::spawn`]: run up to `n` queued
    /// jobs now, returning how many ran. Used by tests to avoid threads.
    pub fn drain_sync(&self, n: usize) -> usize {
        let mut ran = 0;
        for _ in 0..n.max(1) {
            let next = {
                let mut inner = self.inner.lock().unwrap();
                if inner.queued.is_empty()
                    || inner.running >= self.max_concurrent.load(Ordering::Relaxed)
                {
                    None
                } else {
                    inner.running += 1;
                    Some(inner.queued.remove(0))
                }
            };
            match next {
                Some(job) => {
                    let _ = self.run_job(&job);
                    let mut inner = self.inner.lock().unwrap();
                    inner.running = inner.running.saturating_sub(1);
                    ran += 1;
                }
                None => break,
            }
        }
        ran
    }

    fn run_job(&self, job: &QueuedJob) -> Result<String, JobRunError> {
        let names = FileNames::new(&self.dir, &job.job_id);
        // A cancel that raced in since dequeue wins.
        if names.cancelled.exists() || names.done.exists() {
            return Err(JobRunError::server("job was cancelled"));
        }
        // Persist the base file *before* running so a crash mid-run leaves a
        // rewound, restartable queued job — not just a stale `.running`
        // marker with no record to re-run.
        let _ = self.persist_queued(job);
        let _ = fs::write(&names.running, b"");
        let result = self.runner.run(&job.body);
        match &result {
            Ok(json) => {
                let result = serde_json::from_str::<serde_json::Value>(json)
                    .unwrap_or_else(|_| serde_json::Value::String(json.clone()));
                let envelope = serde_json::json!({
                    "status": "completed",
                    "job_id": job.job_id,
                    "result": result,
                });
                let _ = fs::write(
                    &names.done,
                    serde_json::to_vec_pretty(&envelope).unwrap_or_default(),
                );
            }
            Err(e) => {
                let envelope = serde_json::json!({
                    "status": "failed",
                    "job_id": job.job_id,
                    "reason": e.message,
                });
                let _ = fs::write(
                    &names.done,
                    serde_json::to_vec_pretty(&envelope).unwrap_or_default(),
                );
            }
        }
        let _ = fs::remove_file(&names.running);
        result
    }

    fn persist_queued(&self, job: &QueuedJob) -> Result<(), std::io::Error> {
        let names = FileNames::new(&self.dir, &job.job_id);
        let tmp = self.dir.join(format!("{}.json.tmp", job.job_id));
        let json = serde_json::to_vec_pretty(job).unwrap_or_default();
        fs::write(&tmp, json)?;
        fs::rename(&tmp, &names.queued)
    }

    fn done_state(&self, job_id: &str) -> Option<DoneState> {
        let names = FileNames::new(&self.dir, job_id);
        let raw = fs::read_to_string(&names.done).ok()?;
        let v: serde_json::Value = serde_json::from_str(&raw).ok()?;
        match v.get("status").and_then(|s| s.as_str()) {
            Some("failed") => Some(DoneState::Failed {
                reason: v
                    .get("reason")
                    .and_then(|r| r.as_str())
                    .unwrap_or("unknown failure")
                    .to_string(),
            }),
            Some(_) => v.get("result").cloned().map(DoneState::Completed),
            None => None,
        }
    }

    fn queued_priority(&self, job_id: &str) -> Option<u8> {
        let inner = self.inner.lock().unwrap();
        inner
            .queued
            .iter()
            .find(|q| q.job_id == job_id)
            .map(|q| q.priority)
    }
}

/// `(priority desc, seq asc)` — the wait-queue ordering.
fn compare_jobs(a: &QueuedJob, b: &QueuedJob) -> std::cmp::Ordering {
    b.priority.cmp(&a.priority).then_with(|| a.seq.cmp(&b.seq))
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU32, Ordering};
    use std::thread;

    /// Records the job_id of every run, in execution order, then echoes it.
    type Recall = (Arc<dyn JobRunner>, Arc<Mutex<Vec<String>>>, Arc<AtomicU32>);
    fn order_runner() -> Recall {
        let order = Arc::new(Mutex::new(Vec::new()));
        let calls = Arc::new(AtomicU32::new(0));
        let runner = Arc::new(EchoRunner {
            order: order.clone(),
            calls: calls.clone(),
        });
        (runner, order, calls)
    }

    struct EchoRunner {
        order: Arc<Mutex<Vec<String>>>,
        calls: Arc<AtomicU32>,
    }
    impl JobRunner for EchoRunner {
        fn run(&self, body: &[u8]) -> Result<String, JobRunError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let id: serde_json::Value = serde_json::from_slice(body)
                .ok()
                .and_then(|v: serde_json::Value| v.get("job_id").cloned())
                .unwrap_or(serde_json::Value::String("x".into()));
            let id = id.as_str().unwrap_or("x").to_string();
            self.order.lock().unwrap().push(id.clone());
            Ok(format!(
                r#"{{"status":"accepted","job_id":{}}}"#,
                serde_json::json!(id)
            ))
        }
    }

    /// Runner that blocks until `release` becomes true. Executes nothing else;
    /// used to occupy a slot from a background thread.
    fn gate_runner(release: Arc<Mutex<bool>>) -> Arc<dyn JobRunner> {
        Arc::new(GateRunner { release })
    }

    struct GateRunner {
        release: Arc<Mutex<bool>>,
    }
    impl JobRunner for GateRunner {
        fn run(&self, _body: &[u8]) -> Result<String, JobRunError> {
            loop {
                if *self.release.lock().unwrap() {
                    break;
                }
                std::thread::sleep(Duration::from_millis(5));
            }
            Ok(r#"{"status":"accepted","job_id":"x"}"#.into())
        }
    }

    /// Occupy the single slot on a background thread: the enqueue fast path
    /// runs synchronously there and blocks inside the gate runner.
    fn hold_slot(q: &Arc<JobQueue>, job_id: &str) -> thread::JoinHandle<()> {
        let q = q.clone();
        let body = format!(r#"{{"job_id":"{job_id}"}}"#);
        let job_id = job_id.to_string();
        thread::spawn(move || {
            let _ = q.enqueue(&job_id, 0, body.as_bytes()).unwrap();
        })
    }

    /// Wait (up to `ms`) for a condition, panicking if it never holds.
    fn wait_until(ms: u64, cond: impl Fn() -> bool) {
        let deadline = std::time::Instant::now() + Duration::from_millis(ms);
        while std::time::Instant::now() < deadline {
            if cond() {
                return;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        panic!("condition not met within {ms}ms");
    }

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("vtq-{tag}"));
        let _ = fs::remove_dir_all(&dir);
        dir
    }

    #[test]
    fn valid_job_id_rejects_traversal_and_empty() {
        assert!(valid_job_id("abc-123_.~"));
        assert!(!valid_job_id(""));
        assert!(!valid_job_id("../etc/passwd"));
        assert!(!valid_job_id("a/b"));
        assert!(!valid_job_id("a b"));
        assert!(!valid_job_id("a\u{0}b"));
        assert!(!valid_job_id(&"a".repeat(MAX_JOB_ID_LEN + 1)));
    }

    #[test]
    fn enqueue_runs_now_when_slot_free() {
        let (runner, order, calls) = order_runner();
        let dir = temp_dir("fast");
        let q = JobQueue::new(&dir, runner, 1, 16);
        let out = q.enqueue("j1", 0, br#"{"job_id":"j1"}"#).unwrap();
        assert!(matches!(out, EnqueueOutcome::RanNow(_)));
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        assert_eq!(*order.lock().unwrap(), vec!["j1"]);
        // Fast path records running→done on disk.
        assert!(dir.join("j1.json.done").exists());
        assert!(!dir.join("j1.json.running").exists());
        let status = q.status("j1").unwrap().unwrap();
        assert_eq!(status["status"], "completed");
        assert_eq!(status["result"]["job_id"], "j1");
    }

    #[test]
    fn busy_node_queues_and_drains_in_order() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("busy");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        let holder = hold_slot(&q, "first");
        wait_until(2000, || dir.join("first.json.running").exists());
        assert_eq!(q.status("first").unwrap().unwrap()["status"], "running");

        // Second job cannot run — it queues.
        let out = q.enqueue("second", 0, br#"{"job_id":"second"}"#).unwrap();
        match out {
            EnqueueOutcome::Queued { position } => assert_eq!(position, 1),
            _ => panic!("expected queued"),
        }
        assert_eq!(q.position("second"), Some(1));
        assert_eq!(q.status("second").unwrap().unwrap()["status"], "queued");

        q.spawn();
        *release.lock().unwrap() = true;
        wait_until(2000, || dir.join("second.json.done").exists());
        assert_eq!(q.status("second").unwrap().unwrap()["status"], "completed");
        holder.join().unwrap();
    }

    #[test]
    fn priority_then_fifo_orders_draining() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("prio");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        let holder = hold_slot(&q, "held");
        wait_until(2000, || dir.join("held.json.running").exists());

        // Busy: everything below queues. Submitted low first, then higher —
        // the drainer must run high, then high2 (same tier, FIFO), then mid, low.
        q.enqueue("low", 1, br#"{"job_id":"low"}"#).unwrap();
        q.enqueue("mid", 5, br#"{"job_id":"mid"}"#).unwrap();
        q.enqueue("high", 9, br#"{"job_id":"high"}"#).unwrap();
        q.enqueue("high2", 9, br#"{"job_id":"high2"}"#).unwrap();

        // Ready queue is sorted priority-desc, seq-asc.
        let inner = q.inner.lock().unwrap();
        let ready: Vec<&str> = inner.queued.iter().map(|j| j.job_id.as_str()).collect();
        assert_eq!(ready, vec!["high", "high2", "mid", "low"]);
        drop(inner);

        // The drainer pops from the head, so execution order follows `ready`.
        q.spawn();
        *release.lock().unwrap() = true;
        for id in ["high", "high2", "mid", "low"] {
            wait_until(2000, || {
                q.status(id)
                    .unwrap()
                    .is_some_and(|s| s["status"] == "completed")
            });
        }
        holder.join().unwrap();
    }

    #[test]
    fn reconfigure_raises_concurrency_for_the_drainer() {
        // max_concurrent 1, one slot held by a gate runner.
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("reconfig");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        let holder = hold_slot(&q, "first");
        wait_until(2000, || dir.join("first.json.running").exists());

        // Second job must queue while only one slot exists.
        match q.enqueue("second", 0, br#"{"job_id":"second"}"#).unwrap() {
            EnqueueOutcome::Queued { position: 1 } => {}
            other => panic!("expected queued at 1, got {other:?}"),
        }

        // Widen to 2 slots while the first still runs: the drainer may now
        // start the wait job before the gate opens.
        q.reconfigure(2, 16);
        assert_eq!(q.max_concurrent(), 2);
        q.spawn();
        wait_until(2000, || dir.join("second.json.running").exists());
        assert_eq!(q.status("first").unwrap().unwrap()["status"], "running");
        assert_eq!(q.status("second").unwrap().unwrap()["status"], "running");

        *release.lock().unwrap() = true;
        wait_until(2000, || dir.join("second.json.done").exists());
        holder.join().unwrap();
    }

    #[test]
    fn reconfigure_never_drops_below_running_count() {
        // Two slots, both busy. A shrink to "1 slot" must clamp to the 2 in
        // flight — a running job is never told to stop.
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("reconfig-clamp");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 2, 16);
        let a = hold_slot(&q, "a");
        let b = hold_slot(&q, "b");
        wait_until(2000, || dir.join("a.json.running").exists());
        wait_until(2000, || dir.join("b.json.running").exists());

        q.reconfigure(1, 16);
        assert_eq!(q.max_concurrent(), 2, "must clamp to the running jobs");
        assert_eq!(q.max_queue_len(), 16, "unrelated cap is untouched");

        *release.lock().unwrap() = true;
        wait_until(2000, || dir.join("a.json.done").exists());
        wait_until(2000, || dir.join("b.json.done").exists());
        a.join().unwrap();
        b.join().unwrap();
    }

    #[test]
    fn reconfigure_tightens_the_wait_queue_cap() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("reconfig-full");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        let holder = hold_slot(&q, "held");
        wait_until(2000, || dir.join("held.json.running").exists());

        q.reconfigure(1, 2);
        assert_eq!(q.max_queue_len(), 2);
        q.enqueue("a", 0, br#"{"job_id":"a"}"#).unwrap();
        q.enqueue("b", 0, br#"{"job_id":"b"}"#).unwrap();
        let err = q.enqueue("c", 0, br#"{"job_id":"c"}"#).unwrap_err();
        assert_eq!(err.status, 503);
        assert!(err.message.contains("queue full"));

        *release.lock().unwrap() = true;
        holder.join().unwrap();
    }

    #[test]
    fn queue_full_returns_server_error() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("full");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 2);
        let holder = hold_slot(&q, "held");
        wait_until(2000, || dir.join("held.json.running").exists());
        q.enqueue("a", 0, br#"{"job_id":"a"}"#).unwrap();
        q.enqueue("b", 0, br#"{"job_id":"b"}"#).unwrap();
        let err = q.enqueue("c", 0, br#"{"job_id":"c"}"#).unwrap_err();
        assert_eq!(err.status, 503);
        assert!(err.message.contains("queue full"));
        *release.lock().unwrap() = true;
        holder.join().unwrap();
    }

    #[test]
    fn cancel_only_works_on_queued_jobs() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("cancel");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        let holder = hold_slot(&q, "held");
        wait_until(2000, || dir.join("held.json.running").exists());
        q.enqueue("waiter", 0, br#"{"job_id":"waiter"}"#).unwrap();

        // Cancelling a running job is refused.
        assert_eq!(
            q.cancel("held").unwrap().unwrap_err(),
            "job already past queued"
        );

        // Cancelling the queued job works.
        assert!(q.cancel("waiter").unwrap().is_ok());
        assert_eq!(q.status("waiter").unwrap().unwrap()["status"], "cancelled");

        // Unknown id is a 404.
        assert_eq!(q.cancel("nowhere"), Err(QueueLookupError::NoSuchJob));
        *release.lock().unwrap() = true;
        holder.join().unwrap();
    }

    #[test]
    fn restart_recovers_running_and_queued_jobs() {
        let release = Arc::new(Mutex::new(false));
        let dir = temp_dir("restart");
        let q = JobQueue::new(&dir, gate_runner(release.clone()), 1, 16);
        // "held" is mid-run (fast path, only a `.running` marker at first).
        let holder = hold_slot(&q, "held");
        wait_until(2000, || dir.join("held.json.running").exists());
        q.enqueue("beta", 2, br#"{"job_id":"beta"}"#).unwrap();
        q.enqueue("alpha", 9, br#"{"job_id":"alpha"}"#).unwrap();
        // Drop q "without going through GC" — scene of the crash.
        drop(q);

        // "After reboot": a fresh queue re-reads the *base* files it can,
        // including the rewound running job, sorted priority-desc then FIFO.
        let (runner2, _order, _calls) = order_runner();
        let q2 = JobQueue::new(&dir, runner2, 1, 16);
        assert_eq!(q2.position("alpha"), Some(1));
        assert_eq!(q2.position("beta"), Some(2));
        assert_eq!(q2.position("held"), Some(3), "running job must be rewound");
        q2.drain_sync(10);
        assert_eq!(q2.status("alpha").unwrap().unwrap()["status"], "completed");
        assert_eq!(q2.status("beta").unwrap().unwrap()["status"], "completed");
        assert_eq!(
            q2.status("held").unwrap().unwrap()["status"],
            "completed",
            "rewound job re-runs on the fresh queue"
        );
        *release.lock().unwrap() = true;
        holder.join().unwrap();
    }

    #[test]
    fn done_file_carries_the_runner_json() {
        let (runner, _order, _calls) = order_runner();
        let dir = temp_dir("donejson");
        let q = JobQueue::new(&dir, runner, 1, 16);
        q.enqueue("j1", 0, br#"{"job_id":"j1"}"#).unwrap();
        let status = q.status("j1").unwrap().unwrap();
        assert_eq!(status["status"], "completed");
        assert_eq!(status["result"]["job_id"], "j1");
        assert_eq!(status["result"]["status"], "accepted");
    }

    #[test]
    fn failed_runner_is_recorded() {
        struct Fail;
        impl JobRunner for Fail {
            fn run(&self, _body: &[u8]) -> Result<String, JobRunError> {
                Err(JobRunError::server("kaput"))
            }
        }
        let dir = temp_dir("fail");
        let q = JobQueue::new(&dir, Arc::new(Fail), 1, 16);
        _ = q.enqueue("j1", 0, br#"{"job_id":"j1"}"#);
        let status = q.status("j1").unwrap().unwrap();
        assert_eq!(status["status"], "failed");
        assert_eq!(status["reason"], "kaput");
        // No running marker left behind.
        assert!(!dir.join("j1.json.running").exists());
    }
}
