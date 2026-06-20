# Vtessera — VTESS Token Design (Voted Multi-Asset Reserve, authoritative)

> Supersedes the rough notes in `TOKENOMICS.md` and the earlier layered-reserve
> draft of this file. This is the agreed model.
> Still a **later phase** than the Phase-0 package; see `ROADMAP.md`.
> Nothing here is legal or investment advice — see "Legal" at the end.

## Model in one paragraph

VTESS is a **fixed-supply** Solana SPL token used as the utility/settlement and
governance token for the Vtessera GNU/Linux compute marketplace. There is **no
mint-on-use and no ongoing inflation** — the cap is created once and the mint
authority is revoked. The token is anchored by a **voted, transparent
multi-asset reserve** funded from real buyer payments. The reserve has two
parts: a **protected EURC stability floor** that the vote cannot drop below,
and an **above-floor diversification basket** (SOL, BTC, EURC, USDC) whose
weights holders set by **biannual vote**. Compute is priced in euros and
settled in VTESS at an oracle rate once liquidity is deep enough; until then
it settles directly in EURC.

## 1. Supply & distribution (fixed, no inflation)

- **Total supply: 20,000,000 VTESS, created once.**
- **15,000,000 VTESS** seeded into an EURC/VTESS pool on **Raydium** (Solana)
  at launch. The pool is a **constant-product (CPMM) Standard AMM pool**, not
  concentrated liquidity. Rationale in §1.1.
- **5,000,000 VTESS** placed in a **permanently locked, non-circulating
  treasury**. These tokens never vest, never enter circulation, and never
  vote. They exist only as a permanent supply anchor (see §1.2).
- **Zero insider, founder, team, or VC allocation.** No time-locked team
  slice, no advisor grants, no investor SAFTs. The only on-chain VTESS in
  human hands at launch is what comes through the seed pool.
- **Revoke the SPL mint authority after creation.** This makes the 20M cap a
  cryptographic guarantee, not a promise. **Revoke the freeze authority too.**

There is no emission schedule, no halving curve, and no vesting unlock. Every
parameter the earlier draft pushed forward into "Phase N" — vesting cliffs,
insider lockups, future circulating overhang — is gone. Circulating supply
is the 15M seeded into the pool, full stop.

### 1.1 Venue + curve: Raydium CPMM at bootstrap, CLMM migration gated

The seed pool is **Raydium Standard AMM (constant-product, xy=k)** at launch.
Concentrated-liquidity migration is **not** done at bootstrap, even though
Raydium offers CLMM.

Why constant-product first:

- CPMM spreads liquidity across the entire price curve. There is no narrow
  active band an attacker can push the price outside of.
- A new EURC/VTESS pair will have shallow, lumpy LP participation in its
  first months. CLMM at that depth is sandwich-bait: LPs cluster around the
  perceived fair price, and an attacker who can move price outside the band
  drains the active range cheaply.
- The §5.1 oracle architecture (Pyth primary, pool TWAP cross-check) is
  stricter under CPMM because pool TWAP is harder to spike with a single
  transaction.

CLMM migration is **governance-gated** and requires all of:

- Phase 3 has already activated.
- Pool depth at ±2% of mid exceeds the depth threshold of the Pyth-confidence
  gate for ≥ N consecutive months (specific N is a governance parameter).
- LP diversity: no single LP holds > X% of total liquidity (X is
  governance-set; suggested starting value 25%).
- A migration plan is published with a no-loss path for existing Standard
  AMM LPs.

Until those conditions are jointly met, the pool stays CPMM.

### 1.2 The 5M permanent lock

The 5M locked treasury is a **permanently non-circulating anchor**, not a
deferred allocation. It is locked in a contract with no unlock path and no
governance escape hatch. The intent is:

- These tokens never vote in any governance action, including the §3.3
  reserve-composition vote.
- They are excluded from circulating supply when computing per-token reserve
  backing (see §3.5).
