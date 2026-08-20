# Design spec — Module 2 lifecycle: contract creation + x402 verification

**Date:** 2026-08-20
**Status:** Draft
**Supersedes:** (none — first design for this area)

## Motivation

Module 2 (discovery + marketplace) has the offer, MCP, A2A, and offer-index
layers built, but the job lifecycle is incomplete. Two gaps block the end-to-end
flow:

1. **No contract creation.** When a job is submitted (free or paid), the node
   runs it through the executor but never writes a `JobContract`. Settlement
   needs this contract to compute the completion fraction `f` — without it,
   signed receipts have nothing to settle against.

2. **Paid jobs return 501.** The `VerifyAndRun` branch in `node-api` refuses
   payment proofs with "not implemented." The x402-client binary exercises this
   path and documents it as expected v0 behavior, but the paid flow needs to
   actually work.

This spec wires both: the node creates a contract on every accepted job, and
paid jobs verify the x402 payment proof via off-chain Solana RPC before
executing.

## Goals

1. Every accepted job (free or paid) produces a `JobContract` file on disk
   before execution.
2. x402 payment proofs are verified off-chain via Solana RPC before execution.
3. The node-api stays library-only — no Solana client dependency in the lib
   crate.
4. The x402-client binary exercises the full paid flow end-to-end (no more 501).
5. MCP `submit_job` routes through the same lifecycle as HTTP `POST /jobs`.

## Non-goals (this spec)

- On-chain escrow program changes (Module 4) — the existing devnet program
  is sufficient.
- Milestone-based streaming release — v1 uses single final settlement.
- Payment verification in the MCP binary (stdio-only, no HTTP transport).
- Job queuing, scheduling, concurrency control — separate design.

## Architecture

```
Agent POST /jobs { image, command, devices, ... }
    │
    ▼
[classify_job_request]
    │
    ├─ Free offer ──────────────────────────────────────────────┐
    │                                                           │
    ├─ Paid, no x-payment header ──▶ 402 + x402 challenge      │
    │                                                           │
    └─ Paid, x-payment header ─────────────────────────────────┐│
                                                               ││
                                                               ▼│
                                                    [verify_payment]
                                                               │
                                                     ┌─────────┴─────────┐
                                                     │  RPC: getTransaction
                                                     │  Check: confirmed,
                                                     │  escrow match,
                                                     │  amount ≥ price,
                                                     │  job_id cross-check
                                                     └─────────┬─────────┘
                                                               │
                                                               ▼
                                                     [create_contract]
                                                               │
                                                               ▼
                                                     [run through executor]
                                                               │
                                                               ▼
                                                     [sign receipt + write]
                                                               │
                                                               ▼
                                                     200 + metering JSON
```

## Detailed design

### 1. Contract creation (`crates/settlement`)

Add two public functions:

```rust
/// Construct a JobContract from the offer and job request.
/// Panics if `job_id` is empty (caller must validate).
pub fn create_contract(
    job_id: String,
    node_id: String,
    device_class: DeviceClass,
    agreed_device_seconds: u64,
) -> JobContract {
    JobContract {
        job_id,
        node_id,
        device_class,
        agreed_device_seconds,
        milestones: Vec::new(),
    }
}

/// Write a JobContract to <state_dir>/contracts/<job_id>.json.
/// Creates the `contracts/` directory if it doesn't exist.
pub fn write_contract(contract: &JobContract, state_dir: &Path) -> io::Result<()> {
    let dir = state_dir.join("contracts");
    fs::create_dir_all(&dir)?;
    let path = dir.join(format!("{}.json", contract.job_id));
    let json = serde_json::to_string_pretty(contract)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    fs::write(path, json)
}
```

No changes to `JobContract`, `settle()`, `sweep()`, or existing types.

### 2. Payment verification (`crates/node-api`)

Add a trait and error type:

