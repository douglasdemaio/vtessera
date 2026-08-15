# Design — Direct stablecoin settlement + per-transaction SOL protocol fee

Date: 2026-08-15
Status: Approved by user (2026-08-15)
Owner: Douglas DeMaio

## Problem

The escrow program's production `finalize_pro_rata` path assumes the seller
earns **Helium (HNT)**: the caller bundles a Jupiter swap (stablecoin → HNT)
into the escrow's HNT ATA, the program Pyth-guards the post-condition, burns a
slice, and pays the seller in HNT. Three problems:

1. **The operator cannot mint HNT.** HNT is Helium's asset; Vtessera does not
   control its supply, so any "swap to HNT and burn" thesis depends entirely on
   an external market. It makes the project's payout model hostage to Helium.
2. **It is unnecessary complexity.** The escrow already holds the buyer's
   stablecoin (EURC or USDC — whichever the node's signed offer specifies). The
   natural model is to pay the seller the earned slice in that same stablecoin.
3. **The fee is a placeholder.** `pay_for_compute` already moves a flat
   `DRAFT_FEE_LAMPORTS` (0.0001 SOL) fee, but the wallet is a TODO and the
   value is a `DRAFT_` constant, not on-chain configuration.

## Decisions (user-approved)

1. **Pay the seller in the buyer's stablecoin.** No HNT, no Pyth, no Jupiter,
   no burn, no token minting anywhere in the protocol. The seller's earned
   slice `f × price` is paid directly in the contract's mint; `(1 − f) × price`
   is refunded to the buyer in the same mint.
2. **0.0001 SOL protocol fee per agent↔node transaction**, directed to SOL
   address `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`. Charged on
   `pay_for_compute` (payer = buyer), `finalize_pro_rata` (payer = settlement
   authority), and `cancel_before_start` (payer = buyer) — even when a contract
   never completes. The fee funds infrastructure costs and scales per
   transaction regardless of the job's outcome codes.
3. **No governance.** This is a single-operator project; there is no
   governance token and no governance instructions. `init_config` is the only
   setup call and `Config` is immutable afterward. `update_settlement_authority`
   is removed and no `update_fee_config` is added.
4. **Settlement authority is a functional gate, not governance.** It is the
   operator's key, pinned at deploy in `Config`, and it signs `finalize_pro_rata`
   so no arbitrary caller can finalize an escrow with a fabricated `f` (which
   would refund the buyer and pay the seller nothing). Changing it later
   requires a redeploy — accepted.

## Program changes (`programs/vtessera-escrow`)

### `Config` account

```rust
pub struct Config {
    pub settlement_authority: Pubkey, // operator key; finalize signer (pinned, immutable)
    pub fee_wallet: Pubkey,           // J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh
    pub fee_lamports: u64,            // 100_000 = 0.0001 SOL
    pub bump: u8,
}
// LEN = 32 + 32 + 8 + 1 = 73
```

- `init_config(settlement_authority, fee_wallet, fee_lamports)` — the **only**
  setup call, run once right after deploy. Sets all four fields; `Config` is
  immutable afterwards (no update instructions exist).
- `update_settlement_authority` and its accounts struct are **deleted**.
- `fee_lamports == 0` disables the fee (no-op transfer skipped).

### Fee helper

A private helper charges the SOL fee from a payer account to `config.fee_wallet`
via `system_instruction::transfer`, skipping when `fee_lamports == 0`:

- `pay_for_compute`: fee payer = `buyer`. `PayForCompute` gains a read-only
  `config` account; `DRAFT_FEE_LAMPORTS`/`DRAFT_FEE_WALLET_TODO` constants are
  removed and the transfer reads `config.fee_lamports`/`config.fee_wallet`.
- `finalize_pro_rata`: fee payer = `settlement_authority`. `FinalizePro` gains
  `system_program` and a `mut` `fee_wallet` account (validated against
  `config.fee_wallet`); reads `config.fee_lamports`.
- `cancel_before_start`: fee payer = `buyer`. The accounts struct gains
  `config` (read-only), `fee_wallet` (`mut`, validated against
  `config.fee_wallet`), and `system_program`.
- `init_config` is **not** charged (bootstrap).

### `finalize_pro_rata` becomes the stablecoin path

The instruction keeps its name and its stablecoin refund logic, and the
seller's earned slice is paid in the contract's mint:

- Add `seller_stablecoin_ata` to `FinalizePro`:
  `mint == contract.stablecoin_mint`, `owner == contract.seller_payout`.
- Delete: the HNT/Pyth earned-slice block, `hnt_mint`, `escrow_hnt_ata`,
  `seller_hnt_ata`, `pyth_hnt_usd`, `pyth_stablecoin_usd`, `expected_hnt_atomic`
  and its unit tests, and the constants `HNT_MINT`, `HNT_USD_FEED_ID_HEX`,
  `USDC_USD_FEED_ID_HEX`, `EUR_USD_FEED_ID_HEX`, `HNT_DECIMALS`,
  `MAX_PYTH_STALENESS_SECS`, `DRAFT_MAX_SLIPPAGE_BPS`, `DRAFT_BURN_BPS`.
- `finalize_pro_rata_stub` and `FinalizeProStub` are **deleted** — the
  production path now pays stablecoin, so the devnet bypass is obsolete.
- `pyth-solana-receiver-sdk` is removed from `Cargo.toml`.

### Errors

`EscrowError` keeps variants `6000–6006` in their current order (their numeric
codes are pinned by the drift-guard test). The four Pyth/swap variants
`PythStale` (6007), `BadFeedId` (6008), `BadOraclePrice` (6009),
`SwapBelowMinimum` (6010) are removed.

### Program ID

Unchanged (`6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`); the devnet program
is redeployed after this lands. `Contract.seller_payout` keeps its field and
the doc comment is updated to "stablecoin ATA".

## Off-chain callers

- `crates/x402-client/src/main.rs` — step 5 switches from
  `finalize_pro_rata_stub` to `finalize_pro_rata`, with the new account order:
  `settlement_authority` (signer), `config`, `contract`, `escrow_stablecoin_ata`,
  `buyer_stablecoin_ata`, `seller_stablecoin_ata`, `fee_wallet` (mut),
  `token_program`, `system_program`. Comments updated (no more "devnet lacks
  HNT mint/Pyth").
- `crates/devnet-demo/src/main.rs` — same switch; assert the fee was charged
  (buyer SOL balance drops by `fee_lamports` on pay) and seller/escrow
  stablecoin balances as today.
- `crates/node-api` MCP/A2A strings and the `crates/offer`/`crates/settlement`
  doc comments that reference HNT are updated (text-only).

## Tests

### Program unit tests (`src/lib.rs`)

- `expected_hnt_atomic` tests deleted (function deleted).
- Drift guard updated to pin `6000–6006` for the seven remaining variants.

### Adversarial suite (`tests/adversarial`)

Mirror the program changes:
- Delete HNT/Pyth fixtures: `HNT_MINT_STR`, feed IDs, price fixtures,
  `pyth_*` PDA helpers, `inject_pyth_account`, `inject_hnt_side`,
  `inject_pyth_defaults`, `set_hnt_balance`, `simulate_swap_consumes_earned`.
- Rebuild the finalize builder without HNT/Pyth accounts, adding
  `seller_stablecoin_ata`, `fee_wallet`, `system_program`.
- Rewrite/delete HNT-specific tests (`finalize_stale_*_feed_reverts`,
  `finalize_mismatched_feed_id_reverts`, `finalize_swap_underdelivery_reverts`,
  `finalize_nonpositive_price_reverts`, `finalize_overflow_math_reverts`,
  `production_finalize_happy_path`, `finalize_eur_feed_fallback_succeeds`,
  `settlement_authority_can_rotate`).
- Keep/repurpose: `stub_finalize_happy_path` → `finalize_happy_path` (seller
  paid in stablecoin, escrow drained, buyer refunded), `cancel_*`.
- New coverage: fee charged on `pay_for_compute` (buyer SOL down by
  `fee_lamports`, fee wallet up), fee charged on `finalize`, fee charged on
  `cancel`, `fee_lamports = 0` disables the fee, `update_*` instructions absent
  (init-then-frozen), config immutability after `init_config`.

## Documentation

- **`ROADMAP.md`** — module table; §0 "Stablecoin → HNT swap and the flat
  protocol fee" → direct stablecoin + per-tx SOL fee; §4c workthrough and §4d
  "Sellers are paid in HNT" thesis → paid in EURC/USDC (no protocol token, no
  swap, single-operator); ASCII diagram; the mainnet-checklist summary line;
  milestone M4 wording. Remove all authority-rotation/multisig references.
- **`README.md`** — intro ("sellers settling in HNT" → paid in EURC/USDC),
  ASCII flow diagram, "Where Vtessera fits" HNT thesis → direct stablecoin,
  devnet-demo/x402 references.
- **`BUILD.md`** — module-contract description ("earned slice swapped to HNT,
  Jupiter, Pyth-guarded, burn" → paid in the contract's stablecoin mint).
- **`MAINNET-CHECKLIST.md`** — §1 "Wire the Jupiter swap + Pyth price guard"
  → "Confirm finalize pays stablecoin directly"; drop HNT mint/feed/burn
  items; §3 multisig settlement-authority setup/rotation → single operator key
  pinned in `Config`; fee wallet/amount confirmed; no governance instructions.
- **`docs/DESIGN.md`** — "Why HNT, not a Vtessera token" → "Why EURC/USDC
  directly, no Vtessera token": no mintable token, no external-market
  dependency, sellers earn what buyers pay in, credible neutrality, single
  operator.
- **`programs/vtessera-escrow/README.md`** — flow, DRAFT constants table
  (fee now in `Config`, wallet/lamports pinned), status section.
- **`packaging/flatpak/README.md`** and the flatpak design spec — "HNT swap"
  phrasing → direct stablecoin payout.

## Definition of done

- `programs/vtessera-escrow`: `anchor build --no-idl` succeeds; unit tests
  pass (`cargo test --locked` in the program dir).
- `tests/adversarial`: `cargo test --locked` passes against the rebuilt ELF.
- Host workspace unaffected (no host crates touched beyond text comments;
  `cargo fmt --check` + `cargo test --locked` excluding `vtessera-gui` green).
- `x402-client` and `devnet-demo` build (each has its own lockfile) and the
  demo flow documents/asserts the fee.
- No HNT / Pyth / Jupiter / "swap to HNT" references remain in code, comments,
  or docs (verified by grep).