- They cannot be sold, transferred, lent, used as collateral, or moved.
  The lock contract holds them at a known address that anyone can verify
  on-chain.

**Consequence:** because the 5M never circulates, none of the protocol's
development, ecosystem, or contributor funding comes from VTESS. All such
funding routes through the §4 EURC dev-treasury split. This is intentional:
a permanent lock and a dev-funding source cannot be the same pile of tokens.

## 2. EURC — stable settlement medium and floor asset

EURC is Circle's euro-denominated stablecoin, native on Solana. It serves
three distinct roles in this design:

- **Liquidity / price discovery.** The EURC/VTESS AMM pool gives VTESS a
  floating price that rises and falls with demand.
- **Settlement medium.** Early on, compute settles directly in EURC. Later,
  compute is priced in euros and settled in VTESS at the oracle rate (§5).
- **Stability floor of the reserve.** The reserve's protected floor is held
  exclusively in EURC (§3.2). The vote cannot move the floor portion into
  any other asset.

### 2.1 EURC redemption posture

The protocol receives revenue as EURC on Solana. Circle gates EURC issuance
and redemption to KYC'd entities, and the protocol itself does not (and
cannot, as an on-chain governance object) hold a bank relationship. The
adopted posture is therefore:

> Redemption of EURC to fiat is performed exclusively via a **contracted,
> licensed payment/crypto-asset counterparty in whatever jurisdiction the
> operating entity sits in.** The protocol never attempts to redeem with
> Circle directly, and never holds fiat itself.

Implications:

- **Counterparty selection** is launch-blocking for Phase 2. The
  counterparty's name and license lives in a separate addendum so it can
  rotate without a token-design rev.
- **KYC scope is on the counterparty, not on protocol users.** Buyers and
  hosts continue to interact only with on-chain EURC.
- **Operating entity required.** The protocol needs a legal entity that
  holds the counterparty agreement, signs redemption requests, and is itself
  KYC'd by the counterparty. This entity has no claim on VTESS supply and
  no discretion over governance-decided splits; its role is mechanical
  execution of the §4 flow of funds. The operating entity may be located in
  any jurisdiction whose regulatory regime fits the project — choice of
  jurisdiction is a project-level decision, not a token-design parameter.
- **Redemption ceiling and cadence are governance-set.** The board (§6)
  publishes a maximum redemption rate so the entity cannot drain the
  stability floor on its own initiative. The §3.2 EURC floor binds the
  redemption ceiling.
- **Transparency obligation.** Every redemption appears on-chain (EURC
  leaves the reserve address) and is matched off-chain by a counterparty
  statement; the operating entity publishes the reconciliation at the same
  cadence as the §3.4 proof-of-reserves.

## 3. The reserve — voted, multi-asset, with a protected floor

The reserve backs VTESS with a transparent basket of crypto-assets funded
from real marketplace revenue. It has two parts:

1. A **protected EURC floor** that the §3.3 vote cannot move (§3.2).
2. An **above-floor diversification basket** whose weights holders set
   biannually (§3.3).

Eligible assets in the above-floor basket: **SOL, BTC, EURC, USDC.** No
other asset can enter the basket without the new-asset vote described in
§3.3.

### 3.1 Execution paths

The reserve receives EURC from §4. The protocol then executes the asset mix
the current vote prescribes:

- **EURC → SOL**: direct swap on a Solana aggregator (Jupiter).
- **EURC → USDC**: direct swap on a Solana aggregator (Jupiter).
- **EURC → native BTC**: two-hop via Solana DEX (EURC → USDC or SOL) and
  then THORChain (USDC/SOL → native BTC). THORChain does not support
  arbitrary SPL tokens, so the BTC leg never touches VTESS directly.

Per-buy policy:

- **Max slippage budget** per buy (e.g., 1.5% for SOL/USDC, higher cap for
  BTC because of the second hop). Abort and retry later if exceeded.
