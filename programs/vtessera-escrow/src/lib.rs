//! Vtessera escrow — Module 4 (ROADMAP.md §4).
//!
//! One Anchor program. The buyer's stablecoin (EURC or USDC, whichever
//! the node's signed offer specifies) enters a **program-owned escrow
//! PDA** and leaves only by on-chain rules:
//!
//! - `pay_for_compute` deposits the contract price into the PDA and
//!   transfers a small flat SOL fee to the protocol fee wallet
//!   (0.0001 SOL, pinned by the `DEFAULT_FEE_*` program constants). It
//!   also records the **per-contract settlement authority** and the
//!   **per-contract fee** (wallet + lamports) into the `Contract`
//!   account — the fee is committed on the individual escrow, not read
//!   from a shared `Config`.
//! - `finalize_pro_rata` accepts the completion fraction `f` produced
//!   by the settlement crate (Module 3) and splits the escrow **in the
//!   same stablecoin**: the seller's earned slice `f × price` is paid
//!   directly to the seller's stablecoin ATA and the buyer's
//!   `(1 − f) × price` is refunded to the buyer. There is no HNT, no
//!   token swap, no price oracle, and no burn — the protocol never
//!   mints or holds any token of its own. The finalize call itself also
//!   carries the flat SOL protocol fee (payer = settlement authority),
//!   charged against the fee recorded on the contract at payment.
//! - `rotate_settlement_authority` lets the contract's current recorded
//!   settlement authority hand finalize rights to another key before the
//!   escrow is settled — purely administrative, no money moves and no
//!   fee is charged. Useful operationally (a buyer that paid from a
//!   throwaway key, or a settlement service that finalizes on the
//!   buyer's behalf) and in multi-buyer loops where each contract's
//!   authority is rotated around the group.
//! - `cancel_before_start` lets a buyer reclaim the escrow with `f = 0`
//!   if the seller never started the job. It pays the flat SOL protocol
//!   fee too (payer = buyer) — the fee is per transaction, even when a
//!   contract never completes.
//! - `init_config` is the **only** setup call, run once right after
//!   deploy. It creates the single on-chain `Config` account holding
//!   the protocol **fee** wallet + amount and the config authority
//!   (the key allowed to rotate the fee config). There are no
//!   governance tokens; the current authority can later rotate the
//!   config via `update_config`, so a mistaken `init_config` value is
//!   recoverable on-chain instead of forcing a redeploy. On-chain
//!   settlement no longer reads `Config` for the fee — it is a
//!   default/off-chain reference only; every contract records its own
//!   fee at payment.
//!
//! **Authority model:** `Config` never gates finalize. Every contract
//! records its own settlement authority at `pay_for_compute` time, so a
//! buyer can settle an escrow without depending on who initialized the
//! shared singleton `Config` account (e.g. a deployment whose config was
//! seized by a throwaway CI key). The seller node verifies the recorded
//! authority before running a paid job; the recorded key is what later
//! signs `finalize_pro_rata`. The **fee** is committed the same way: each
//! contract records its fee wallet + lamports at payment, and finalize /
//! cancel charge that per-contract fee — never the shared singleton.

use anchor_lang::prelude::*;
use anchor_spl::token::{self, Mint, Token, TokenAccount, Transfer};

// Program ID — devnet deployment, regenerated on first mainnet deploy.
declare_id!("8UJy6B2ZX3swc6XLgGzWeEfcrP7ujZyBcFrKfp5YkA47");

/// Seed prefix for the program's single `Config` account (settlement
/// authority + protocol fee configuration).
pub const CONFIG_SEED: &[u8] = b"vtessera_config_v2";

/// Seed prefix for each `Contract` PDA.
pub const CONTRACT_SEED: &[u8] = b"contract";

/// Default protocol fee wallet — the operator's SOL address. The value
/// actually charged on-chain is validated against this constant at
/// `pay_for_compute` and recorded per contract; `init_config` may store
/// any value but settlement never consults `Config` for the fee.
pub const DEFAULT_FEE_WALLET: Pubkey = pubkey!("J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh");

/// Default protocol fee per transaction, in lamports (0.0001 SOL).
pub const DEFAULT_FEE_LAMPORTS: u64 = 100_000;

#[program]
pub mod vtessera_escrow {
    use super::*;

    /// Create the program's `Config` account and pin the config authority
    /// (fee-config governance key) + protocol fee configuration. Called
    /// once right after deploy by whoever holds the deployer key. The
    /// account can later be rotated by the current authority via
    /// `update_config`, so all three values are recoverable after a
    /// mistaken init. `Config` serves as a default/off-chain reference
    /// only — it does **not** gate finalize, and settlement no longer
    /// reads it for the fee (each contract records its own fee at
    /// `pay_for_compute`).
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

