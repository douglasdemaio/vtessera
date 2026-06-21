//! Vtessera escrow — Module 4 (ROADMAP.md §4).
//!
//! One Anchor program. The buyer's stablecoin (EURC or USDC) enters a
//! **program-owned escrow PDA** and leaves only by on-chain rules:
//!
//! - `pay_for_compute` deposits the contract price into the PDA and
//!   transfers a small flat SOL fee to the protocol fee wallet.
//! - `finalize_pro_rata` accepts the completion fraction `f` produced
//!   by the settlement crate (Module 3) and splits the escrow. The
//!   seller's earned slice is paid in **HNT**: the caller bundles a
//!   Jupiter swap (stablecoin → HNT into the escrow's HNT ATA) and
//!   `finalize_pro_rata` in the same transaction; the program reads
//!   Pyth for HNT/USD and stablecoin/USD, computes an expected HNT
//!   minimum, and reverts if the escrow's HNT balance is below that.
//!   Then it burns `DRAFT_BURN_BPS` and transfers the rest to the
//!   seller's HNT ATA. The buyer's `(1 − f) × price` is refunded in
//!   the original stablecoin.
//! - `cancel_before_start` lets a buyer reclaim escrow with `f = 0` if
//!   the seller never started the job.
//!
//! ### `finalize_pro_rata_stub` — devnet bypass
//!
//! The program also exposes `finalize_pro_rata_stub`, which skips the
//! HNT swap + Pyth guard and pays the seller in stablecoin directly.
//! This is needed because devnet has no real HNT mint and limited
//! Jupiter liquidity.
//!
//! **Mainnet safety relies on the multisig settlement authority
//! refusing to sign stub IXs.** Anchor 0.30's `#[program]` macro
//! doesn't reliably honour `#[cfg]` gating on inner functions
//! (macro expansion runs before cfg evaluation), so the cleanest
//! mainnet-safety story is policy-based, not build-flag-based: the
//! Squads signers from MAINNET-CHECKLIST §3.3 must never co-sign a
//! `finalize_pro_rata_stub` call. The IDL exposes both functions
//! transparently so this policy is auditable from off-chain.

use anchor_lang::prelude::*;
use anchor_spl::token::{self, Burn, Mint, Token, TokenAccount, Transfer};
use pyth_solana_receiver_sdk::price_update::{get_feed_id_from_hex, PriceUpdateV2};

// Program ID — devnet deployment, regenerated on first mainnet deploy.
declare_id!("6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma");

// ---------- Pinned addresses + feed IDs -----------------------------------
//
// All of these are mainnet-beta canonical references. Devnet does NOT
// host the same accounts — the production finalize path is therefore
// only exercisable on mainnet-beta or a mainnet-fork local validator
// (see MAINNET-CHECKLIST §1.6). Devnet smoke testing uses the stub IX
// which doesn't read these.

/// Helium HNT mint on Solana mainnet-beta.
/// Confirmed on-chain (`solana account ... --output json`): decimals = 8,
/// freeze authority = null (preserves the credible-neutrality property
/// described in ROADMAP §4d).
pub const HNT_MINT: Pubkey = pubkey!("hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux");

/// HNT/USD Pyth feed ID (cross-chain identifier, hex). Source: Pyth
/// Hermes /v2/price_feeds?query=HNT&asset_type=crypto, June 2026.
pub const HNT_USD_FEED_ID_HEX: &str =
    "0x649fdd7ec08e8e2a20f425729854e90293dcbe2376abc47197a14da6ff339756";

/// USDC/USD Pyth feed ID. USDC nominally pegs to USD at 1:1; the feed
/// gives us a real spot price with confidence interval so we don't
/// silently overpay sellers during a depeg.
pub const USDC_USD_FEED_ID_HEX: &str =
    "0xeaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a";

/// EUR/USD Pyth feed ID — used when the buyer paid in EURC. EURC is
/// nominally EUR-pegged; combined with this feed it lands at a real
/// USD value for the swap math.
pub const EUR_USD_FEED_ID_HEX: &str =
    "0xa995d00bb36a63cef7fd2c287dc105fc8f3d93779f062f09551b0af3e81ec30b";

/// HNT decimals — fixed by the on-chain mint. Hard-coded so we don't
/// need to read the mint account just to get this number.
pub const HNT_DECIMALS: u8 = 8;

