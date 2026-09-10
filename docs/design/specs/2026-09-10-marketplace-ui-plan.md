# Vtessera Marketplace UI — Implementation Plan

**Spec:** `docs/design/specs/2026-09-10-marketplace-ui-design.md`
**Date:** 2026-09-10

## Phase 1: `src/marketplace.rs` — data layer

New module. Types + fetching + pure label helpers, all unit-testable without GTK.

**Changes:**
- `pub const DEFAULT_MARKETPLACE_URL: &str = "https://douglasdemaio.github.io/vtessera/nodes.json"`
- `pub struct Listing { node_id, endpoint_id, endpoint_host, device_kind, spec_label, mode_free, price_per_hour_label, issued_unix, expires_unix, agent_cmd }`
- `pub struct MarketplaceSnapshot { updated_at, nodes: Vec<Listing> }`
- `pub fn fetch_marketplace(url: &str) -> Result<MarketplaceSnapshot, String>` — ureq GET, read JSON. Never panics; returns `Err` string on HTTP error / malformed body.
- `pub fn node_label(listing) -> String` — "CPU 8 vCPU · 16 GB" / "NVIDIA RTX 4090 · 24 GB" / "NVIDIA MIG A100 · 1g.10gb" / "AMD RX 7900 · 24 GB"
- `pub fn price_per_hour(micros_per_sec: u64, currency) -> String` — "€0.05/h", "0.1000 USDC/h", rollover handling
- `pub fn price_label(listing, currency_symbol) -> String` — "free" or `"€0.05/h"`
- `pub fn agent_command(listing) -> String` — `vtessera-agent --node <endpoint_host> submit --job job.json`, or `--node <endpoint_id>` (iroh) when no HTTP endpoint
- JSON parsing via `serde_json::Value` defensive getters (no `deny_unknown_fields`, tolerate missing optional fields like `candidates`).

**Verify:** `cargo test -p vtessera-gui` — new fixture-based tests pass (free, paid, MIG/vGPU, outbound-only).

## Phase 2: dependency + Flatpak source

**Changes:**
- `crates/vtessera-gui/Cargo.toml`: add `ureq = { version = "3", default-features = false, features = ["rustls", "json"] }` (workspace style)
- Regenerate `packaging/flatpak/cargo-sources.json` (flatpak-build skill) after `Cargo.lock` churns.

**Verify:** `cargo build -p vtessera-gui` compiles. `cargo-sources.json` regenerated.

## Phase 3: Marketplace tab shell in `main.rs`

Build the split view as a 4th notebook page.

**Changes:**
- New `build_marketplace_tab(ui) -> gtk4::Box` in `main.rs`:
  - Outer horizontal `Box`, margin 24
  - Left `Pane` (~2/3, `hexpand=true`): Browse pane
  - Right `Pane` (~1/3, fixed width): My Listing pane
- Browse pane: header row (filter segmented buttons free/paid ✓ all, refresh button, live count label), then a scrollable `Box` for listing rows
- My Listing pane: status card, quick price editor (mode segmented + price spin + currency label + micros readout), "what buyers see" preview
- New `Ui` fields: `marketplace_rows: gtk4::Box`, `marketplace_count: gtk4::Label`, `marketplace_status: gtk4::Label`, `browse_filter_active: Rc<Cell<bool>>` etc. (filter state), `ml_mode_free_btn`, `ml_mode_paid_btn`, `ml_price_spin`, `ml_currency_label`, `ml_micros_label`, `ml_preview_label`, `ml_cmd_label`, `ml_status_label`
- Append to notebook after Jobs.
- CSS: reuse `.dashboard-card`, `.dim-label`, `.mode-segmented`, `monospace` TextView for the command lines; add `.market-label` for the little "free"/"€0.05/h" price chips.

**Verify:** App launches. Marketplace tab renders with both panes; filter buttons and refresh button respond.

## Phase 4: Browse — fetch + render

**Changes:**
- `fn refresh_marketplace(ui: &Ui)` — kicks a worker thread (`std::thread::spawn`) that calls `marketplace::fetch_marketplace`, pushes result into an `Arc<Mutex<Vec<...>>>`-style pending slot; the existing 200ms timeout (or a new 1s timeout) applies it on the main thread
- Row build: `fn build_listing_row(listing) -> gtk4::Box` — mode dot, `node_label`, `price_label`, `endpoint_host`/truncated id; a click toggles an inline detail `Box` (full ids, iss/exp, `agent_command` in monospace `SelectableLabel` + Copy button)
- Only one row expanded at a time — keep `current_expanded` in a shared `Rc<Cell<Option<String>>>`
- Filter: free/paid/all radios filter the rendered rows from the last snapshot (keep `last_snapshot: Rc<RefCell<Vec<Listing>>>`)
- 60s timer: `glib::timeout_add_local(Duration::from_secs(60), ...)` → `refresh_marketplace`
- Count label: "N nodes listed"; offline error → "marketplace unreachable (retry last ok HH:MM)".

**Verify:** With network, rows populate within a few seconds. Empty marketplace → "0 nodes listed". Kill network → status label flips, UI stays responsive.

## Phase 5: My Listing — status + price editor

**Changes:**
- `fn refresh_my_listing(ui: &Ui, state: &NodeState)`:
  - Status card populated from `Settings` (load via `Settings::load_or_default`) + `current_state()`/daemon running state + `state_dir` daemon probes (reuse existing helpers where present)
  - Preview label from `offer::build_offer_json(&settings, &key)` rendered human-readable (device + mode + price)
  - Agent command line from `settings.endpoint`
- Price editor Save: writes `mode` + `price_per_cpu_hour` into the `Ui`'s current settings, `settings.save(settings_path())`, then restart node if running (existing restart path). Currency symbol follows settings currency.
- Live micros readout updates on spin/value-changed: `offer::price_per_cpu_second_micros(hour)` → "≈ N micros/s per device".
- Disabled + hint when node not running.

**Verify:** Edit price → save → `settings.toml` updates, node restarts (if running), offer.json regenerated. Free/paid toggle re-enables payout/price fields (mirrors Settings mode behavior).

## Phase 6: Tests + CI

**Changes:**
- Unit tests in `marketplace.rs` (price_label edge cases, node_label all 5 device variants, agent_command HTTP vs iroh, fixture parse)
- `cargo fmt --check`
- `cargo clippy -p vtessera-gui --all-targets -- -D warnings`
- `cargo test -p vtessera-gui --locked`
- `cargo build -p vtessera-gui` (with `--features serve`? GUI has none; plain)
- Flatpak smoke if feasible (flatpak-build skill)

**Verify:** All green.

## Dependencies

New: `ureq 3` (rustls, json). Cargo.lock churn → flatpak `cargo-sources.json` regen (flatpak-build skill).

## Estimated Effort

| Phase | Effort |
|-------|--------|
| 1. Data layer + tests | 1.5 h |
| 2. Dependency + flatpak | 30 min |
| 3. Tab shell | 1 h |
| 4. Browse fetch/render | 1.5 h |
| 5. My Listing editor | 1 h |
| 6. Tests + CI | 30 min |
| **Total** | **~6 h** |