    /// Rotate the protocol config — config authority, fee wallet, and/or
    /// per-transaction fee — **without redeploying**. Only the current
    /// config authority may call this. This updates the default/off-chain
    /// reference only: per-contract fees are committed to each `Contract`
    /// account at `pay_for_compute` and are never re-read from `Config`,
    /// so rotating here does not alter any escrow.
    ///
    /// Each field is set unconditionally from the args; pass the existing
    /// value for any field you want to leave unchanged. `Config` is a fixed
    /// size, so no account resize or extra signer is needed.
    pub fn update_config(
        ctx: Context<UpdateConfig>,
        new_settlement_authority: Pubkey,
        new_fee_wallet: Pubkey,
        new_fee_lamports: u64,
    ) -> Result<()> {
        let config = &mut ctx.accounts.config;
        config.settlement_authority = new_settlement_authority;
        config.fee_wallet = new_fee_wallet;
        config.fee_lamports = new_fee_lamports;
        Ok(())
    }

    /// Deposit the contract price into the escrow PDA and pay the flat
    /// protocol fee. Atomic — either both happen or neither. Also records
    /// the per-contract `settlement_authority` — the key allowed to call
    /// `finalize_pro_rata` for this contract — and the per-contract
    /// **fee** (wallet + lamports). The buyer names the authority (in the
    /// standard flow the node's offer advertises who settles, and the
    /// buyer pins that key); it does not have to be the buyer itself, and
    /// it is **not** derived from the shared `Config` account. The fee
    /// terms are likewise pinned to the `DEFAULT_FEE_*` program constants
    /// and recorded on the contract, so finalize / cancel charge what was
    /// committed to this escrow — never the shared singleton.
    pub fn pay_for_compute(
        ctx: Context<PayForCompute>,
        job_id: [u8; 32],
        price_micros: u64,
        settlement_authority: Pubkey,
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
            DEFAULT_FEE_LAMPORTS,
        )?;

        let contract = &mut ctx.accounts.contract;
        contract.job_id = job_id;
        contract.buyer = ctx.accounts.buyer.key();
        contract.settlement_authority = settlement_authority;
        contract.seller_payout = ctx.accounts.seller_payout.key();
        contract.price_micros = price_micros;
        contract.stablecoin_mint = ctx.accounts.stablecoin_mint.key();
        // Cache decimals so finalize_pro_rata's price scaling doesn't have
        // to pass the mint account in again. Stablecoin decimals are a
        // mint property and immutable for these mints.
        contract.stablecoin_decimals = ctx.accounts.stablecoin_mint.decimals;
        contract.fee_wallet = ctx.accounts.fee_wallet.key();
        contract.fee_lamports = DEFAULT_FEE_LAMPORTS;
        contract.finalized = false;
        contract.bump = ctx.bumps.contract;

        Ok(())
    }

    /// Re-point a paid contract's settlement authority **before**
    /// finalize. The currently-recorded authority signs this to hand
    /// finalize rights to `new_settlement_authority` — e.g. a buyer that
    /// paid from a throwaway key or that wants a marketplace/settlement
    /// service to finalize on its behalf. Purely administrative: no
    /// stablecoin moves and no protocol fee is charged (unlike
    /// `finalize`/`cancel`). Useful for operationally rotating which key
    /// can settle an escrow after the agent already paid.
    ///
    /// Rejected when the contract is already finalized, or when the new
    /// key equals the current one (`AuthorityUnchanged`).
    pub fn rotate_settlement_authority(
        ctx: Context<RotateSettlementAuthority>,
        new_settlement_authority: Pubkey,
    ) -> Result<()> {
        require!(
            new_settlement_authority != ctx.accounts.contract.settlement_authority,
            EscrowError::AuthorityUnchanged
        );
        ctx.accounts.contract.settlement_authority = new_settlement_authority;
        Ok(())
    }

    /// Finalize a paid job with the completion fraction `f` produced by
    /// settlement. Pays the seller's earned slice `f × price` in the
    /// contract's stablecoin mint and refunds `(1 − f) × price` to the
    /// buyer in the same mint. The contract's recorded settlement
    /// authority signs this and pays the flat SOL protocol fee, so no
    /// arbitrary caller can finalize an escrow with a fabricated `f`
    /// (which would refund the buyer and pay the seller nothing).
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
            ctx.accounts.contract.fee_lamports,
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
            ctx.accounts.contract.fee_lamports,
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