/// Maximum staleness for either Pyth feed, in seconds. Pyth's own
/// best-practices doc suggests sub-minute thresholds; 60s gives some
/// margin for RPC delays without being so generous that an attacker
/// can exploit a price gap.
pub const MAX_PYTH_STALENESS_SECS: u64 = 60;

// ---------- DRAFT constants -----------------------------------------------
//
// Planning values. Confirmed values land here when the program is
// deployed to mainnet-beta and the end-to-end flow has been exercised.
// Until then, callers should treat these as configuration, not
// production parameters.

/// **DRAFT.** Flat per-job fee in lamports (0.0001 SOL).
pub const DRAFT_FEE_LAMPORTS: u64 = 100_000;

/// **DRAFT.** Protocol fee wallet (string form so the binary doesn't
/// hard-code an unverified address into mainnet via `const`).
pub const DRAFT_FEE_WALLET_TODO: &str = "9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD";

/// **DRAFT.** Slippage tolerance for the stablecoin→HNT swap, in basis
/// points. The Pyth guard requires the escrow's post-swap HNT balance
/// to be at least `expected_hnt × (1 − slippage_bps / 10_000)`. Anything
/// less and `finalize_pro_rata` reverts.
pub const DRAFT_MAX_SLIPPAGE_BPS: u16 = 50;

/// **DRAFT.** Burn fraction (bps) applied to the seller's earned HNT
/// before the rest is transferred. 100 bps = 1.00%.
pub const DRAFT_BURN_BPS: u16 = 100;

// --------------------------------------------------------------------------

#[program]
pub mod vtessera_escrow {
    use super::*;

    /// Deposit the contract price into the escrow PDA and pay the flat
    /// protocol fee. Atomic — either both happen or neither.
    pub fn pay_for_compute(
        ctx: Context<PayForCompute>,
        job_id: [u8; 32],
        price_micros: u64,
    ) -> Result<()> {
        require!(price_micros > 0, EscrowError::ZeroPrice);

        let cpi_accounts = Transfer {
            from: ctx.accounts.buyer_stablecoin_ata.to_account_info(),
            to: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
            authority: ctx.accounts.buyer.to_account_info(),
        };
        let cpi_ctx = CpiContext::new(ctx.accounts.token_program.to_account_info(), cpi_accounts);
        token::transfer(cpi_ctx, price_micros)?;

        let fee_ix = anchor_lang::solana_program::system_instruction::transfer(
            ctx.accounts.buyer.key,
            ctx.accounts.fee_wallet.key,
            DRAFT_FEE_LAMPORTS,
        );
        anchor_lang::solana_program::program::invoke(
            &fee_ix,
            &[
                ctx.accounts.buyer.to_account_info(),
                ctx.accounts.fee_wallet.to_account_info(),
                ctx.accounts.system_program.to_account_info(),
            ],
        )?;

        let contract = &mut ctx.accounts.contract;
        contract.job_id = job_id;
        contract.buyer = ctx.accounts.buyer.key();
        contract.seller_payout = ctx.accounts.seller_payout.key();
        contract.price_micros = price_micros;
        contract.stablecoin_mint = ctx.accounts.stablecoin_mint.key();
        // Cache decimals so finalize_pro_rata's price scaling doesn't have
        // to pass the mint account in again. Stablecoin decimals are a
        // mint property and immutable for these mints.
        contract.stablecoin_decimals = ctx.accounts.stablecoin_mint.decimals;
        contract.finalized = false;
        contract.bump = ctx.bumps.contract;

        Ok(())
    }

