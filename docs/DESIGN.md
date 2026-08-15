# Vtessera Design

Vtessera is **AI-agent compute settled in EURC/USDC**: a Cargo
workspace of small, audited crates that together let a machine owner
rent CPU/GPU capacity to AI agents over MCP + x402, with sellers
settling in the same stablecoin the buyer pays and a flat SOL protocol
fee. There is no Vtessera token.

This document is an index. The authoritative design lives in:

- [README.md](../README.md) — Project overview, install, quickstart.
- [ROADMAP.md](../ROADMAP.md) — **Start here** for the full picture.
  Modules 0–5, build order, fee model (flat per-transaction SOL),
  neutral-settlement principle, milestones.
- [BUILD.md](../BUILD.md) — Authoritative v0 build specification for
  `vtesserad` (scope, hard rules, module contracts, systemd hardening,
  CI, definition of done). v0 must not widen beyond this; new modules
  live in separate crates.

## Workspace map

| Path | Module (ROADMAP §) | Status |
| ---- | ------------------ | ------ |
| `crates/vtesserad` | v0 metering daemon | shipped (CI green) |
| `crates/executor` | Module 1 — execution + accelerators | shipped (CPU backends wired) |
| `crates/offer` | Module 2 — signed machine-readable offers | shipped |
| `crates/node-api` | Module 2 — x402 / MCP HTTP surface (feature-gated) | shipped |
| `crates/mini-http` | Module 2 — shared HTTP/1.1 server primitives | shipped |
| `crates/offer-index` | Module 2a — central offer index (verify + serve) | shipped |
| `crates/settlement` | Module 3 — job receipts + `vtessera-settle` | shipped (non-TEE) |
| `programs/vtessera-escrow` | Module 4 — Anchor escrow program | shipped (devnet) |

Skeleton crates land with the types, traits, and tests that pin the
interface; the heavy implementation work (Kata + VFIO, DCGM telemetry,
etc.) lands per the ROADMAP milestones. "Shipped" means
the crate's contract is implemented and tested. Job execution is wired
through the `JobRunner` hook in `crates/node-api`: the `serve`-gated
`vtessera-node`/`vtessera-mcp` binaries supply the executor from
`crates/executor` via `--backend` (`noop-cpu` default, `local-cpu` for
unisolated host execution). On-chain payment verification is still
unwired, so paid jobs return honest 501s until it lands.

**Settlement flow (Module 3, non-TEE first):** after every job run,
`vtessera-node` signs a per-job metering receipt with its Ed25519
identity key (loaded via `--key`, its `node_id` must match the offer it
advertises) and writes it to `<state-dir>/job-receipts/<job_id>.json`.
The `vtessera-settle` service (watch loop, or `--once` for CI) sweeps a
shared state dir: for each `contracts/<job_id>.json` it verifies the
job's signed receipt (schema / pubkey / self-attesting node_id /
signature — any failure is a permanent reject, never a partial credit),
aggregates device-seconds via the agreed device class (a GPU contract
credits GPU-seconds, never CPU-seconds), and writes
`settlements/<job_id>.json` containing the completion fraction `f`.
The escrow program (§4) consumes `f` to split the held stablecoin.
TEE/attestation deployment is a follow-up, per the roadmap.

## Why EURC/USDC, no Vtessera token

An earlier draft proposed a fixed-supply VTESS token with a voted
multi-asset reserve. That direction is **superseded**: Vtessera ships
as technology, not a token. Sellers are paid in the **same stablecoin
the buyer paid** — EURC or USDC, whichever the node's signed offer puts
on it — paid directly by the escrow program with no swap and no burn.
There is no mintable protocol token and no external-market dependency
(the payout model isn't hostage to any third-party asset); the protocol
adds only an escrow program and a flat SOL fee. See
[ROADMAP.md](../ROADMAP.md) §4c–4d.
