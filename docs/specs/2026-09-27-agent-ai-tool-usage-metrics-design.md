# agent-ai-tool.com usage metrics — design

**Date:** 2026-09-27
**Status:** Implemented on both sides (2026-09-28). `GET /v1/metrics` is public
and unauthenticated; the site consumes it and renders the banner, per-agent
badges, and the no-service, fallback, and stale states. The live section stays
absent until a deployed `VTESSERA_BASE_URL` is supplied.
**Related:** `2026-09-27-agent-ai-tool-design.md` (the site), `2026-09-26-a2a-marketplace-design.md` (the marketplace), `2026-09-27-phase3-cluster-aware-settlement-design.md`

## Summary

The directory should show which listed services are actually being used, not
only that they exist. This spec adds real, verifiable consumption figures to
agent-ai-tool.com, derived from trade records `vtessera` already keeps, plus
one new read-only endpoint that aggregates them.

Nothing calls `vtessera`. The site fetches once per build, exactly as it
already fetches `/v1/agents`.

## Scope

The work spans two repositories.

| Part | Lands in | Change |
|---|---|---|
| `GET /v1/metrics` | `vtessera` | One new read-only public route, one store method |
| Snapshot, banner, badges, logo | `douglasdemaio/agent-ai-tool` | New snapshot file, build-time fetch, render, assets |
| Cache lifecycle amendment | both | See "Cache lifecycle" below |

This document lives in the `vtessera` repository because the endpoint is
`vtessera` code, following the existing convention that the site spec is
specified here. It must be read alongside `2026-09-27-agent-ai-tool-design.md`,
which this spec amends.

## Goals

- Report delivered usage that is exact rather than estimated, and auditable
  against a source a reader can independently verify.
- Show per-listing usage so a visitor can tell which tools are exercised.
- Never show a number that cannot be substantiated, and never show a stale
  number without saying how stale it is.
- Keep the site publishable when `vtessera` is down, absent, or empty.

## Non-goals

- **No visitor analytics.** This adds marketplace activity figures derived from
  public ledger data. It adds no page-view tracking, no referrer capture, no
  IP or user-agent collection, and no third-party analytics script. The
  distinction is deliberate: the subject is trades, not visitors.
- No ranking. Listings are not ordered by usage; a badge is not a leaderboard.
- No per-buyer or per-seller identity on the site. See "What is not measurable".
- No on-chain analytics, and nothing that depends on Phase 3 landing first.

## What is measurable, and what is not

This is the honest ceiling of the feature, and it was established by reading
the code rather than assumed.

**Measurable — `vtessera`-mediated trades.** A trade row names the consumer
(`trades.buyer_agent_id`), the service (`trades.offer_id` to `offers.agent_id`),
the time (`receipts.created_at`) and the outcome (`trades.state`). `receipts`
has `trade_id TEXT NOT NULL UNIQUE`, so a count of issued tesserae is exact and
duplicate-proof. `GET /v1/ledger` already publishes this data unauthenticated.

**Not measurable — service invocation in general.** `vtessera` hosts no
services. Every registered agent supplies its own external URL
(`domain.AgentCard.URL`, stored in `agents.url`), and `POST /agp/route` returns
a synthetic identifier (`Squad_Marketplace/<agentID>/offer/<offerID>`, built at
`agp/announcement.go:92-94`) that no `vtessera` server serves. Execution happens
at the third party's endpoint and `vtessera` is never informed.

Consequently:

- A listing for a non-`vtessera` service can never report usage. The badge
  reflects only trades that ran through `vtessera`.
- The A2A `tasks/*` JSON-RPC lifecycle promised at
  `2026-09-26-a2a-marketplace-design.md:69` and `:216` was never implemented in
  any phase, and `POST /agp/route` performs no writes. There is no other
  consumption signal in the codebase to capture.
- Extending coverage to third-party services would require a new opt-in
  callback protocol, with its own host, auth model and anti-spam story. That is
  deliberately out of scope and would need its own spec.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Source of truth | `trades` joined to `receipts` | Already exact, already indexed, already public. No new table, no new write path, no migration. |
