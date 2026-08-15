//! Vtessera escrow — Module 4 (ROADMAP.md §4).
//!
//! One Anchor program. The buyer's stablecoin (EURC or USDC, whichever
//! the node's signed offer specifies) enters a **program-owned escrow
//! PDA** and leaves only by on-chain rules:
//!
//! - `pay_for_compute` deposits the contract price into the PDA and
//!   transfers a small flat SOL fee to the configured protocol fee
//!   wallet (0.0001 SOL, read from `Config`).
//! - `finalize_pro_rata` accepts the completion fraction `f` produced
//!   by the settlement crate (Module 3) and splits the escrow **in the
//!   same stablecoin**: the seller's earned slice `f × price` is paid
//!   directly to the seller's stablecoin ATA and the buyer's
//!   `(1 − f) × price` is refunded to the buyer. There is no HNT, no
//!   token swap, no price oracle, and no burn — the protocol never
//!   mints or holds any token of its own. The finalize call itself also
//!   carries the flat SOL protocol fee (payer = settlement authority).
//! - `cancel_before_start` lets a buyer reclaim the escrow with `f = 0`
//!   if the seller never started the job. It pays the flat SOL protocol
//!   fee too (payer = buyer) — the fee is per transaction, even when a
//!   contract never completes.
//! - `init_config` is the **only** setup call, run once right after
//!   deploy. It creates the single on-chain `Config` account holding
//!   the **settlement authority** (the operator's key, pinned at deploy
//!   and immutable afterwards) and the protocol fee wallet + amount.
//!   There are no governance instructions: this is a single-operator
//!   protocol with no governance token, so nothing can be changed
//!   on-chain after init. Changing the settlement authority or fee
//!   configuration requires a redeploy.

use anchor_lang::prelude::*;
use anchor_spl::token::{self, Mint, Token, TokenAccount, Transfer};

// Program ID — devnet deployment, regenerated on first mainnet deploy.
declare_id!("6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma");

/// Seed prefix for the program's single `Config` account (settlement
/// authority + protocol fee configuration).
pub const CONFIG_SEED: &[u8] = b"vtessera_config_v2";

/// Seed prefix for each `Contract` PDA.
pub const CONTRACT_SEED: &[u8] = b"contract";

/// Default protocol fee wallet — the operator's SOL address. The value
/// actually used on-chain is whatever `init_config` stored in `Config`;
/// this constant only exists so off-chain tooling and tests have a
/// canonical reference.
pub const DEFAULT_FEE_WALLET: Pubkey = pubkey!("J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh");

/// Default protocol fee per transaction, in lamports (0.0001 SOL).
pub const DEFAULT_FEE_LAMPORTS: u64 = 100_000;

#[program]
pub mod vtessera_escrow {
    use super::*;

    /// Create the program's `Config` account and pin the settlement
    /// authority + protocol fee configuration. Called once right after
    /// deploy by whoever holds the deployer key; the account is
    /// immutable afterwards (there are no update instructions), so all
    /// three values are fixed for the life of this program ID.
    ///
    /// **Race note:** `init` fails if the account already exists, and
    /// anyone may call this first. The config PDA is derivable from the
    /// program ID alone, so a griefer could front-run deploy + init.
    /// Mitigation: initialize in the same block as the deploy (the
    /// program is deployed with the config account as a planned
    /// step of the deploy transaction batch). Cost of a successful
    /// front-run on devnet is a DoS of finalize; on mainnet it costs
    /// the griefer real rent and requires winning the deploy block.
    pub fn init_config(
        ctx: Context<InitConfig>,
        settlement_authority: Pubkey,
        fee_wallet: Pubkey,
        fee_lamports: u64,
    ) -> Result<()> {
        let config = &mut ctx.accounts.config;
        config.settlement_authority = settlement_authority;
        config.fee_wallet = fee_wallet;
        config.fee_lamports = fee_lamports;
        config.bump = ctx.bumps.config;
        Ok(())
    }

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

