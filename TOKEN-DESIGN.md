# Vtessera — VTESS Token Design (Layered Reserve, authoritative)

> Supersedes the rough notes in `TOKENOMICS.md`. This is the agreed model.
> Still a **later phase** than the Phase-0 package; see `ROADMAP.md`.
> Nothing here is legal or investment advice — see "Legal" at the end.

## Model in one paragraph

VTESS is a **fixed-supply** Solana SPL token used as the utility/settlement and
governance token for the openSUSE compute marketplace. There is **no
mint-on-use and no ongoing inflation** — the cap is created once and the mint
authority is revoked. Value is *not* propped up by minting; it is supported by
two reserves funded from **real buyer payments**: a near-term **EURC stability
layer** (settlement medium + price-floor + liquidity) and a long-term **BTC
accumulation layer** (the "eternal buyer"). Compute is priced in euros and
settled in VTESS at an oracle rate once liquidity is deep enough; until then it
settles directly in EURC.

## 1. Supply & distribution (fixed, no inflation)

- **Total supply: 20,000,000 VTESS, created once.**
- **15,000,000 VTESS** seeded into an EURC/VTESS AMM pool on Solana (price
  discovery + liquidity).
- **5,000,000 VTESS** locked in treasury under a **published vesting schedule**
  (define cliff + linear unlock + purpose up front; this is future circulating
  overhang and must not be an unexplained cliff).
- **Revoke the SPL mint authority after creation.** This makes the 20M cap a
  cryptographic guarantee, not a promise — directly serving the "don't devalue"
  goal. Strongly consider revoking the freeze authority too, for credibility.

There is no emission schedule and no halving curve; those belonged to the
earlier mint-on-use model, which is abandoned. "More tokens in circulation"
over time comes from the **5M unlocking on schedule**, not from minting.

## 2. Layer 1 — EURC (stability + liquidity + settlement)

EURC is Circle's MiCA-regulated, 1:1 euro-backed stablecoin, native on Solana —
the right fit for an EU-aligned project. Note it is a *payment* stablecoin that
pays **no yield**; an EURC reserve is a **stable anchor, not a growth asset**.
That is exactly what you want for the layer agents transact on.

Roles of this layer:

- **Liquidity / price discovery.** The EURC/VTESS AMM pool gives VTESS a floating
  price that rises and falls with demand.
- **Settlement medium.** Early on, compute settles directly in EURC. Later,
  compute is priced in euros and settled in VTESS at the oracle rate (see §5).
- **Stability reserve / floor.** A share of fees accumulates as held EURC,
  providing a euro-denominated floor and deepening pool liquidity over time.

## 3. Layer 2 — BTC (long-term accumulation, the "eternal buyer")

A share of fees is converted to **native BTC** and held as a long-term,
appreciating treasury — the MicroStrategy-style store of value, but funded by
marketplace cash flow, not leverage.

- **Execution path:** EURC → BTC via THORChain (THORChain settles native SOL,
  USDC, and BTC; route EURC→SOL/USDC→native BTC). THORChain does **not** support
  arbitrary SPL tokens, so the BTC leg never touches VTESS directly.
- **Custody:** multisig / MPC, transparent on-chain proof-of-reserves, governed
  movements only. A defensible BTC treasury is a target; treat custody as a
  launch blocker.

## 4. Flow of funds (the core mechanic — no VTESS dilution, no VTESS dump)

Per settlement:

1. Compute is **priced in euro-cents** (oracle).
2. Buyer pays the **host** for compute — in VTESS at oracle rate (this is VTESS's
   demand sink: agents must acquire VTESS to pay, creating buy pressure), or in
   EURC during bootstrap.
3. Buyer also pays a **reserve contribution**, set by governance — your "1 for 1"
   instinct is the aspirational ceiling (contribution = host payment); realistic
   values are lower (~10–30%). **Collect this contribution in EURC**, not by
   selling VTESS, so neither reserve leg creates sell pressure on the token.
4. The reserve contribution splits (governance-set %): **EURC stability reserve**
   / **BTC treasury** / **dev treasury**.
5. The settlement enclave (see `SECURITY.md` / settlement-enclave design)
   verifies metering before releasing host payment.

Key invariant: **reserves are funded by real buyer value in EURC, never by
minting VTESS and dumping it.** That money-pump (mint → sell → buy reserve) was
the fatal flaw of the earlier design and is explicitly forbidden here.

## 5. Settlement: euro-priced, VTESS-settled — gated on liquidity

We price in euros and settle in VTESS at an oracle rate so VTESS can appreciate
without making compute unaffordable (the quantity of VTESS per job auto-adjusts).
**But an oracle is only as safe as the market it reads.** A €1,000-deep pool is
trivially manipulable, so:

- **Bootstrap:** settle in **EURC directly**; VTESS is incentive/governance only.
- **Deepen:** grow EURC pool depth + LPs until the VTESS price is robust.
- **Switch on:** only then enable oracle-priced VTESS settlement.

Governance sets the depth/robustness threshold that flips this on.

## 6. Governance board

Sets and can adjust: protocol fee %, the reserve-contribution %, the
EURC:BTC:dev split, the 5M unlock schedule, oracle configuration and the
liquidity threshold for VTESS settlement, and BTC custody/treasury movements.
**The board is itself a centralization point regulators scrutinize** — document
its mandate, limits, and transparency obligations.

## 7. Hard rules / invariants

- Fixed 20M supply; **mint authority revoked** after creation.
- Reserves funded from **buyer payments in EURC**, never from minting/dumping VTESS.
- BTC leg routes EURC→SOL/USDC→BTC via THORChain; never sells VTESS to buy BTC.
- BTC custody = multisig/MPC + proof-of-reserves.
- Compute priced in euros; VTESS settlement only after liquidity passes threshold.
- Billing data stays separate from broader (opt-in) telemetry.
- openSUSE stays free; the token rides the *optional* provider package only.
- No large insider/VC allocation.

## 8. Risks & legal

- **Liquidity / oracle safety.** Thin pool → manipulable price → unsafe
  settlement. Liquidity depth is the gating constraint, not a detail.
- **The two layers have opposite regulatory profiles.** The EURC layer is
  MiCA-clean (stable, no yield). The **BTC-appreciation layer + governance +
  any redemption right reintroduces an investment-contract profile** (Howey in
  the US; MiCA's asset-referenced/utility distinctions in the EU, where SUSE
  sits). This is the central legal tension of the layered model and needs
  counsel **before mainnet** — the stable layer does not launder the
  appreciation layer's risk.
- **Custody** of the BTC treasury is a single point of failure.
- **THORChain routing** adds slippage/liquidity risk on every BTC buy.
- **Reserve drag.** Even funded in EURC, the BTC conversion spends reserve
  value; size the contribution so accumulation doesn't starve the stability
  floor.

## 9. How this maps to the rollout

- **Phase 0–1:** package + enclave; settle in EURC; VTESS not required.
- **Phase 2:** VTESS live (fixed 20M, mint authority revoked), EURC/VTESS pool
  seeded, reserve contributions begin accruing to the **EURC** layer. Requires
  legal sign-off.
- **Phase 3:** BTC accumulation switched on once volume is meaningful
  (~€10–50M/yr+ range from the earlier model); oracle-priced VTESS settlement
  enabled once pool depth passes threshold.
