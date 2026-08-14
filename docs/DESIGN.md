# Vtessera Design

Vtessera is **AI-agent compute for the HNT ecosystem**: a Cargo
workspace of small, audited crates that together let a machine owner
rent CPU/GPU capacity to AI agents over MCP + x402, with sellers
settling in HNT and buyers paying in EURC/USDC. There is no Vtessera
token.

This document is an index. The authoritative design lives in:

- [README.md](../README.md) — Project overview, install, quickstart.
- [ROADMAP.md](../ROADMAP.md) — **Start here** for the full picture.
  Modules 0–5, build order, fee model (DRAFT), neutral-settlement
  principle, milestones.
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
| `crates/settlement` | Module 3 — receipt verification + `f` | skeleton |
| `programs/vtessera-escrow` | Module 4 — Anchor escrow program | shipped (devnet) |

Skeleton crates land with the types, traits, and tests that pin the
interface; the heavy implementation work (Kata + VFIO, DCGM telemetry,
Jupiter CPI, etc.) lands per the ROADMAP milestones. "Shipped" means
the crate's contract is implemented and tested. Job execution is wired
through the `JobRunner` hook in `crates/node-api`: the `serve`-gated
`vtessera-node`/`vtessera-mcp` binaries supply the executor from
`crates/executor` via `--backend` (`noop-cpu` default, `local-cpu` for
unisolated host execution). On-chain payment verification is still
unwired, so paid jobs return honest 501s until it lands.

## Why HNT, not a Vtessera token

An earlier draft proposed a fixed-supply VTESS token with a voted
multi-asset reserve. That direction is **superseded**: Vtessera ships
as technology that plugs into the existing HNT economy, not a new
token. Every paid job is a real on-market HNT buy (Jupiter,
Pyth-guarded) and a small burn — compute demand becomes recurring
demand for HNT, without governance, vesting, or treasury overhead. See
[ROADMAP.md](../ROADMAP.md) §4c–4d for the full rationale.
