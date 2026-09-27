# `GET /v1/metrics` implementation plan

**Date:** 2026-09-27
**Spec:** `../specs/2026-09-27-agent-ai-tool-usage-metrics-design.md`
**Status:** Ready to execute

## Scope

Step 1 of the spec's ordering only: the `vtessera` side. The site repository
does not exist and the apex `A` records are unconfirmed, so steps 2 and 3 are
blocked on work outside this repository. They are listed here only to record
what the endpoint is being built for.

Not in scope: any change to the counting rules, the response shape, or the
presentation. The spec settled those. This plan implements them.

## Verification

```bash
export TMPDIR="${TMPDIR:-$HOME/.cache/go-tmp}"
make          # fmt, vet, test, build
make race     # the aggregation is read-only but joins three tables
```

No validator needed. Everything here is hermetic SQLite.

## Task 1 — domain types

`internal/domain/domain.go`, or a new `internal/domain/metrics.go` if the file
is already long. The spec's response shape, verbatim:

```go
type UsageTotals struct {
	Delivered int `json:"delivered"`
	Disputed  int `json:"disputed"`
	Cancelled int `json:"cancelled"`
	Consumers  int `json:"consumers"`
	Services   int `json:"services"`
}

type AgentUsage struct {
	AgentID   string `json:"agentId"`
	Delivered int    `json:"delivered"`
	Disputed  int    `json:"disputed"`
	Cancelled int    `json:"cancelled"`
}

type UsageMetrics struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	AsOf        *time.Time   `json:"asOf"`
	Totals      UsageTotals  `json:"totals"`
	Agents      []AgentUsage `json:"agents"`
}
```

`AsOf` is a pointer so an empty marketplace serialises to `null` rather than
`0001-01-01T00:00:00Z`. The site must be able to tell "no deliveries have ever
happened" from "the last delivery was in 2019", and a zero `time.Time` is a
plausible-looking lie. `Agents` is a slice, never nil, so it serialises as `[]`
— the site's badge loop must not have to nil-check.

## Task 2 — the aggregation query

New file `internal/store/metrics.go`, one method:

```go
func (s *Store) UsageMetrics(ctx context.Context) (domain.UsageMetrics, error)
```

Two queries, then assemble. `receipts.trade_id` is `UNIQUE`, so the join is at
most one-to-one and `COUNT(r.trade_id)` is exact rather than an overcount. That
uniqueness is the whole basis of the counting rule and it is already enforced
by the schema.

Totals:

```sql
SELECT
  COUNT(r.trade_id)                                                 AS delivered,
  COUNT(CASE WHEN t.state = 'disputed'  THEN 1 END)                 AS disputed,
  COUNT(CASE WHEN t.state = 'cancelled' THEN 1 END)                 AS cancelled,
  COUNT(DISTINCT CASE WHEN r.trade_id IS NOT NULL THEN t.buyer_agent_id END) AS consumers,
  COUNT(DISTINCT CASE WHEN r.trade_id IS NOT NULL THEN t.offer_id     END) AS services,
  (SELECT MAX(created_at) FROM receipts)                            AS as_of
FROM trades t
LEFT JOIN receipts r ON r.trade_id = t.id
```

Per agent:

```sql
SELECT
  o.agent_id                                                        AS agent_id,
  COUNT(r.trade_id)                                                 AS delivered,
  COUNT(CASE WHEN t.state = 'disputed'  THEN 1 END)                 AS disputed,
  COUNT(CASE WHEN t.state = 'cancelled' THEN 1 END)                 AS cancelled
FROM trades t
JOIN offers o     ON o.id = t.offer_id
LEFT JOIN receipts r ON r.trade_id = t.id
WHERE r.trade_id IS NOT NULL
   OR t.state IN ('disputed', 'cancelled')
GROUP BY o.agent_id
ORDER BY delivered DESC, agent_id ASC
```

**The `LEFT JOIN` in the second query is load-bearing, not stylistic.** An agent
can have only failed trades and no deliveries at all. `disputed` is reachable
from `accepted` and `settlement_pending`, and `cancelled` from `proposed`,
`negotiating` and `settlement_pending` — every one of those paths passes
through a state that issues no receipt. An inner join would silently omit such
an agent, and the per-agent `disputed` and `cancelled` figures would not sum to
the totals. The `WHERE` clause admits any agent with a delivery *or* a
failure. Task 5 pins this with a test, because the failure mode is a
plausible-looking number rather than an error.

**A useful invariant falls out of the transition map.** `recorded` and `settled`
are both terminal and have no outgoing transitions, and receipts are issued for
exactly those two states. So a trade that holds a receipt can never go on to be
`disputed` or `cancelled`, which means `delivered`, `disputed` and `cancelled`
are **mutually exclusive** and partition the terminal trades. The `LEFT JOIN`
therefore cannot double-count: the row multiplier that would make it dangerous
does not exist here, because no row can satisfy both sides at once. This is
worth knowing before anyone "simplifies" either query, and it is asserted in
Task 5.

Timestamps follow the existing convention: `created_at` is `INTEGER` nanos,
read back through `fromNanos`. `as_of` is scanned as `sql.NullInt64` and left
nil when absent.

