# Overview — Aggregated "Go Over Index" Command

**Status:** in-progress design (2026-09-10)
**Supersedes:** nothing
**ROADMAP:** §2e (Internet connectivity — iroh integration), closing the last
open Phase-2 debt row from `docs/design/zero-config-connectivity.md:830`
("Aggregated 'go over index' overview", marked `–`).

This spec adds a display-only `vtessera-agent overview` command that aggregates
**every** offer visible to the agent — from the offer-index and the public
marketplace, free **and** paid — into a single dense table (or normalized JSON).
It intentionally does not submit, probe, or act; it is the agent's "what's out
there" view that sits alongside `discover` / `offer` / `submit`.

## 1. Problem

Today `vtessera-agent discover` is the only "what nodes exist" command, and it
hides most of the network:

- It filters to **free, available** offers only (`/offers?available=1&mode=free`,
  `crates/agent-cli/src/main.rs:239`).
- It renders just four columns — `NODE_ID`, `DEVICE`, `REACH`, `DIAL`
  (`:320-346`) — so the agent cannot tell a free CPU from a paid GPU, a fresh
  heartbeat from a stale entry, or a claimed node from an unclaimed one.
- There is no combined view of the local index **and** the marketplace beyond
  the free-node merge, and no normalized JSON shape for scripts.

The design doc that shipped Phase 2 (dial-by-EndpointId, marketplace resolver,
coordinator federation) explicitly records the remaining item: the **aggregated
"go over index" overview** (`docs/design/zero-config-connectivity.md:830,
:856`). This spec implements it client-side in the agent CLI.

## 2. Goals

- One command to see **all** nodes the agent can reach: local index + public
  marketplace, free + paid, claimed + unclaimed, in a single merged table.
- Each row answers at a glance: identity, device, price, how to reach it, how
  to dial it, whether it is claimed, how many dial candidates it has, and how
  fresh its heartbeat is.
- `--json` emits a normalized, script-consumable array (not the raw merge).
- `discover` keeps its current output byte-for-byte; the index-merge logic is
  shared so the two commands cannot drift.
- No server-side changes. The offer-index already exposes every field needed;
  filtering reuses its existing query params.

## 3. Non-goals (explicitly deferred)

- **No acting.** No auto-submit, no best-node auto-pick, no dialing/probing.
- **No live reachability probe.** The `DIAL`/`REACH` columns describe what the
  offer *advertises* (QUIC vs HTTP), not whether a dial would succeed — honest
  reachability probing stays the job of `health`/`submit`/`--node-id`.
- **No rate/aggregate statistics** (counts by mode/device, capacity totals,
  freshness histograms) — rows today, aggregates as a follow-up if wanted.
- **No GUI surface.** This is an agent-CLI command (the marketplace GUI already
  renders offers browser-side).

## 4. Command shape

```
vtessera-agent overview [--mode free|paid] [--device cpu|nvidia_gpu|nvidia_mig|nvidia_vgpu|amd_gpu] [--available] [--json]
```

Existing global flags apply unchanged: `--index` (default `http://127.0.0.1:8403`),
`--marketplace <url>`, `--json`. A table is printed when `--json` is absent.

New, optional flags:

| Flag | Effect | Pass-through |
|---|---|---|
| `--mode free\|paid` | only free or only paid offers | `/offers?mode=…` |
| `--device <kind>` | only that device class | `/offers?device=…` |
| `--available` | only entries with no live claim | `/offers?available=1` |

Absent all three flags the agent requests `/offers` unfiltered: **all** modes,
devices, claimed + unclaimed. Invalid `--mode`/`--device` values are rejected
by clap (enum typing) before any network call.

## 5. Data flow

### 5.1 Fetch

1. `GET {index}/offers` with any of the three filter params appended.
2. If `--marketplace <url>` was given, `GET <url>` (the GitHub Pages
   `nodes.json` document: `{ "nodes": [...] }`).

### 5.2 Merge (shared with `discover`)

Extract the existing merge loop (`crates/agent-cli/src/main.rs:255-298`) into a
helper and reuse it:

- **Identity key** = entry-level `endpoint_id`, else offer-body `endpoint_id`,
  else first HTTP `endpoint` (`offer_endpoint`). This is the same precedence
  `discover` uses today, so outbound-only nodes (no HTTP endpoint) stay
  addressable by id.
- **Local index wins**; marketplace entries dedup-fill only identities not
  already seen.

### 5.3 Rendering

For each merged offer, build a row:

| Column | Value |
|---|---|
| `NODE_ID` | offer-body `node_id` (truncated id) |
| `DEVICE` | offer-body `device.kind` (e.g. `cpu`, `nvidia_gpu`) |
| `PRICE` | `free`, or `"<per_device_second_micros>/1_000_000> <currency>/s"` e.g. `0.002792/s eurc` for paid |
| `REACH` | first HTTP endpoint, else `iroh:<endpoint_id>` |
| `DIAL` | `quic` when `endpoint_id` present, else `http` |
| `CLAIM` | `–` when unclaimed, else `claimed by <agent_id>` |
| `CAND` | `candidates` array length |
| `HEARTBEAT` | `never` (0), `fresh`, or `stale` |

Heartbeat classification: `fresh` when `now - last_heartbeat_unix <=
DEFAULT_ENTRY_TTL_SECS` (120s), else `stale`.

### 5.4 JSON output (`--json`)

One normalized document (not the raw `{local_index, marketplace}` merge):

```json
{
  "count": 2,
  "offers": [
    {
      "node_id": "abc123",
      "endpoint_id": "…64 hex…",
      "device": {"kind": "cpu", "vcpus": 8, "mem_mb": 16384},
      "price": {"mode": "free"},
      "reach": "http://192.168.1.100:8402",
      "dial": "http",
      "candidates": [ {"kind": "host", "transport": "iroh_quic", "addr": "…", "priority": 200} ],
      "claimed_by": null,
      "claimed_until_unix": 0,
      "last_heartbeat_unix": 0
    }
  ]
}
```

Paid offers serialize `price` as
`{"mode":"paid","currency":"eurc","per_device_second_micros":2792}`. Fields with
no data (no `endpoint_id`, no claim) serialize as `null`/`0`/empty list, never
as `?`-style human text.

## 6. Error handling

- **Empty result:** human mode prints `no nodes in view` plus a tip to add
  `--marketplace <url>` when none was given (mirrors `discover`); JSON emits
  `{"count":0,"offers":[]}` with exit 0.
- **Index unreachable, no marketplace:** error (non-zero), same convention as
  `discover` — a dead-local-index agent that was pointed at a marketplace still
  renders from the marketplace.
- **Both unreachable:** non-zero error with the index error surfaced first.
- **Malformed entries:** skipped per-entry, counted in a debug note only; one
  bad row must not take down the overview.

## 7. Tests (unit, offline — no network)

In `crates/agent-cli`:

1. **Merge dedup and precedence** — fixture JSON: index + marketplace share an
   `endpoint_id`; assert one row, index entry wins.
2. **Identity fallback chain** — entry-level id → offer-body id → HTTP
   endpoint; outbound-only offers (no endpoint) still key by `endpoint_id`.
3. **Price rendering** — `free` and paid (`per_device_second_micros`,
   currency) both render; micros/1e6 formatting is exact (no float drift).
4. **Claim rendering** — unclaimed `–`; claimed shows agent id.
5. **Heartbeat classification** — `never` (0), `fresh`, `stale` boundary at
   `DEFAULT_ENTRY_TTL_SECS`.
6. **JSON shape** — normalized document round-trips; `null`/empty for absent
   fields; `count` matches `offers.len()`.
7. **Filter arg validation** — invalid `--mode`/`--device` rejected pre-network.

Existing `discover` behavior is covered by its current tests; a regression test
pins that `discover`'s rendered columns are unchanged.

## 8. Out of scope follow-ups (noted, not built)

- Live reachability probing from `overview` (`health`-style per row).
- Server-side `/overview` aggregate endpoint on the offer-index.
- Aggregate statistics mode (counts by mode/device, stale-entry totals).