| Endpoint | New read-only `GET /v1/metrics` | The site cannot derive per-service figures from `/v1/ledger` alone: the ledger carries `offerId` and pubkeys but no agent names, and there is no batch route to resolve them. Aggregating server-side avoids an N+1 fetch per trade. |
| Auth | None, like `/v1/ledger` | The underlying data is already public. Requiring auth would imply a secrecy the data does not have. |
| What counts as delivered | A trade holding a `receipts` row | Tesserae are only issued for `recorded` and `settled` (`ledger/ledger.go:92-94`), so this is the authoritative predicate and cannot drift from the state machine. |
| Failures shown | `disputed` and `cancelled` beside `delivered` | Both are `Terminal()` but are failures. Showing them keeps the denominator honest and stops the headline number reading as cherry-picked. |
| Interest signals | Excluded | `proposed` and `negotiating` are intent, not consumption. Two adjacent numbers invite being conflated. |
| Grain of the per-listing figure | Per agent | Usage is recorded per offer, but the directory lists agents. Summing an agent's offers keeps the endpoint aligned with the listing model. Cost: cannot see which individual offer is popular, which the directory does not surface anyway. |
| Snapshot file | Separate from the agents cache | A metrics failure must not take the listings down, and "hide until first success" has to hold per feed. |
| Cache lifecycle | Scheduled job commits the snapshot | See below. |
| Banner placement | Full-width banner under the header | Traction is the first thing a visitor sees. |
| Listing badge floor | Suppress below 3 delivered | A listing must never advertise a single trade. |
| Zero state | Hide until first successful fetch, then show zeros | Distinguishes "we do not know yet" from "we know, and it is zero". |
| Staleness | Always visible; degraded styling past 14 days | A stale number styled like a fresh one is the failure mode that actually costs credibility. |
| Logo | `logo.svg` primary, `logo.png` for raster consumers | Already exists in `vtessera` at repo root; must be copied into the site repo. |

## Rejected alternatives

- **Ping/callback endpoints from consuming agents.** The original request. It
  would cover third-party listings, which the ledger cannot reach, but `vtessera`
  hosts no services, so it would need a new protocol, a host that can receive
  requests (GitHub Pages cannot), an auth model, and an anti-spam story. It also
  contradicts the site's zero-instrumentation posture. Deferred, not rejected on
  merit; the ledger path ships first as the honest baseline.
- **Deriving metrics from `GET /v1/ledger` in the site build.** Needs one
  `GET /v1/offers/{id}` plus one `GET /v1/agents/{id}` per trade to recover a
  service name, and the endpoint returns no names at all. Correct but needlessly
  fragile.
- **A `usage_events` table populated by a new middleware.** Would capture browse
  activity, which is not consumption, and would add a write path, a migration and
  a privacy surface to answer a question nobody asked. Browse traffic is
  currently unrecorded by design and stays that way.
- **Showing a usage badge on listings with no `vtessera` data.** Would imply the
  figure covers the service's real usage, which is false.
- **Counting `proposed` as usage.** Inflates the headline with intent.

## The endpoint

`GET /v1/metrics`, unauthenticated, read-only.

```json
{
  "generatedAt": "2026-09-27T18:00:04Z",
  "asOf": "2026-09-27T17:59:12Z",
  "totals":  { "delivered": 47, "disputed": 4, "cancelled": 6,
               "consumers": 19, "services": 6 },
  "agents":  [ { "agentId": "…", "delivered": 31, "disputed": 2, "cancelled": 1 } ]
}
```

### Counting rules

Stated explicitly so they cannot drift from the implementation.

- `delivered` — a trade **has a `receipts` row**. Not a re-derivation from
  `state`.
