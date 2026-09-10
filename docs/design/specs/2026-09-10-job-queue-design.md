# Job Queue — Node-Local Queuing + Scheduling

**Status:** in-progress design (2026-09-10)
**Supersedes:** nothing
**PRD shortfall:** "No queuing / scheduling — jobs run ad hoc; there is no job
queue, preemption, prioritization, or fair scheduling across a fleet"

This spec ships the **node-local slice** of that shortfall: a durable, bounded,
priority-aware wait queue and drainer on a single Vtessera node. It is the
foundation the later "fleet scheduling" work builds on. Preemption of running
jobs and cross-node fair scheduling are explicitly **out of scope** (deferred
follow-ups).

## 1. Problem

Today `POST /jobs` (and the MCP `submit_job` equivalent) executes the job
**synchronously inside the request handler**:

- free jobs: `run_free` parses the spec, writes a contract, calls
  `runner.run(body)`, returns the metering + signed receipt in the 200 body
  (`crates/node-api/src/lib.rs:472`);
- paid jobs: `handle_paid_job` verifies the x402 proof, writes a contract,
  then calls the same runner inline (`:397`);
- `ExecutorRunner::run` in the node binary blocks the request thread for the
  whole job (`crates/node-api/src/bin/vtessera_node.rs:448`).

Consequences:

1. **No concurrency cap.** Every request thread spawns a backend. An agent
   that fires N parallel paid jobs runs N concurrent workloads regardless of
   the node's advertised capacity.
2. **No wait queue.** A second job while the first is running must be retried
   by the agent — there is no "accepted, will run" state, no ordering, no
   priority.
3. **Synchronous contract.** The agent's only way to get the result is the
   HTTP response. A queue breaks this contract unless we give agents a job
   status/result surface.

## 2. Goals

- A node runs at most `max_concurrent_jobs` at once (default **1**), across
  all agents and device classes.
- Jobs beyond that are admitted into a **durable, bounded, priority-aware
  wait queue** and drained as slots free up.
- The agent is told it was queued (HTTP **202**) with a status URL, and can
  poll status and retrieve the result when the job completes.
- Backwards compatible: when no queue is configured, and when a slot is free,
  behavior is **byte-for-byte identical to today** (synchronous 200 with
  metering + receipt).
- The GUI shows queued/running/finished job state and lets the operator set
  the concurrency cap.

## 3. Non-goals (explicitly deferred)

- **Preemption:** killing a running job, especially a paid one mid-run, risks
  losing money; refund/reconciliation semantics are a separate design.
- **Fleet scheduling / fair-share across nodes:** requires the coordinator /
  index to route; building directly on this spec's node-local engine.
- **Rate limiting / admission quotas** (separate PRD shortfall; `max_queue_len`
  here is a bound, not a per-agent quota).
- **Spool rotation** of completed jobs (separate PRD shortfall).

## 4. Architecture

### 4.1 Crate placement

The queue engine lives in **`crates/node-api`** — the crate that already owns
`NodeState`, `/jobs` dispatch, and the `JobRunner` trait. `NodeState` gains:

```rust
/// Optional job queue + drainer. `None` preserves today's synchronous
/// behavior exactly (all existing tests and consumers unaffected).
pub queue: Option<Arc<JobQueue>>,
```

The **node binary** (`vtessera_node --max-concurrent-jobs N`) constructs the
`JobQueue` and hands it to `NodeState`. This preserves the lib's "transport
and decisioning only, never executes" rule: the drainer calls the injected
`JobRunner` trait (the same trait `handle_jobs` uses today), and the lib links
nothing privileged.

### 4.2 The `JobQueue`

```rust
pub struct JobQueue {
    state_dir: PathBuf,            // <state>/job-queue/
    runner: Arc<dyn JobRunner>,
    max_concurrent: usize,         // default 1
    max_queue_len: usize,          // default 16
    inner: Mutex<QueueInner>,      // ordering + counters
}

struct QueueInner {
    queued: Vec<QueuedJob>,        // ordered (priority desc, seq asc)
    running: u32,                  // slots in use
    next_seq: u64,
}

struct QueuedJob {
    seq: u64,
    job_id: String,
    priority: u8,                  // 0 (default) ..= 9
    body: Vec<u8>,                 // raw POST /jobs body, replayed to runner
    created_unix: u64,
    position: usize,               // cached 1-based queue position
}
```

- **Ordering:** FIFO within a priority tier. Primary key `priority` descending,
  secondary key `seq` ascending. Strictly ordered; low-prio jobs are never
  starved by design intent (bounded 10 levels), and in the common case (all
  priority 0) it is pure FIFO.
