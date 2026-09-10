# Vtessera Job Queue (node-local) — Implementation Plan

**Spec:** `docs/design/specs/2026-09-10-job-queue-design.md`
**Date:** 2026-09-10

## Phase 1: `JobQueue` engine in `crates/node-api`

New module (`src/queue.rs`) containing the queue engine. Pure, synchronous,
unit-testable without a binary.

**Changes:**
- `pub struct QueuedJob { seq: u64, job_id: String, priority: u8, body: Vec<u8>, created_unix: u64 }`
- `pub struct JobQueue { state_dir: PathBuf, runner: Arc<dyn JobRunner>, max_concurrent: usize, max_queue_len: usize, inner: Mutex<QueueInner> }`
- `pub struct QueueInner { queued: Vec<QueuedJob>, running: u32, next_seq: u64 }`
- `pub fn JobQueue::new(state_dir, runner, max_concurrent, max_queue_len) -> Arc<JobQueue>` — creates `<state>/job-queue/`, re-scans existing `<job_id>.json` into `queued` sorted `(priority desc, seq asc)`, rewinds leftover `.running` markers to `running=0`.
- `pub fn JobQueue::enqueue(&self, body: &[u8], priority: u8) -> Result<EnqueueOutcome, JobRunError>`:
  - `EnqueueOutcome::RanNow(String)` — slot free, ran via `runner.run(body)` synchronously; records running→done (see below); returns the runner's exact 200 JSON.
  - `EnqueueOutcome::Queued { position }` — appended + persisted; position is 1-based.
  - `QueueFull` → `Err(JobRunError::server("queue full"))` (503).
