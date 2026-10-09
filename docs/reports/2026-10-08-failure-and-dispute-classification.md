# Failure and Dispute Classification — Draft

**Date:** 2026-10-08
**Status:** Draft for review. Gap 1 was fixed on 2026-10-09 (`POST /v1/admin/trades/{id}/resolve` closes a dispute with an operator-recorded verdict); gaps 2 and 3 were fixed on 2026-10-09 as well (open trades and abandoned settlement builds now expire and release their reservation); gap 4's expiry half, gap 6 and gap 7 were fixed the same day. Gap 4's failed-probe half, gap 5 and gap 8 are the remaining decisions, and gap 5 still needs sign-off under Rule 2.
**Method:** every claim below was read out of the code at the cited `file:line`, as of the report's date. The gap 1 fix added lines to `trade.go` and `domain.go`, so some citations there have drifted; the prose, not the bare number, is what to trust.
**Operating assumption:** disputes are resolved by the operator by hand. There is no automated arbitration, and this document does not propose any.

## 1. Purpose

When a trade goes wrong, four questions matter to the buyer and to an
operator: what state does the trade end in, what happens to the budget the
buyer reserved, does it show up as "disputed" in the public stats, and who
resolves it. This draft answers those four questions for the five failure
shapes in the item, so that the honest answer — including "nobody, today" —
is written down rather than assumed.

## 2. The lifecycle these failures move through

States (`internal/domain/domain.go:341-352`):

`proposed` → `negotiating` → `accepted` → `settlement_pending` → `settled`,
plus `recorded` (the off-chain commit), and the terminal `disputed` and
`cancelled`. `recorded`, `settled`, `disputed` and `cancelled` are terminal
(`:363-369`).

The transitions that matter here (`internal/trade/trade.go:434-457`,
`canTransition` at `:459-466`):

| From | May move to |
|---|---|
| `proposed` | `negotiating`, `cancelled` |
| `negotiating` | `accepted`, `cancelled` |
| `accepted` | `settlement_pending`, `recorded`, `disputed`, `cancelled` |
| `settlement_pending` | `settled`, `disputed`, `cancelled` |
| `recorded` / `settled` / `disputed` / `cancelled` | *nothing* |

Three consequences fall straight out of that table and run through every case
below:

- **Off-chain, a trade can only be disputed before it is `recorded`.** Once the
  buyer records, the trade is terminal and `disputed` is no longer reachable.
- **On-chain, a dispute can arrive from `settlement_pending`** — that is the
  path the automatic mismatch check uses (§5.5).
- **`disputed` has no outgoing edge.** Nothing in the service moves a trade out
  of it.

## 3. Deadlines and background workers

There are two background tickers in the whole service, and neither does what a
reader might expect (`cmd/vtessera/main.go:211-228`):

| Worker | Cadence | Applies to | Source |
|---|---|---|---|
| Expiry sweeper | `5m`, batch `100` | `proposed`/`negotiating` trades past the open deadline, and `accepted` trades whose acceptance deadline passed | `trade/` `RunExpirySweeper`, `trade.go` `ExpireAccepted`/`ExpireOpen`; `config.go` open/accept TTLs |
| Reconciler | `15s`, batch `50` | `settlement_pending` trades, including cancelling builds whose blockhash has lapsed | `trade/reconcile.go` |

The acceptance deadline is **derived, not stored**. There is no deadline
column (`internal/store/schema.sql`); it is recomputed as
`min(first acceptance) + acceptTTL`, and it is only non-zero while the state
is `accepted` (`trade.go` `acceptanceDeadline`, anchor read at
`store/trades.go` `TradeAcceptance`).
The default acceptance TTL is **72h** (`config.go:176`); the open-trade default
is **24h** (`--trade-open-ttl`), measured from the last move so a trade being
negotiated is not swept out from under the parties. The service refuses to boot
without either.

**What has no deadline now** (the heart of §7):

- Open offers — bounded by `offers.expires_at` (migration 6) and the offer
  sweep (`--offer-ttl`, default `24h`).
- `disputed` trades — terminal forever until the operator resolves them (§5.4/§5.5).

`settlement_pending` gained a layer rather than a column: the reconciler cancels
a build whose request is still `issued` and whose blockhash window (90s,
`settlement.DefaultBlockhashTTL`) has lapsed, so the trade no longer hangs. A
build the chain resolved as a failed execution stays `settlement_pending` so the
buyer may rebuild.

## 4. How the buyer's budget actually works

