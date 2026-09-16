# Job-Queue Plan — Save 3 (after Phase 6, Phase 7 CI green)

## Objective
Implement PRD §11 "No queuing / scheduling" as a **node-local job queue + drainer** per `docs/design/specs/2026-09-10-job-queue-design.md` / `-plan.md` (commits `89ec802`/`fe5610c`). Phases 1–7 done: all committed on `feat/job-queue`; only the PR remains.

## Committed on feat/job-queue (off main `6345cf7` + 2 doc commits)
- `0ff6ae7` — Phase 1 engine `crates/node-api/src/queue.rs` (9+ tests)
- `fd7ec89` — Phase 2: wire POST /jobs → 200 fast / 202 queued / 503 full + priority validation
- `f110b14` — Phase 3: GET /jobs/<id>/status + DELETE /jobs/<id> (cancel queued only)
- `a1236fe` — Phase 4: daemon `--max-concurrent-jobs`/`--max-queue-len`, drainer spawn + **fix: run_job persists base .json before running** (crash-mid-run durability)
- `51e9830` — Phase 5: MCP (local + marketplace) + agent-cli surface the 202 queued outcome; `isError` = not (200 || 202); `x402_challenge_and_outcomes_are_classified` asserts 202 Ok
- `a2c0ee1` — Phase 6: GUI — Settings "Max concurrent jobs" (spin 1..=1024, default 1, validated ≥ 1), start_node passes `--max-concurrent-jobs`; Jobs table merges `job-receipts/` + `job-queue/` (rank cancelled>done>running>queued, queue position + prio, colored dot), 7-col header, CSS classes status-running/queued/failed/cancelled; unit tests for scan_jobs_from

## Design decisions (locked)
- Node-local queue; sync 200 fast path / 202 + `status_url` + position / 503 `queue_full`; max_concurrent default 1; max_queue_len default 16 (bound, not quota); order = priority desc (0–9, default 0) then FIFO (seq asc); disk-backed `<state>/job-queue/<id>.json`; status from markers on disk; `DELETE` cancels queued only (running/finished → 409); no preemption; `None` queue = old behavior.
- Fast path runs inline in the request thread AND persists base + running→done markers (uniform status surface). **Durability fix**: base `persist_queued` happens inside `run_job` now, so a rewound running job re-runs after cold restart (was: lost + stale `.running` forever).
- Claim gate (gated mode) runs at submission BEFORE queue admission (`gated_job_claim_runs_before_queue_admission`).
- `JobSpec.priority` (`#[serde(default)]`, executor/lib.rs); invalid job_id → 400; priority>9 → 400.
- Engine std-only (no new deps → no Flatpak cargo-sources regen).

## Verification status (all green — Phase 7 run)
- `cargo fmt --check` clean; `cargo clippy --all-targets -- -D warnings` clean (whole workspace).
- `cargo test --locked` all pass: node-api lib 42, serve 80, agent-cli 5, gui 25, executor 12, rest 0/1 ignored.
- `cargo deny check` ok (advisories/bans/licenses/sources). (`cargo audit` not installed locally.)
- **Live end-to-end smoke** (`/tmp/opencode/queue-smoke.sh`, node on 127.0.0.1:8402, local-cpu, `gen_offer` free): j1 holds slot (202s-fast rule: first job runs 200 inline so holder submitted backgrounded), j2/j3→202 rue, j4→503, GET status queued@1, DELETE j2→204→cancelled→re-cancel 409, kill -9 mid-run + restart same state-dir → j1 rewound & re-runs, j3 recovered queued, cancel-after-restart works, j5 drains→completed, unknown ids 404. PASS.
- Smoke script lives at `/tmp/opencode/queue-smoke.sh` (recreatable; not committed — queue-demo.sh in scripts/ is the unrelated P1.7 coordinator demo).

## Incident (recovered)
- Reordering `fn main()` before the tests module with a Python one-liner wrote only the file suffix back, destroying Phase 6 `crates/vtessera-gui/src/main.rs` (was uncommitted). Rebuilt from HEAD `main.rs` + re-applied Phase 6 changes (verified: fmt/clippy/tests identical). Lesson: use a proper `git add -p`/Edit-tool reorder, not in-place suffix slicing on uncommitted files.

## Next Move
- Phase 7 CI is done; the remaining work is the PR: push `feat/job-queue`, open PR against `main`, link the two design doc commits (`89ec802`, `fe5610c`) and the six phase commits.

## Relevant files
- `crates/node-api/src/queue.rs` — engine: `JobQueue::new` (rescan+sort+next_seq), `enqueue` (fast-path slot reservation), `run_job` (persist+markers), `spawn`/`drain_sync`, `status`/`cancel`, `persist_queued`, `valid_job_id`, `EnqueueOutcome`, `QueueLookupError`; `mod tests` covers order, priority, cancel, restart (rewound running job at position 3).
- `crates/node-api/src/lib.rs` — `NodeState.queue` (:255), `HttpMethod::Delete`, `dispatch` (status/cancel prefix routes), `execute_or_queue` (serve-gated), `spec_priority`, `handle_job_status`/`handle_job_cancel` (501 no-queue), `mod queue_admission` tests (serve-gated).
- `crates/node-api/src/bin/vtessera_node.rs` — CLI args `--max-concurrent-jobs`/`--max-queue-len` (defaults 1/16, concurrent clamped ≥1), queue build + drainer thread (:1251), MiniMethod::Delete mapping, QUIC parser DELETE.
- `crates/node-api/src/mcp.rs` — Phase 5: local `McpServer` (NodeState queue → 202 live) + `MarketplaceMcpServer::tool_submit_job`; `mod gate` test `gated_submit_job_on_busy_node_returns_202_queued`.
- `crates/agent-cli/src/main.rs` — `render_submit_outcome` 202 branch (~:544); `x402_challenge_and_outcomes_are_classified` asserts 202.
- `crates/executor/src/lib.rs` — `JobSpec.priority`.
- `crates/vtessera-gui/src/{settings.rs,daemon.rs,main.rs}` — Phase 6: `DEFAULT_MAX_CONCURRENT_JOBS`, `max_concurrent_jobs` field + validation, `StartOptions.max_concurrent_jobs` → `--max-concurrent-jobs`; `scan_jobs_from` + `JobsRow` + `read_receipt_metering` + `tests` module.

## Misc notes
- Extremely careful: get `rg`/grep before editing. Avoid sleep in unit tests >~2s. The smoke first proved "fast path is synchronous" (first job's curl blocks; holder must be backgrounded).
- PR #100 (docs, branch `docs/org-and-marketplace-ui-spec`) still open; PR #101 (marketplace UI) merged.