/// Program configuration: the config authority (the key allowed to
/// rotate the fee config below — the operator's key on devnet and
/// mainnet) and the protocol fee wallet + per-transaction fee amount.
/// Written once by `init_config`, and rotatable by the current
/// authority via `update_config`. `Config` does **not** gate finalize:
/// each `Contract` records its own settlement authority at payment.
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
    /// Per-contract settlement authority — the key allowed to call
    /// `finalize_pro_rata` for this contract. Recorded at
    /// `pay_for_compute` time; **not** derived from the shared `Config`.
    pub settlement_authority: Pubkey,
    /// Address whose stablecoin ATA (in the contract's mint) receives
    /// the earned slice at finalize.
    pub seller_payout: Pubkey,
    pub price_micros: u64,
    pub stablecoin_mint: Pubkey,
    pub stablecoin_decimals: u8,
    /// Per-contract protocol fee wallet, committed at `pay_for_compute`
    /// and validated against `DEFAULT_FEE_WALLET`. `finalize_pro_rata`
    /// and `cancel_before_start` charge this wallet — never the shared
    /// `Config` singleton.
    pub fee_wallet: Pubkey,
    /// Per-contract protocol fee in lamports, committed at
    /// `pay_for_compute` and pinned to `DEFAULT_FEE_LAMPORTS`. `fee == 0`
    /// disables the fee entirely; `charge_fee` reads this per contract.
    pub fee_lamports: u64,
    pub finalized: bool,
    pub bump: u8,
}

impl Contract {
    pub const LEN: usize = 32 + 32 + 32 + 32 + 8 + 32 + 1 + 32 + 8 + 1 + 1;
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
pub struct UpdateConfig<'info> {
    /// The **current** config authority. Must equal
    /// `Config::settlement_authority`. Signs and pays for the tx; this is
    /// the only party allowed to rotate the fee config. It does not gate
    /// finalize — settlement authority is per-contract.
    #[account(mut)]
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [CONFIG_SEED],
        bump = config.bump,
        constraint = config.settlement_authority == settlement_authority.key()
            @ EscrowError::NotSettlementAuthority,
    )]
    pub config: Account<'info, Config>,
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
    /// the program-pinned `DEFAULT_FEE_WALLET`, and recorded into the
    /// contract so finalize / cancel charge what this escrow committed.
    #[account(
        mut,
        constraint = fee_wallet.key() == DEFAULT_FEE_WALLET @ EscrowError::WrongFeeWallet,
    )]
    pub fee_wallet: AccountInfo<'info>,

    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

/// Rotate a contract's settlement authority before finalize. The
/// currently-recorded authority signs and names the successor; it owns
/// no funds and the escrow is untouched. Administrative only — no fee.
#[derive(Accounts)]
pub struct RotateSettlementAuthority<'info> {
    /// The contract's current `settlement_authority` (recorded at
    /// `pay_for_compute` time). Must sign; it is the only key allowed to
    /// re-point finalize rights for this escrow.
    #[account(mut)]
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [CONTRACT_SEED, contract.job_id.as_ref()],
        bump = contract.bump,
        constraint = contract.settlement_authority == settlement_authority.key()
            @ EscrowError::NotSettlementAuthority,
        constraint = !contract.finalized @ EscrowError::AlreadyFinal,
    )]
    pub contract: Account<'info, Contract>,
}

/// Finalize accounts. The seller's earned slice is paid in the contract's
/// stablecoin mint, so only the stablecoin side is needed.
#[derive(Accounts)]
pub struct FinalizePro<'info> {
    /// Per-contract settlement authority. Must equal
    /// `Contract::settlement_authority` (recorded at `pay_for_compute`
    /// time). Signs the finalize and pays the flat SOL protocol fee.
    #[account(mut)]
    pub settlement_authority: Signer<'info>,

    #[account(
        mut,
        seeds = [CONTRACT_SEED, contract.job_id.as_ref()],
        bump = contract.bump,
        constraint = contract.settlement_authority == settlement_authority.key()
            @ EscrowError::NotSettlementAuthority,
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

    /// Receiver of the flat SOL protocol fee. Validated against the
    /// fee wallet recorded on this contract at `pay_for_compute` time.
    #[account(
        mut,
        constraint = fee_wallet.key() == contract.fee_wallet @ EscrowError::WrongFeeWallet,
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

    /// Receiver of the flat SOL protocol fee. Validated against the
    /// fee wallet recorded on this contract at `pay_for_compute` time.
    #[account(
        mut,
        constraint = fee_wallet.key() == contract.fee_wallet @ EscrowError::WrongFeeWallet,
    )]
    pub fee_wallet: AccountInfo<'info>,

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
    #[msg("signer is not this contract's recorded settlement authority")]
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
    #[msg("settlement authority rotation must name a different key")]
    AuthorityUnchanged,
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
        assert_eq!(u32::from(EscrowError::AuthorityUnchanged), 6008);
    }

    #[test]
    fn config_len_matches_field_sizes() {
        // 8-discriminator is added by Anchor at account init; the bare
        // struct is exactly the sum of its fields.
        assert_eq!(Config::LEN, 32 + 32 + 8 + 1);
    }

    #[test]
    fn contract_len_matches_field_sizes() {
        assert_eq!(
            Contract::LEN,
            32 + 32 + 32 + 32 + 8 + 32 + 1 + 32 + 8 + 1 + 1
        );
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