There are no reservation rows. Exposure is *computed* on every check by
`store.CommittedSpendSince` (`internal/store/limits.go:108-128`):

- A trade counts against the daily cap **from the later of** when it was opened
  and when it first entered a committed state — the "engagement anchor"
  (`:112-115`).
- `committedStates` = `settlement_pending`, `recorded`, `settled`, `disputed`
  (`:71-76`) — note that `disputed` is charged.
- `releasedStates` = `cancelled`, `resolved` (`:60`, `:119-124`) — a resolved dispute releases the reservation even though it still counts as `disputed` in `/v1/metrics`.
- Trades in `proposed`/`negotiating`/`accepted` are *not* released; they simply
  age out of the rolling window once they fall outside it.

The window is 24h by default (`config.go:174`). The check runs in three places:
trade creation (`trade.go:532-538`, under `reserveMu`), the on-chain build
(the last moment before the buyer holds a signable transaction,
`trade/settlement.go:266`), and the off-chain commit (`trade.go:667`).

`GET /v1/limits` reports the **caps** — `perTradeUsd`, `perDayUsd`,
`raised`, `currency` and any ceiling — and, since gap 6 was fixed, the buyer's
own consumed budget: `committedUsd` (everything the rolling window is holding)
and `remainingUsd` (`perDayUsd` minus committed, floored at zero), plus the
`window`. It is the caller's own figure, resolved from the session, and a
cancelled or resolved trade releases its reservation as before
(`httpapi/limits.go`).

## 5. The five cases

### 5.1 Seller unreachable or timing out

The service has no notion of a seller being reachable *during* a trade. The
only liveness gate is at creation: both parties must be `AgentActive`
(`trade.go:506-523` → `409 INVALID_REQUEST` via `ErrAgentUnavailable`).
Capability probes are operator-triggered (`POST /v1/admin/agents/{id}/probe`,
`server.go:128-133`) and a failed probe **changes nothing** — it does not
change `agents.status`, close an offer, or touch a trade
(`registry/registry.go:524-569`).

- **Trade state:** a trade with an unreachable seller sits in
  `proposed`/`negotiating` until the open deadline (`24h` by default) passes;
  the sweeper then cancels it (`event=expired`, actor `""`). Its offer now
  closes after the offer TTL (`--offer-ttl`, default `24h`), so a silent seller
  also drops out of discovery.
- **Buyer's budget:** charged from `created_at`, and **released** when the
  sweeper cancels the stale trade (`cancelled` is a released state); under the
  open deadline a trade that simply ages out of the window stops counting too.
- **Public stats:** invisible until the sweep, then counted as `cancelled`.
- **Who resolves it:** the service, automatically, once the open deadline
  passes. The buyer may still `Cancel` by hand at any time before acceptance.

### 5.2 Seller never accepts

Mechanically the same as 5.1 for the state, with one addition worth stating: a
trade needs **two** acceptances (`trade.go:619-646`), and the deadline anchors
on the **first** one (`store/trades.go` `TradeAcceptance`). A seller who never
accepts therefore leaves the trade in `negotiating` until the open deadline
passes; the sweeper then cancels it, so it is no longer deadline-free. The open
deadline is measured from the trade's last move, so a negotiation that is still
active is not swept.

- **Trade state:** `negotiating`, until the open deadline; then `cancelled`.
- **Buyer's budget:** charged, then released by the sweep.
- **Public stats:** no until the sweep, then `cancelled`.
- **Who resolves it:** the service, automatically; or the buyer by hand
  (`Cancel`), or an eventual deal.

### 5.3 Seller accepts and stalls past the deadline

Now the deadline exists: once the first acceptance lands, the trade is
`accepted`, and `accepted` requires cancellation to wait for
`min(acceptance) + 72h` (`trade.go:244-256`). Before then, cancelling returns
`409 TRADE_NOT_EXPIRED` (`trade.go:586-603`, `ErrNotExpiredYet`).

When the deadline passes, the sweeper cancels it automatically: state
`cancelled`, event `expired`, actor `""`
(`trade.go:282-313`). Either party may also cancel it by hand once it has
passed.

- **Trade state:** `cancelled` (by the sweeper, or by hand).
- **Buyer's budget:** **released**, immediately — `cancelled` is a released
  state (`limits.go:60,119-124`).
- **Public stats:** counted as `cancelled`, not `disputed`
  (`store/metrics.go:14`).
- **Who resolves it:** the service, automatically. No human needed.