On an empty database both queries must succeed and return zeros. `COUNT`
returns a row of zeros over an empty table; nothing here is "no rows", so no
`sql.ErrNoRows` handling is needed on either. Run `mapErr` on execute errors
to match the rest of the package.

## Task 3 — service passthrough

`internal/trade/trade.go`:

- Add `UsageMetrics(ctx context.Context) (domain.UsageMetrics, error)` to the
  `Store` interface.
- Add `func (s *Service) UsageMetrics(ctx context.Context) (domain.UsageMetrics, error)`
  which calls it and sets `GeneratedAt` from the service's injected `now`.

Setting `GeneratedAt` in the service rather than the handler is deliberate: it
keeps the clock injectable, which is how the rest of the package stays
testable. Do not call `time.Now()` in the handler.

`*store.Store` already satisfies the widened interface, and `trade.New` takes
no new argument, so no wiring changes in `cmd/` or `main.go`. The trade tests
use the real store against a temp database rather than a fake, so there is no
mock to update either. This is why the method goes on the existing `Store`
interface rather than a new one.

## Task 4 — the route

`internal/httpapi/server.go`:

```go
s.mux.HandleFunc("GET /v1/metrics", s.handleMetrics)
```

Place it beside the other public reads, next to `/v1/ledger`. The handler:

```go
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.trades.UsageMetrics(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
```

**Not wrapped in `s.authed`.** The underlying data is already public through
`GET /v1/ledger`, and requiring a credential would imply a secrecy these
aggregates do not have. The spec requires a test that the route answers with no
credentials, specifically to stop a later well-meaning change from adding one.

## Task 5 — store tests

New `internal/store/metrics_test.go`, using the existing `testStore(t)`
helper. Seed a known shape and assert exact numbers:

| Trade | State | Receipt | Counts as |
|---|---|---|---|
| t1 | `recorded` | yes | delivered, consumer A, service 1 |
| t2 | `settled` | yes | delivered, consumer B, service 2 |
| t3 | `recorded` | yes | delivered, consumer A again, service 1 again |
| t4 | `disputed` | no | disputed only — **no receipt** |
| t5 | `cancelled` | no | cancelled only |
| t6 | `negotiating` | no | nothing |

Expect `delivered 3`, `disputed 1`, `cancelled 1`, `consumers 2`,
`services 2`. t3 is the case that catches a double-count: consumer A and
service 1 must not be counted twice.

Then:

- **`asOf` equals the newest receipt timestamp**, and specifically does *not*
  move when only a state transition occurs. `SetTradeState` bumps
  `trades.updated_at` on every transition, which is the trap; the spec forbids
  using it.
- **Per-agent `disputed` and `cancelled` sum to the totals.** This is the
  `LEFT JOIN` regression guard from Task 2. t4 and t5 belong to an agent with
  zero deliveries, and that agent must still appear in the array.
- **The three counts partition the terminal trades** —
  `delivered + disputed + cancelled` equals the number of trades in a terminal
  state, and no trade appears in two of them. This follows from `recorded` and
  `settled` being terminal, and it is the cheapest way to catch a future change
  to the transition map that would let a delivered trade also be disputed.
- **A second receipt attempt for one trade does not change the counts**, pinning
  the `receipts.trade_id UNIQUE` property the counting rule leans on. The
  insert will fail on the constraint, so assert the error and the unchanged
  counts.
- **An empty store returns zeros, no error, and a nil `AsOf`** — the
  first-deploy state the site depends on.

## Task 6 — HTTP test

`internal/httpapi/server_test.go`, following the existing fixture style:

- `GET /v1/metrics` with **no credentials returns 200**. Decode into
  `domain.UsageMetrics` and compare against the store's value.
- The `totals` and `agents` keys are present in the raw body, so a future
  `omitempty` cannot silently drop them from the site's contract.
- An empty marketplace returns `200` with `"asOf": null`, not a zero timestamp.

## Task 7 — documentation

`AGENTS.md` requires the relevant document to change in the same change as the
behaviour.

- The metrics spec: mark the `vtessera` step implemented, and record that the
  per-agent query uses `LEFT JOIN` with the reason, so a future "simplification"
  does not reintroduce the undercount.
- `2026-09-27-agent-ai-tool-design.md`: the open item noting that usage metrics
  depend on `GET /v1/metrics` becomes satisfied; the site work remains.
- The Phase 2 settlement report lists the routes the service serves. Add the
  route to that list.

## Notes for the site work, when it is unblocked

Two things this plan surfaces that the site spec does not yet spell out:

- **Staleness is the age of the fetch, not the age of `asOf`.** A marketplace
  that is simply quiet has an old `asOf` and a perfectly fresh snapshot.
  Deriving the 14-day staleness rule from `asOf` would mark a healthy quiet
  marketplace as broken. The snapshot's own age is what the banner shows.
- **`asOf` can legitimately be `null`.** The site's "no data yet" handling must
  branch on that, not on a zero value.

## Commit

One commit for Tasks 1-4, one for Tasks 5-6, so a failing test is attributable.
Task 7 amends the docs. Do not commit `.superpowers/`, which is untracked and
not in `.gitignore`.
