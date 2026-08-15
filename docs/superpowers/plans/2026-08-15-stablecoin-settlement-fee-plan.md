# Plan — Direct stablecoin settlement + per-transaction SOL protocol fee

Spec: `docs/superpowers/specs/2026-08-15-stablecoin-settlement-fee-design.md`
Branch: `stablecoin-settlement` (new, off `main`; the spec commit rides along)
Verify: per-phase gates below; full Definition of Done in the spec.

## Phase 1 — Program rewrite (`programs/vtessera-escrow/src/lib.rs`)

1. Header doc: rewrite module docs — no HNT/Pyth/Jupiter; seller paid in the
   contract's stablecoin mint; per-tx SOL fee (payer per IX); Config immutable
   after `init_config`; settlement authority = operator key, finalize signer;
   remove the "stub / mainnet multisig policy" block.
2. Constants: delete `HNT_MINT`, `HNT_USD_FEED_ID_HEX`, `USDC_USD_FEED_ID_HEX`,
   `EUR_USD_FEED_ID_HEX`, `HNT_DECIMALS`, `MAX_PYTH_STALENESS_SECS`,
   `DRAFT_MAX_SLIPPAGE_BPS`, `DRAFT_BURN_BPS`, `DRAFT_FEE_LAMPORTS`,
   `DRAFT_FEE_WALLET_TODO`. Keep `CONFIG_SEED`.
3. `Config` struct → `{ settlement_authority, fee_wallet, fee_lamports, bump }`,
   `LEN = 73`.
4. Instructions:
   - `init_config(settlement_authority, fee_wallet, fee_lamports)` — the only
     setup; no fee charged.
   - Delete `update_settlement_authority` + `UpdateSettlementAuthority`.
   - Add private `charge_fee(payer, fee_wallet, system_program, lamports)` helper
     (skips when `lamports == 0`).
   - `pay_for_compute`: add read-only `config`; replace the DRAFT transfer with
     `charge_fee(buyer, config.fee_wallet, system_program, config.fee_lamports)`.
   - `finalize_pro_rata`: delete the HNT/Pyth earned-slice block; pay `earned_stable`
     to `seller_stablecoin_ata` (same mint); refund unchanged; charge the fee from
     `settlement_authority` to `config.fee_wallet`; set `finalized`, emit.
   - Delete `finalize_pro_rata_stub`.
   - `cancel_before_start`: charge fee from `buyer`; refund unchanged.
5. Accounts: rewrite `FinalizePro` (drop hnt/pyth fields, add
   `seller_stablecoin_ata`, `fee_wallet` (mut, `constraint owner/wallet ==
   config.fee_wallet`), `system_program`); `FinalizeProStub` deleted;
   `CancelBeforeStart` gains `config`, `fee_wallet`, `system_program`;
   `PayForCompute` gains `config`.
6. `Contract.seller_payout` doc → "stablecoin ATA". `JobFinalized` doc →
   stablecoin-only (no "before any swap").
7. Errors: delete `PythStale`, `BadFeedId`, `BadOraclePrice`, `SwapBelowMinimum`.
   Drift-guard test → pin `6000–6006` (NotSettlementAuthority, ZeroPrice,
   FractionOutOfRange, AlreadyFinal, WrongMint, WrongOwner, MathOverflow).
   Delete `expected_hnt_atomic` + its four unit tests.
8. `Cargo.toml`: drop `pyth-solana-receiver-sdk`; drop `use
   pyth_solana_receiver_sdk` import; drop unused imports if any.

Verify: `cd programs/vtessera-escrow && cargo fmt --check && cargo test --locked`
(may need the Anchor/rust toolchain per the crate's rust-toolchain; if the host
toolchain is 1.79-pinned as CI, use that).

## Phase 2 — Adversarial suite (`tests/adversarial`)

9. Constants: drop `HNT_MINT_STR`, HNT feed ID, `HNT_USD_PRICE`, `PYTH_EXPO`,
   burn/slippage mirrors, `FEE_WALLET_STR` stays (same wallet). Add
   `FEE_LAMPORTS = 100_000`, `DEFAULT_FEE_WALLET_STR`.
10. Builders: `finalize_ix` → new account set (config, contract, escrow/buyer/
    seller stablecoin ATAs, fee_wallet mut, token_program, system_program);
    delete `finalize_stub_ix`; `pay_ix` builder gains `config`.