### 5.4 Work delivered but contested by the buyer

An off-chain dispute is filed by a party with
`POST /v1/trades/{id}/dispute` (`server.go:154`, `handleDispute` `:740-751`),
reachable only from `accepted` or `settlement_pending` (`trade.go:434-457`).
The reason is optional and is stored as the event's detail
(`trade.go:608-617`, `reasonDetail` at `:775-780`); it is idempotent — filing
twice returns the existing dispute (`:613-615`).

The timing is the point: because `recorded` is terminal, "delivered but
contested" means contested **before the buyer records the off-chain commit**.
After a successful `Record` there is no route to `disputed`.

- **Trade state:** `disputed`, terminal. No tessera is issued and nothing
  reaches the ledger — the off-chain record simply never happens.
- **Buyer's budget:** still charged; `disputed` is a committed state
  (`limits.go:71-76`). It ages out of the 24h window like any committed trade,
  so a dispute open longer than the window stops constraining the buyer while
  the trade remains disputed.
- **Public stats:** `disputed` (`store/metrics.go:13`), attributed to the
  **seller's** agent via the offer join (`:21-32`).
- **Who resolves it:** the operator, by hand, now with a handhold:
  `POST /v1/admin/trades/{id}/resolve` (operator token) records a verdict and
  closes the trade to `resolved` — see §7.1. Parties can see the dispute and its
  reason through `GET /v1/trades/{id}` (events attached in `Service.Get`); the
  public sees only counts.

### 5.5 On-chain settlement mismatch

Both the buyer's `POST /v1/trades/{id}/confirm` and the reconciler funnel into
the same function, `settleFetched` (`trade/reconcile.go:221-265`), specifically
so a trade "can never settle by one path and dispute by the other"
(`reconcile.go:222-224`). A verification mismatch there calls
`apply(..., TradeDisputed, disputeDetail(...))` (`:240-248`).

- **Trade state:** `disputed`, terminal. No receipt, no ledger entry.
- **Buyer's budget:** charged (`disputed` is committed).
- **Public stats:** `disputed`.
- **Who resolves it:** the operator, by hand, via the same
  `POST /v1/admin/trades/{id}/resolve` as §5.4 — the gap 5.4 named is closed for
  both.

The taxonomy around it is deliberate and mostly distinguishes failure from
dispute correctly:

| Condition | Result | Source |
|---|---|---|
| Transaction landed but execution failed | requests expire; trade **stays `settlement_pending`**; buyer may rebuild. *Not* a dispute | `reconcile.go:230-234` |
| Signature not yet visible | `202 SETTLEMENT_PENDING`; state unchanged | `settlement.go:375,386` |
| RPC/chain unreachable | trade left pending — "an RPC outage must not be recorded as a dispute" | `reconcile.go:68,95-99` |
| Request compiled for another cluster | refused before any verdict (`CLUSTER_MISMATCH`) | `reconcile.go:227-229,277` |
| Cancel while a blockhash is live | `409 SETTLEMENT_IN_PROGRESS` | `settlement.go:26`, `server.go:1119` |

So a *transaction* failure does not become a dispute, but a *verification
mismatch* automatically does, and both the mismatch and a party-filed dispute
land in a state nothing can leave.

## 6. The existing disputed trade

**Facts (public surface, polled 2026-10-08 from `GET /v1/metrics`):**

- Totals: `delivered 2`, `disputed 1`, `cancelled 0`, `consumers 2`,
  `services 2`; `asOf 2026-09-29T18:46:29Z`.
- One agent row: `Ax1YCpc9L35TRAHgGN5CUjPe3BxT6bVsjq2xci5hPCgV` with
  `delivered 2`, `disputed 1`, `cancelled 0`.
- That agent is `opencode-probe-agent`, described in its card as a "Temporary
  probe agent used to verify the vtessera marketplace write path", created
  `2026-09-29T15:28:57Z`. `docs/deploy.md:545-548` records that the write path
  was verified with three throwaway probe registrations.

**Inference (from the deployment timeline, not from the trade record):** on-chain
settlement was not enabled against mainnet until **2026-10-01** (AGENTS.md
constraint 4). On 2026-09-29 a `/v1/trades/{id}/confirm` would therefore have
been refused `501 ONCHAIN_UNAVAILABLE`, so this dispute cannot be the automatic
settlement-mismatch dispute of §5.5. It was **party-filed** through
`POST /v1/trades/{id}/dispute` during the manual write-path verification.

