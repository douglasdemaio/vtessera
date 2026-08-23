---
name: observability
description: Add Prometheus metrics and Grafana dashboards to vtessera (node-api, offer-index, vtesserad, marketplace-server). Use when the user mentions metrics, /metrics, Prometheus, Grafana, analytics, monitoring, dashboards, or "what is my fleet doing". Explains how to expose Prometheus text format from the hand-rolled mini-http stack WITHOUT adding heavy dependencies, and ships the packaging/observability compose stack.
---

# Vtessera observability (Prometheus + Grafana)

Current state: **zero observability**. No `tracing`, no `log`, no exporter,
no `/metrics` anywhere — only `println!`/`eprintln!` and three health
endpoints (`node-api /healthz`, `offer-index /healthz`, marketplace-server
`/api/v1/health`). The GUI dashboard polls the latest receipt file every 2s.
The only structured measurement is `vtesserad`'s
`ResourceSample { ts_unix, cpu_pct, mem_used_kb, disk_free_kb }`
(`crates/vtesserad/src/metrics.rs`), which feeds receipts.

## Hard constraints

- **Dep budget** (BUILD.md §1.3): `vtesserad` must stay lean and opens no
  sockets (`crates/vtesserad/tests/no_socket.rs`). Do NOT put an HTTP
  exporter in vtesserad — export its numbers via node-api, which already
  reads the spool.
- **mini-http, not axum/hyper**, serves node-api and offer-index. The
  Prometheus text exposition format is plain text — hand-roll it (an
  `AtomicU64`-based registry module, ~150 lines) instead of pulling the
  `prometheus` crate into core crates. `marketplace-server` is already Axum,
  so `axum-prometheus` or `metrics-exporter-prometheus` is acceptable THERE
  only.
- ROADMAP.md:183 "no dashboards required" is a statement about the machine
  buyer's flow, not a ban on operator dashboards. Seller/operator dashboards
  are in scope; never make the buy path depend on them.
- Every new listening surface must be declared per CONSENT.md. `/metrics`
  rides the existing declared ports — no new sockets.

## Implementation task list (for the executing model)

1. **`crates/vtessera-metrics`** (new, zero-dep): static registry of counters
   and gauges, `render() -> String` in Prometheus text format 0.0.4. Types:
   counter, gauge, histogram-as-summary buckets if needed later.
2. **node-api `GET /metrics`** (in `dispatch`, `crates/node-api/src/lib.rs`):
   jobs submitted/completed/failed, MCP calls by method, x402 payments seen,
   receipts written, latest ResourceSample gauges (cpu_pct, mem_used_kb,
   disk_free_kb) read from the vtesserad spool, iroh connection state.
3. **offer-index `GET /metrics`**: registered offers (by mode free/paid, by
   device kind), heartbeats received, stale offers evicted, claims
   granted/denied/expired, rate-limiter rejections, request counts by path.
4. **marketplace-server**: receipts ingested, receipts rejected by auth,
   store size.
5. **`packaging/observability/`**: `docker-compose.yml` (or podman) with
   Prometheus + Grafana, `prometheus.yml` scraping node/index/marketplace,
   and provisioned dashboards (JSON) — "Fleet overview" (offers, heartbeats,
   claims), "Node detail" (jobs, resource gauges), "Settlement" (receipts,
   completion fractions). Follow the dataviz skill for dashboard design.
6. **Settlement analytics**: derive earnings series from receipts +
   `finalize_pro_rata` fractions; expose as metrics on marketplace-server
   for the enterprise track (public track keeps receipts local).
7. Tests: golden-file test for exposition format; assert `/metrics` appears
   in each dispatch table; keep `vtesserad` no_socket test green.
8. Run the `checks` skill before commit (clippy -D warnings will catch
   unused registry fields).

## Definition of done

`curl localhost:<node-port>/metrics | promtool check metrics` passes;
`docker compose up` in `packaging/observability/` shows live fleet and node
dashboards in Grafana against a locally running node + offer-index.