11. Harness: delete `inject_pyth_*`, `inject_hnt_side`, `set_hnt_balance`,
    `simulate_swap_consumes_earned`, `price_update_bytes`, `feed_id`.
    Add config init helper (authority = payer, wallet, lamports).
12. Tests:
    - Rename `stub_finalize_happy_path` → `finalize_happy_path` (seller paid in
      stablecoin, escrow drained, buyer refunded).
    - Delete the HNT/Pyth/rotation tests listed in the spec.
    - `cancel_before_start_refunds_buyer` — assert fee charged.
    - New: `pay_for_compute_charges_sol_fee` (buyer SOL −fee, wallet +fee);
      `finalize_charges_sol_fee`; `cancel_charges_sol_fee`;
      `zero_fee_lamports_disables_fee`; `init_config_sets_fee_fields`;
      `config_immutable_after_init` (no update instruction → no account
      mutation path); drift guard updated to 6000–6006.
    - `finalize_rejects_non_settlement_authority` kept (proves the operator-key
      gate works).

Verify: `anchor build --no-idl` in `programs/` first, then
`cd tests/adversarial && cargo fmt --check && cargo clippy --all-targets -- -D
warnings && cargo test --locked`.

## Phase 3 — Off-chain callers + text strings

13. `crates/x402-client/src/main.rs`: step 5 → `finalize_pro_rata` with new
    account order (settlement_authority, config, contract, escrow_stablecoin_ata,
    buyer_stablecoin_ata, seller_stablecoin_ata, fee_wallet mut, token_program,
    system_program); update comments; the agent's SOL ledger assertion accounts
    for the fee on pay + finalize is by the authority, not the buyer — verify
    assertions still hold (buyer pays fee only on pay_for_compute).
14. `crates/devnet-demo/src/main.rs`: same switch; assert buyer SOL drop on pay
    includes `FEE_LAMPORTS`; drop HNT comments.
15. `crates/node-api/src/lib.rs` (A2A card line ~252, MCP resource line ~419) and
    `crates/node-api/src/mcp.rs` (~140, 345): "HNT" → "paid to the seller in
    EURC/USDC".
16. `crates/offer/src/lib.rs` module doc (~31–34) and `PriceQuote::Paid`
    `payout_id` doc (~121–123): remove "ultimately receives HNT / swapped to HNT".
17. `crates/settlement/src/lib.rs` (~487–490): "swapped to HNT" → "paid directly".

Verify: host workspace `cargo fmt --check`; per-crate clippy + tests for
node-api (default + serve), offer, settlement.

## Phase 4 — Docs

18. `ROADMAP.md` (module table ~39–40; §0 ~50–58; §4c workthrough ~318–342; §4d
    ~345–368; ASCII ~419; checklist ~478–498; M4 ~546): direct-stablecoin model,
    per-tx SOL fee, single operator, no governance, no HNT/swap/burn.
19. `README.md` (~3–7, ~33, ~52, ~92–107, ~389, ~395): same; update demo notes
    (x402/demo now finalizes via `finalize_pro_rata`).
20. `BUILD.md` (~93, ~386–391).
21. `MAINNET-CHECKLIST.md` §1 (rewrite), §3 (single-operator key, no multisig),
    §4.3/4.4 wording, fee-wallet item.
22. `docs/DESIGN.md` "Why HNT" section (~72–79) → "Why EURC/USDC, no Vtessera
    token".
23. `programs/vtessera-escrow/README.md` (flow, constants table, status).
24. `packaging/flatpak/README.md` (~86) + `docs/superpowers/specs/2026-08-13-
    vtessera-gui-flatpak-design.md` (~93).

Verify: `grep -ri "HNT\b\|swap\|Jupiter\|pyth\|Pyth" --include=*.md --include=*.rs`
over repo (minus target/, lockfiles, programs' old builds) → only clean
references remain (any legitimate historical mention is removed, not kept).

## Phase 5 — Final gates + maintainer review

25. Gates: `anchor build --no-idl` + program unit tests; adversarial suite;
    workspace `cargo fmt --check`; `cargo test --locked --workspace --exclude
    vtessera-gui`; `x402-client` + `devnet-demo` build (own lockfiles);
    final grep for HNT/swap/Pyth residue.
26. Maintainer review: read the full diff of the program, callers, tests, and
    docs with a maintainer's eye; fix anything that doesn't meet bar as part of
    this pass; then write the acceptance verdict to the user.
27. Commit increments per phase (spec commit already in).