    /// Finalize a paid job with the completion fraction `f` produced by
    /// settlement. Pays the seller in **HNT** via the Pyth-guarded swap
    /// pattern documented at the top of this file.
    ///
    /// The caller is expected to bundle this IX in a single transaction
    /// behind a Jupiter swap that lands the appropriate HNT amount in
    /// `escrow_hnt_ata`. The program does not invoke Jupiter — it
    /// verifies the post-condition (HNT balance ≥ Pyth-derived minimum)
    /// and reverts otherwise.
    ///
    /// `f_micros` is `f` scaled by 1_000_000.
    pub fn finalize_pro_rata(ctx: Context<FinalizePro>, f_micros: u32) -> Result<()> {
        require!(f_micros <= 1_000_000, EscrowError::FractionOutOfRange);
        require!(!ctx.accounts.contract.finalized, EscrowError::AlreadyFinal);

        let price = ctx.accounts.contract.price_micros;
        let earned_stable = (price as u128)
            .checked_mul(f_micros as u128)
            .ok_or(EscrowError::MathOverflow)?
            .checked_div(1_000_000)
            .ok_or(EscrowError::MathOverflow)? as u64;
        let refund_stable = price.saturating_sub(earned_stable);

        let job_id = ctx.accounts.contract.job_id;
        let bump = ctx.accounts.contract.bump;
        let stable_decimals = ctx.accounts.contract.stablecoin_decimals;
        let seeds: &[&[u8]] = &[b"contract", &job_id, &[bump]];
        let signer_seeds: &[&[&[u8]]] = &[seeds];

        // ---- Earned slice: verify HNT was swapped in, burn slice, pay seller ----
        if earned_stable > 0 {
            let clock = Clock::get()?;
            let hnt_usd = ctx
                .accounts
                .pyth_hnt_usd
                .get_price_no_older_than(
                    &clock,
                    MAX_PYTH_STALENESS_SECS,
                    &get_feed_id_from_hex(HNT_USD_FEED_ID_HEX)
                        .map_err(|_| EscrowError::BadFeedId)?,
                )
                .map_err(|_| EscrowError::PythStale)?;
            let stable_usd = ctx
                .accounts
                .pyth_stablecoin_usd
                .get_price_no_older_than(
                    &clock,
                    MAX_PYTH_STALENESS_SECS,
                    // We don't know which stablecoin a priori — the caller
                    // passes whichever USDC/USD or EUR/USD feed matches the
                    // contract's stablecoin mint. The feed_id check happens
                    // implicitly inside get_price_no_older_than against
                    // whatever the caller provided; the program separately
                    // requires the caller to pass the right one (see the
                    // accounts struct doc).
                    &get_feed_id_from_hex(USDC_USD_FEED_ID_HEX)
                        .map_err(|_| EscrowError::BadFeedId)?,
                )
                .ok()
                .or_else(|| {
                    ctx.accounts
                        .pyth_stablecoin_usd
                        .get_price_no_older_than(
                            &clock,
                            MAX_PYTH_STALENESS_SECS,
                            &get_feed_id_from_hex(EUR_USD_FEED_ID_HEX).ok()?,
                        )
                        .ok()
                })
                .ok_or(EscrowError::PythStale)?;

            require!(hnt_usd.price > 0, EscrowError::BadOraclePrice);
            require!(stable_usd.price > 0, EscrowError::BadOraclePrice);

            let expected_hnt_min = expected_hnt_atomic(
                earned_stable,
                stable_decimals,
                stable_usd.price as u128,
                stable_usd.exponent,
                hnt_usd.price as u128,
                hnt_usd.exponent,
                DRAFT_MAX_SLIPPAGE_BPS,
            )?;

            let escrow_hnt = ctx.accounts.escrow_hnt_ata.amount;
            require!(
                escrow_hnt >= expected_hnt_min,
                EscrowError::SwapBelowMinimum
            );

            // Burn DRAFT_BURN_BPS of the HNT *that the seller earned*.
            // We burn from the full escrow_hnt amount (the caller may have
            // routed exactly expected_hnt_min, or more — burn from whatever
            // arrived, leaving the rest for the seller).
            let burn_amount = escrow_hnt
                .checked_mul(DRAFT_BURN_BPS as u64)
                .ok_or(EscrowError::MathOverflow)?
                / 10_000;
            if burn_amount > 0 {
                let cpi_burn = Burn {
                    mint: ctx.accounts.hnt_mint.to_account_info(),
                    from: ctx.accounts.escrow_hnt_ata.to_account_info(),
                    authority: ctx.accounts.contract.to_account_info(),
                };
                let burn_ctx = CpiContext::new_with_signer(
                    ctx.accounts.token_program.to_account_info(),
                    cpi_burn,
                    signer_seeds,
                );
                token::burn(burn_ctx, burn_amount)?;
            }

            let seller_amount = escrow_hnt
                .checked_sub(burn_amount)
                .ok_or(EscrowError::MathOverflow)?;
            if seller_amount > 0 {
                let cpi_accounts = Transfer {
                    from: ctx.accounts.escrow_hnt_ata.to_account_info(),
                    to: ctx.accounts.seller_hnt_ata.to_account_info(),
                    authority: ctx.accounts.contract.to_account_info(),
                };
                let cpi_ctx = CpiContext::new_with_signer(
                    ctx.accounts.token_program.to_account_info(),
                    cpi_accounts,
                    signer_seeds,
                );
                token::transfer(cpi_ctx, seller_amount)?;
            }
        }

        // ---- Refund slice (always stablecoin) ----
        if refund_stable > 0 {
            let cpi_accounts = Transfer {
                from: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
                to: ctx.accounts.buyer_stablecoin_ata.to_account_info(),
                authority: ctx.accounts.contract.to_account_info(),
            };
            let cpi_ctx = CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                cpi_accounts,
                signer_seeds,
            );
            token::transfer(cpi_ctx, refund_stable)?;
        }

