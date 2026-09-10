# Vtessera Marketplace UI (Browse + Listing Manager)

**Date:** 2026-09-10
**Status:** Approved
**Scope:** New "Marketplace" tab in vtessera-gui — browse live public listings + manage the seller's own listing/price

## Summary

The GUI is seller-only today: it publishes an offer but has no way to browse the market or manage pricing from a UI. `docs/PRD.md` §11 calls this out ("no human-facing marketplace UI," "sellers hand-edit config for price"). This adds a 4th Notebook tab that lets a human buyer browse live nodes and a seller check their listing and edit price in place.

## Tab Structure

| Tab | Purpose |
|-----|---------|
| **Settings** | Unchanged — seller profile, consent switches, backend |
| **Dashboard** | Unchanged — live metering cards + status + log |
| **Jobs** | Unchanged — table of completed jobs with earnings |
| **Marketplace** | NEW — split view: Browse (public nodes) \| My Listing (own offer + price editor) |

## Layout (Decided in Brainstorming, 2026-09-10)

Side-by-side split inside the tab:
- **Browse** (wide, ~2/3) — the public marketplace, compact expandable rows
- **My Listing** (narrow, ~1/3) — the seller's own offer: status, quick price editor, "what buyers see" preview

## Browse Pane

### Data source

Public marketplace registry: `https://douglasdemaio.github.io/vtessera/nodes.json` — the same source `vtessera-agent discover --marketplace` uses. Response shape: `{ "version": 1, "updated_at": <epoch>, "nodes": [ { node_id, offer: { body: { ... } }, sig_hex, updated_at } ] }`.

The offer `body` carries (from `crates/offer`):
- `node_id` — truncated SHA-256 pubkey, stable node identity
- `endpoint_id` — full Ed25519 pubkey / iroh EndpointId
- `endpoint` — `Vec<String>` of reachable URLs (direct IP:port, queue/coordinator, relayed)
- `device` — `AdvertisedDevice`: `Cpu { vcpus, mem_mb }`, `NvidiaGpu/AmdGpu { model, vram_mb }`, `NvidiaMig/NvidiaVgpu { parent_model, profile, vram_mb }`
- `price` — `PriceQuote::Free` or `PriceQuote::Paid { currency, per_device_second_micros, payout_id }`
- `issued_unix`, `expires_unix`

### Header

- Filter: free / paid / all (segmented toggle, same widget pattern as Settings' mode toggle)
- Sort: price (free first, then by per-hour) / vcpus or vram / newest — default price
- Refresh button (manual)
- Live count label: "44 nodes listed"
- Auto-refresh: 60 s `glib::timeout_add_local` (mirrors the 2s Dashboard pattern; long enough to be cheap, short enough for a live market)

### Rows

Compact one-line rows (density option C), each showing:
- Mode dot: green `●` free, purple `◆` paid (matches Dashboard palette)
- Device label: "CPU 8 vCPU · 16 GB" / "NVIDIA RTX 4090 · 24 GB"
- Price: "free" (green) or per-hour (gold, e.g. "€0.05/h") computed from `per_device_second_micros` — `micros/s × 3600 / 1_000_000`
- Reach: first `endpoint` host (e.g. `192.168.1.5:8402`) or truncated `endpoint_id` when no endpoint list (`iroh:9f2a…c441`)

### Expandable detail

Click a row to expand an inline detail box (collapses the others — `Expander` or a rebuilt row):
- Full `node_id`, full `endpoint_id` (copyable)
- Issue / expiry, offer schema version
- Ready-to-copy agent command: `vtessera-agent --node <host-or-id> submit --job job.json` — HTTP endpoint when present, `--node-id <endpoint_id>` (iroh dial) when the node is outbound-only
- Copy button (writes to clipboard)

## My Listing Pane

### Status card

- State dot + "offline" / "live" (reuses the daemon healthz probe / `NodeState`)
- Mode: free (donating) / paid
- Device: "CPU 16 vCPU · 32 GB" (from `vtessera-gui/src/offer.rs::host_vcpus` / `host_mem_mb`, or the signed offer)
- Endpoint: from settings

### Quick price editor

- Segmented free / paid toggle (same CSS class as Settings' mode toggle)
- Price spin: "€/CPU-hour" decimal (defaults from `settings.price_per_cpu_hour`)
- Currency label follows settings (EURC = €, USDC = $)
- Live converted readout: "≈ 14 micros/s per device (what buyers see)" — via `vtessera-gui/src/offer.rs::price_per_cpu_second_micros`
- **Save** button: writes `mode` + `price_per_cpu_hour` to `settings.toml` (existing Settings save path), re-derives the signed offer, and restarts the node if running (existing `start_node` / restart path)

### "What buyers see" preview

- Rendered label, e.g. "CPU 16 vCPU · 32 GB — free · donate" or "... — €0.05/h eurc"
- Copyable agent command (same builder as Browse rows)
- Disabled entirely when node is offline (hint: "Start the node to publish this offer")

## Implementation Notes

- **New module `src/marketplace.rs`** to keep `main.rs` from growing further (~1705 lines today). Contains: `MarketplaceSnapshot`/`Listing` types, `fetch_marketplace()`, `node_label()`/`price_label()`/`agent_command()` helpers, plus unit tests. UI building stays in `main.rs` (a `build_marketplace()` helper; expands `build_ui`).
- **New dependency: `ureq`** (workspace-pinned `ureq 3`, `default-features=false, features=["rustls"]`). This is the GUI's first HTTP client and changes `Cargo.lock` → the Flatpak `packaging/flatpak/cargo-sources.json` must be regenerated (flatpak-build skill).
- Fetch runs on a worker thread (existing pattern: `daemon::pump_output` worker threads push into a shared handle); results surface via `glib::timeout_add_local` so the UI thread never blocks.
- All new UI reuses the existing inline GTK CSS classes (`dashboard-card`, `mode-segmented`, `status-green`, summary metric styling, monospace `TextView` for commands).
- No new IPC: My Listing reads `settings.toml` + re-derives the offer via `build_offer_json`.

## Error Handling

- Marketplace empty (`nodes: []`) → "0 nodes listed" (not an error)
- HTTP error / offline → status label "marketplace unreachable (retry)" + shows last-good time; UI stays usable
- Malformed `nodes.json` → same offline label, no crash
- `settings.toml` missing → My Listing shows an empty/"not configured" state, same as a fresh install
- Node not running → My Listing price editor disabled with hint; Browse unaffected

## Testing

- **Unit tests** in `marketplace.rs`:
  - `price_label`: free, paid micros→per-hour rounding (0, low, rollover)
  - `node_label`: CPU, NVIDIA GPU, MIG/vGPU variants
  - `agent_command`: HTTP endpoint vs iroh-only (`--node-id`)
  - `fetch_marketplace`: parse a fixture `nodes.json` (free + paid + outbound-only nodes)
- **Build verification:** `cargo clippy -p vtessera-gui --all-targets -- -D warnings`, `cargo fmt --check`, `cargo build -p vtessera-gui`
- **Flatpak smoke** after regenerating `cargo-sources.json` (flatpak-build skill)
- No GUI automation in-repo; manual smoke: start GUI, Marketplace tab renders, nodes listed (or offline state), price edit saves + restarts node