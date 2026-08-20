# Implementation plan — Module 2 lifecycle (contract creation + x402 verification)

**Date:** 2026-08-20
**Spec:** `docs/superpowers/specs/2026-08-20-module2-lifecycle-design.md`
**Branch:** `module1-ch-cpu`

## Overview

Wire the job lifecycle end-to-end: every accepted job (free or paid) creates a
`JobContract` on disk before execution, and paid jobs verify x402 payment
proofs via off-chain Solana RPC. The node-api stays library-only; the real
verifier lives in the binary.

## Phases

### Phase 1: Contract creation helpers (`crates/settlement`)

**Files:** `crates/settlement/src/lib.rs`

1. Add `pub fn create_contract(job_id, node_id, device_class, agreed_device_seconds) -> JobContract`
   - Validates `job_id` is non-empty (panic on empty — caller must check)
   - Sets `milestones: Vec::new()` (v1: single final settlement)
2. Add `pub fn write_contract(contract: &JobContract, state_dir: &Path) -> io::Result<()>`
   - Creates `<state_dir>/contracts/` if missing
   - Writes `<state_dir>/contracts/<job_id>.json` (pretty-printed JSON)
3. Unit tests:
   - `create_contract` produces correct fields
   - `write_contract` creates file on disk
   - Round-trip: write → read → deserialize → compare
   - Empty job_id panics

### Phase 2: PaymentVerifier trait (`crates/node-api`)

**Files:** `crates/node-api/src/lib.rs`

1. Add `PaymentVerifyError` enum:
   - `MalformedProof(String)`
   - `TransactionNotFound(String)`
   - `EscrowMismatch { expected, found }`
   - `InsufficientAmount { expected, found }`
   - `JobIdMismatch { expected, found }`
   - `RpcUnavailable(String)`
2. Add `PaymentVerifier` trait:
   - `fn verify(&self, proof: &str, escrow_account: &str, network: &str) -> Result<(String, u64), PaymentVerifyError>`
3. Add to `NodeState`:
   - `verifier: Option<Arc<dyn PaymentVerifier>>`
   - `state_dir: Option<PathBuf>`
4. Update test helpers to include new fields (set to `None`)
5. Unit tests:
   - Mock verifier that accepts a known proof
   - Mock verifier that rejects with each error variant

### Phase 3: Dispatch wiring — free path

**Files:** `crates/node-api/src/lib.rs`

1. Extract contract creation into a shared helper:
   ```rust
   fn accept_job(state: &NodeState, spec: &JobSpec) -> Option<HttpResponse>
   ```
   - Creates `JobContract` via `create_contract`
   - Writes to disk via `write_contract` (log error, don't fail)
   - Returns `None` on success, `Some(error_response)` on bad spec
2. Update `RunFree` branch:
   - Parse body as `JobSpec`
   - Call `accept_job` → if error, return it
   - Run through executor
3. Unit tests:
   - `RunFree` with valid spec → contract written, executor called, 200
   - `RunFree` with bad spec JSON → 400, no contract
   - `RunFree` with no runner → 501, contract still written

### Phase 4: Dispatch wiring — paid path

**Files:** `crates/node-api/src/lib.rs`

1. Replace the `VerifyAndRun` 501 stub:
   - Check `state.verifier` — if `None`, return 501
   - Parse body as `JobSpec`
   - Call `verifier.verify(proof, escrow_account, network)`
   - On `MalformedProof` or `JobIdMismatch` → 400
   - On other errors → 402 (re-challenge)
   - On success → call `accept_job`, run through executor
2. Unit tests:
   - Valid proof → contract written, executor called, 200
   - Invalid proof → 402, no contract
   - No verifier → 501
   - Job-id mismatch → 400
   - Malformed proof → 400

### Phase 5: Binary wiring (`crates/node-api`)

**Files:** `crates/node-api/src/bin/vtessera_node.rs`, `crates/node-api/Cargo.toml`

1. Add `--rpc-url` CLI flag (default: `https://api.devnet.solana.com`)
2. Add `solana-client` and `solana-sdk` deps to Cargo.toml behind `serve` feature
3. Implement `SolanaPaymentVerifier`:
   - Deserialize proof JSON: `{ "scheme", "job_id", "tx", "amount_micros", "mint", "network" }`
   - `rpc.get_transaction(signature, UiTransactionEncoding::JsonParsed)`
   - Confirm `Finalized` commitment
   - Check `account_keys` includes escrow account
   - Check token transfer amount ≥ offer price
   - Cross-check `job_id` from proof matches request
4. Construct verifier + pass into `NodeState`
5. Pass `state_dir` into `NodeState` (already available via `--state-dir`)
6. Integration test: x402-client binary end-to-end on devnet (should get 200 now)

### Phase 6: MCP + ROADMAP

**Files:** `crates/node-api/src/mcp.rs`, `ROADMAP.md`

1. Update MCP `submit_job` tool to route through `accept_job` for free jobs
   - Paid MCP submissions still get 501 (no verifier in MCP binary)
   - Free MCP submissions now create contracts and execute
2. Update `mcp_manifest` description to remove "not yet wired" language
3. Update ROADMAP.md:
   - §2b: mark x402 challenge + payment verification as shipped
   - §2c: mark job contract + lifecycle as shipped

### Phase 7: Tests + clippy

**Files:** various

1. Run `cargo clippy --all-targets --all-features`
2. Run `cargo fmt --check`
3. Run full test suite: `cargo test --workspace`
4. Verify x402-client binary exercises paid flow on devnet

## File change summary

| File | Lines (est.) | Phase |
|------|-------------|-------|
| `crates/settlement/src/lib.rs` | +40 | 1 |
| `crates/node-api/src/lib.rs` | +120 | 2, 3, 4 |
| `crates/node-api/src/bin/vtessera_node.rs` | +80 | 5 |
| `crates/node-api/Cargo.toml` | +10 | 5 |
| `crates/node-api/src/mcp.rs` | +15 | 6 |
| `ROADMAP.md` | ~10 | 6 |

**Total estimate:** ~275 lines across 6 files.