```rust
pub trait PaymentVerifier: Send + Sync {
    /// Verify an x402 payment proof against the chain.
    ///
    /// `proof` — the raw `x-payment` header value (JSON string).
    /// `escrow_account` — the expected escrow PDA (from the 402 challenge).
    /// `network` — Solana network (e.g. "solana-devnet").
    ///
    /// Returns Ok((mint_pubkey, amount_micros)) on success.
    fn verify(
        &self,
        proof: &str,
        escrow_account: &str,
        network: &str,
    ) -> Result<(String, u64), PaymentVerifyError>;
}

pub enum PaymentVerifyError {
    /// Proof JSON is malformed or missing required fields.
    MalformedProof(String),
    /// Transaction not found or not confirmed.
    TransactionNotFound(String),
    /// Transaction doesn't involve the expected escrow account.
    EscrowMismatch { expected: String, found: Vec<String> },
    /// Token transfer amount is less than the offer price.
    InsufficientAmount { expected: u64, found: u64 },
    /// The on-chain job_id doesn't match the submitted job_id.
    JobIdMismatch { expected: String, found: String },
    /// RPC endpoint unreachable.
    RpcUnavailable(String),
}
```

### 3. NodeState changes (`crates/node-api`)

```rust
pub struct NodeState {
    // ... existing fields ...
    /// Optional payment verifier. Some for paid offers with on-chain
    /// verification wired; None means paid jobs return 501 (honest refusal).
    pub verifier: Option<Arc<dyn PaymentVerifier>>,
    /// State directory for contracts and receipts. None means contracts
    /// are not persisted (e.g. standalone mode).
    pub state_dir: Option<PathBuf>,
}
```

### 4. Dispatch changes (`crates/node-api`)

**`handle_jobs` — the `VerifyAndRun` branch:**

```rust
JobDecision::VerifyAndRun { payment_proof, body } => {
    let Some(verifier) = &state.verifier else {
        return HttpResponse::json(501, r#"{"status":"not-implemented","reason":"payment verification not wired"}"#);
    };

    // 1. Parse the job request to get job_id
    let spec: JobSpec = match serde_json::from_slice(&body) {
        Ok(s) => s,
        Err(e) => return HttpResponse::json(400, format!("bad job spec: {e}")),
    };

    // 2. Verify payment
    match verifier.verify(&payment_proof, &state.escrow_account, &state.network) {
        Ok((_mint, _amount)) => { /* proceed */ }
        Err(PaymentVerifyError::MalformedProof(e)) =>
            return HttpResponse::json(400, format!("bad payment proof: {e}")),
        Err(PaymentVerifyError::JobIdMismatch { expected, found }) =>
            return HttpResponse::json(400, format!(
                "payment job_id mismatch: expected {expected}, got {found}")),
        Err(e) =>
            return HttpResponse::json(402, format!("payment verification failed: {e}")),
    }

    // 3. Create contract
    let contract = create_contract(
        spec.job_id.clone(),
        state.offer.body.node_id.clone(),
        device_class_from_offer(&state.offer.body.device),
        spec.max_duration_secs,
    );
    if let Some(dir) = &state.state_dir {
        let _ = write_contract(&contract, dir); // log error, don't fail
    }

    // 4. Run through executor
    match &state.runner {
        Some(runner) => match runner.run(&body) {
            Ok(json) => HttpResponse::json(200, json),
            Err(e) => HttpResponse::json(e.status, e.message),
        },
        None => HttpResponse::json(501, "job execution not wired"),
    }
}
```

**`RunFree` branch — add contract creation before execution:**

Same pattern: parse body → create contract → write to disk → run.

### 5. Helper: device class from offer

```rust
fn device_class_from_offer(device: &AdvertisedDevice) -> DeviceClass {
    match device {
        AdvertisedDevice::Cpu { .. } => DeviceClass::Cpu,
        AdvertisedDevice::NvidiaGpu { .. } => DeviceClass::NvidiaGpu,
        AdvertisedDevice::NvidiaMig { .. } => DeviceClass::NvidiaMig,
        AdvertisedDevice::NvidiaVgpu { .. } => DeviceClass::NvidiaVgpu,
        AdvertisedDevice::AmdGpu { .. } => DeviceClass::AmdGpu,
    }
}
```

### 6. Binary changes (`crates/node-api/src/bin/vtessera_node.rs`)

Behind `#[cfg(feature = "serve")]`:

1. Add `--rpc-url <solana-rpc>` CLI flag (default: `https://api.devnet.solana.com`).
2. Construct a `SolanaPaymentVerifier` that wraps `solana_client::rpc_client::RpcClient`.
3. Pass it into `NodeState::verifier`.
4. Add `state_dir` to `NodeState` (already passed to the binary via `--state-dir`).

The `SolanaPaymentVerifier` implementation:

1. Deserialize proof JSON to extract `tx` (signature), `job_id`, `amount_micros`, `mint`.
2. Call `rpc.get_transaction(signature, UiTransactionEncoding::JsonParsed)`.
3. Confirm transaction status is `Finalized`.
4. Check the transaction's `account_keys` include the escrow account.
5. Check the token transfer amount ≥ the offer's `per_device_second_micros × agreed_seconds`.
6. Cross-check the `job_id` from the proof matches the job request's `job_id`.
7. Return `(mint, amount)` on success.

### 7. MCP changes (`crates/node-api/src/mcp.rs`)

The `submit_job` MCP tool currently calls `classify_job_request` and handles
the result. Update it to route through the same contract-creation and
verification path as the HTTP handler. The MCP binary does **not** wire a
`PaymentVerifier` (stdio transport, no HTTP for RPC calls), so paid MCP
submissions will still get the honest 501 via the verifier-None path.

Free MCP submissions will now create contracts and execute — matching the HTTP
behavior.

### 8. ROADMAP.md update

Mark Module 2 sub-sections as shipped:
- §2a: already marked shipped
- §2b: x402 challenge + payment verification — mark shipped
- §2c: job contract + lifecycle — mark shipped

## Testing

### Unit tests (crates/settlement)

- `create_contract` produces correct fields
- `write_contract` creates `contracts/<job_id>.json` on disk
- Round-trip: write → read → compare

### Unit tests (crates/node-api)

- Mock `PaymentVerifier` that accepts a known proof
- `VerifyAndRun` with valid proof → contract written, executor called, 200
- `VerifyAndRun` with invalid proof → 400/402, no contract written
- `VerifyAndRun` with no verifier → 501
- `RunFree` → contract written before executor call, 200
- Job-id mismatch between proof and request → 400

### Integration test

- Free job: submit → contract on disk → receipt on disk → `settle()` computes `f`
- Paid job: exercise with mock verifier (same as unit, larger scope)
- x402-client binary: already runs end-to-end on devnet; now gets 200 instead of 501

## Error handling

| Failure | Response | Contract written? |
|---------|----------|-------------------|
| Bad job spec JSON | 400 | No |
| Proof malformed | 400 | No |
| RPC unreachable | 503 | No |
| Transaction not found | 402 (re-challenge) | No |
| Escrow mismatch | 402 | No |
| Insufficient amount | 402 | No |
| Job-id mismatch | 400 | No |
| Executor failure | 500 | Yes (contract exists, receipt shows failure) |
| No verifier wired | 501 | No |
| No runner wired | 501 | Yes (contract exists, no execution) |

## Design decisions

1. **Agent-provided job_id.** The escrow PDA is derived from the job_id
   (`find_program_address(&[b"contract", &job_id])`), so the agent must know
   the job_id before paying.    Generating the job_id only in the 402
   challenge response would break this flow. The agent provides it; the node
   uses it and cross-checks against the on-chain payment.

2. **Library-only node-api.** The `PaymentVerifier` is a trait; the real
   Solana RPC implementation lives in the binary behind `#[cfg(feature = "serve")]`.
   This keeps the library crate testable without pulling in `solana-client`
   (which is heavy and brings its own dependency tree).

3. **Contract written before execution.** If the executor crashes, the contract
   still exists on disk — settlement can compute `f = 0` against it. This is
   correct: the buyer agreed to work, the node accepted it, but nothing
   happened. The escrow should refund fully.

4. **402 re-challenge on verification failure.** Rather than a permanent 403,
   the node returns 402 again with a fresh challenge. The agent can fix its
   payment and retry. This matches the x402 spec's intent.

5. **No state_dir hard-fail.** If `write_contract` fails (disk full,
   permissions), the job still executes. The receipt is still signed. Settlement
   may fail later when it can't find the contract, but the job itself shouldn't
   be blocked by filesystem issues. Log the error, continue.