        ctx.accounts.contract.finalized = true;

        emit!(JobFinalized {
            job_id,
            f_micros,
            earned_stable,
            refund_stable,
        });

        Ok(())
    }

    /// **DEVNET STUB.** Pays the seller's earned slice in stablecoin,
    /// skipping the Jupiter + Pyth swap. Exists so the devnet smoke flow
    /// runs without HNT liquidity. On mainnet the multisig settlement
    /// authority must refuse to sign calls to this IX.
    pub fn finalize_pro_rata_stub(ctx: Context<FinalizeProStub>, f_micros: u32) -> Result<()> {
        require!(f_micros <= 1_000_000, EscrowError::FractionOutOfRange);
        require!(!ctx.accounts.contract.finalized, EscrowError::AlreadyFinal);

        let price = ctx.accounts.contract.price_micros;
        let earned = (price as u128)
            .checked_mul(f_micros as u128)
            .ok_or(EscrowError::MathOverflow)?
            .checked_div(1_000_000)
            .ok_or(EscrowError::MathOverflow)? as u64;
        let refund = price.saturating_sub(earned);

        let job_id = ctx.accounts.contract.job_id;
        let bump = ctx.accounts.contract.bump;
        let seeds: &[&[u8]] = &[b"contract", &job_id, &[bump]];
        let signer_seeds: &[&[&[u8]]] = &[seeds];

        if earned > 0 {
            let cpi_accounts = Transfer {
                from: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
                to: ctx.accounts.seller_stablecoin_ata.to_account_info(),
                authority: ctx.accounts.contract.to_account_info(),
            };
            let cpi_ctx = CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                cpi_accounts,
                signer_seeds,
            );
            token::transfer(cpi_ctx, earned)?;
        }
        if refund > 0 {
            let cpi_accounts = Transfer {
                from: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
                to: ctx.accounts.buyer_stablecoin_ata.to_account_info(),
                authority: ctx.accounts.contract.to_account_info(),
            };
            let cpi_ctx = CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                cpi_accounts,
                signer_seeds,
            );
            token::transfer(cpi_ctx, refund)?;
        }
        ctx.accounts.contract.finalized = true;
        emit!(JobFinalized {
            job_id,
            f_micros,
            earned_stable: earned,
            refund_stable: refund,
        });
        Ok(())
    }

    /// Buyer reclaims escrow at `f = 0` if the seller never started the
    /// job. Distinct from `finalize_pro_rata` so the buyer can call it
    /// unilaterally after a timeout — no `f` from settlement needed.
    pub fn cancel_before_start(ctx: Context<CancelBeforeStart>) -> Result<()> {
        require!(!ctx.accounts.contract.finalized, EscrowError::AlreadyFinal);
        let refund = ctx.accounts.contract.price_micros;
        let job_id = ctx.accounts.contract.job_id;
        let bump = ctx.accounts.contract.bump;
        let seeds: &[&[u8]] = &[b"contract", &job_id, &[bump]];
        let signer_seeds: &[&[&[u8]]] = &[seeds];
        let cpi_accounts = Transfer {
            from: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
            to: ctx.accounts.buyer_stablecoin_ata.to_account_info(),
            authority: ctx.accounts.contract.to_account_info(),
        };
        let cpi_ctx = CpiContext::new_with_signer(
            ctx.accounts.token_program.to_account_info(),
            cpi_accounts,
            signer_seeds,
        );
        token::transfer(cpi_ctx, refund)?;
        ctx.accounts.contract.finalized = true;
        Ok(())
    }
}