- **DCA cadence**: rebalances between votes execute in small increments over
  the inter-vote window, not as lumpy one-shot trades. Slippage budgets in
  §3.1 bound each increment.

### 3.2 EURC stability floor (guardrail the vote cannot override)

**Automatic floor gate:** the above-floor basket only receives funding while
the EURC reserve balance is at or above the floor:

> EURC floor = (rolling 90-day average of host payouts × 12 months) × 1.2

While the floor is satisfied, reserve contributions are split according to
the §3.3 voted weights.

While the floor is unmet, the BTC/SOL/USDC shares of the reserve
contribution are **redirected to EURC until the floor is restored.** No
above-floor purchases of BTC/SOL/USDC execute during this period.

The §3.3 vote sets weights for the **above-floor portion only**. Holders
cannot vote the floor smaller, set it to a non-EURC asset, or skip the
redirect. This is an invariant of the design (§7).

USDC sits only in the above-floor portion; it is not eligible for the floor.
USDC is dollar-denominated, and the §2 settlement medium is EUR — putting
USDC into the floor reintroduces the EUR/USD FX exposure §2 deliberately
avoids.

### 3.3 Biannual reserve-composition vote

Holders vote on the **above-floor basket weights** twice a year. The vote
mechanism is designed to be implementable and abuse-resistant.

**Voting windows.** Two windows per year, each open for one week:

- Opens 2026-05-15, closes **2026-05-22** (Bitcoin Pizza Day).
- Opens 2026-10-24, closes **2026-10-31** (Bitcoin whitepaper day).
- Subsequent years repeat the same dates.

**Eligible assets.** SOL, BTC, EURC, USDC at launch. Any new asset is
admitted only via the new-asset path in this section.

**Vote weight.** One-VTESS-one-vote based on a **balance snapshot taken at
the Solana slot when the window opens.** Snapshotting at open prevents
flash-borrowed VTESS from swinging mid-window results. Wallets that
acquire or move VTESS during the window vote at their snapshot balance,
not their current balance.

**Excluded from voting:**

- The §1.2 permanently locked 5M treasury.
- The seed pool's VTESS (held by the AMM contract).
- Any VTESS held in protocol-controlled contracts (vesting locks, the
  custodian §3.4 address, etc.) — none exist at launch under the zero-
  insider rule but the exclusion is permanent and forward-compatible.

These exclusions stop the protocol from voting on itself and keep the §1
zero-insider invariant from being undercut.

**Passing bars.**

- **Reweighting the existing set** (changing the % each of {SOL, BTC, EURC,
  USDC} gets in the above-floor basket): simple majority of votes cast,
  subject to a participation quorum (starting parameter: 10% of eligible
  circulating supply).
- **Admitting a new asset** to the eligible set: two-thirds supermajority
  of votes cast, the same participation quorum, **plus a one-window
  cooldown delay** — i.e., the new asset only becomes purchasable after the
  *following* vote window closes. The cooldown gives the market time to
  react, lets reviewers flag manipulation, and prevents same-day add-and-
  pump dynamics.

**Execution.** After a window closes, the protocol DCAs from the prior
weights to the new weights across the inter-vote window, within the §3.1
slippage budgets. Settlement of the new mix is not a single transaction.

**Tie / quorum failure.** If quorum fails, the prior weights persist
unchanged until the next window.

The participation quorum (10% starting value) and the new-asset cooldown
length (one window starting value) are themselves governable parameters
under §6, settable by the board, not by the holder vote.

### 3.4 Custody and proof-of-reserves

The reserve is held by a **qualified custodian that publishes proof-of-
reserves**, such as Coinbase Prime / Custody or an equivalent institutional
custodian. Pure self-custody multisig was considered and rejected: a
voted multi-asset reserve needs an audited custodian and clean PoR more
than it needs full decentralisation of signing.

- **Signer mix authorising movements.** Movements from the custodian
  require signatures from a defined mix of categories: core team,
  independent custodian-side signer(s), and a community-elected
  signer. The threshold and exact mix live in a governance addendum so
  signer rotation does not require a token-design rev.
