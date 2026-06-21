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
| `crates/executor` | Module 1 — execution + accelerators | skeleton |
| `crates/offer` | Module 2 — signed offers (MCP-shaped) | skeleton |
| `crates/node-api` | Module 2 — x402 / MCP HTTP surface (feature-gated) | skeleton |
| `crates/settlement` | Module 3 — receipt verification + `f` | skeleton |
| `programs/vtessera-escrow` | Module 4 — Anchor escrow program | skeleton |

Skeleton crates land with the types, traits, and tests that pin the
interface; the heavy implementation work (Kata + VFIO, DCGM telemetry,
Jupiter CPI, etc.) lands per the ROADMAP milestones.

## Why HNT, not a Vtessera token

An earlier draft proposed a fixed-supply VTESS token with a voted
multi-asset reserve. That direction is **superseded**: Vtessera ships
as technology that plugs into the existing HNT economy, not a new
token. Every paid job is a real on-market HNT buy (Jupiter,
Pyth-guarded) and a small burn — compute demand becomes recurring
demand for HNT, without governance, vesting, or treasury overhead. See
[ROADMAP.md](../ROADMAP.md) §4c–4d for the full rationale.
