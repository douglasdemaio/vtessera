# Vtessera Settlement Service — Design (Module 3, non-TEE first)

Date: 2026-08-14
Status: Approved (pending spec review)
Related: ROADMAP.md §3, BUILD.md §4, crates/settlement, crates/executor, crates/node-api

## Context

Module 2 (marketplace) is shipped: offers sign/verify, the node serves an MCP
endpoint and executes free jobs through the executor, and the offer index runs
standalone. What is missing before the paid path can be honest is **settlement**:
turning signed receipts into the completion fraction `f ∈ [0, 1]` that escrow
splits against.

Today the pieces exist only as a library:

- `vtessera-settlement` already implements window-receipt verification
  (`verify_signed_receipt`, `canonical_bytes`, schema_ver 1) and the `settle()`
  completion-fraction math.
- **Nothing signs a per-job metering receipt.** The node returns `JobMetering`
  in the HTTP response but there is no signed, verifiable record of the work a
  job actually did. Window receipts (`vtesserad`) are signed but are window-
  scoped, not job-scoped.
- `vtessera-node` has no identity key; it takes `node_id` straight from the
  offer.

This iteration (non-TEE first, per ROADMAP §3) adds the signed job receipt, a
settlement service binary, and the JSON spool that is the non-DB persistence.

## Scope

In:

- Job-receipt schema (schema_ver 2) in `vtessera-settlement` — canonical bytes,
  signing, verification, key loading.
- Node-side signing in the `vtessera-node` binary (`--key`, `--state-dir`).
- `vtessera-settle` binary (watch loop + `--once`) writing a JSON spool.
- `JobContract.device_class` so the aggregator picks the right meter.
- Tests, CI stanza, docs.

Out (follow-ups, not this iteration):

- Window-receipt ↔ job-window cross-check.
- GUI spawn args for `vtessera-node --key/--state-dir`.
- Database persistence (JSON spool is the non-TEE first step).
- `devnet-demo` re-linking to the host crates (blocked by the ed25519-dalek 2 /
  solana-sdk 1.18 curve25519 conflict; stays mirrored).
- TEE attestation (SEV-SNP / TDX).

## Architecture

### Component 1 — Job receipt (crates/settlement)

`vtessera-settlement` gains a dependency on `vtessera-executor` (for
`JobMetering`, `Backend`, `DeviceClass`, `ExitStatus`) and on `serde` /
`serde_json` (derive on the new types; JSON is the spool wire format).

```rust
pub struct JobReceipt {
    pub schema_ver: u16,               // always 2 in this schema
    pub node_id: String,               // self-attesting node identity
    pub payout_id: String,             // seller payout wallet (offer body)
    pub metering: JobMetering,         // vtessera-executor type
}

pub struct SignedJobReceipt {
    pub receipt: JobReceipt,
    pub pubkey: [u8; 32],
    pub sig: [u8; 64],
}
```

`job_receipt_canonical_bytes(&JobReceipt) -> Vec<u8>` — deterministic little-
endian layout:

```
schema_ver       : u16
node_id_len      : u16 + node_id bytes
payout_id_len    : u16 + payout_id bytes
metering.job_id_len : u16 + job_id bytes
metering.backend : u8  (stable table, below)
device kind      : u8  (stable table, below) + payload
cpu_seconds      : f64
peak_mem_kb      : u64
gpu_seconds      : f64
vram_gb_hours    : f64
exit tag         : u8  (stable table, below) + payload
elapsed_secs     : u64
```

Stable tag tables (documented in code; additions append, never reorder/remove —
a change here invalidates every existing receipt, and a test locks the tables):

- Backend: `0` NoopCpu, `1` LocalCpu, `2` KataCloudHypervisor,
  `3` CloudHypervisor, `4` QemuVfio.
- Device: `0` Cpu; `1` NvidiaGpu + `u16 len + model`; `2` NvidiaMig +
  `u16 len + parent_model` + `u16 len + profile`; `3` AmdGpu + `u16 len + model`.
- Exit: `0` Completed; `1` Failed + `i32 code`; `2` TimedOut; `3` Cancelled.

Functions:

- `sign_job_receipt(&JobReceipt, &SigningKey) -> SignedJobReceipt`
- `verify_signed_job_receipt(&SignedJobReceipt) -> Result<(), VerifyError>` —
  the same four checks as the window receipt: schema_ver known, pubkey is a
  valid Ed25519 key, `node_id == derive_node_id(pubkey)`, signature verifies
  over the canonical bytes. Reuses the existing `VerifyError` enum.
- `load_node_key(path) -> io::Result<SigningKey>` — port of the *load* half of
  `vtesserad::sign::load_or_generate`: raw 32-byte seed, refuse any key whose
  mode & 0o077 != 0. Keeps the node binary independent of the daemon crate.

