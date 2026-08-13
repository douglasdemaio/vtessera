# Vtessera GUI Flatpak — Design

Date: 2026-08-13
Status: Approved by user 2026-08-13

## Goal

Ship Vtessera as a desktop Flatpak (`io.github.douglasdemaio.Vtessera`) so a
machine owner can turn their hardware into a Vtessera compute node for AI
agents, set a **Solana payout address** and an **editable price per CPU-hour**
(or choose to **donate compute for free**), and have the existing Vtessera
components run and be managed from a GTK4 UI.

## What the app does

A GTK4 application (new crate `crates/vtessera-gui`) that:

1. Collects seller settings:
   - Mode: **Sell compute** (`paid`) or **Donate compute (free)**.
   - Solana payout address (base58, 32–44 chars) — required only in paid mode.
   - Price per CPU-hour (decimal, e.g. `0.05`) and currency (`eurc` | `usdc`) —
     editable at any time, applied on restart.
   - Advanced: node port (default `8402`), advertised endpoint, sampling
     interval, escrow account + network (defaults: devnet program
     `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`, `solana-devnet`).
2. On save:
   - Writes `vtessera.toml` (with new `mode` / `price_per_cpu_hour` /
     `currency` fields) into the Flatpak config dir.
   - Bootstraps the Ed25519 identity key (`vtesserad --once`), then builds a
     **signed offer** via `vtessera-offer` (`PriceQuote::Paid` vs `Free`).
   - Starts two background children:
     - `vtesserad --config <cfg>` — metering daemon, signed receipts.
     - `vtessera-node --bind 0.0.0.0:<port> --offer <offer.json>
       --escrow <pda> --network <id>` — agent-facing x402/MCP HTTP server.
   - Stops/restarts children on settings change; kills children on quit.
3. Status view: process state, node id, mode/price summary, recent receipts,
   live log tail.

## Code changes

### `crates/vtesserad` (backward compatible)

`config.rs` gains optional fields (all `#[serde(default)]`, config keeps
`deny_unknown_fields`):

- `mode` — `"paid"` (default) | `"free"`.
- `currency` — `"eurc"` (default) | `"usdc"`.
- `price_per_cpu_hour` — `f64`, default `0.0`; informational for v0, used by
  the GUI/offer builder.
- `payout_id` becomes optional (default empty); `validate()` requires a valid
  base58 address only when `mode == "paid"`.

Receipt schema (`schema_ver = 1`) is **unchanged**; in free mode `payout_id`
is empty in receipts, which the canonical-byte format already handles.

`packaging/vtessera.toml.example` documents the new fields.

### `crates/vtessera-gui` (new workspace member)

Dependencies: `gtk4`, `glib`/`gio` (via gtk4-rs), plus workspace crates
`vtessera-offer` (offer signing), `ed25519-dalek`, `rand`, `toml`, `serde`,
`hex`. Own Cargo.toml so v0's dep budget (BUILD.md §1.3) is unaffected.

- `src/config.rs` — load/save TOML, validate address/price.
- `src/offer.rs` — build signed offer JSON from settings + identity key,
  write `offer.json`.
- `src/daemon.rs` — spawn/kill `vtesserad` + `vtessera-node`, capture
  stderr/stdout into the log view.
- `src/main.rs` — GTK window: settings form, mode toggle, save/start/stop,
  status + log + receipts view.

Price conversion: `price_per_cpu_hour` (human) →
`per_device_second_micros = (price * 1_000_000 / 3600).round()` for the offer.

### `packaging/flatpak/`

- `io.github.douglasdemaio.Vtessera.yaml` — org.gnome.Platform/Sdk 49,
  `org.freedesktop.Sdk.Extension.rust-stable`, offline cargo build using a
  generated `cargo-sources.json`; installs `vtessera-gui` (command),
  `vtesserad`, `vtessera-node` to `/app/bin`; neutralizes `rust-toolchain.toml`
  during build (pinned 1.96.0 + musl target would force offline rustup
  downloads).
- finish-args: `--share=ipc`, `--socket=wayland`, `--socket=fallback-x11`,
  `--share=network` (serve agents / future receipt submission), `--device=dri`.
- `io.github.douglasdemaio.Vtessera.metainfo.xml`, `.desktop`,
  `icon.svg`/`*.png` (from repo `logo.png`), `README.md`, `.gitignore`.

## Scope / non-goals

- Execution backends upstream are stubs (`/jobs` returns 202 "accepted");
  this ships metering + offer/discovery/x402 negotiation. Running third-party
  workloads inside the sandbox is a later module.
- Escrow program deployment, Jupiter swap, HNT payout wiring are unchanged
  from upstream.

## Out of scope (unchanged upstream behavior)

Free-mode receipts carry an empty `payout_id`; paid-mode requires a valid
address. No token, no treasury, no fee changes.
