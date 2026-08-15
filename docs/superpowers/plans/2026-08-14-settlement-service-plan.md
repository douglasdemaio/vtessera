# Vtessera Settlement Service — Implementation Plan

Spec: `docs/superpowers/specs/2026-08-14-settlement-service-design.md`
Branch: `settlement-service` (new, off main)

## Amendment to the spec

"GUI spawn args" moves from out-of-scope to a minimal in-scope pass-through:
`vtessera-node` gains *required* `--key`/`--state-dir`, which would break the
GUI's spawn. The GUI already exposes `settings::key_path()` and
`settings::state_dir()`, so the fix is two new `StartOptions` fields + two
`--arg`s + two call-site values. Richer GUI handling (settle status UI,
offer-index wiring) remains follow-up.

## Phase 1 — Job-receipt schema in vtessera-settlement

1. `crates/settlement/Cargo.toml`: add deps `vtessera-executor` (path),
   `serde` (workspace), `serde_json` (workspace).
2. `crates/settlement/src/lib.rs`:
   - `pub const JOB_RECEIPT_SCHEMA_VER: u16 = 2;`
   - `#[derive(Serialize, Deserialize)] pub struct JobReceipt { schema_ver,
     node_id, payout_id, metering: JobMetering }` and `SignedJobReceipt`.
   - Tag tables (encode-only; verifier recomputes canonical bytes from the
     parsed struct): `backend_tag(&Backend) -> u8`, `device_tag(&DeviceClass)
     -> Vec<u8>` (kind byte + length-prefixed payloads), `exit_tag(&ExitStatus)
     -> Vec<u8>` (kind byte + optional i32 code).
   - `job_receipt_canonical_bytes(&JobReceipt) -> Vec<u8>` per the spec layout.
   - `sign_job_receipt(&JobReceipt, &SigningKey) -> SignedJobReceipt`.
   - `verify_signed_job_receipt(&SignedJobReceipt) -> Result<(), VerifyError>`
     (reuses existing `VerifyError`).
   - `load_node_key(path) -> io::Result<SigningKey>`: raw 32-byte seed;
     refuses mode & 0o077 (unix) and wrong length.
   - `pub fn device_seconds_for(metering: &JobMetering, class: &DeviceClass)
     -> f64` (cpu_seconds for Cpu, else gpu_seconds).
   - `JobContract` gains `pub device_class: DeviceClass`; add serde derives to
     `JobContract` and `Settlement`.
   - Update existing `settle` tests for the new `JobContract.device_class`.
3. Unit tests: canonical determinism; tag-table stability (assert concrete tag
   numbers); sign/verify roundtrip; tamper → SignatureMismatch; node_id spoof →
   NodeIdMismatch; bad schema → UnsupportedSchema; serde JSON roundtrip;
   `load_node_key` (valid, wrong-length, 0644 refusal on unix);
   `device_seconds_for` (cpu vs gpu selection).

Verify: `cargo fmt --check` + `cargo clippy -p vtessera-settlement
--all-targets -- -D warnings` + `cargo test -p vtessera-settlement --locked`.

## Phase 2 — Node signs job receipts

4. `crates/node-api/Cargo.toml`: `serve` feature gains `"dep:vtessera-
   settlement"`; add `vtessera-settlement = { path = "../settlement",
   optional = true }`.
5. `crates/node-api/src/bin/vtessera_node.rs`:
   - New required args `--key <path>` and `--state-dir <dir>`; update
     usage/help.
   - Startup: `load_node_key`, derive node_id, exit(1) unless it equals
     `offer.body.node_id`. Create `<state-dir>/job-receipts`.
   - `ExecutorRunner` gains `signing_key: SigningKey`, `payout_id: String`,
     `node_id: String`, `receipts_dir: PathBuf`. After a successful
     `executor.run()`, sign a `JobReceipt` and write
     `<state-dir>/job-receipts/<job_id>.json`. A persistence failure is a
     server error (500) — no signed proof, no settleable work.
6. `crates/vtessera-gui/src/daemon.rs`: `StartOptions` gains `key_path: String`
   and `state_dir: String`; the node spawn adds `--key`/`--state-dir`.
   `crates/vtessera-gui/src/main.rs`: pass `settings::key_path()` /
   `settings::state_dir()`.
7. `crates/node-api/examples/gen_offer.rs`: optional `--key-out <path>` that
   writes the deterministic 32-byte seed. `scripts/x402-demo.sh`: generate the
   key file, pass `--key`/`--state-dir` to the node, and run `vtessera-settle
   --once` after the job (Phase 3 provides the binary).

Verify: `cargo clippy -p vtessera-node-api --all-targets --features serve --
-D warnings`; `cargo test -p vtessera-node-api --locked --features serve`;
GUI `cargo check -p vtessera-gui` (host, as before).

## Phase 3 — vtessera-settle binary

8. `crates/settlement/Cargo.toml`: `[[bin]]` `vtessera-settle` →
   `src/bin/vtessera_settle.rs` (no feature gating; opens no sockets).
9. `crates/settlement/src/bin/vtessera_settle.rs`:
   - Args: `--state-dir <dir>` (required), `--interval <secs>` (default 60),
     `--once`. Usage + exit 2 on bad args.
   - Sweep: for each `contracts/<job_id>.json` → skip if
     `settlements/<job_id>.json` exists; read that job's receipts; missing =
     transient (retry next sweep); verify each (permanent hard reject on
     failure, log loudly, leave unsettled); node_id == contract.node_id;
     aggregate `device_seconds_for`; `settle()`; write `settlements/<job_id>.
     json` (job_id, node_id, device_class, device_seconds,
     agreed_device_seconds, receipt_count, completion_fraction,
     milestone_reached).
   - `--once` does one sweep and exits; otherwise loop on `--interval`.
10. `crates/settlement/tests/settle_bin.rs` (uses `CARGO_BIN_EXE_vtessera-
    settle` + the lib to sign receipts): temp dir; happy path (contract + two
    receipts → `--once` → correct settlement JSON, idempotent on rerun); poison
    path (tampered receipt → no settlement file written).

Verify: `cargo fmt --check`; `cargo clippy -p vtessera-settlement --all-targets
-- -D warnings`; `cargo test -p vtessera-settlement --locked`.

## Phase 4 — Docs + CI + e2e

11. Docs: BUILD.md §4 job-receipt schema (schema_ver 2); README node
    `--key`/`--state-dir` + settlement service section; ROADMAP §3 status;
    DESIGN.md data flow. Update the spec's out-of-scope line for the GUI
    amendment.
12. CI: confirm `.github/workflows/ci.yml` covers `vtessera-settlement` tests
    and the node-api `serve` build; add stanzas if missing.
13. E2E (manual script, mirrors x402-demo.sh style): node with `--key`/
    `--state-dir` → submit free job via curl → `vtessera-settle --once` →
    printed `f` and `settlements/<job_id>.json`.

Final gates (before PR): full-workspace `cargo fmt --check`, per-crate clippy
`-D warnings`, `cargo test --locked` (workspace), node-api `--features serve`,
GUI check.
