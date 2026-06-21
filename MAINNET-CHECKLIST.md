# Vtessera Mainnet Deploy — Pre-Flight Checklist

> Authoritative tracker for the six items that must hold before the
> Vtessera escrow program touches Solana mainnet. The high-level
> entries here are gated by the "Mainnet criteria (DEFERRED)" block in
> `ROADMAP.md`; this file is the per-step expansion with checkboxes.
>
> **Status today:** all six items open. Devnet program at
> `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` is the only deployment.

## How to read this file

Each section: **what it is** (in plain English) → **what breaks if we
skip it** → numbered concrete steps with checkboxes → who does each
step. "Me" = the coding agent doing the source work. "You" = the
project owner who holds the keys and the budget.

---

## 1. Wire the Jupiter swap + Pyth price guard

**What it is.** When a job pays out, the buyer paid in **USDC or
EURC** (a stablecoin). The roadmap promises the seller earns **HNT**
(Helium's token). Two on-chain pieces convert one to the other:

- **Jupiter** is Solana's standard DEX aggregator — given "I have 1
  USDC, get me HNT", it routes across all liquidity pools and picks
  the cheapest path. We invoke it as a CPI (cross-program invocation)
  from inside `finalize_pro_rata`.
- **Pyth** is an on-chain price oracle — it publishes "HNT is worth
  $X right now" with a confidence interval. We read Pyth's HNT/USD
  price, compute what Jupiter's trade *should* return, and abort the
  transaction if Jupiter's actual output is more than 0.5% off. This
  kills MEV / sandwich attacks: a bot can't move the price unfavorably
  against our swap because the program rejects the result.

**What breaks if we skip it.** Sellers earn stablecoin, not HNT
(current devnet-stub behavior). The README's central claim "every
paid job is a real on-market HNT buy + burn" becomes false. Without
Pyth, MEV bots systematically drain a few percent of every transaction.

### Steps

- [ ] **1.1** Look up the **HNT mint** address on Solana mainnet from
      the Helium docs. Confirm freeze authority is null (otherwise the
      neutrality claim in ROADMAP §4d doesn't hold).
- [ ] **1.2** Look up the **Pyth price-feed account** IDs:
      - HNT/USD (mainnet + devnet)
      - EUR/USD (for EURC-denominated jobs)
- [ ] **1.3** Add to `FinalizePro` accounts struct in
      `programs/vtessera-escrow/src/lib.rs`:
      `hnt_mint`, `escrow_hnt_ata`, `seller_hnt_ata`, `pyth_hnt_usd`,
      `pyth_eur_usd` (optional), plus Jupiter route accounts as
      remaining_accounts.
- [ ] **1.4** Rewrite the "earned slice" path:
      - Read Pyth → `expected_hnt_min = earned_stablecoin × pyth_price × (1 − slippage_bps/10_000)`
      - CPI to Jupiter with the off-chain-computed route
      - Verify the actual HNT output ≥ `expected_hnt_min` → revert otherwise
      - SPL-burn `DRAFT_BURN_BPS / 10_000` of the HNT
      - Transfer the rest to `seller_hnt_ata`
- [ ] **1.5** Confirm Pyth feed freshness gate: if the most recent
      Pyth publish slot is more than N slots old, revert. Pick N from
      Pyth's own staleness recommendations.
- [ ] **1.6** Devnet has no real HNT and limited Jupiter liquidity, so
      test on **mainnet-fork**: run a local validator that snapshots
      mainnet state, fork transactions against it. Solana docs:
      `solana-test-validator --clone-feed <PROGRAM> --url mainnet-beta`.
- [ ] **1.7** Keep a `--devnet-stub` cargo feature so the devnet
      smoke flow keeps working without HNT.

**Who.** All me. You don't need to do anything in this section.

**Effort.** 1-2 days.

---

## 2. Adversarial test suite

**What it is.** Right now the program has been tested on the happy
path (`f = 0.5`, valid accounts, single shot). An adversarial suite
tries to break it: pass malformed args, wrong-mint ATAs, wrong-owner
ATAs, double-finalize, `f > 1`, math overflows. Each case asserts the
program rejects with the **correct error code** — rejecting for the
wrong reason is still a bug.

**What breaks if we skip it.** The constraints in the program (e.g.
`constraint = ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint`)
are written but never verified. A typo in any constraint = an attacker
can slip through.

### Steps

- [ ] **2.1** Pick a test harness — recommend **`litesvm`**
      (in-process Solana validator simulator, faster than the official
      `solana-program-test`).
- [ ] **2.2** Write `programs/vtessera-escrow/tests/` with the following
      cases. Each = one Rust test:
      - [ ] **2.2a** `pay_for_compute(price_micros = 0)` → fails with
            `ZeroPrice`
      - [ ] **2.2b** Same `job_id` twice → second `pay_for_compute`
            fails (PDA already exists)
      - [ ] **2.2c** `finalize_pro_rata(f_micros = 1_000_001)` → fails
            with `FractionOutOfRange`
      - [ ] **2.2d** `finalize_pro_rata` twice → second fails with
            `AlreadyFinal`
      - [ ] **2.2e** Buyer ATA with wrong mint → fails with `WrongMint`
      - [ ] **2.2f** Seller ATA owned by someone other than
            `contract.seller_payout` → fails with `WrongOwner`
      - [ ] **2.2g** `cancel_before_start` signed by non-buyer → fails
      - [ ] **2.2h** `cancel_before_start` after `finalize_pro_rata` →
            fails with `AlreadyFinal`
      - [ ] **2.2i** Math: `price = u64::MAX, f_micros = 999_999` →
            no silent overflow
      - [ ] **2.2j** Math: `price = 1, f_micros = 1` → split rounds
            consistently
      - [ ] **2.2k** `finalize_pro_rata` signed by a key other than
            the settlement_authority → fails
- [ ] **2.3** Wire into CI — every push runs the harness.
- [ ] **2.4** Re-run the suite against the post-§1 program (swap +
      guard add new failure modes worth covering).

**Who.** All me.

**Effort.** ~1 day for the harness + the 11 cases.

---

## 3. Multisig for settlement authority and upgrade authority

**What it is.** Two separate signer roles that today are both the
single keypair at `~/.config/solana/id.json` on this laptop.

- **Settlement authority** — signs `finalize_pro_rata`. Today that
  keypair can declare any `f` for any active escrow and drain funds to
  itself.
- **Upgrade authority** — can replace the on-chain program with new
  code. Today that keypair can deploy a malicious version that routes
  all escrows to a chosen address.

**Multisig** = an on-chain vault that requires *N of M* signers to
approve any action. e.g. 2-of-3 means three signers exist, any two
must co-sign. **Squads** (squads.so) is the standard Solana multisig
tool — a web UI that creates the vault on-chain.

**What breaks if we skip it.** Laptop theft = total loss of every
active escrow. Compromised SSH key = total loss. An accidental
`git add ~/.config` followed by `git push` = total loss. The README's
"credibly neutral, no one holds the funds" framing only holds if no
single party can change the program after deploy.

### Steps

- [ ] **3.1** Acquire a **hardware wallet** if you don't already have
      one. Ledger Nano X or Trezor Safe 3 are the standard choices.
      **Critical:** a multisig made entirely of laptop keys is theatre,
      not security. At least one signer must be hardware.
- [ ] **3.2** Pick signers and threshold. Recommended: **2-of-3** with:
      - Signer A: hardware wallet (acquired in 3.1)
      - Signer B: laptop wallet (current `~/.config/solana/id.json`)
      - Signer C: backup — second hardware wallet, or a trusted
        person's wallet
- [ ] **3.3** Create the Squads multisig:
      - Go to https://squads.so
      - Connect Signer B (laptop) to fund creation
      - Add Signer A and Signer C as members
      - Set threshold to 2
      - Name the vault "vtessera-mainnet"
      - Cost: ~0.01 SOL
- [ ] **3.4** Record the Squads vault PDA — this is the address that
      will receive both the upgrade authority and settlement
      authority.
- [ ] **3.5** Update `programs/vtessera-escrow/src/lib.rs`:
      `FinalizePro`'s `settlement_authority` becomes constrained to
      equal the Squads vault PDA. Constant in the program OR config
      account holding the address. Redeploy to devnet.
- [ ] **3.6** Transfer the **upgrade authority** to Squads:
      ```
      solana program set-upgrade-authority 6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma \
        --new-upgrade-authority <SQUADS_VAULT_PDA> --url devnet
      ```
      After this, future upgrades require 2 of the 3 signers to
      approve from inside Squads.
- [ ] **3.7** Verify on-chain — `solana program show <PROGRAM_ID>`
      should report Authority = Squads vault PDA.
- [ ] **3.8** Document signer set, threshold, and Squads vault PDA in
      the README so users can independently verify on-chain.

**Who.**
- **You:** acquire hardware wallet (3.1), create Squads multisig
  (3.2-3.4), run `set-upgrade-authority` (3.6). The current keypair
  must be present to authorize the handover — only you can run that
  command.
- **Me:** update the program code to gate `finalize_pro_rata` on the
  multisig PDA (3.5). Redeploy to devnet.

**Effort.** Half a day plus 1-2 weeks of wall-clock if you need to
order a hardware wallet.

---

## 4. Third-party audit

**What it is.** Someone who didn't write the program reads it and
tries to break it. I (the coding agent) am the worst person to audit
the program because my blind spots in writing it are the same blind
spots in reviewing it. A second pair of eyes catches a different
category of bug.

### Audit tiers

| Tier | Cost | Time | Trust signal |
| --- | --- | --- | --- |
| Paid pro firm (OtterSec, Neodyme, Sec3, Halborn) | $20k-$50k | 1-3 weeks | Strong |
| Code4rena / Sherlock contest | $5k-$15k | 1-2 weeks | Decent |
| Community review (Solana Discord, r/solana) | Free | days | Weakest, but useful as a first pass |
| Reciprocal review with another small project | Free | days | Decent for the friendliness |

### Honest recommendation

Pre-revenue, pre-users, a paid pro audit is overkill. **Start with
community review.** When the project has revenue or real funds at
risk, get the paid audit.

**What breaks if we skip it.** You ship with unknown unknowns. A bug
found by a user post-mainnet means: (a) hope the multisig moves fast
enough to upgrade before exploitation, or (b) accept the loss.

### Steps

- [ ] **4.1** Decide tier. Default: community review first, paid audit
      gated on revenue / TVL.
- [ ] **4.2** Prepare for review:
      - Write `programs/vtessera-escrow/SECURITY.md` with:
        - Threat model — what we defend against (custodial drain by
          settlement authority, MEV, double-spend), what we don't
          (Solana validator censorship, Pyth feed compromise, Jupiter
          rugpull, Circle freezing edge addresses)
        - Known limitations
        - Deploy procedure (the immutable / multisig step)
      - Tag the audit-ready commit hash
- [ ] **4.3** Post to channels:
      - Solana Discord `#auditing`
      - r/solana
      - Anchor Discord
      - Twitter / Bluesky if relevant
      Link the repo, the commit, SECURITY.md.
- [ ] **4.4** Triage findings. Each lands in one of:
      - **Fix** — patch, re-deploy to devnet for verification, request
        re-review
      - **Acknowledge** — document as known limitation
      - **Dispute** — reviewer misread; write a public reply

**Who.**
- **Me:** write SECURITY.md, prepare the audit-ready commit, address
  findings.
- **You:** decide tier, post (your identity, not mine), pay if
  applicable, accept/reject findings.

**Effort.** 1-3 weeks calendar time, mostly waiting.

---

## 5. Reproducible BPF build with committed SHA

**What it is.** When the program is deployed, the bytecode (`.so`
file) lives on-chain. The promise we want to make is: "the bytecode
on-chain is what this source code compiles to — verify yourself." A
**reproducible build** means anyone cloning the repo gets a `.so`
with the same SHA-256 as what's deployed. The tool is
[`solana-verify`](https://github.com/Ellipsis-Labs/solana-verifiable-build) —
it builds inside a pinned Docker image so the build environment is
identical for everyone.

**What breaks if we skip it.** Once the program is immutable (or
multisig-upgraded), the on-chain bytecode IS the source of truth.
Without reproducibility, users have to trust your word that the repo
matches what's running. With it, no trust required.

### Steps

- [ ] **5.1** Install `solana-verify`:
      ```
      cargo install solana-verify
      ```
- [ ] **5.2** From `programs/vtessera-escrow/`, run:
      ```
      solana-verify build
      ```
      Confirm the `.so` SHA-256 is reproducible across two clean
      builds.
- [ ] **5.3** Commit the SHA to
      `programs/vtessera-escrow/DEPLOYED_SHA256.txt` with the deploy
      date, program ID, and the commit hash it corresponds to.
- [ ] **5.4** Document the verification command in the README:
      ```
      solana-verify verify-from-repo https://github.com/douglasdemaio/vtessera \
        --program-id <MAINNET_PROGRAM_ID> --url mainnet-beta
      ```
- [ ] **5.5** Run the verify command yourself after mainnet deploy and
      include the output in the release notes.

**Who.** All me, except step 5.5 which you run.

**Effort.** 2-4 hours, mostly fighting Docker.

---

## 6. Devnet soak — at least one week

**What it is.** Run the demo *a lot*, with varied parameters, over
real wall-clock time. Tests cover what we anticipated. Soak-testing
catches what we didn't.

**What breaks if we skip it.** Bugs that only surface under realistic
patterns. Examples: rent-exemption edges when `price` is very small;
race conditions when two finalizes land near-simultaneously;
specific `f_micros` values where integer math behaves oddly;
ATA-creation collisions; RPC failures mid-transaction.

### Steps

- [ ] **6.1** Write a soak-runner — `crates/devnet-demo/src/bin/soak.rs`
      or similar. Each iteration:
      - Pick random `price_micros` (1 to 10_000_000)
      - Pick random `f_micros` (0, 1, 500_000, 990_000, 1_000_000,
        uniform random)
      - With some probability, fire `cancel_before_start` instead of
        `pay`+`finalize`
      - Random seller pubkey
      - Run the flow; log result and any error
- [ ] **6.2** Cron / systemd-timer it to fire every N minutes. Target
      ~100 runs / week.
- [ ] **6.3** Vary specifically:
      - Concurrent jobs (2-3 in flight at once) to catch any
        non-serializable race
      - One run with `price = 1` to find rounding edges
      - One run with `f_micros = 1_000_000` to verify buyer refund is
        exactly 0
- [ ] **6.4** Watch the log file. Any non-zero error rate gets
      investigated **before** mainnet.
- [ ] **6.5** Run for at least **one week** of continuous green
      operation after #1-#3 are all merged and re-deployed.

**Who.**
- **Me:** write the soak runner, document the failure-investigation
  procedure.
- **You:** keep the devnet payer keypair funded. `solana airdrop 2
  --url devnet` is free but rate-limited; you'll likely run 2-3 over
  the week.

**Effort.** Half a day to build; one week wall-clock to soak.

---

## Suggested timeline

These steps overlap; calendar time is less than sum of efforts.

| Week | Me | You |
| --- | --- | --- |
| 1 | Start #1, start #2 | Order hardware wallet if needed; set up Squads (#3.1-3.4) |
| 2 | Finish #1, #2; do #5 | Run `set-upgrade-authority` (#3.6) once #3.5 lands |
| 3 | Update settlement_authority (#3.5), redeploy. Start #6 soak. Prepare #4 audit kit. | Post audit request (#4.3) |
| 4-5 | Process audit findings | — |
| 6 | Final devnet soak with all changes in. Reproducible build of audited code. | — |
| 7 | Mainnet deploy. Small amounts only. | Run verify command (#5.5) yourself |

**5-7 weeks** realistic if everything goes well. Long pole: the audit.
Community-review-only shaves 1-2 weeks but lowers the trust signal.

## Decision points before starting

- **Audit tier (item #4).** Community / contest / paid firm. Default
  recommendation: community.
- **Hardware wallet (item #3).** Required. Confirm you have one or
  order before week 1.

## Mainnet deploy gate

**Do not deploy to Solana mainnet-beta until every checkbox above is
ticked.** The cost of being wrong on mainnet (lost user funds, broken
neutrality claim, recoverable only by upgrade-key custodial
intervention) is asymmetric versus the benefit of an earlier ship.