- **Disk layout** under `<state>/job-queue/`:
  - `<job_id>.json` — one file per admitted (queued) job. Content is the
    `QueuedJob` minus the cached `position`. Writing is `write` + `rename` so a
    crash never leaves a partial file. `job_id` is validated
    (file-path-safe: `[A-Za-z0-9._-]`) at admission; a path-traversal job_id is
    a 400 before anything is written.
  - On startup the queue **re-scans** the directory and rebuilds `queued`
    (survives node restarts). Files with a `running` marker (see 4.4) are
    resumed; files with a `done` marker are collected for status and then
    pruned per §5.

### 4.3 Admission (`POST /jobs`)

`handle_jobs` behavior changes only when `state.queue` is `Some`:

1. Parse the spec (400 on bad JSON / invalid job_id path).
2. Paid path: verify x402 proof as today (402 / 400 / 503 unchanged).
3. Write the contract as today.
4. Free path: run the claim gate as today — the submit claims the node. A
   free job that is admitted to the queue is **not re-gated in the drainer**:
   it already passed admission, and re-checking at drain time would race
   the lease TTL (the index claim is lease-style, 60s renewable). Concretely,
   the gate runs once at submission (same semantics as today), the drainer
   replays the job regardless of claim state, and a queued free job whose
   index claim lapses (>60s wait) still runs when its slot opens — the
   node-side queue is authoritative for jobs it has admitted. Fleet/claim
   reconciliation is a follow-up design concern, not this spec's.
5. Ask the queue for admission:
   - **Slot free** (`running < max_concurrent`): run synchronously via the
     runner (identical to today). The queue records the job as
     `running → done` around the call (see 4.4) so status/result surface is
     uniform, but the **response is the existing 200 body** (metering +
     receipt) — the fast path is unchanged for both agents and tests.
   - **Slot busy, queue not full:** append `QueuedJob`, persist the file,
     respond **202**:
     ```json
     { "status": "queued", "job_id": "...", "position": 2,
       "status_url": "/jobs/<id>/status", "priority": 0 }
     ```
   - **Queue full:** respond **503** `{"status":"queue_full"}` (with the
     x402 challenge on the paid path re-attached so a retrying agent re-pays
     cleanly). `max_queue_len` defaults to 16.

### 4.4 Drainer

A single worker thread spawned by the binary (`JobQueue::spawn()`) runs a loop:

1. Lock, pop the head `QueuedJob`, increment `running`, unlock. Write a
   `.running` marker file (`<job_id>.json.running`).
2. Replay `body` to `runner.run(&body)`.
3. Write the result: on `Ok(json)` persist `<job_id>.json.done` containing
   the runner's 200 JSON (the same body the fast path returns — metering +
   receipt); on `Err` persist `<job_id>.json.done` with `{"status":"failed",
   "reason": ...}` and the error's status. Write-then-rename for atomicity.
4. Remove the `.running` marker, decrement `running`, loop.

Fast-path synchronous jobs (4.3) take the same `running → done` tour so every
job — sync or queued — ends with a `done` file that the status endpoint and
GUI read uniformly. The `done` file content mirrors the response body so a
status poll returns the complete result, including the signed receipt.

**Why a single drainer thread?** The `Executor` contract is synchronous by
design ("the privileged layer runs a small number of long-running jobs";
`crates/executor/src/lib.rs:269`). One worker serializes dispatch, which is
exactly `max_concurrent = 1`. A later spec can widen `max_concurrent` to N
drainer threads; the engine already counts `running` generically.

### 4.5 Status (`GET /jobs/<id>/status`)

Reads `<state-dir>/job-queue/<job_id>.json*` and synthesizes:

- `queued` — file present, no marker: `{"status":"queued","position":k,
  "priority":p,"created_unix":...}`
- `running` — `.running` marker present: `{"status":"running"}`
- `completed` — `.done` present: `{"status":"completed","result":{...}}`
  (the exact 200 JSON the runner returned, including `metering` +
  `receipt:"signed"`)
- `failed` — `.done` with a failure envelope: `{"status":"failed","reason":...}`
- `cancelled` — `.cancelled` marker present (see §6)
- otherwise **404**.

A completed job's status stays queryable as long as its `done` file exists;
the GUI/CLI and the settlement loop can read the receipt from it directly.
Pruning is §8.

### 4.6 Agent / MCP surface

- `vtessera-node`: new `GET /jobs/<id>/status` and `DELETE /jobs/<id>`
  routes beside `POST /jobs`. The node's `dispatch` gains the routes when a
  `queue` is configured.
- Local MCP (`submit_job`): when it detects a **202** it returns a structured
  result `{ "status":"queued", "job_id":..., "status_url":... }` so the tool
  doesn't crash on a busy node. A `get_job_status` tool is **blocked /
  follow-up** — the agent-facing webhook/poll protocol is a fleet-scheduling
  design decision, not a node-local one. (Explicitly: agents on a node-local
  queue use HTTP status polling via `status_url`; this spec does not add MCP
  status tools.)