- `pub fn JobQueue::status(&self, job_id: &str) -> Result<JobStatusJson, String>` and `pub fn JobQueue::cancel(&self, job_id: &str) -> Result<JobOutcome, JobRunError>`.
- States on disk under `<state>/job-queue/`:
  - `<job_id>.json` — queued job (full `QueuedJob` minus cached position).
  - `<job_id>.json.running` — marker for a running/finished job's slot.
  - `<job_id>.json.done` — final result (the runner's 200 JSON, or a failure envelope `{"status":"failed","reason":...}`).
  - `<job_id>.json.cancelled` — cancel marker.
  - All writes are `write` a temp name + `rename`.
- `JobQueue::spawn(&self)` — the drainer loop (single thread): lock → pop head → `running += 1` → unlock → write `.running` → `runner.run(body)` → write `.done` → remove `.running` → `running -= 1` → loop; sleeps briefly when queue empty (`e.g. 250ms`).
- `run_queued_sync(job, runner)` shared by `enqueue`'s fast path and the drainer — one code path for "execute a job + write running/done markers".
- `job_id` path-safety validation: `[A-Za-z0-9._-]`, non-empty, length cap (`<= 128`); else 400 before any write.
- `priority` clamped/validated 0–9 (10+ → 400).

**Verify:** `cargo test -p vtessera-node-api` — new unit tests:
- ordering: priority desc then seq asc (mixed priorities)
- FIFO within a tier
- concurrency cap honored (runner called at most `max_concurrent` at once — fake runner that sleeps/records)
- 202 vs 200 admission path
- queue-full → 503
- cancel queued → 204-marker, cancel running/completed → 409, unknown → 404
- path-traversal job_id → 400
- restart recovery: pre-seed queue dir, `new()` re-reads order + rewinds running
- `done` file content == runner's JSON

## Phase 2: Wire `POST /jobs` admission

**Changes** (`crates/node-api/src/lib.rs`):
- `NodeState` gains `pub queue: Option<Arc<JobQueue>>`.
- `handle_jobs` / `run_free` / `handle_paid_job`: after the existing parse (and payment verify for paid) and `create_and_write_contract`, branch on `state.queue`:
  - `None` → existing synchronous path unchanged (byte-for-byte).
  - `Some(q)` → parse `priority` from the spec (`#[serde(default)] priority: u8` on `vtessera_executor::JobSpec`), validate 0–9, call `q.enqueue(body, priority)`:
    - `RanNow(json)` → 200 with `json` (identical to today).
    - `Queued { position }` → 202 body `{"status":"queued","job_id":...,"position":k,"status_url":...,"priority":..}`.
    - `Err` → 503 queue-full with the x402 challenge re-attached on the paid path.
- FREE jobs: `check_claim_gate` runs at submission (unchanged position), NOT in the drainer.

**Verify:** all existing node-api `/jobs` tests pass with `queue: None`; new tests with `queue: Some` cover 200/202/503/priority-validation and claim-gate-at-submission.

## Phase 3: `GET /jobs/<id>/status` + `DELETE /jobs/<id>`

**Changes** (`crates/node-api/src/lib.rs`):
- `dispatch`: add
  - `(HttpMethod::Get, "/jobs/...")` → `handle_job_status` (parse id, delegate to queue; 404 if none)
  - `(HttpMethod::Delete, "/jobs/...")` → `handle_job_delete` (204/409/404, 501 if no queue)
  - Path parsing: strip the `/jobs/` prefix and require a single segment (no `/`); the queue validates the job_id charset.
- `JobStatusJson` cases from the spec §4.5 (queued/running/completed/failed/cancelled/404) — reads the disk files.

**Verify:** endpoint tests — status per state, delete queued 204, delete running/completed 409, unknown 404, no-queue 501 for both routes.

## Phase 4: Node binary wiring + arg

**Changes** (`crates/node-api/src/bin/vtessera_node.rs`):
- New `--max-concurrent-jobs <n>` (default 1) and `--max-queue-len <n>` (default 16) args.
- After constructing `runner` + `state_dir`: `if let Some(dir) = &state_dir { let q = JobQueue::new(dir.join("job-queue"), runner.clone(), max_concurrent, max_queue_len); q.spawn(); state.queue = Some(q); }`.
- Update `--help` text and the `wants_inbound_listener`/connectivity bootstrap flow so the drainer thread outlives the request handlers (spawn before `TcpListener::bind`; detach — the process owns it).

**Verify:** `cargo build -p vtessera-node-api --features serve`; node boots with queue configured; a free smoke job still returns 200.

## Phase 5: Agent/MCP surface

**Changes:**
- `crates/node-api/src/mcp.rs` `tool_submit_job`: detect the **202** response (currently the helper parses a 200 or 402) — return a structured result `{"status":"queued","job_id":...,"status_url":...}` instead of erroring. 200 unchanged; 402 unchanged (already handled).
- `crates/agent-cli/src/main.rs` `submit`: on a 202, print the queued status line instead of assuming a 200-with-metering body (e.g. `job <id> queued — poll /jobs/<id>/status`).

**Verify:** MCP unit tests for the 202 path; agent-cli `submit` against a busy node prints the queued line and exits 0.

## Phase 6: GUI — Jobs tab + Settings row

**Changes** (`crates/vtessera-gui/src/main.rs`):
- Extend `refresh_jobs_table` to also read `<state>/job-queue/`:
  - `queued` → "queued · #k · prio p"
  - `running` → "running"
  - `done` (completed) → merge into the existing receipt rows (read the `metering` from the `done` file, same shape as receipts)
  - `failed` → "failed · reason"
  - `cancelled` → "cancelled"
  - Dedupe with `job-receipts/` rows (same job may appear in both).
- Settings tab: "Max concurrent jobs" spin (1–N, default 1), stored in the GUI settings struct and passed as `--max-concurrent-jobs` when `start_node` spawns the daemon.
- The GUI Jobs tab poll already runs on the 2s timer — the new reads piggyback on it.

**Verify:** `cargo build -p vtessera-gui`; manual: queue a 2nd job while one runs → Jobs tab shows queued + position, flips to running/completed. `cargo test -p vtessera-gui --bin vtessera-gui`.

## Phase 7: Tests + CI

**Changes:**
- `JobSpec` priority field test (default 0; 10 → 400 via node-api admission).
- Full CI per repo convention (`docs/ROADMAP.md` / `.github/workflows/ci.yml`):
  - `cargo fmt --check`
  - `cargo clippy --workspace --all-targets -- -D warnings`
  - `cargo test --workspace --locked`
  - `cargo audit` / `cargo deny` (unchanged, still green)
- Flatpak regen NOT needed (no new deps).

**Verify:** All green.

## Dependencies

None new (engine uses `std` + existing `ed25519_dalek`/`serde_json` in node-api; GUI reads files only). No `Cargo.lock` churn → no flatpak source regen.

## Estimated Effort

| Phase | Effort |
|-------|--------|
| 1. Queue engine + unit tests | 3 h |
| 2. POST /jobs admission | 1 h |
| 3. status + cancel endpoints | 1 h |
| 4. Node binary wiring + args | 45 min |
| 5. MCP + agent-cli 202 handling | 45 min |
| 6. GUI Jobs tab + Settings | 1.5 h |
| 7. Tests + CI | 30 min |
| **Total** | **~8.5 h** |