- **Proof-of-reserves cadence**: weekly attestation plus on every
  reserve movement. Both the custodian's attestation and the on-chain
  address holdings are published.
- **Geographic distribution**: signers from at least two distinct
  jurisdictions, so a single subpoena cannot freeze movements.
- **Recovery process** for lost signers is documented in the same
  addendum.

### 3.5 Per-token backing metric

Because the reserve transparently holds known balances at a known
custodian, a per-token **backing metric** can be computed and published:

> backing per VTESS = (reserve NAV in EUR) / (circulating supply)
>
> circulating supply = 20,000,000 − (5,000,000 locked under §1.2) −
> (VTESS held in protocol-controlled contracts)
>                    = 15,000,000 at launch

This is a **backing figure**, not a price. The market sets VTESS's price; the
reserve sets the protocol's verifiable, on-chain anchor under it. The
backing metric is published at the §3.4 PoR cadence.

## 4. Flow of funds (no VTESS dilution, no VTESS dump)

Per settlement:

1. Compute is **priced in euro-cents** (oracle).
2. Buyer pays the **host** for compute. Two payment paths are supported
   (see §4.1 for the buyer UX):
   - In VTESS at oracle rate (post-Phase-3), or
   - In EURC directly (bootstrap and ongoing fallback).
3. Buyer also pays a **reserve contribution**, set by governance. **The
   contribution is collected in EURC**, never by selling VTESS, so the
   reserve mechanism creates no sell pressure on the token.
4. The reserve contribution splits (governance-set %): **reserve** (subject
   to the §3.2 floor gate and §3.3 voted weights) / **dev treasury**. The
   dev-treasury slice is paid out in EURC and routes through the §2.1
   counterparty when fiat is needed.
5. The settlement enclave (see `SECURITY.md` / settlement-enclave design)
   verifies metering before releasing host payment.

Key invariant: **the reserve is funded by real buyer value in EURC, never
by minting VTESS and dumping it.** That money-pump (mint → sell → buy
reserve) was the fatal flaw of the earlier design and is explicitly
forbidden here.

### 4.1 Two-asset UX with both payment paths

A buyer can pay the host in **either** VTESS or EURC. The reserve
contribution is always paid in EURC (see §4 step 3). The honest
specification: a buyer can choose a path that minimises the assets they
need to hold, but is not forced to hold both for every payment.

- **Path A (EURC-native buyer).** Buyer pays host in EURC at the euro
  price, and pays the reserve contribution in EURC. Buyer never touches
  VTESS. This is the bootstrap default and remains available after Phase 3.
- **Path B (VTESS-native buyer).** Buyer holds VTESS, pays host in VTESS
  at the oracle rate, and pays the reserve contribution in EURC. To pay
  the reserve contribution, the buyer either holds a small EURC balance
  for that purpose or supplies extra VTESS that the settlement enclave
  routes to a JIT swap (EURC out, into the reserve). The JIT-swap path is
  available only after Phase 3 has activated and is subject to the same
  Pyth-confidence and slippage gates as oracle-priced settlement (§5).

Both paths are honest about what the buyer actually owes (a host payment
and a reserve contribution) and what the protocol does with each. Neither
path silently moves the buyer into an asset they did not consent to hold.

## 5. Settlement: euro-priced, VTESS-settled — gated on liquidity

Compute is priced in euros and settled in VTESS at an oracle rate so VTESS
can appreciate without making compute unaffordable (the quantity of VTESS
per job auto-adjusts). **But an oracle is only as safe as the market it
reads.** A thin pool is trivially manipulable, so:

- **Bootstrap:** settle in **EURC directly**; VTESS is incentive/governance
  only.
- **Deepen:** grow EURC pool depth + LPs until the VTESS price is robust.
- **Switch on:** only then enable oracle-priced VTESS settlement.