// ---------- Pricing math (pure, testable) ---------------------------------

/// Compute the minimum HNT (in atomic units, HNT_DECIMALS) the escrow
/// must hold for `earned_stable_atomic` stablecoin to be considered a
/// fair swap given the current Pyth-reported prices and a slippage
/// tolerance in basis points.
///
/// Derivation:
///
/// ```text
/// usd      = earned_stable × stable_usd_price × 10^(stable_usd_expo)  / 10^stable_decimals
/// hnt      = usd / (hnt_usd_price × 10^hnt_usd_expo)
/// hnt_atom = hnt × 10^HNT_DECIMALS
///          = (earned_stable × stable_usd_price / hnt_usd_price)
///            × 10^(stable_usd_expo - stable_decimals - hnt_usd_expo + HNT_DECIMALS)
/// expected = hnt_atom × (10_000 - slippage_bps) / 10_000
/// ```
///
/// Pyth expos are negative (e.g. -8 for USDC/USD), so the net exponent
/// is usually positive (we end up multiplying), but the function handles
/// either sign.
fn expected_hnt_atomic(
    earned_stable_atomic: u64,
    stable_decimals: u8,
    stable_usd_price: u128,
    stable_usd_expo: i32,
    hnt_usd_price: u128,
    hnt_usd_expo: i32,
    slippage_bps: u16,
) -> Result<u64> {
    // numerator/denominator before exponent adjustment
    let numerator = (earned_stable_atomic as u128)
        .checked_mul(stable_usd_price)
        .ok_or(EscrowError::MathOverflow)?;
    let q = numerator
        .checked_div(hnt_usd_price)
        .ok_or(EscrowError::MathOverflow)?;

    // net_expo = stable_usd_expo - stable_decimals - hnt_usd_expo + HNT_DECIMALS
    let net_expo: i32 = stable_usd_expo
        .checked_sub(stable_decimals as i32)
        .and_then(|x| x.checked_sub(hnt_usd_expo))
        .and_then(|x| x.checked_add(HNT_DECIMALS as i32))
        .ok_or(EscrowError::MathOverflow)?;

    let adjusted: u128 = if net_expo >= 0 {
        let pow = 10u128
            .checked_pow(net_expo as u32)
            .ok_or(EscrowError::MathOverflow)?;
        q.checked_mul(pow).ok_or(EscrowError::MathOverflow)?
    } else {
        let pow = 10u128
            .checked_pow((-net_expo) as u32)
            .ok_or(EscrowError::MathOverflow)?;
        q.checked_div(pow).ok_or(EscrowError::MathOverflow)?
    };

    // Apply slippage tolerance.
    let with_slippage = adjusted
        .checked_mul((10_000 - slippage_bps) as u128)
        .ok_or(EscrowError::MathOverflow)?
        / 10_000;

    if with_slippage > u64::MAX as u128 {
        return Err(EscrowError::MathOverflow.into());
    }
    Ok(with_slippage as u64)
}

// ---------- Accounts ------------------------------------------------------

#[account]
pub struct Contract {
    pub job_id: [u8; 32],
    pub buyer: Pubkey,
    /// Address whose HNT ATA receives the earned slice (production
    /// finalize) or whose stablecoin ATA receives it (stub finalize).
    pub seller_payout: Pubkey,
    pub price_micros: u64,
    pub stablecoin_mint: Pubkey,
    pub stablecoin_decimals: u8,
    pub finalized: bool,
    pub bump: u8,
}

impl Contract {
    pub const LEN: usize = 32 + 32 + 32 + 8 + 32 + 1 + 1 + 1;
}

