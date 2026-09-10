# Vtessera GUI Dashboard Enhancement

**Date:** 2026-08-20
**Status:** Approved
**Scope:** Dashboard view, job history table, visual polish

## Summary

Replace the current 2-tab notebook (Settings / Status) with a 3-tab layout adding a live dashboard and structured job history. Apply a GitHub Dark visual theme across all widgets.

## Tab Structure

| Tab | Purpose |
|-----|---------|
| **Settings** | Unchanged — seller profile, consent switches, backend |
| **Dashboard** | New — live metering cards + status + log |
| **Jobs** | New — table of completed jobs with earnings |

## Dashboard Tab

### Top Section: 2x2 Grid Cards

| Card | Content | Data Source |
|------|---------|-------------|
| **Status** | State dot (gray/green/yellow) + "Off" / "Metering only" / "Accepting jobs" + node ID | `current_state()` + `current_node_id()` |
| **CPU** | Percentage + thin progress bar | Latest `receipt_*.json` → `totals.cpu_pct_avg` |
| **Memory** | GB used + progress bar | Latest `receipt_*.json` → `totals.mem_used_kb_avg` |
| **Jobs** | Total count + "3 active, 9 completed" subtitle | `job-receipts/` directory listing |

### Middle Section

Settlement honesty note — one paragraph, same copy as current status page.

### Bottom Section

Live log in monospace `TextView` — unchanged from current behavior.

### Refresh

2-second timer (same as today's `refresh_status`). New `refresh_dashboard(ui)` function reads latest receipt file and updates card values.

## Jobs Tab

### Summary Bar

Above the table — three metrics:
- Total jobs completed
- Total earnings (sum of all receipts)
- Average CPU across all jobs

### Table Columns

| Column | Content |
|--------|---------|
| **Status** | Colored dot — green (completed), yellow (active) |
| **Job ID** | Truncated hash (first 8 chars) |
| **CPU** | Average CPU % during job |
| **Memory** | Average memory used |
| **Earnings** | Amount in currency (USDC/EURC) |
| **Time** | Relative timestamp ("2m ago", "15m ago") |

Sorted newest-first. Reads from `job-receipts/*.json` files.

## Visual Theme: GitHub Dark

| Element | Value |
|---------|-------|
| Window background | `#0d1117` |
| Card/surface background | `#161b22` |
| Border | `1px solid #30363d` |
| Primary text | `#e6edf3` |
| Secondary text | `#8b949e` |
| CPU accent | `#58a6ff` (blue) |
| Memory accent | `#d2a8ff` (purple) |
| Status green | `#3fb950` |
| Earnings gold | `#fbbf24` |
| Card border-radius | `6px` |
| Progress bar height | `4px`, rounded |

All CSS stays inline in `install_css()` — no external files.

## Data Flow

No new dependencies. All data comes from existing receipt files:

1. **Dashboard metrics**: Read latest `receipt_*.json` from `state_dir()` on 2-second refresh
2. **Job history**: Parse `job-receipts/*.json` for CPU/memory/earnings (extends existing `refresh_jobs()`)
3. **Live log**: Unchanged — `log_pending` `Arc<Mutex<Vec<String>>>` + 200ms drain timer

New function: `refresh_dashboard(ui)` reads latest receipt and updates four card values. Existing `refresh_status()` continues handling button sensitivity and daemon state.

## Scope Boundaries

- **No GPU metrics** — v0 daemon only samples CPU/memory/disk
- **No charts/graphs** — text + progress bars only
- **No real-time streaming** — 2-second file poll
- **No job detail drill-down** — table shows all info inline
- **No persistence of earnings** — recomputed from receipts on each refresh

## Files Modified

- `crates/vtessera-gui/src/main.rs` — tab structure, dashboard widgets, job table, CSS
- `crates/vtessera-gui/src/settings.rs` — no changes
- `crates/vtessera-gui/src/daemon.rs` — no changes