Governance sets the depth/robustness threshold that flips this on
(launch-blocker, tracked separately from the closed reserve issues).

### 5.1 Oracle: Pyth primary, pool TWAP cross-check

The price feed used by the settlement enclave is **Pyth Network** (Solana-
native, first-party publisher model, on-chain). The EURC/VTESS pool TWAP
is a **sanity-check deviation alarm**, never the primary source.

- **Primary:** Pyth VTESS/EUR feed (or VTESS/USD bridged through Pyth's
  EUR feed if a direct VTESS/EUR feed is not initially supported).
- **Cross-check:** the daemon and the settlement enclave both compute a
  rolling TWAP of the EURC/VTESS pool. If Pyth and the pool TWAP diverge
  by more than the governance-set deviation threshold for longer than a
  governance-set dwell window, **settlement halts** and operators are
  alerted. Settlement resumes automatically once both come back into
  band, or manually after a governance review.

Three parameters remain governable and start at conservative defaults the
board can tighten as data arrives: deviation threshold, dwell window, TWAP
window.

The Phase 3 activation gate checks Pyth's published confidence interval and
the pool depth jointly: if Pyth's published confidence is wider than a
governance-set fraction of price, or pool depth is below the threshold,
Phase 3 stays gated.

## 6. Governance board

Sets and can adjust:

- Protocol fee %.
- Reserve-contribution % (the §4 step 3 number).
- The reserve / dev-treasury split of the contribution.
- The §3.3 vote's participation quorum and new-asset cooldown.
- Oracle configuration and the liquidity threshold for switching on VTESS
  settlement.
- The §3.2 EURC floor formula parameters (months of payouts, buffer
  multiplier — but not the existence of the floor; that is permanent).
- The §3.4 custody signer mix and threshold.
- The EURC redemption ceiling / cadence applied to the §2.1 counterparty.

What the board **cannot** do:

- Vote the §3.3 above-floor weights (that is the holder vote).
- Move VTESS from the §1.2 permanent lock.
- Re-introduce an insider/VC allocation (§1).
- Replace Pyth as the primary oracle without a token-design rev.

The board is itself a centralisation point; its mandate, limits, and
transparency obligations are documented in a governance addendum.

## 7. Hard rules / invariants

The substance of the model is the line between what is **permanent** (a
cryptographic guarantee or load-bearing invariant) and what is
**governable** (a parameter the governance board or holder vote can move
within published limits).

| Invariant | Status |
| --- | --- |
| Total supply 20M VTESS | **Permanent** — mint authority revoked at launch. |
| Freeze authority | **Permanent** — revoked at launch. |
| 15M seeded into EURC/VTESS pool | **Permanent** — one-shot at launch. |
| 5M permanently locked, non-circulating, non-voting | **Permanent** — invariant of §1.2. No unlock path. |
| Zero insider/founder/team/VC allocation | **Permanent** — invariant of §1. |
| Reserve funded from buyer EURC, never from minted/sold VTESS | **Permanent** — invariant of §4. |
| BTC/SOL/USDC reserve legs never sell VTESS | **Permanent** — invariant of §3.1. |
| EURC stability floor protected from the §3.3 vote | **Permanent** — invariant of §3.2. |
| USDC ineligible for the protected floor | **Permanent** — invariant of §3.2. |
| Above-floor basket eligible assets = {SOL, BTC, EURC, USDC} until vote-admitted | **Permanent until amended via §3.3 new-asset vote**. |
| Biannual vote windows close 2026-05-22 and 2026-10-31 (Bitcoin Pizza Day / whitepaper day) | **Permanent** — invariant of §3.3. |
| Snapshot voting (balances at window-open slot, not flash-borrowable mid-window) | **Permanent** — invariant of §3.3. |
| Locked treasury and protocol-controlled VTESS excluded from voting | **Permanent** — invariant of §3.3. |
| GNU/Linux distros stay free; token rides the *optional* provider package only | **Permanent**. |
| Oracle source = Pyth primary, pool TWAP cross-check | **Permanent** — invariant of §5.1. |
| AMM venue at launch = Raydium; curve at launch = CPMM (Standard AMM) | **Permanent** — invariant of §1.1. |
| EURC redeemed only via licensed counterparty (never direct Circle, never via the protocol itself) | **Permanent** — invariant of §2.1. |
| Protocol fee % | Governable (board). |
| Reserve-contribution % | Governable (board). |
| Reserve / dev-treasury split of the contribution | Governable (board). |
| EURC floor formula parameters (months, buffer) | Governable (board); existence of floor is permanent. |
| Above-floor basket weights {SOL %, BTC %, EURC %, USDC %} | Governable (holder vote, §3.3). |
| Pyth-vs-pool deviation threshold / dwell window / TWAP window | Governable (board). |
| Phase 3 liquidity + Pyth-confidence gate | Governable (board). |
| §3.3 participation quorum and new-asset cooldown | Governable (board). |
| Custody signer mix and threshold | Governable (board, with notice period). |
| Identity of the §2.1 counterparty and §3.4 custodian | Governable; rotates outside this doc. |
| CLMM migration trigger (depth, dwell, LP concentration thresholds) | Governable (board). |

