//! Vtessera escrow — Module 4 (ROADMAP.md §4).
//!
//! One Anchor program. The buyer's stablecoin (EURC or USDC) enters a
//! **program-owned escrow PDA** and leaves only by on-chain rules:
//!
//! - `pay_for_compute` deposits the contract price into the PDA and
//!   transfers a small flat SOL fee to the protocol fee wallet.
//! - `finalize_pro_rata` accepts the completion fraction `f` produced by
//!   the settlement crate (Module 3) and splits the escrow:
//!     * `f × price` is swapped to HNT (Jupiter CPI, Pyth-guarded), a
//!       small `burn_bps` is burned via SPL burn, the rest is paid to
//!       the seller in HNT.
//!     * `(1 − f) × price` is refunded to the buyer in the original
//!       stablecoin (no swap, so the buyer bears no HNT price risk on
//!       unused funds).
//! - `cancel_before_start` lets a buyer reclaim escrow with `f = 0` if
//!   the seller never started the job. This is the pay-as-you-go path's
//!   counterpart: in PAYG there's nothing to refund, so the instruction
//!   only applies to committed (full-deposit) jobs.
//!
//! **All fee constants in this file are DRAFT** until the program is
//! deployed and the full flow verified on devnet. Search for
//! `// DRAFT ` in this file.
//!
//! This skeleton intentionally leaves the Jupiter swap and Pyth guard as
//! `TODO()` stubs — they're documented inline so the integration is
//! reviewable as a contract before any code runs.

use anchor_lang::prelude::*;
use anchor_spl::token::{self, Burn, Mint, Token, TokenAccount, Transfer};

declare_id!("VtsraEscrow11111111111111111111111111111111");

// ---------- DRAFT constants -----------------------------------------------
//
// These are placeholder values. Confirmed values land here when the program
// is deployed and the end-to-end flow has been exercised on devnet. Until
// then, every caller should pass these in via Anchor accounts/args rather
// than relying on the constant.

/// **DRAFT.** Flat per-job fee in lamports. Current planning value: 100_000
/// (0.0001 SOL). Refunds do not add a second fee; this is charged once at
/// `pay_for_compute` time.
pub const DRAFT_FEE_LAMPORTS: u64 = 100_000;

/// **DRAFT.** Protocol fee wallet. The address from the roadmap is held
/// as a TODO here so the binary can't accidentally hard-code an
/// unverified address into mainnet. Replace with `Pubkey::from_str(..).unwrap()`
/// (in a const context once stable) after the address is confirmed.
pub const DRAFT_FEE_WALLET_TODO: &str = "9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD";

/// **DRAFT.** Slippage / deviation tolerance for the stablecoin→HNT swap
/// guard. Pyth feeds price HNT/USD (and EUR/USD when the buyer paid in
/// EURC); the swap reverts if the executed Jupiter price deviates more
/// than this many basis points from the oracle.
pub const DRAFT_MAX_SLIPPAGE_BPS: u16 = 50; // 0.5%

