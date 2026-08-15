# Vtessera Mainnet Deploy — Pre-Flight Checklist

> Authoritative tracker for the six items that must hold before the
> Vtessera escrow program touches Solana mainnet. The high-level
> entries here are gated by the "Mainnet criteria (DEFERRED)" block in
> `ROADMAP.md`; this file is the per-step expansion with checkboxes.
>
> **Status today:** items 1, 2 done (incl. devnet redeploy + `init_config`);
> 3 decided — **immutable (Option A)**, execution at mainnet deploy via
> the §3.3 runbook; the devnet program stays upgradeable while the soak
> (§6) and audit (§4) are open. Devnet program at
> `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` is the only deployment
> and now runs the stablecoin build; program security.txt is published on
> devnet (`security.json`, metadata PDA
> `42YbtUqT4w2u2rECYvL5daaaZM7ANkqCsqXG6sH8wvCg`). The devnet config PDA
> uses the `vtessera_config_v2` seed with the throwaway CI soak key as
> settlement authority, so no operator key is used in automation.

## How to read this file

Each section: **what it is** (in plain English) → **what breaks if we
skip it** → numbered concrete steps with checkboxes → who does each
step. "Me" = the coding agent doing the source work. "You" = the
project owner who holds the keys and the budget.

---

## 1. Direct stablecoin settlement

**What it is.** When a job pays out, the escrow program pays the
seller's earned slice **directly in the stablecoin the buyer paid** —
EURC or USDC, whichever the node's signed offer puts on it — via the
seller's ATA in `contract.stablecoin_mint`, and refunds the unearned
slice to the buyer in the same mint. There is **no swap, no oracle, and
no burn**: the program never converts the escrowed asset. The
per-transaction SOL fee (100,000 lamports to
`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`) is charged on
`pay_for_compute`, `finalize_pro_rata`, and `cancel_before_start`.

**What breaks if we skip it.** The seller is not paid and the escrow
can't finalize. There is nothing to defer here — the direct-stablecoin
path **is** the production design. The old devnet stub is deleted and
the swap/oracle/burn work is gone; the remaining item is redeploying the
new build to devnet.

### Design decision