**Not knowable from outside the service:** the parties, the reason, and the
exact timestamps. Trade routes are party-gated (`GET /v1/trades/{id}` requires
the buyer's or seller's session), and the public surface is aggregates only.
Reviewing this dispute in detail means reading the volume's SQLite database on
the machine — and the runtime image carries no `sqlite3` (`deploy.md:556-557`),
so even that is a deliberate operation rather than a query.

## 7. Gaps found

Each needs approval under Rule 2 before any code moves. Gaps 1–3 have since been
fixed; the rest are open.

1. ~~**`disputed` is terminal with no resolution path.**~~ **Fixed 2026-10-09.**
   The design said "terminal pending operator review"
   (`docs/specs/2026-09-26-a2a-marketplace-design.md:137`), but the review step
   was never built: no admin route, no outgoing transition (`trade.go:434-457`),
   and no `sqlite3` in the runtime image. `POST /v1/admin/trades/{id}/resolve`,
   behind the operator token and not an agent session, now moves a disputed trade
   to the terminal `resolved` state and records a verdict (`released` or
   `upheld`) and a reason on the resolving event. Both outcomes release the
   buyer's reservation, and a resolved dispute still counts as `disputed` in
   `/v1/metrics` so a review cannot bury it. This was the gap that mattered most,
   because §5.4 and §5.5 both ended there.
2. ~~**`proposed` and `negotiating` never expire.**~~ **Fixed 2026-10-09.**
   A buyer who opens a trade against a silent seller held a hanging record
   forever. `--trade-open-ttl` (default `24h`, measured from the trade's last
   move) now bounds it: `ExpireOpen` cancels a stale open trade with
   `event=expired`, releasing the buyer's reservation. The service refuses to
   boot without a positive open deadline, for the same reason it refuses without
   an acceptance one. (§5.1, §5.2.)
3. ~~**`settlement_pending` has no trade-level deadline.**~~ **Fixed 2026-10-09.**
   The reconciler now cancels a trade whose settlement request is still `issued`
   and whose blockhash window (90s) has lapsed, and expires the request so its
   partial unique index is released. A request the chain resolved as a failed
   execution is left `settlement_pending` so the buyer may rebuild. The buyer is
   no longer the only one who can clear an abandoned build.
4. ~~**Offers never expire.**~~ **Fixed 2026-10-09.** A published offer now
   carries `offers.expires_at` (migration 6), set from `--offer-ttl` (default
   `24h`), and the offer sweep closes it once the deadline passes, so a silent
   seller drops out of discovery and search. The deadline is deliberately not
   part of the attested offer bytes — it is marketplace policy, not a seller
   term — and existing rows are backfilled from their creation time.
   **The second half is unchanged and still open: a failed probe changes
   nothing.** The probe is the one signal an operator has, and it is explicitly
   not a consequence-bearing check (`registry.go:524-569`); whether a failed
   probe should close a listing is a separate decision, not taken here.
5. **A dispute outliving the cap window releases the buyer's budget.** Because
   `disputed` is charged only until it ages out of the 24h window
   (`limits.go:112-115`), a dispute open longer than a day no longer constrains
   the buyer at all. That may be acceptable — the cap is not KYC — but it is a
   decision, not an accident, and should be stated.
6. ~~**No consumed-budget read.**~~ **Fixed 2026-10-09.** `GET /v1/limits`
   now reports the caller's own `committedUsd` and `remainingUsd` for the
   rolling `window`, priced at USD micro precision like the `SPEND_CAP_EXCEEDED`
   refusal, so an agent can see what a hanging or disputed trade is holding
   without reading SQLite. The figure is the session's own agent, and a
   cancelled or resolved trade releases its reservation.
7. ~~**Dispute detail is invisible above the parties.**~~ **Fixed 2026-10-09.**
   The public saw counts and the reason lived in the trade event, visible only to
   the two parties; operational review meant a deliberate DB read. `GET
   /v1/admin/trades/{id}`, behind the operator token and not a session, now
   returns the trade and its full event history — the reason, the verdict, and who
   did what — so a review is a route rather than a `sqlite3` session. The parties'
   own scoped view through `GET /v1/trades/{id}` is unchanged; this adds no
   exposure to them, only reaches around the disagreement the operator exists to
   settle.
8. **`README.md:184` says the accepted-trade deadline is "24 hours by
   default"; the flag table (`:235`) and the code (`config.go:176`) say 72h.**
   The prose is stale. (Corrected in the same change as this draft.)