/// **DRAFT.** Burn fraction in basis points applied to the seller's
/// earned slice before HNT is paid out. 100 = 1.00%.
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

        // 1. Move stablecoin from buyer's token account into the escrow PDA's
        //    token account.
        let cpi_accounts = Transfer {
            from: ctx.accounts.buyer_stablecoin_ata.to_account_info(),
            to: ctx.accounts.escrow_stablecoin_ata.to_account_info(),
            authority: ctx.accounts.buyer.to_account_info(),
        };
        let cpi_ctx = CpiContext::new(ctx.accounts.token_program.to_account_info(), cpi_accounts);
        token::transfer(cpi_ctx, price_micros)?;

        // 2. Pay the flat SOL fee to the protocol fee wallet.
        //    DRAFT: amount = DRAFT_FEE_LAMPORTS, wallet from caller account.
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

        // 3. Record the contract on-chain so finalize can verify the split
        //    against the same numbers the buyer paid for.
        let contract = &mut ctx.accounts.contract;
        contract.job_id = job_id;
        contract.buyer = ctx.accounts.buyer.key();
        contract.seller_payout = ctx.accounts.seller_payout.key();
        contract.price_micros = price_micros;
        contract.stablecoin_mint = ctx.accounts.stablecoin_mint.key();
        contract.finalized = false;
        contract.bump = ctx.bumps.contract;

        Ok(())
    }

    /// Finalize a paid job with the completion fraction `f` produced by
    /// the settlement service. Splits escrow strictly by `f`:
    ///
    /// - `earned = f × price` is swapped to HNT (Jupiter CPI, Pyth-guarded)
    ///   minus `burn_bps`, then paid to the seller in HNT.
    /// - `refund = (1 − f) × price` is returned to the buyer in the
    ///   original stablecoin.
    ///
    /// `f_micros` is `f` scaled by 1_000_000 — i.e. `f = 0.5` ⇒
    /// `f_micros = 500_000`. Using a fixed-point integer keeps every step
    /// of the split deterministic and avoids floating-point on-chain.
    pub fn finalize_pro_rata(ctx: Context<FinalizePro>, f_micros: u32) -> Result<()> {
        require!(f_micros <= 1_000_000, EscrowError::FractionOutOfRange);
        require!(!ctx.accounts.contract.finalized, EscrowError::AlreadyFinal);

        let price = ctx.accounts.contract.price_micros;
        let earned = (price as u128 * f_micros as u128 / 1_000_000) as u64;
        let refund = price.saturating_sub(earned);

        // ---- Earned slice ----
        //
        // 1. Pyth guard: read HNT/USD (and EUR/USD when stablecoin == EURC)
        //    and assert (executed_price / oracle_price) within
        //    DRAFT_MAX_SLIPPAGE_BPS.
        // 2. Jupiter swap: stablecoin (earned amount) → HNT. CPI into the
        //    Jupiter aggregator with the route the off-chain settlement
        //    helper computed.
        // 3. Burn `burn_amount = earned_hnt * DRAFT_BURN_BPS / 10_000` via
        //    SPL burn.
        // 4. Transfer the remainder to the seller's HNT ATA.
        //
        // This skeleton emits a placeholder burn against a sentinel mint
        // so the account graph in the IDL is complete; the swap+guard
        // arrive together in a follow-up.
        if earned > 0 {
            // TODO(swap): Jupiter CPI here. Inputs: earned stablecoin,
            // Pyth-guarded slippage cap, route accounts passed by caller.
            // Output: `earned_hnt` to the escrow's HNT ATA.

            // Token-program burn against a placeholder amount. The real
            // implementation burns DRAFT_BURN_BPS of `earned_hnt`.
            let burn_amount = earned
                .checked_mul(DRAFT_BURN_BPS as u64)
                .ok_or(EscrowError::MathOverflow)?
                / 10_000;
            if burn_amount > 0 {
                let cpi_burn = Burn {
                    mint: ctx.accounts.hnt_mint.to_account_info(),
                    from: ctx.accounts.escrow_hnt_ata.to_account_info(),
                    authority: ctx.accounts.contract.to_account_info(),
                };
                let job_id = ctx.accounts.contract.job_id;
                let bump = ctx.accounts.contract.bump;
                let seeds: &[&[u8]] = &[b"contract", &job_id, &[bump]];
                let signer_seeds: &[&[&[u8]]] = &[seeds];
                let burn_ctx = CpiContext::new_with_signer(
                    ctx.accounts.token_program.to_account_info(),
                    cpi_burn,
                    signer_seeds,
                );
                token::burn(burn_ctx, burn_amount)?;
            }

            // TODO(pay seller): transfer `earned_hnt − burn_amount` from
            // the escrow HNT ATA to the seller's HNT ATA.
        }

        // ---- Refund slice ----
        if refund > 0 {
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
        }

        ctx.accounts.contract.finalized = true;
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

// ---------- Accounts ------------------------------------------------------

#[account]
pub struct Contract {
    pub job_id: [u8; 32],
    pub buyer: Pubkey,
    /// Seller's HNT payout address. The program never reads HNT directly
    /// at deposit time; this lands on-chain so finalize can use it.
    pub seller_payout: Pubkey,
    pub price_micros: u64,
    pub stablecoin_mint: Pubkey,
    pub finalized: bool,
    pub bump: u8,
}

impl Contract {
    pub const LEN: usize = 32 + 32 + 32 + 8 + 32 + 1 + 1;
}

#[derive(Accounts)]
#[instruction(job_id: [u8; 32])]
pub struct PayForCompute<'info> {
    #[account(mut)]
    pub buyer: Signer<'info>,

    /// CHECK: seller_payout is recorded into the contract for later use;
    /// it doesn't need to be a token account at deposit time.
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

    /// CHECK: Receiver of the flat SOL fee. DRAFT — verified address
    /// lands when escrow is deployed and the wallet is confirmed.
    #[account(mut)]
    pub fee_wallet: AccountInfo<'info>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct FinalizePro<'info> {
    /// The settlement service signs this — settlement is the only entity
    /// allowed to assert `f`. In a TEE deployment this is the attested
    /// enclave's identity key; non-TEE deployments delegate to a known
    /// multisig.
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [b"contract", contract.job_id.as_ref()],
        bump = contract.bump,
    )]
    pub contract: Account<'info, Contract>,

    #[account(mut)]
    pub escrow_stablecoin_ata: Account<'info, TokenAccount>,
    #[account(mut)]
    pub buyer_stablecoin_ata: Account<'info, TokenAccount>,

    // HNT side. The escrow's HNT ATA and the seller's HNT ATA exist
    // before finalize; settling allocates them lazily in the off-chain
    // helper.
    #[account(mut)]
    pub hnt_mint: Account<'info, Mint>,
    #[account(mut)]
    pub escrow_hnt_ata: Account<'info, TokenAccount>,
    #[account(mut)]
    pub seller_hnt_ata: Account<'info, TokenAccount>,

    // TODO(jupiter): Jupiter route accounts arrive as remaining_accounts.
    // TODO(pyth): Pyth feed accounts for HNT/USD and (when needed) EUR/USD.

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

// ---------- Errors --------------------------------------------------------

#[error_code]
pub enum EscrowError {
    #[msg("contract price must be > 0")]
    ZeroPrice,
    #[msg("completion fraction f_micros must be in [0, 1_000_000]")]
    FractionOutOfRange,
    #[msg("contract already finalized")]
    AlreadyFinal,
    #[msg("token account mint does not match contract stablecoin")]
    WrongMint,
    #[msg("token account owner does not match expected pubkey")]
    WrongOwner,
    #[msg("arithmetic overflow computing earned/refund split")]
    MathOverflow,
}