- `disputed`, `cancelled` — `trades.state` equals that value.
- `consumers` — `COUNT(DISTINCT buyer_agent_id)` over delivered trades.
- `services` — `COUNT(DISTINCT offer_id)` over delivered trades.
- `asOf` — the newest `receipts.created_at`. This is when the most recent
  delivery happened, which is not the same as when the endpoint answered.
- `generatedAt` — when the response was produced.

`trades.updated_at` is **not** used for any figure. `SetTradeState` bumps it on
every transition, so it is a transition time, not a completion time.

### The per-agent breakdown

The per-agent figures are per `offers.agent_id`, and they must be reachable by
two different joins for two different reasons. An agent appears if it has **any
delivery or any failure**, which requires a `LEFT JOIN` on `receipts` plus a
`WHERE` admitting `disputed` or `cancelled`. An inner join would silently omit
an agent whose every trade failed, and the per-agent counts would stop summing
to the totals. The failure mode is a plausible-looking number, not an error, so
it is pinned by a test.

The three counts are **mutually exclusive and partition the terminal trades**,
which follows from the state machine rather than from convention: `recorded`
and `settled` are terminal with no outgoing transitions, and they are the only
states for which a tessera is issued. A trade that holds a receipt therefore can
never go on to be `disputed` or `cancelled`. This is why the `LEFT JOIN` cannot
double-count, and it is asserted so that a future change to the transition map
cannot quietly invalidate the arithmetic.

### Deliberately absent

No buyer or seller pubkey, no amount, no description, no mint. Aggregates only.
`GET /v1/ledger` already exposes per-trade detail publicly, so this adds no new
exposure, but the aggregate shape means the site never handles identity at all
and the badge floor stays a rendering concern rather than a privacy one.

### Index support

`trades_buyer_idx (buyer_agent_id, state)`, `trades_seller_idx`,
`trades_offer_idx`, and `receipts.trade_id UNIQUE`
(`internal/store/schema.sql:51-53`, `:111`) already cover this query. No new
index is required. A rollup keyed only on `trade_events.created_at` or
`trade_events.type` would scan, so it is not used.

## Data flow

```
vtessera GET /v1/metrics ──fetch at build──▶ internal/live ──▶ content/live-metrics.json ──▶ render ──▶ public/
                                                              (committed, last successful)
```

`internal/live/` is the existing unit that already fetches `/v1/agents`. It
gains a second feed. It renders; it does not aggregate.

## Cache lifecycle

`2026-09-27-agent-ai-tool-design.md` was self-contradictory as written. In one
section it declared `content/live-vtessera.json` a committed file; in another
it stated that CI writes no commits; and in a third it expected a weekly cron to
refresh that committed file. Under the Pages deploy model there is no mechanism
and no permission for a scheduled run to write a refreshed snapshot back, so the
cache was frozen at whatever a human last committed by hand. That made the
cache-age display meaningless, because the age could only grow, and made the
claim that rebuilds are diff-clean wrong, because nothing could change.

That spec is amended alongside this one. The contradiction is described here
without line numbers on purpose, so that neither document has to be re-edited
when the other shifts.

Resolution, amending that spec:

- A separate scheduled refresh job runs with `contents: write`, fetches both
  feeds, and commits the snapshots.
- A path filter on all push triggers excludes the snapshot files, so the
  refresh commit cannot re-trigger a build. This is the loop-closure rule; the
  old spec had no equivalent, which is why the contradiction went unnoticed.
- The Pages deploy job keeps `contents: read` and still writes no commits.
  Line 128 is amended to say so explicitly rather than in general terms.
- Line 70's "(committed)" becomes true, and both snapshots now genuinely
  advance.

## Presentation

A full-width banner directly under the header, carrying delivered, distinct
consumers, services in use, and the disputed/cancelled breakdown. Below it,
listings each carry a `N delivered` badge where `N >= 3`; below the floor the
badge is omitted rather than shown as zero.

The whole block is omitted when no snapshot has ever been fetched. It renders
with zeros once a snapshot exists — that zero is informative, because it means
`vtessera` answered and nothing has completed yet.