        charge_fee(
            &ctx.accounts.buyer,
            &ctx.accounts.fee_wallet,
            &ctx.accounts.system_program,
            ctx.accounts.config.fee_lamports,
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
    /// settlement. Pays the seller's earned slice `f × price` in the
    /// contract's stablecoin mint and refunds `(1 − f) × price` to the
    /// buyer in the same mint. The settlement authority signs this and
    /// pays the flat SOL protocol fee, so no arbitrary caller can
    /// finalize an escrow with a fabricated `f` (which would refund the
    /// buyer and pay the seller nothing).
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
        let seeds: &[&[u8]] = &[CONTRACT_SEED, &job_id, &[bump]];
        let signer_seeds: &[&[&[u8]]] = &[seeds];

        // ---- Earned slice: pay the seller in the contract's mint ----
        if earned_stable > 0 {
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
            token::transfer(cpi_ctx, earned_stable)?;
        }

        // ---- Refund slice ----
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

        charge_fee(
            &ctx.accounts.settlement_authority,
            &ctx.accounts.fee_wallet,
            &ctx.accounts.system_program,
            ctx.accounts.config.fee_lamports,
        )?;

        ctx.accounts.contract.finalized = true;

        emit!(JobFinalized {
            job_id,
            f_micros,
            earned_stable,
            refund_stable,
        });

        Ok(())
    }

    /// Buyer reclaims escrow at `f = 0` if the seller never started the
    /// job. Distinct from `finalize_pro_rata` so the buyer can call it
    /// unilaterally after a timeout — no `f` from settlement needed.
    /// Pays the flat SOL protocol fee (per transaction, even for
    /// contracts that never complete).
    pub fn cancel_before_start(ctx: Context<CancelBeforeStart>) -> Result<()> {
        require!(!ctx.accounts.contract.finalized, EscrowError::AlreadyFinal);
        let refund = ctx.accounts.contract.price_micros;
        let job_id = ctx.accounts.contract.job_id;
        let bump = ctx.accounts.contract.bump;
        let seeds: &[&[u8]] = &[CONTRACT_SEED, &job_id, &[bump]];
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

        charge_fee(
            &ctx.accounts.buyer,
            &ctx.accounts.fee_wallet,
            &ctx.accounts.system_program,
            ctx.accounts.config.fee_lamports,
        )?;

        ctx.accounts.contract.finalized = true;
        Ok(())
    }
}

// ---------- Protocol fee --------------------------------------------------

/// Transfer the flat protocol fee from `payer` to `fee_wallet`.
/// `fee_lamports == 0` disables the fee. Both accounts must be writable
/// and `payer` must be a signer of the transaction.
fn charge_fee<'info>(
    payer: &AccountInfo<'info>,
    fee_wallet: &AccountInfo<'info>,
    system_program: &Program<'info, System>,
    fee_lamports: u64,
) -> Result<()> {
    if fee_lamports == 0 {
        return Ok(());
    }
    let fee_ix = anchor_lang::solana_program::system_instruction::transfer(
        payer.key,
        fee_wallet.key,
        fee_lamports,
    );
    anchor_lang::solana_program::program::invoke(
        &fee_ix,
        &[
            payer.to_account_info(),
            fee_wallet.to_account_info(),
            system_program.to_account_info(),
        ],
    )?;
    Ok(())
}

// ---------- Accounts ------------------------------------------------------

/// Program configuration: the settlement authority (the single key that
/// may finalize jobs — the operator's key on devnet and mainnet) and the
/// protocol fee wallet + per-transaction fee amount. Written once by
/// `init_config`; there are no update instructions, so it is immutable
/// for the life of the program ID.
#[account]
pub struct Config {
    pub settlement_authority: Pubkey,
    pub fee_wallet: Pubkey,
    pub fee_lamports: u64,
    pub bump: u8,
}

impl Config {
    pub const LEN: usize = 32 + 32 + 8 + 1;
}

#[account]
pub struct Contract {
    pub job_id: [u8; 32],
    pub buyer: Pubkey,
    /// Address whose stablecoin ATA (in the contract's mint) receives
    /// the earned slice at finalize.
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
pub struct InitConfig<'info> {
    /// Whoever deploys pays for the account. See the `init_config` race
    /// note in the instruction docs.
    #[account(mut)]
    pub authority: Signer<'info>,

    #[account(
        init,
        payer = authority,
        space = 8 + Config::LEN,
        seeds = [CONFIG_SEED],
        bump,
    )]
    pub config: Account<'info, Config>,

    pub system_program: Program<'info, System>,
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
        seeds = [CONTRACT_SEED, job_id.as_ref()],
        bump,
    )]
    pub contract: Account<'info, Contract>,

    /// CHECK: Receiver of the flat SOL protocol fee. Validated against
    /// the fee wallet pinned in `Config`.
    #[account(
        mut,
        constraint = fee_wallet.key() == config.fee_wallet @ EscrowError::WrongFeeWallet,
    )]
    pub fee_wallet: AccountInfo<'info>,

    #[account(
        seeds = [CONFIG_SEED],
        bump = config.bump,
    )]
    pub config: Account<'info, Config>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

