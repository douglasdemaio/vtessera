# Failure and Dispute Classification — Draft

**Date:** 2026-10-08
**Status:** Draft for review. Originally documentation only; gap 1 has since been fixed (2026-10-09) — `POST /v1/admin/trades/{id}/resolve` closes a dispute with an operator-recorded verdict, and both §5.4 and §5.5 now have a handhold. The remaining gaps in §7 still need sign-off under Rule 2 before any fix.
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
| Expiry sweeper | `5m`, batch `100` | `accepted` trades whose acceptance deadline passed | `trade/` `RunExpirySweeper`, `trade.go:318-336`; `config.go:177-178` |
| Reconciler | `15s`, batch `50` | `settlement_pending` trades | `trade/reconcile.go:26-28,120-134` |

The acceptance deadline is **derived, not stored**. There is no deadline
column (`internal/store/schema.sql:35-48`); it is recomputed as
`min(first acceptance) + acceptTTL`, and it is only non-zero while the state
is `accepted` (`trade.go:244-256`, anchor read at `store/trades.go:95-108`).
The default TTL is **72h** (`config.go:176`), and the service refuses to boot
without one.

**What has no deadline at all** (this is the heart of §7):

- `proposed` and `negotiating` trades — never swept (`acceptanceDeadline`
  returns zero for non-`accepted`, `trade.go:245-247`; the sweep selects
  `state='accepted'` only, `store/trades.go:121`).
- `settlement_pending` trades — no trade-level clock; only the settlement
  *request*'s blockhash window (90s, `settlement.DefaultBlockhashTTL`).
- Open offers — the `offers` table has no expiry column
  (`schema.sql:16-33`).
- `disputed` trades — terminal forever.

## 4. How the buyer's budget actually works

There are no reservation rows. Exposure is *computed* on every check by
`store.CommittedSpendSince` (`internal/store/limits.go:108-128`):

- A trade counts against the daily cap **from the later of** when it was opened
  and when it first entered a committed state — the "engagement anchor"
  (`:112-115`).
- `committedStates` = `settlement_pending`, `recorded`, `settled`, `disputed`
  (`:71-76`) — note that `disputed` is charged.
- `releasedStates` = `cancelled` only (`:60`, `:119-124`).
- Trades in `proposed`/`negotiating`/`accepted` are *not* released; they simply
  age out of the rolling window once they fall outside it.

The window is 24h by default (`config.go:174`). The check runs in three places:
trade creation (`trade.go:532-538`, under `reserveMu`), the on-chain build
(the last moment before the buyer holds a signable transaction,
`trade/settlement.go:266`), and the off-chain commit (`trade.go:667`).

`GET /v1/limits` reports the **caps** only — `perTradeUsd`, `perDayUsd`,
`raised`, `currency` and any ceiling — and deliberately exposes **no spend or
reservation figure** (`httpapi/limits.go:16-30`).

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
  `proposed`/`negotiating` and stays there. Its offers stay `open` and
  discoverable forever.
- **Buyer's budget:** charged from `created_at` and ages out of the window
  after 24h (`limits.go:112-115`); it is not "released" — no cancellation
  happened.
- **Public stats:** invisible. It is neither `disputed` nor `cancelled`, and it
  has no receipt, so `/v1/metrics` does not count it.
- **Who resolves it:** the buyer, by hand, with `Cancel` — always available
  before acceptance (`trade.go:572-605`; the deadline gate only exists inside
  `if tr.State == accepted`, `:586`).

### 5.2 Seller never accepts

Mechanically the same as 5.1 for the state, with one addition worth stating: a
trade needs **two** acceptances (`trade.go:619-646`), and the deadline anchors
on the **first** one (`store/trades.go:95-108`). So a seller who never accepts
leaves the trade in `negotiating` with no deadline and nothing to sweep.

- **Trade state:** `negotiating`, indefinitely.
- **Buyer's budget:** charged, ages out of the window; not released.
- **Public stats:** no.
- **Who resolves it:** the buyer, by hand (`Cancel`), or an eventual deal.

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

Each needs approval under Rule 2 before any code moves. Gap 1 has since been
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
2. **`proposed` and `negotiating` never expire.** A buyer who opens a trade
   against a silent seller holds a hanging record forever; only the buyer's own
   `Cancel` clears it, and the reserved budget is bounded solely by the 24h
   window age-out, not by any deadline. (§5.1, §5.2.)
3. **`settlement_pending` has no trade-level deadline.** Only the per-request
   blockhash window (90s) bounds it; a buyer who abandons after a build leaves
   the reconciler examining a trade that nothing ever finishes. Cancellation is
   available to the buyer once the blockhash lapses, but it is manual.
4. **Offers never expire, and a failed probe changes nothing.** A silent
   seller's listing stays `open` and discoverable with no expiry column
   (`schema.sql:16-33`); the probe is the one signal an operator has, and it is
   explicitly not a consequence-bearing check (`registry.go:524-569`).
5. **A dispute outliving the cap window releases the buyer's budget.** Because
   `disputed` is charged only until it ages out of the 24h window
   (`limits.go:112-115`), a dispute open longer than a day no longer constrains
   the buyer at all. That may be acceptable — the cap is not KYC — but it is a
   decision, not an accident, and should be stated.
6. **No consumed-budget read.** `GET /v1/limits` shows caps, never spend or
   reservations (`httpapi/limits.go:16-30`); an operator cannot see how much of
   a buyer's day a hanging or disputed trade is holding without reading SQLite.
7. **Dispute detail is invisible above the parties.** The public sees counts;
   the reason lives in the trade event and is visible only to the two parties
   (`trade.go:692-696`). Operational/audit review of a dispute needs a
   deliberate DB read.
8. **`README.md:184` says the accepted-trade deadline is "24 hours by
   default"; the flag table (`:235`) and the code (`config.go:176`) say 72h.**
   The prose is stale. (Corrected in the same change as this draft.)