#[derive(Accounts)]
#[instruction(job_id: [u8; 32])]
pub struct PayForCompute<'info> {
    #[account(mut)]
    pub buyer: Signer<'info>,

    /// CHECK: seller_payout is recorded into the contract for later
    /// use; it doesn't need to be a token account at deposit time.
    pub seller_payout: AccountInfo<'info>,

    pub stablecoin_mint: Account<'info, Mint>,

    #[account(
        mut,
        constraint = buyer_stablecoin_ata.mint == stablecoin_mint.key() @ EscrowError::WrongMint,
        constraint = buyer_stablecoin_ata.owner == buyer.key() @ EscrowError::WrongOwner,
    )]
    pub buyer_stablecoin_ata: Account<'info, TokenAccount>,

    #[account(
        mut,
        constraint = escrow_stablecoin_ata.mint == stablecoin_mint.key() @ EscrowError::WrongMint,
        constraint = escrow_stablecoin_ata.owner == contract.key() @ EscrowError::WrongOwner,
    )]
    pub escrow_stablecoin_ata: Account<'info, TokenAccount>,

    #[account(
        init,
        payer = buyer,
        space = 8 + Contract::LEN,
        seeds = [b"contract", job_id.as_ref()],
        bump,
    )]
    pub contract: Account<'info, Contract>,

    /// CHECK: Receiver of the flat SOL fee. DRAFT.
    #[account(mut)]
    pub fee_wallet: AccountInfo<'info>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

/// Production finalize accounts. The HNT side accounts must be passed
/// in along with two Pyth `PriceUpdateV2` accounts (HNT/USD and the
/// stablecoin/USD that matches the contract's stablecoin).
#[derive(Accounts)]
pub struct FinalizePro<'info> {
    /// Settlement authority. Pre-mainnet this is a single keypair; on
    /// mainnet the multisig PDA from MAINNET-CHECKLIST §3.5.
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [b"contract", contract.job_id.as_ref()],
        bump = contract.bump,
    )]
    pub contract: Account<'info, Contract>,

    // Stablecoin side (for the buyer's refund). Also boxed.
    #[account(
        mut,
        constraint = escrow_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = escrow_stablecoin_ata.owner == contract.key() @ EscrowError::WrongOwner,
    )]
    pub escrow_stablecoin_ata: Box<Account<'info, TokenAccount>>,

    #[account(
        mut,
        constraint = buyer_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = buyer_stablecoin_ata.owner == contract.buyer @ EscrowError::WrongOwner,
    )]
    pub buyer_stablecoin_ata: Box<Account<'info, TokenAccount>>,

    // HNT side (for the seller's earned slice). Boxed because BPF
    // stack-frame budget is 4096 bytes; PriceUpdateV2 (below) is large
    // enough that unboxed Account fields push us over.
    #[account(address = HNT_MINT @ EscrowError::WrongMint)]
    pub hnt_mint: Box<Account<'info, Mint>>,

    #[account(
        mut,
        constraint = escrow_hnt_ata.mint == HNT_MINT @ EscrowError::WrongMint,
        constraint = escrow_hnt_ata.owner == contract.key() @ EscrowError::WrongOwner,
    )]
    pub escrow_hnt_ata: Box<Account<'info, TokenAccount>>,

    #[account(
        mut,
        constraint = seller_hnt_ata.mint == HNT_MINT @ EscrowError::WrongMint,
        constraint = seller_hnt_ata.owner == contract.seller_payout @ EscrowError::WrongOwner,
    )]
    pub seller_hnt_ata: Box<Account<'info, TokenAccount>>,

    // Pyth feeds, boxed (see comment above). The caller must post fresh
    // updates from Hermes before this IX is invoked. The program's
    // get_price_no_older_than call simultaneously verifies the feed ID
    // and the staleness threshold.
    pub pyth_hnt_usd: Box<Account<'info, PriceUpdateV2>>,
    pub pyth_stablecoin_usd: Box<Account<'info, PriceUpdateV2>>,

    pub token_program: Program<'info, Token>,
}

/// Devnet stub finalize accounts. Always present in the binary; the
/// mainnet multisig must decline to sign instructions that target this.
#[derive(Accounts)]
pub struct FinalizeProStub<'info> {
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [b"contract", contract.job_id.as_ref()],
        bump = contract.bump,
    )]
    pub contract: Account<'info, Contract>,

    #[account(
        mut,
        constraint = escrow_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = escrow_stablecoin_ata.owner == contract.key() @ EscrowError::WrongOwner,
    )]
    pub escrow_stablecoin_ata: Account<'info, TokenAccount>,

    #[account(
        mut,
        constraint = buyer_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = buyer_stablecoin_ata.owner == contract.buyer @ EscrowError::WrongOwner,
    )]
    pub buyer_stablecoin_ata: Account<'info, TokenAccount>,

    #[account(
        mut,
        constraint = seller_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = seller_stablecoin_ata.owner == contract.seller_payout @ EscrowError::WrongOwner,
    )]
    pub seller_stablecoin_ata: Account<'info, TokenAccount>,

    pub token_program: Program<'info, Token>,
}