The program's `Config` — `{settlement_authority, fee_wallet,
fee_lamports, bump}` — is set once by `init_config` and is **immutable**:
there are no governance instructions. The settlement authority is the
operator's key, pinned at deploy; it is the only signer that can
trigger `finalize_pro_rata`, so no arbitrary caller can finalize an
escrow with a fabricated `f` (which would refund the buyer and pay the
seller nothing).

### Steps

- [x] **1.1** `Config` account with `{settlement_authority, fee_wallet,
      fee_lamports, bump}`; `init_config` is the only setup call (not
      charged); no update instructions exist.
- [x] **1.2** `finalize_pro_rata` (`FinalizePro`) pays the seller via
      `seller_stablecoin_ata` in `contract.stablecoin_mint` and refunds
      the buyer unchanged. All swap/oracle account fields and the
      oracle-receiver SDK dependency are deleted.
- [x] **1.3** Per-transaction SOL fee (100,000 lamports) charged on
      `pay_for_compute` (buyer), `finalize_pro_rata` (settlement
      authority), and `cancel_before_start` (buyer), paid to
      `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`; skipped when
      `fee_lamports == 0`.
- [x] **1.4** The devnet bypass (the stub finalize instruction) deleted
      — the production path now pays stablecoin.
- [x] **1.5** Redeploy to devnet with the new build and run
      `init_config`. **Done** (2026-08-15): in-place upgrade of
      `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`; `init_config` ran
      against the `vtessera_config_v2` seed with the **throwaway CI soak
      key** (`Dtb4KYwzrEUomtWTcBJ1DziTzHbfHDyp9RPmRbjKuGVA`) as
      settlement authority — no operator key is used in automation.
      Config PDA `45JFFH3PQKxRAZqu432h3gdkF48ZSLfZAxtSpjKxdz9V`
      (fee wallet `J59EPyP…`, fee 100,000 lamports). Full devnet-demo run
      green (pay 2.0 → finalize f=0.5 → seller 1.0m / buyer refund 9.0m,
      escrow drained).

**Status.** Program change implemented, unit-tested (drift guard pins
error codes 6000–6007), and covered by the adversarial suite (§2).
Devnet redeploy + `init_config` live (§1.5). No swap, oracle, or burn
code remains.

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

- [x] **2.1** Pick a test harness — recommend **`litesvm`**
      (in-process Solana validator simulator, faster than the official
      `solana-program-test`). **Done:** `tests/adversarial/` is a
      standalone crate pinning LiteSVM 0.15.2 + the Agave 4.x tree, with
      its own lockfile (the 1.18 tree the program pins and the 4.x tree
      LiteSVM needs cannot share `subtle`).
- [x] **2.2** Write `programs/vtessera-escrow/tests/` with the following
      cases. Each = one Rust test (the suite lives at
      `tests/adversarial/tests/adversarial.rs` — see its coverage map):
      - [x] **2.2a** `pay_for_compute(price_micros = 0)` → fails with
            `ZeroPrice`
      - [x] **2.2b** Same `job_id` twice → second `pay_for_compute`
            fails (PDA already exists)
      - [x] **2.2c** `finalize_pro_rata(f_micros = 1_000_001)` → fails
            with `FractionOutOfRange`
      - [x] **2.2d** `finalize_pro_rata` twice → second fails with
            `AlreadyFinal`
      - [x] **2.2e** Buyer ATA with wrong mint → fails with `WrongMint`
      - [x] **2.2f** Seller ATA owned by someone other than
            `contract.seller_payout` → fails with `WrongOwner`
      - [x] **2.2g** `cancel_before_start` signed by non-buyer → fails
      - [x] **2.2h** `cancel_before_start` after `finalize_pro_rata` →
            fails with `AlreadyFinal`
      - [x] **2.2i** Math: `price = u64::MAX, f_micros = 999_999` →
            no silent overflow
      - [x] **2.2j** Math: `price = 1, f_micros = 1` → split rounds
            consistently
      - [x] **2.2k** `finalize_pro_rata` signed by a key other than
            the settlement_authority → fails
      - Plus §2.4 additions: fee charged on `pay_for_compute` (buyer
        SOL down by `fee_lamports`, fee wallet up), fee charged on
        finalize, fee charged on cancel, `fee_lamports = 0` disables
        the fee, `init_config` sets the fee fields, Config immutable
        after init (no update instruction), fraction=0 refund-only,
        finalize happy path (seller paid in stablecoin, escrow drained,
        buyer refunded), buyer unilateral cancel.
- [x] **2.3** Wire into CI — every push runs the harness.
      **Done:** `.github/workflows/ci.yml` installs Agave 3.1.14 + Anchor
      0.30.1, runs `anchor build`, the program's unit tests (which pin
      the numeric `EscrowError` codes as a drift guard), and the
      adversarial suite with `--locked`.
- [x] **2.4** Re-run the suite against the post-§1 program (the
      direct-stablecoin rewrite adds the fee-charging and
      config-immutability coverage listed above). **Done** — the suite
      is rebuilt for the new account set (config, escrow/buyer/seller
      stablecoin ATAs, fee wallet, system program) and covers the fee
      on all three instructions, zero-fee disable, and config
      immutability.

**Who.** All me.

**Effort.** ~1 day for the harness + the 11 cases.

---

## 3. Settlement authority and upgrade authority

**What it is.** Two signer roles:

- **Settlement authority** — the operator's key, pinned in `Config` at
  deploy. It signs `finalize_pro_rata`; this is a functional gate, not
  governance: it stops an arbitrary caller from finalizing an escrow
  with a fabricated `f`. `Config` is immutable after `init_config` and
  there are no governance instructions, so the authority cannot be
  rotated on-chain — changing it requires a redeploy (accepted for a
  single-operator project).
- **Upgrade authority** — can replace the on-chain program with new
  code. Today that keypair could deploy a version that routes escrows to
  a chosen address.

**What breaks if we skip it.** Laptop theft or a compromised key = total
loss of every active escrow (settlement key) or the ability to change
the program (upgrade key). The "credibly neutral, no one holds the
funds" framing only holds if no single party can change the program
after deploy.

### Steps

- [x] **3.1** `init_config(settlement_authority, fee_wallet,
      fee_lamports)` is the only setup call; no rotation instruction
      exists — the authority is the operator's key locked at deploy.
      Covered by the adversarial suite: a non-authority signer is
      rejected (§2.2k) and no update path exists
      (`config_immutable_after_init`).
- [x] **3.2** Devnet redeploy + `init_config` with a non-operator key
      (**done** 2026-08-15 — see §1.5; config PDA
      `45JFFH3PQKxRAZqu432h3gdkF48ZSLfZAxtSpjKxdz9V` is live with the
      throwaway CI soak key as authority, and the soak signed
      `finalize_pro_rata` with it).
- [x] **3.3** Handle the **upgrade authority** for mainnet.
      **Decided (2026-08-15): Option A — immutable.** The program freezes
      permanently with `--final`; exact sequence in the runbook below.
      The devnet program stays upgradeable until the audit (§4) closes
      so any findings can still be patched — the freeze happens only at
      mainnet deploy.
- [ ] **3.4** Verify on-chain **at mainnet deploy** — `solana program
      show <MAINNET_PROGRAM_ID>` reports `Authority: None` (immutable)
      after the freeze.

### Mainnet freeze runbook (immutable, Option A)

Order matters. Freeze **last**, once the program is deployed, configured,
and a first small flow is verified end-to-end:

1. Deploy the reproducible `.so` (§5) to mainnet:
   `solana program deploy --program-id <MAINNET_PROGRAM_ID> <reproduced .so>`
2. `init_config` with the mainnet settlement authority (Squads vault —
   or the operator key if that's the call).
3. Sanity-check: one small `pay_for_compute` + `finalize_pro_rata`.
4. Freeze permanently:
   `solana program set-upgrade-authority <MAINNET_PROGRAM_ID> --final`
5. Verify (§3.4): `solana program show <MAINNET_PROGRAM_ID>` →
   `Authority: None`.

`--final` is **irreversible** — no rollback exists, and after step 4 the
bytecode on-chain is the source of truth forever. Do not freeze the
devnet program while the soak (§6) and audit (§4) are still open.

**Who.**
- **You:** the operator — hold the deploy key, run `init_config` on
  devnet (3.2), decide and execute the upgrade-authority path (3.3).

**Effort.** Half a day.

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
found by a user post-mainnet means: (a) hope the upgrade authority moves
fast enough to fix it, or (b) accept the loss.

### Steps

- [ ] **4.1** Decide tier. Default: community review first, paid audit
      gated on revenue / TVL.
- [ ] **4.2** Prepare for review:
      - Write `programs/vtessera-escrow/SECURITY.md` with:
        - Threat model — what we defend against (custodial drain by
          settlement authority, double-spend), what we don't
          (Solana validator censorship, Circle freezing edge addresses)
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

- [x] **6.1** Write a soak-runner — `crates/devnet-demo/src/bin/soak.rs`.
      Done: seeded xorshift64* PRNG (replayable via `SOAK_SEED`), per
      iteration picks random `price_micros` in 1..=10_000_000, weighted
      `f_micros` pool, `cancel_before_start` with probability
      `--cancel-p` (default 0.2), random seller keypair; verifies the
      on-chain split/refund after each iteration and exits non-zero on
      any unexpected failure. Idempotent `init_config`. Run via
      `cargo run --bin soak -- --iters N` (`SOAK_RPC` to point at a
      local validator). Each iteration:
      - Pick random `price_micros` (1 to 10_000_000)
      - Pick random `f_micros` (0, 1, 500_000, 990_000, 1_000_000,
        uniform random)
      - With some probability, fire `cancel_before_start` instead of
        `pay`+`finalize`
      - Random seller pubkey
      - Run the flow; log result and any error
- [x] **6.2** Cron / systemd-timer it to fire every N minutes. Target
      ~100 runs / week. **Done:** `.github/workflows/soak-devnet.yml`
      runs the soak hourly via a GitHub Actions scheduled workflow (fires
      on `main` once merged; manual `workflow_dispatch` supported; payer
      top-up via devnet airdrop; soak log uploaded as an artifact). The
      payer keypair is the throwaway CI key in the `DEVNET_PAYER_KEYPAIR`
      repo secret — deliberately *not* the operator's key.
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
| 1 | #1, #2 done (direct-stablecoin + adversarial suite) | Redeploy devnet + run `init_config` (#1.5, #3.2); decide upgrade-authority path (#3.3) |
| 2 | Start #6 soak. Prepare #4 audit kit. | Run `set-upgrade-authority` (#3.3) |
| 3 | Start #6 soak; prepare #4 audit kit | Post audit request (#4.3) |
| 4-5 | Process audit findings | — |
| 6 | Final devnet soak with all changes in. Reproducible build of audited code. | — |
| 7 | Mainnet deploy. Small amounts only. | Run verify command (#5.5) yourself |

**5-7 weeks** realistic if everything goes well. Long pole: the audit.
Community-review-only shaves 1-2 weeks but lowers the trust signal.

## Decision points before starting

- **Audit tier (item #4).** Community / contest / paid firm. Default
  recommendation: community.
- **Upgrade-authority path (item #3).** Immutable vs multisig — decide
  before week 1; the settlement authority itself is the operator's key
  pinned at deploy.

## Mainnet deploy gate

**Do not deploy to Solana mainnet-beta until every checkbox above is
ticked.** The cost of being wrong on mainnet (lost user funds, broken
neutrality claim, recoverable only by upgrade-key custodial
intervention) is asymmetric versus the benefit of an earlier ship.