### Component 2 — Node-side signing (crates/node-api/bin/vtessera_node.rs)

New required args: `--key <path>` and `--state-dir <dir>`.

- Startup: load the key, derive `node_id`, and exit with a clear error unless
  it equals the offer body's `node_id` (the seller's offer must match the
  signing identity — otherwise receipts would be signed by a different node
  than the one advertised).
- `ExecutorRunner` gains the signing key, `node_id`, `payout_id` (from the
  offer body), and the job-receipts directory. After every `executor.run()`
  that returns `JobMetering`, it signs a `JobReceipt` and writes
  `<state-dir>/job-receipts/<job_id>.json` (serde_json).
- Receipts are written for every outcome that produced metering — Completed,
  Failed, and TimedOut. Admission rejects never reach the signer.
- `scripts/x402-demo.sh` is updated to pass `--key` and `--state-dir`.

### Component 3 — vtessera-settle binary (crates/settlement/src/bin)

New bin `vtessera-settle`. No sockets, so no `serve` feature gating.

Args:

- `--state-dir <dir>` (required; job receipts, contracts, and settlements all
  live under it).
- `--interval <secs>` (default 60; ignored with `--once`).
- `--once` (single sweep, exit; used by CI and tests).

Layout under `--state-dir`:

- `contracts/<job_id>.json` — `JobContract` (serde).
- `job-receipts/<job_id>.json` — written by `vtessera-node`.
- `settlements/<job_id>.json` — written by this binary.

Sweep logic, per `JobContract`:

1. Skip if `settlements/<job_id>.json` already exists (idempotent).
2. Read that job's signed receipts from `job-receipts/`.
3. **Missing receipts** → not ready yet; retry next sweep (this is transient).
4. **Verify** every receipt (`verify_signed_job_receipt`). Any failure is
   permanent: log loudly, never settle, leave the job stuck until operator
   intervention. No partial credit.
5. Check each receipt's `node_id == contract.node_id` (NodeMismatch).
6. Aggregate `device_seconds` by summing the right meter: `cpu_seconds` when
   `contract.device_class == Cpu`, else `gpu_seconds` (downgrade guard: a GPU
   contract settled against CPU-only receipts must not credit GPU seconds).
7. `settle(&contract, &usage)` → write
   `settlements/<job_id>.json` containing: `job_id`, `node_id`, `device_class`,
   `device_seconds`, `agreed_device_seconds`, `receipt_count`,
   `completion_fraction`, `milestone_reached`.

### Data flow

```
node runs job → sign → job-receipts/<job_id>.json
vtessera-settle (polls) → verify sigs → aggregate device-seconds → f
                    → settlements/<job_id>.json  └──► (Module 4 finalize reads f)
```

## Public API changes

- `vtessera-settlement`: new `JobReceipt`, `SignedJobReceipt`,
  `job_receipt_canonical_bytes`, `sign_job_receipt`,
  `verify_signed_job_receipt`, `load_node_key`. `JobContract` gains
  `device_class: DeviceClass`.
- `vtessera-node` (bin): new required `--key` and `--state-dir` args.

## Error handling

- Missing receipts: transient, retry next sweep.
- Verification failure (tamper, node_id spoof, bad schema): permanent; log
  loudly, never settle, require operator intervention.
- Key mismatch at node startup (offer node_id ≠ derived node_id): refuse to
  start.
- `vtessera-settle` bad args: usage message, exit 2 (matching sibling
  binaries).

## Testing

- Unit (settlement crate): canonical-bytes determinism; tag-table stability
  regression; sign/verify roundtrip; tamper rejection; node-id spoof;
  unsupported schema; serde JSON roundtrip.
- Integration (`vtessera-settle`): temp state dir → generate key → write
  contract + receipts → `--once` → assert settlement JSON values; poison case
  (tampered receipt) left unsettled; idempotency (second run skips).
- E2E: extend the demo flow to run `vtessera-node --key/--state-dir`, submit a
  free job over HTTP, run `vtessera-settle --once`, print `f`. Manual script
  (matches `x402-demo.sh` style).
- CI gates: `cargo fmt --check`, per-crate `cargo clippy --all-targets --
  -D warnings`, `cargo test --locked` (workspace plus settlement). `vtessera-settle`
  tests run under the settlement crate's `cargo test`.

## Docs

- BUILD.md §4: add the job-receipt schema (schema_ver 2) spec next to the
  window-receipt spec.
- README.md: settlement service section; node `--key`/`--state-dir`.
- ROADMAP.md §3: mark the non-TEE settlement service done once landed.
- DESIGN.md: settlement service data flow.

## Milestone fit

This is the M3 "settlement computing the completion fraction f" item. It is a
prerequisite for the honest paid path (Module 4 / the payment-proof verifier):
`f` is what `finalize_pro_rata` splits against.
