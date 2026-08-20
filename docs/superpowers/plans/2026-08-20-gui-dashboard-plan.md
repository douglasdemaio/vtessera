# Vtessera GUI Dashboard — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-08-20-gui-dashboard-design.md`
**Date:** 2026-08-20

## Phase 1: CSS Theme

Expand `install_css()` in `main.rs` with the GitHub Dark palette.

**Changes:**
- Add CSS rules for `.dashboard-card`, `.progress-bar`, `.progress-fill`, `.job-row`, `.summary-metric`
- Window background `#0d1117`, card background `#161b22`, borders `#30363d`
- Text colors: primary `#e6edf3`, secondary `#8b949e`
- Accent classes: `.cpu-accent`, `.mem-accent`, `.status-green`, `.earnings-gold`
- Progress bar: 4px height, rounded, dark track

**Verify:** App launches with dark theme applied to existing widgets (settings page, buttons, entries).

## Phase 2: Tab Restructuring

Replace the 2-tab notebook with 3 tabs.

**Changes:**
- Rename `status_page` to `dashboard_page` (vertical box)
- Create new `jobs_page` (vertical box)
- Add third notebook page: `notebook.append_page(&jobs_page, Some(&Label::new(Some("Jobs"))))`
- Move settlement note + live log from old status page into `dashboard_page`
- `jobs_page` gets summary bar (3 labels) + `TreeView` for the table

**Verify:** App launches with 3 tabs. Settings and Dashboard tabs render correctly.

## Phase 3: Dashboard Grid Cards

Build the 2x2 metric card grid in the dashboard page.

**Changes:**
- Create `gtk4::Grid` with 2 columns, 2 rows, 12px spacing
- Each cell is a `gtk4::Box` with `.dashboard-card` class containing:
  - Title label (`.dim-label`, uppercase)
  - Value label (large, colored)
  - Progress bar (CPU/memory only)
- Add widgets to `Ui` struct: `status_dot_label`, `status_value_label`, `node_id_value_label`, `cpu_value_label`, `cpu_bar`, `mem_value_label`, `mem_bar`, `jobs_value_label`, `jobs_subtitle_label`
- Insert grid between intro label and settlement note in `dashboard_page`

**Verify:** Four cards render with placeholder values. Layout matches the mockup.

## Phase 4: Dashboard Data Refresh

Wire the dashboard cards to live data.

**Changes:**
- New function `refresh_dashboard(ui: &Ui)`:
  - Call `current_state()` → set status dot color + text
  - Call `current_node_id()` → set node ID label
  - Find newest `receipt_*.json` in `state_dir()` → parse JSON → extract `totals.cpu_pct_avg`, `totals.mem_used_kb_avg`
  - Count `job-receipts/*.json` → set jobs count label
- Add `serde_json` dependency to `vtessera-gui/Cargo.toml` (for receipt parsing)
- Hook `refresh_dashboard()` into the existing 2-second timer

**Verify:** Start the node → dashboard cards update with real metrics. Status shows correct state.

## Phase 5: Jobs Table

Build the tabular job history view.

**Changes:**
- Create `gtk4::ColumnView` with 6 columns (Status, Job ID, CPU, Memory, Earnings, Time)
- Each column uses `gtk4::SignalListItemFactory` with label widgets
- Data model: `gio::ListStore` holding `JobRow` GObject (or use `gtk4::NoOpListModel` wrapper)
- New function `refresh_jobs_table(ui: &Ui)`:
  - Read `job-receipts/*.json` files
  - Parse each for node_id, cpu_pct_avg, mem_used_kb_avg, timestamp
  - Sort newest-first
  - Rebuild the list store
- Summary bar: 3 `gtk4::Label` widgets above the `ColumnView`

**Verify:** Job receipts appear in the table. Summary bar shows totals.

## Phase 6: Visual Polish

Final pass on spacing, colors, and responsive behavior.

**Changes:**
- Card padding: 16px internal
- Grid gaps: 12px
- Progress bar track color: `#30363d`
- Status dot: colored circle (use CSS `::before` pseudo-element or colored label)
- Table row hover: subtle background highlight
- Window default size: increase to 860x780 (wider for table columns)

**Verify:** App looks clean at default size. Resize behavior is acceptable.

## Phase 7: Tests + CI

Ensure existing tests pass and add coverage for new code.

**Changes:**
- Run `cargo test -p vtessera-gui` — existing settings tests must pass
- Run `cargo clippy -p vtessera-gui` — no new warnings
- Run `cargo fmt --check` — formatting clean

**Verify:** CI passes. No regressions.

## Dependencies

New dependency: `serde_json` (for parsing receipt JSON files in the GUI).

Current `vtessera-gui/Cargo.toml` already has `serde` and `toml`. Adding `serde_json` is consistent with the existing dep budget.

## Estimated Effort

| Phase | Effort |
|-------|--------|
| 1. CSS Theme | 30 min |
| 2. Tab Restructuring | 30 min |
| 3. Dashboard Grid | 1 hour |
| 4. Data Refresh | 1 hour |
| 5. Jobs Table | 1.5 hours |
| 6. Visual Polish | 30 min |
| 7. Tests | 30 min |
| **Total** | **~5 hours** |