Operational invariants enforced by code or process, not by chain state:

- Compute priced in euros; VTESS settlement only after liquidity passes
  threshold.
- Billing data stays separate from broader (opt-in) telemetry.

## 8. Risks

- **Liquidity / oracle safety.** Thin pool → manipulable price → unsafe
  settlement. Liquidity depth is the gating constraint, not a detail.
- **Custody.** A qualified custodian is a single point of contractual
  failure even with PoR. §3.4 mitigates with signer-mix + jurisdiction
  diversity + recovery process, but does not eliminate the risk.
- **Two-hop BTC routing.** THORChain + Solana-DEX adds slippage and bridge
  risk on every BTC buy. The §3.1 per-buy slippage budget bounds the cost
  of a single bad hop; it does not bound the cost of THORChain failing as
  a system.
- **Vote capture.** A holder concentrated enough to swing the §3.3 vote
  could push the basket toward an asset that benefits them. The §3.3
  snapshot + quorum + new-asset cooldown raise the bar but do not remove
  it; published per-window vote breakdowns are the social check.
- **Reserve drag** even when floor is satisfied. Above-floor purchases of
  non-EURC assets spend reserve value on the way in; size the
  contribution and the floor parameters so accumulation doesn't starve
  the stability layer at the edges of the gate.
- **Regulatory uncertainty.** A voted multi-asset reserve sits closer to
  the line that some jurisdictions draw around asset-referenced tokens
  and collective-investment vehicles than a single-purpose utility token
  does. The project's posture is that the operating entity's jurisdiction
  is itself a project-level choice; this document does not pre-restrict
  the design to any one regulator's preferences. Counsel for the chosen
  jurisdiction reviews before mainnet.

## 9. How this maps to the rollout

- **Phase 0–1:** package + enclave; settle in EURC; VTESS not required.
- **Phase 2:** VTESS live (fixed 20M, mint authority revoked, 5M
  permanently locked), EURC/VTESS pool seeded, reserve contributions
  begin accruing to the EURC floor. First §3.3 vote occurs at the next
  scheduled window after Phase 2 launch. The receipt's `payout_id` field
  uses **Solana base58 Ed25519 addresses** (32–44 chars) — receipts
  written today are forward-compatible with the Phase-2 settlement
  enclave.
- **Phase 3:** above-floor reserve accumulation switches on once the EURC
  floor is satisfied and the §3.3 vote has set above-floor weights;
  oracle-priced VTESS settlement enables once pool depth passes the §5
  threshold.

## Legal

Nothing in this document is legal, financial, tax, or investment advice. It
describes a design under active discussion. Implementation of any phase is
subject to counsel review for the operating entity's chosen jurisdiction.