- `vtessera-agent submit` (agent CLI): unchanged — it already surfaces the
  200 fast path; on a 202 it should print the queued status line (small CLI
  tweak, §11) instead of failing.

## 5. Priority field

The agent may post an optional `"priority": <u8>` in the job spec body
(`vtessera_executor::JobSpec` gains `#[serde(default)] priority: u8`, 0–9;
values 10+ are a 400 admission rejection). It is **advisory and node-local**:
it only influences ordering on this node's wait queue, is never written into
contracts or receipts, and is ignored by a node with no queue configured.

## 6. Cancel (`DELETE /jobs/<id>`)

- If the job is `queued`: remove its file, write a `.cancelled` marker,
  respond **204**. Releasing a free job's *claim* (if index-backed and this
  job held the only claim) is the **index's** lease TTL job (60s) — the node
  does not hold a long-lived lease; no special handling needed.
- If `running` or already `completed`: **409** with the current status.
  Never preempt.
- Unknown id: **404**.

## 7. Queue-full + backpressure

`max_queue_len` full → 503. This is a **bound, not a quota**: any agent may
consume any fraction of the queue; per-agent admission quotas are a separate
PRD shortfall (§3) and are deliberately not sneaked into this spec.

## 8. Pruning / lifecycle

- `done` files are retained (they carry the settlement-facing receipt) and
  removed only by a **follow-up spool-rotation change** (§3). The queue
  directory content is bounded by `max_queue_len` queued + however many jobs
  have completed since install — same growth profile as today's
  `job-receipts/`, which is acceptable and honesty-documented here.
- `.running` markers from a crashed node: on restart the drainer treats a
  leftover `.running` as **queued-running**: it rewinds the count and lets the
  job be re-run on the next slot (the job's contract already exists, and the
  executor is idempotent at the job_id level; a duplicate receipt would
  overwrite — matching today's restart behavior).

## 9. GUI

`vtessera-gui` Jobs tab (currently reads `job-receipts/`) additionally lists
`<state>/job-queue/` entries so a human sees the live pipeline:

| State | Shown as |
| --- | --- |
| `queued` | "queued · #2 · prio 0" |
| `running` | "running" |
| `completed` | existing receipt row (= done file receipt) |
| `failed` | "failed · <reason>" |
| `cancelled` | "cancelled" |

Settings tab gains a **"Max concurrent jobs"** spin (default 1, range 1–N).
The GUI reads/sets the same value the binary consumes
(`--max-concurrent-jobs`); the Settings row writes it to the shared
settings/state so `start_node` passes it to the daemon.

## 10. Testing

- **Engine unit tests** (node-api, no binary): ordering (priority desc, then
  seq asc), FIFO within a tier, concurrency cap honored, 202 vs 200
  admission, queue-full 503, cancel queued / 409 running / 404 unknown,
  path-traversal job_id rejected, disk restart-recovery (queued files
  re-read; `.running` rewound), `done` file content equality with the 200
  body.
- **Endpoint tests**: existing shape-preservation — with `queue: None` the
  full `/jobs` suite (payment, claim gate, 402 replay) is unchanged and
  green.
- **Priority validation test**: priority 10 → 400; priority absent → defaults
  0.
- **GUI tests**: jobs-table merge renders queued/running rows without
  breaking receipt rows (receipt parsing already covered).
- CI: `cargo fmt --check`, `cargo clippy --workspace --all-targets --
  -D warnings`, `cargo test --workspace --locked` (per repo convention).

## 11. Follow-ups (blocked / later)

- Fleet scheduling (coordinator/index routing on top of this engine).
- Preemption of running jobs + paid-refund reconciliation.
- Per-agent admission quotas / rate limits.
- Spool rotation of `done` + receipt files.
- MCP `get_job_status` tool (waiting on the agent-facing poll/webhook
  protocol decision).
- `vtessera-agent submit` 202 handling (small, but out of the node-local
  core; listed so the stale "sync 200" assumption is documented).

## 12. Summary of decisions

| Decision | Choice |
| --- | --- |
| Scope | Node-local queue + drainer; not fleet scheduling, not preemption |
| Crate | engine + routes in `crates/node-api`; binary owns `JobQueue::spawn()` |
| Admission | sync 200 when slot free; 202+status_url when queued; 503 when full |
| Concurrency | global `max_concurrent_jobs`, default 1 |
| Ordering | priority desc (0–9, default 0) then FIFO (`seq` asc) |
| Persistence | one file per job under `<state>/job-queue/`; restart-safe |
| Bound | `max_queue_len` default 16 (bound, not quota) |
| Cancel | queued-only; running/completed → 409; no preemption |
| Status | from disk; `done` file carries the runner's 200 JSON (incl. receipt) |
| Claim gate | held at submission, not re-checked in drainer |
| GUI | Jobs tab merge + Settings "Max concurrent jobs" |
| Backwards compat | `queue: None` == today exactly |