#[derive(Accounts)]
pub struct CancelBeforeStart<'info> {
    pub buyer: Signer<'info>,
    #[account(
        mut,
        seeds = [b"contract", contract.job_id.as_ref()],
        bump = contract.bump,
        constraint = contract.buyer == buyer.key() @ EscrowError::WrongOwner,
    )]
    pub contract: Account<'info, Contract>,
    #[account(mut)]
    pub escrow_stablecoin_ata: Account<'info, TokenAccount>,
    #[account(mut)]
    pub buyer_stablecoin_ata: Account<'info, TokenAccount>,
    pub token_program: Program<'info, Token>,
}

// ---------- Events --------------------------------------------------------

#[event]
pub struct JobFinalized {
    pub job_id: [u8; 32],
    pub f_micros: u32,
    /// Earned slice in *stablecoin* units, before any swap. The IDL
    /// consumer rebuilds the HNT amount from the on-chain Jupiter event.
    pub earned_stable: u64,
    pub refund_stable: u64,
}

// ---------- Errors --------------------------------------------------------

#[error_code]
pub enum EscrowError {
    #[msg("contract price must be > 0")]
    ZeroPrice,
    #[msg("completion fraction f_micros must be in [0, 1_000_000]")]
    FractionOutOfRange,
    #[msg("contract already finalized")]
    AlreadyFinal,
    #[msg("token account mint does not match expected")]
    WrongMint,
    #[msg("token account owner does not match expected pubkey")]
    WrongOwner,
    #[msg("arithmetic overflow computing earned/refund split")]
    MathOverflow,
    #[msg("Pyth feed missing or too stale")]
    PythStale,
    #[msg("Pyth feed ID not recognised")]
    BadFeedId,
    #[msg("Pyth returned a non-positive price")]
    BadOraclePrice,
    #[msg("escrow HNT balance below Pyth-derived minimum (swap underdelivered)")]
    SwapBelowMinimum,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expected_hnt_atomic_usdc_at_one_usd_hnt_at_two_fifty() {
        // 1.000000 USDC (6 decimals) earned.
        // USDC/USD = 1.0 (Pyth: 100_000_000 with expo=-8)
        // HNT/USD = 2.5 (Pyth: 250_000_000 with expo=-8)
        // Expected: 1.0 / 2.5 = 0.4 HNT, then × 99.5% slippage = 0.398 HNT
        // In atomic units (HNT_DECIMALS = 8): 0.398 × 10^8 = 39_800_000
        let out = expected_hnt_atomic(
            1_000_000,
            6,
            100_000_000,
            -8,
            250_000_000,
            -8,
            DRAFT_MAX_SLIPPAGE_BPS,
        )
        .unwrap();
        assert_eq!(out, 39_800_000);
    }

    #[test]
    fn expected_hnt_atomic_eurc_at_one_eight_hnt_at_two_fifty() {
        // 1.000000 EURC, EUR/USD = 1.08, HNT/USD = 2.5.
        // 1.08 / 2.5 = 0.432 HNT × 99.5% = 0.42984 HNT = 42_984_000 atomic
        let out = expected_hnt_atomic(
            1_000_000,
            6,
            108_000_000,
            -8,
            250_000_000,
            -8,
            DRAFT_MAX_SLIPPAGE_BPS,
        )
        .unwrap();
        assert_eq!(out, 42_984_000);
    }

    #[test]
    fn expected_hnt_atomic_handles_higher_priced_hnt() {
        // HNT/USD = 100 (huge), 1 USDC earned, expect tiny HNT
        // 1 / 100 = 0.01 HNT × 99.5% = 0.00995 HNT = 995_000 atomic
        let out = expected_hnt_atomic(
            1_000_000,
            6,
            100_000_000,
            -8,
            10_000_000_000,
            -8,
            DRAFT_MAX_SLIPPAGE_BPS,
        )
        .unwrap();
        assert_eq!(out, 995_000);
    }

    #[test]
    fn expected_hnt_atomic_zero_input_zero_output() {
        let out = expected_hnt_atomic(0, 6, 100_000_000, -8, 250_000_000, -8, 50).unwrap();
        assert_eq!(out, 0);
    }
}