/// Finalize accounts. The seller's earned slice is paid in the contract's
/// stablecoin mint, so only the stablecoin side is needed.
#[derive(Accounts)]
pub struct FinalizePro<'info> {
    /// Settlement authority. Must equal `Config::settlement_authority`
    /// (the operator's key, pinned at deploy). Signs the finalize and
    /// pays the flat SOL protocol fee.
    #[account(mut)]
    pub settlement_authority: Signer<'info>,

    #[account(
        seeds = [CONFIG_SEED],
        bump = config.bump,
        constraint = config.settlement_authority == settlement_authority.key()
            @ EscrowError::NotSettlementAuthority,
    )]
    pub config: Account<'info, Config>,

    #[account(
        mut,
        seeds = [CONTRACT_SEED, contract.job_id.as_ref()],
        bump = contract.bump,
    )]
    pub contract: Account<'info, Contract>,

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

    #[account(
        mut,
        constraint = seller_stablecoin_ata.mint == contract.stablecoin_mint @ EscrowError::WrongMint,
        constraint = seller_stablecoin_ata.owner == contract.seller_payout @ EscrowError::WrongOwner,
    )]
    pub seller_stablecoin_ata: Box<Account<'info, TokenAccount>>,

    /// Receiver of the flat SOL protocol fee. Validated against the fee
    /// wallet pinned in `Config`.
    #[account(
        mut,
        constraint = fee_wallet.key() == config.fee_wallet @ EscrowError::WrongFeeWallet,
    )]
    pub fee_wallet: AccountInfo<'info>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct CancelBeforeStart<'info> {
    #[account(mut)]
    pub buyer: Signer<'info>,

    #[account(
        mut,
        seeds = [CONTRACT_SEED, contract.job_id.as_ref()],
        bump = contract.bump,
        constraint = contract.buyer == buyer.key() @ EscrowError::WrongOwner,
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

    /// Receiver of the flat SOL protocol fee. Validated against the fee
    /// wallet pinned in `Config`.
    #[account(
        mut,
        constraint = fee_wallet.key() == config.fee_wallet @ EscrowError::WrongFeeWallet,
    )]
    pub fee_wallet: AccountInfo<'info>,

    #[account(
        seeds = [CONFIG_SEED],
        bump = config.bump,
    )]
    pub config: Account<'info, Config>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

// ---------- Events --------------------------------------------------------

#[event]
pub struct JobFinalized {
    pub job_id: [u8; 32],
    pub f_micros: u32,
    /// Earned slice in stablecoin units, paid to the seller in the
    /// contract's mint.
    pub earned_stable: u64,
    /// Refund slice in stablecoin units, returned to the buyer.
    pub refund_stable: u64,
}

// ---------- Errors --------------------------------------------------------

#[error_code]
pub enum EscrowError {
    #[msg("signer is not the configured settlement authority")]
    NotSettlementAuthority,
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
    #[msg("fee wallet does not match the configured protocol fee wallet")]
    WrongFeeWallet,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Drift guard for tests/adversarial/tests/adversarial.rs: the
    /// adversarial suite mirrors `EscrowError` as local constants because
    /// its litesvm / solana-sdk 2.1 tree cannot link this crate's 1.18
    /// tree. Anchor 0.30 encodes custom errors as 6000 + variant index;
    /// pin every code here so a variant reorder/insert fails this test
    /// instead of silently mis-asserting in the adversarial suite.
    #[test]
    fn escrow_error_codes_are_stable() {
        assert_eq!(u32::from(EscrowError::NotSettlementAuthority), 6000);
        assert_eq!(u32::from(EscrowError::ZeroPrice), 6001);
        assert_eq!(u32::from(EscrowError::FractionOutOfRange), 6002);
        assert_eq!(u32::from(EscrowError::AlreadyFinal), 6003);
        assert_eq!(u32::from(EscrowError::WrongMint), 6004);
        assert_eq!(u32::from(EscrowError::WrongOwner), 6005);
        assert_eq!(u32::from(EscrowError::MathOverflow), 6006);
        assert_eq!(u32::from(EscrowError::WrongFeeWallet), 6007);
    }

    #[test]
    fn config_len_matches_field_sizes() {
        // 8-discriminator is added by Anchor at account init; the bare
        // struct is exactly the sum of its fields.
        assert_eq!(Config::LEN, 32 + 32 + 8 + 1);
    }

    #[test]
    fn default_fee_constants_match_the_spec() {
        assert_eq!(DEFAULT_FEE_LAMPORTS, 100_000);
        assert_eq!(
            DEFAULT_FEE_WALLET.to_string(),
            "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh"
        );
    }
}