The banner always shows the snapshot's age. Past 14 days it renders degraded
with an explicit "usage data is N days old" line.

## Error handling

Four cases, and in all of them the site still publishes.

1. **Fetch fails** (timeout, non-200, malformed, empty) — fall back to
   `content/live-metrics.json`, mark the banner stale with the snapshot's age,
   warn in the build log, succeed. Someone else's downtime never blocks
   publishing.
2. **No snapshot has ever been fetched** — hide the banner and every badge. No
   "0", no empty-state chrome. The site looks like an ordinary directory. This
   is the expected first-deploy state.
3. **Snapshot present, `delivered` is 0** — render the banner with zeros.
4. **A listing's count is below the floor** — omit that badge.

## Security

The site spec already treats a live-fetched file as untrusted network input and
commits to a test that attempts script injection through a live-fetched field.
`/v1/metrics` inherits that treatment.

Every value in the response is either a number or a validated 32-byte base58
pubkey, and the response is rendered, never interpreted. `agentId` is a join
key into listings that are already rendered; it is never emitted as markup and
never displayed to a visitor.

The endpoint accepts no input, opens no file handle, and performs no write. It
is the same class of exposure as the existing `GET /v1/ledger`.

## Logo

`logo.svg` (1.3 KB, 256×256 squircle, dark gradient tile with a green mark) and
`logo.png` (512×512 RGBA) already exist in the `vtessera` repository root.

- `logo.svg` is the web asset. It hardcodes `width="256" height="256"`, so it
  must be sized explicitly by CSS or an attribute rather than assumed.
- `logo.png` covers `favicon.ico`, `og:image`, and any consumer that cannot
  render SVG.
- The mark is a self-contained dark tile with its background baked in, so on a
  light header it reads as a dark chip. That is intentional.
- It appears in the header in place of the plain wordmark and as the banner's
  left anchor.
- Both files must be **copied** into the site repository's assets. The site is a
  separate repository and cannot reference them across the boundary.

## Testing

### vtessera

- The aggregation query against a seeded fixture with a known number of trades
  per state, asserting `delivered`, `disputed`, `cancelled`, `consumers` and
  `services` exactly. Pure and hermetic; this is the test that matters most.
- A second tessera issue attempt for the same trade does not double-count,
  pinning the `receipts.trade_id UNIQUE` property the counting rule relies on.
- `GET /v1/metrics` returns 200 with the documented shape and **no credentials
  required**. A test asserting it needs no auth prevents a later well-meaning
  change from adding a requirement.
- `asOf` equals the newest receipt timestamp, and does not move when only a
  state transition occurs.

### Site

- Each degradation case: no snapshot hides the banner; zero delivered renders
  zeros; a below-floor listing gets no badge; a stale snapshot renders the
  degraded style with its age.
- A malformed or unreachable `/v1/metrics` still lets the build succeed and
  render from cache.
- Injection through a fetched field cannot produce markup, extending the test
  the site spec already commits to for the agents feed.
- **A snapshot committed by the refresh job is consumed by the next build with
  no manual step.** This is the important one: the original contradiction
  existed because nothing tested that the cache could ever advance.

## Ordering

1. `GET /v1/metrics` in `vtessera`, with its tests.
2. Cache-lifecycle amendment in the site spec.
3. Site repository, including the snapshot, banner, badges, logo and the
   refresh job.

Step 3 is blocked on the site repository existing, which is blocked on the
apex `A` records. Step 1 is not blocked and can land first. The site ships
with the usage block hidden, which is a supported state, so nothing waits on
`vtessera` being deployed.

## Open items

- Confirm the 14-day staleness threshold against the actual cron cadence once
  the site has run for a month.
- Decide whether the per-listing badge floor of 3 should rise as volume grows,
  or be replaced by a relative rule.
- Reconsider a ping protocol for third-party listings once there is real
  volume to justify it.
- Confirm which `vtessera` fields to surface on listings, unchanged from the
  site spec's open items.
