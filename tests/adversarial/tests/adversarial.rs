//! Adversarial test suite for the vtessera escrow program
//! (MAINNET-CHECKLIST §2).
//!
//! Runs the compiled program `.so` inside [LiteSVM] — an in-process Solana
//! VM — and drives it with hand-built instructions. No validator, no
//! devnet, no external RPC: every test boots a fresh ledger and executes
//! against the real ELF.
//!
//! ## Build prerequisites
//!
//! The suite loads `programs/target/deploy/vtessera_escrow.so`, which is
//! produced by `anchor build` (run it first; CI does `anchor build` then
//! `cargo test`). Override the path with `VTESSERA_ESCROW_SO`.
//!
//! ## Why this is a standalone crate
//!
//! The program is compiled against solana-sdk 1.18 (anchor-lang 0.30),
//! while LiteSVM is a solana-sdk 2.1 runtime. The two trees cannot share a
//! Cargo.lock: the 1.18 tree pins `subtle = 2.4.1` exactly via
//! `solana-frozen-abi`, and the 2.1 tree needs `subtle >= 2.6`
//! (`CtOption::into_option` in `solana-curve25519`). This crate therefore
//! has its own `[workspace]`/lockfile and mirrors everything it needs from
//! the program as local constants (program ID, seeds, discriminators,
//! feed IDs, `EscrowError` codes). `spl-token` / ATA instructions are
//! hand-encoded against their fixed program ABI (instruction discriminants
//! and account layouts are stable public protocol) so no spl crate is
//! pulled in either. The program crate pins its own unit test asserting
//! the numeric error codes to catch mirror drift.
//!
//! ## Coverage map (checklist case → test)
//!
//! | §2.2 case | test |
//! |---|---|
//! | 2.2a zero price | `pay_zero_price_reverts` |
//! | 2.2b same job_id twice | `pay_same_job_id_twice_fails` |
//! | 2.2c fraction out of range | `finalize_rejects_fraction_out_of_range` (+ stub twin) |
//! | 2.2d double finalize | `finalize_rejects_second_finalize` |
//! | 2.2e buyer ATA wrong mint | `pay_rejects_buyer_ata_with_wrong_mint` |
//! | 2.2f wrong owner | `pay_rejects_buyer_ata_with_wrong_owner`, `finalize_rejects_seller_hnt_ata_wrong_owner` |
//! | 2.2g cancel by non-buyer | `cancel_before_start_rejects_non_buyer` |
//! | 2.2h cancel after finalize | `cancel_after_finalize_fails` |
//! | 2.2i overflow math | `finalize_overflow_math_reverts` |
//! | 2.2j tiny-fraction rounding | `finalize_rounds_tiny_fraction_consistently` |
//! | 2.2k finalize by non-authority | `finalize_rejects_non_settlement_authority` |
//! | — stale HNT feed | `finalize_stale_hnt_feed_reverts` |
//! | — stale stablecoin feed | `finalize_stale_stable_feed_reverts` |
//! | — wrong feed id | `finalize_mismatched_feed_id_reverts` |
//! | — swap underdelivers | `finalize_swap_underdelivery_reverts` |
//! | — non-positive oracle price | `finalize_nonpositive_price_reverts` |
//! | §2.4 production happy path | `production_finalize_happy_path` |
//! | §2.4 fraction = 0 (refund only) | `finalize_fraction_zero_refunds_buyer` |
//! | §2.4 EUR feed fallback | `finalize_eur_feed_fallback_succeeds` |
//! | §3.5 authority rotation | `settlement_authority_can_rotate` |
//! | devnet stub happy path | `stub_finalize_happy_path` |
//! | buyer unilateral cancel | `cancel_before_start_refunds_buyer` |

use litesvm::types::TransactionResult;
use litesvm::LiteSVM;
use sha2::{Digest, Sha256};
use solana_account::Account;
use solana_address::Address as Pubkey;
use solana_instruction::{error::InstructionError, AccountMeta, Instruction};
use solana_keypair::Keypair;
use solana_signer::Signer;
use solana_system_interface::instruction as system_instruction;
use solana_transaction::{Transaction, TransactionError};
use std::str::FromStr;

/// Anchor 0.30 assigns custom error codes as `ERROR_CODE_OFFSET + variant
/// index`, where `ERROR_CODE_OFFSET` is 6000. Mirror of the program's
/// `EscrowError` (programs/vtessera-escrow/src/lib.rs); variant ORDER is
/// load-bearing. A drift guard in the program's own unit tests pins the
/// numeric codes so a reorder here surfaces as a build/test failure.
#[derive(Debug, Clone, Copy)]
enum EscrowError {
    NotSettlementAuthority,
    ZeroPrice,
    FractionOutOfRange,
    AlreadyFinal,
    WrongMint,
    WrongOwner,
    MathOverflow,
    PythStale,
    // Not exercised directly (the feed-ID guard reports PythStale), but
    // kept so the variant ORDER mirrors the program's EscrowError exactly
    // — ordinal position is load-bearing for the numeric-code drift guard.
    #[allow(dead_code)]
    BadFeedId,
    BadOraclePrice,
    SwapBelowMinimum,
}

const ERROR_CODE_OFFSET: u32 = 6000;

impl From<EscrowError> for u32 {
    fn from(e: EscrowError) -> u32 {
        ERROR_CODE_OFFSET + e as u32
    }
}

// ---------- Pinned addresses (mainnet-beta canonical) ---------------------

const PROGRAM_ID_STR: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
const HNT_MINT_STR: &str = "hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux";
const TOKEN_PROGRAM_STR: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const ATA_PROGRAM_STR: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const RECEIVER_PROGRAM_STR: &str = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ";
/// DRAFT fee wallet from lib.rs — drives a real lamport transfer.
const FEE_WALLET_STR: &str = "9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD";

// ---------- Pyth feed IDs (lib.rs constants) ------------------------------

const HNT_USD_FEED_HEX: &str = "0x649fdd7ec08e8e2a20f425729854e90293dcbe2376abc47197a14da6ff339756";
const USDC_USD_FEED_HEX: &str =
    "0xeaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a";
const EUR_USD_FEED_HEX: &str = "0xa995d00bb36a63cef7fd2c287dc105fc8f3d93779f062f09551b0af3e81ec30b";

// ---------- Test fixture numbers ------------------------------------------

/// Realistic Pyth values: HNT = $2.50, USDC = $1.00 (both expo -8).
const HNT_USD_PRICE: i64 = 250_000_000;
const STABLE_USD_PRICE: i64 = 100_000_000;
const PYTH_EXPO: i32 = -8;

const STABLE_DECIMALS: u8 = 6;
const PAY_PRICE: u64 = 2_000_000; // 2.000000 stablecoin
const BUYER_MINT_AMOUNT: u64 = 10_000_000;

/// `expected_hnt_atomic(earned=2_000_000, 6, 100_000_000, -8, 250_000_000, -8, 50)`:
/// 2.0 / 2.5 = 0.8 HNT, × 99.5% slippage = 0.796 HNT = 79_600_000 atomic.
const DEFAULT_HNT_ESCROW: u64 = 79_600_000;
/// 79_600_000 × 100 bps / 10_000 = 796_000 burned; rest to seller.
const HNT_BURN: u64 = 796_000;
const SELLER_HNT_EARNED: u64 = DEFAULT_HNT_ESCROW - HNT_BURN;

// --------------------------------------------------------------------------

fn pk(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}

fn prog() -> Pubkey {
    pk(PROGRAM_ID_STR)
}

fn hnt_mint() -> Pubkey {
    pk(HNT_MINT_STR)
}

fn token_prog() -> Pubkey {
    pk(TOKEN_PROGRAM_STR)
}

fn ata_prog() -> Pubkey {
    pk(ATA_PROGRAM_STR)
}

fn receiver_prog() -> Pubkey {
    pk(RECEIVER_PROGRAM_STR)
}

fn fee_wallet() -> Pubkey {
    pk(FEE_WALLET_STR)
}

fn pyth_hnt_usd() -> Pubkey {
    let (p, _) = Pubkey::find_program_address(&[b"pyth-hnt-usd"], &prog());
    p
}

fn pyth_stable_usd() -> Pubkey {
    let (p, _) = Pubkey::find_program_address(&[b"pyth-stable-usd"], &prog());
    p
}

fn feed_id(hex: &str) -> [u8; 32] {
    let hex = hex.strip_prefix("0x").unwrap_or(hex);
    assert_eq!(hex.len(), 64, "feed id must be 32 bytes");
    let mut out = [0u8; 32];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).unwrap();
    }
    out
}

/// Anchor 8-byte instruction discriminator = first 8 bytes of
/// `sha256("global:<ix>")`.
fn disc(ix: &str) -> [u8; 8] {
    let d: [u8; 32] = Sha256::digest(format!("global:{ix}").as_bytes()).into();
    let mut o = [0u8; 8];
    o.copy_from_slice(&d[..8]);
    o
}

/// Decode the base64 payload of a `Program data: <base64>` log line and
/// return the first 8 bytes (an anchor event/instruction discriminator).
fn decode_b64_prefix(log_line: &str) -> Option<[u8; 8]> {
    let b64 = log_line.strip_prefix("Program data: ")?.trim_end();
    let alphabet = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut bits = 0u32;
    let mut nbits = 0u32;
    let mut out = Vec::with_capacity(8);
    for c in b64.bytes() {
        if c == b'=' {
            break;
        }
        let v = alphabet.iter().position(|&a| a == c).unwrap_or(255) as u32;
        if v == 255 {
            return None;
        }
        bits = (bits << 6) | v;
        nbits += 6;
        if nbits >= 8 {
            nbits -= 8;
            out.push((bits >> nbits) as u8);
            if out.len() >= 8 {
                break;
            }
        }
    }
    let mut o = [0u8; 8];
    o.copy_from_slice(&out[..8]);
    Some(o)
}

/// Path to the compiled program ELF.
fn program_so_path() -> String {
    std::env::var("VTESSERA_ESCROW_SO").unwrap_or_else(|_| {
        format!(
            "{}/../../programs/target/deploy/vtessera_escrow.so",
            env!("CARGO_MANIFEST_DIR")
        )
    })
}

/// `set_account` a raw account with rent-exempt lamports.
fn inject_account(svm: &mut LiteSVM, key: &Pubkey, owner: &Pubkey, data: Vec<u8>) {
    let lamports = svm.minimum_balance_for_rent_exemption(data.len());
    let mut acct = Account::new(lamports, data.len(), owner);
    acct.data = data;
    svm.set_account(*key, acct).unwrap();
}

// ---------- Hand-rolled SPL encoding --------------------------------------

/// `TokenInstruction::InitializeMint2` (discriminant 20) — no rent sysvar.
fn initialize_mint2_ix(authority: &Pubkey, mint: &Pubkey) -> Instruction {
    let mut data = vec![20u8, STABLE_DECIMALS];
    data.extend_from_slice(&authority.to_bytes());
    data.extend_from_slice(&[0u8; 36]); // freeze_authority: COption None
    Instruction {
        program_id: token_prog(),
        accounts: vec![AccountMeta::new(*mint, false)],
        data,
    }
}

/// `TokenInstruction::MintTo` (discriminant 7).
fn mint_to_ix(authority: &Pubkey, mint: &Pubkey, to: &Pubkey, amount: u64) -> Instruction {
    let mut data = vec![7u8];
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: token_prog(),
        accounts: vec![
            AccountMeta::new(*mint, false),
            AccountMeta::new(*to, false),
            AccountMeta::new_readonly(*authority, true),
        ],
        data,
    }
}

/// ATA `Create` (empty instruction data).
fn create_ata_ix(payer: &Pubkey, owner: &Pubkey, mint: &Pubkey) -> Instruction {
    Instruction {
        program_id: ata_prog(),
        accounts: vec![
            AccountMeta::new(*payer, true),
            AccountMeta::new(ata(owner, mint), false),
            AccountMeta::new_readonly(*owner, false),
            AccountMeta::new_readonly(*mint, false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
            AccountMeta::new_readonly(token_prog(), false),
        ],
        data: Vec::new(),
    }
}

/// Associated-token-account address: `seeds(owner, token_program, mint)`.
fn ata(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    let (p, _) = Pubkey::find_program_address(
        &[owner.as_ref(), token_prog().as_ref(), mint.as_ref()],
        &ata_prog(),
    );
    p
}

/// Raw `Mint` (spl-token Pack, 82 bytes): no authorities, uninitialized
/// supply, given decimals, initialized.
fn mint_bytes(decimals: u8) -> Vec<u8> {
    let mut d = Vec::with_capacity(82);
    d.extend_from_slice(&[0u8; 36]); // mint_authority: COption None
    d.extend_from_slice(&0u64.to_le_bytes()); // supply
    d.push(decimals);
    d.push(1); // is_initialized
    d.extend_from_slice(&[0u8; 36]); // freeze_authority: COption None
    d
}

/// Raw `TokenAccount` (spl-token Pack, 165 bytes).
fn token_account_bytes(mint: &Pubkey, owner: &Pubkey, amount: u64) -> Vec<u8> {
    let mut d = Vec::with_capacity(165);
    d.extend_from_slice(&mint.to_bytes());
    d.extend_from_slice(&owner.to_bytes());
    d.extend_from_slice(&amount.to_le_bytes());
    d.extend_from_slice(&[0u8; 36]); // delegate: COption None
    d.push(1); // state: Initialized
    d.extend_from_slice(&[0u8; 12]); // is_native: COption None
    d.extend_from_slice(&0u64.to_le_bytes()); // delegated_amount
    d.extend_from_slice(&[0u8; 36]); // close_authority: COption None
    d
}

/// Serialized `PriceUpdateV2` (Full verification, anchor discriminator).
fn price_update_bytes(feed_id: [u8; 32], price: i64, exponent: i32, publish_time: i64) -> Vec<u8> {
    let mut d = Vec::with_capacity(133);
    let dg: [u8; 32] = Sha256::digest(b"account:PriceUpdateV2").into();
    d.extend_from_slice(&dg[..8]); // anchor discriminator
    d.extend_from_slice(&[0u8; 32]); // write_authority
    d.push(1u8); // VerificationLevel::Full
    d.extend_from_slice(&feed_id);
    d.extend_from_slice(&price.to_le_bytes());
    d.extend_from_slice(&0u64.to_le_bytes()); // conf
    d.extend_from_slice(&exponent.to_le_bytes());
    d.extend_from_slice(&publish_time.to_le_bytes());
    d.extend_from_slice(&(publish_time - 1).to_le_bytes()); // prev_publish_time
    d.extend_from_slice(&price.to_le_bytes()); // ema_price
    d.extend_from_slice(&0u64.to_le_bytes()); // ema_conf
    d.extend_from_slice(&0u64.to_le_bytes()); // posted_slot
    d
}

fn inject_pyth_account(
    svm: &mut LiteSVM,
    key: &Pubkey,
    feed: [u8; 32],
    price: i64,
    publish_time: i64,
) {
    inject_account(
        svm,
        key,
        &receiver_prog(),
        price_update_bytes(feed, price, PYTH_EXPO, publish_time),
    );
}

// ---------- Escrow instruction builders -----------------------------------

fn init_config_ix(authority: &Pubkey, config: &Pubkey) -> Instruction {
    let mut data = disc("init_config").to_vec();
    data.extend_from_slice(&authority.to_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*authority, true),
            AccountMeta::new(*config, false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
        ],
        data,
    }
}

fn update_settlement_authority_ix(
    authority: &Pubkey,
    config: &Pubkey,
    new: &Pubkey,
) -> Instruction {
    let mut data = disc("update_settlement_authority").to_vec();
    data.extend_from_slice(&new.to_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new(*config, false),
        ],
        data,
    }
}

#[allow(clippy::too_many_arguments)]
fn pay_ix(
    buyer: &Pubkey,
    seller_payout: &Pubkey,
    mint: &Pubkey,
    buyer_ata: &Pubkey,
    escrow_ata: &Pubkey,
    contract: &Pubkey,
    job_id: [u8; 32],
    price: u64,
) -> Instruction {
    let mut data = disc("pay_for_compute").to_vec();
    data.extend_from_slice(&job_id);
    data.extend_from_slice(&price.to_le_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*buyer, true),
            AccountMeta::new_readonly(*seller_payout, false),
            AccountMeta::new_readonly(*mint, false),
            AccountMeta::new(*buyer_ata, false),
            AccountMeta::new(*escrow_ata, false),
            AccountMeta::new(*contract, false),
            AccountMeta::new(fee_wallet(), false),
            AccountMeta::new_readonly(token_prog(), false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
            // Defensive rent sysvar (same as the devnet demo).
            AccountMeta::new_readonly(solana_sdk_ids::sysvar::rent::id(), false),
        ],
        data,
    }
}

#[allow(clippy::too_many_arguments)]
fn finalize_ix(
    sa: &Pubkey,
    config: &Pubkey,
    contract: &Pubkey,
    escrow_stable: &Pubkey,
    buyer_stable: &Pubkey,
    escrow_hnt: &Pubkey,
    seller_hnt: &Pubkey,
    f_micros: u32,
) -> Instruction {
    let mut data = disc("finalize_pro_rata").to_vec();
    data.extend_from_slice(&f_micros.to_le_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new_readonly(*sa, true),
            AccountMeta::new_readonly(*config, false),
            AccountMeta::new(*contract, false),
            AccountMeta::new(*escrow_stable, false),
            AccountMeta::new(*buyer_stable, false),
            // HNT mint is `mut` in the program (the SPL burn CPI requires
            // it writable), so it must be passed writable here.
            AccountMeta::new(hnt_mint(), false),
            AccountMeta::new(*escrow_hnt, false),
            AccountMeta::new(*seller_hnt, false),
            AccountMeta::new_readonly(pyth_hnt_usd(), false),
            AccountMeta::new_readonly(pyth_stable_usd(), false),
            AccountMeta::new_readonly(token_prog(), false),
        ],
        data,
    }
}

fn finalize_stub_ix(
    sa: &Pubkey,
    config: &Pubkey,
    contract: &Pubkey,
    escrow_stable: &Pubkey,
    buyer_stable: &Pubkey,
    seller_stable: &Pubkey,
    f_micros: u32,
) -> Instruction {
    let mut data = disc("finalize_pro_rata_stub").to_vec();
    data.extend_from_slice(&f_micros.to_le_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new_readonly(*sa, true),
            AccountMeta::new_readonly(*config, false),
            AccountMeta::new(*contract, false),
            AccountMeta::new(*escrow_stable, false),
            AccountMeta::new(*buyer_stable, false),
            AccountMeta::new(*seller_stable, false),
            AccountMeta::new_readonly(token_prog(), false),
        ],
        data,
    }
}

fn cancel_ix(
    buyer: &Pubkey,
    contract: &Pubkey,
    escrow_stable: &Pubkey,
    buyer_stable: &Pubkey,
) -> Instruction {
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*buyer, true),
            AccountMeta::new(*contract, false),
            AccountMeta::new(*escrow_stable, false),
            AccountMeta::new(*buyer_stable, false),
            AccountMeta::new_readonly(token_prog(), false),
        ],
        data: disc("cancel_before_start").to_vec(),
    }
}

// ---------- Harness -------------------------------------------------------

/// A funded, paid, fully-injected ledger ready to run `finalize_*`.
///
/// State built:
/// 1. stablecoin mint (6 decimals, mint authority = payer/buyer)
/// 2. buyer/seller/escrow ATAs for the stablecoin mint
/// 3. `BUYER_MINT_AMOUNT` minted to the buyer
/// 4. `init_config` with settlement authority = payer
/// 5. `pay_for_compute(price)` into the contract PDA
/// 6. HNT mint + escrow/seller HNT ATAs injected (escrow holds
///    `DEFAULT_HNT_ESCROW`)
/// 7. fresh Pyth `PriceUpdateV2` accounts injected at both feed addresses
struct Harness {
    svm: LiteSVM,
    payer: Keypair,
    seller: Keypair,
    mint: Pubkey,
    job_id: [u8; 32],
    contract: Pubkey,
    config: Pubkey,
    price: u64,
    buyer_stable: Pubkey,
    escrow_stable: Pubkey,
    seller_stable: Pubkey,
    escrow_hnt: Pubkey,
    seller_hnt: Pubkey,
}

fn job_id() -> [u8; 32] {
    Sha256::digest(b"vtessera-adversarial:job").into()
}

impl Harness {
    fn new() -> Self {
        Self::new_with(PAY_PRICE, BUYER_MINT_AMOUNT)
    }

    fn setup_only(buyer_balance: u64) -> Self {
        let mut svm = LiteSVM::new();
        svm.add_program_from_file(prog(), program_so_path())
            .unwrap();

        let payer = Keypair::new();
        let seller = Keypair::new();
        svm.airdrop(&payer.pubkey(), 5_000_000_000).unwrap();
        svm.airdrop(&seller.pubkey(), 5_000_000_000).unwrap();
        // The program pays a fixed lamport fee to `fee_wallet` on every
        // deposit. On mainnet that wallet pre-exists (deployer-funded);
        // create it here rent-exempt, or the pay tx would fail with
        // InsufficientFundsForRent (0-byte account needs
        // (128 + 0) * 6960 = 890,880 lamports; DRAFT_FEE_LAMPORTS is only
        // 100,000).
        svm.airdrop(&fee_wallet(), 1_000_000_000).unwrap();

        // 1. stablecoin mint
        let mint_kp = Keypair::new();
        let mint = mint_kp.pubkey();
        let mint_len: usize = 82;
        let create_mint = system_instruction::create_account(
            &payer.pubkey(),
            &mint,
            svm.minimum_balance_for_rent_exemption(mint_len),
            mint_len as u64,
            &token_prog(),
        );
        let init_mint = initialize_mint2_ix(&payer.pubkey(), &mint);
        send(
            &mut svm,
            &payer,
            &[&payer, &mint_kp],
            &[create_mint, init_mint],
        );

        // 2. ATAs for the stablecoin mint
        let job_id = job_id();
        let (contract, _) = Pubkey::find_program_address(&[b"contract", &job_id], &prog());
        let buyer_stable = ata(&payer.pubkey(), &mint);
        let seller_stable = ata(&seller.pubkey(), &mint);
        let escrow_stable = ata(&contract, &mint);
        let ata_ixs = [
            create_ata_ix(&payer.pubkey(), &payer.pubkey(), &mint),
            create_ata_ix(&payer.pubkey(), &seller.pubkey(), &mint),
            create_ata_ix(&payer.pubkey(), &contract, &mint),
        ];
        send(&mut svm, &payer, &[&payer], &ata_ixs);

        // 3. fund the buyer
        let fund = mint_to_ix(&payer.pubkey(), &mint, &buyer_stable, buyer_balance);
        send(&mut svm, &payer, &[&payer], &[fund]);

        // 4. init_config
        let (config, _) = Pubkey::find_program_address(&[b"vtessera_config"], &prog());
        let cfg = init_config_ix(&payer.pubkey(), &config);
        send(&mut svm, &payer, &[&payer], &[cfg]);

        let seller_hnt = ata(&seller.pubkey(), &hnt_mint());
        Harness {
            svm,
            payer,
            seller,
            mint,
            job_id,
            contract,
            config,
            price: 0,
            buyer_stable,
            escrow_stable,
            seller_stable,
            escrow_hnt: ata(&contract, &hnt_mint()),
            seller_hnt,
        }
    }

    fn new_with(price: u64, buyer_balance: u64) -> Self {
        let mut h = Self::setup_only(buyer_balance);
        h.price = price;
        // 5. pay_for_compute
        h.pay(price);
        // 6. HNT side (mint + ATAs), escrow funded
        h.inject_hnt_side(DEFAULT_HNT_ESCROW);
        // 7. fresh Pyth feeds
        h.inject_pyth_defaults();
        h
    }

    /// `pay_for_compute` — separated out so callers can test the guard on
    /// the instruction itself.
    fn pay(&mut self, price: u64) {
        let pay = pay_ix(
            &self.payer.pubkey(),
            &self.seller.pubkey(),
            &self.mint,
            &self.buyer_stable,
            &self.escrow_stable,
            &self.contract,
            self.job_id,
            price,
        );
        send(&mut self.svm, &self.payer, &[&self.payer], &[pay]);
    }

    fn inject_hnt_side(&mut self, escrow_amount: u64) {
        inject_account(&mut self.svm, &hnt_mint(), &token_prog(), mint_bytes(8));
        inject_account(
            &mut self.svm,
            &self.escrow_hnt,
            &token_prog(),
            token_account_bytes(&hnt_mint(), &self.contract, escrow_amount),
        );
        inject_account(
            &mut self.svm,
            &self.seller_hnt,
            &token_prog(),
            token_account_bytes(&hnt_mint(), &self.seller.pubkey(), 0),
        );
    }

    /// Create a second stablecoin mint plus a funded buyer ATA for it.
    /// Used to probe that a buyer ATA whose mint doesn't match the
    /// contract's stablecoin mint is rejected (checklist §2.2e).
    fn add_alt_mint(&mut self) -> (Pubkey, Pubkey) {
        let alt_mint_kp = Keypair::new();
        let alt_mint = alt_mint_kp.pubkey();
        let create_mint = system_instruction::create_account(
            &self.payer.pubkey(),
            &alt_mint,
            self.svm.minimum_balance_for_rent_exemption(82),
            82,
            &token_prog(),
        );
        let init_mint = initialize_mint2_ix(&self.payer.pubkey(), &alt_mint);
        send(
            &mut self.svm,
            &self.payer,
            &[&self.payer, &alt_mint_kp],
            &[create_mint, init_mint],
        );
        let alt_buyer_ata = ata(&self.payer.pubkey(), &alt_mint);
        let create_ata = create_ata_ix(&self.payer.pubkey(), &self.payer.pubkey(), &alt_mint);
        send(&mut self.svm, &self.payer, &[&self.payer], &[create_ata]);
        let fund = mint_to_ix(
            &self.payer.pubkey(),
            &alt_mint,
            &alt_buyer_ata,
            BUYER_MINT_AMOUNT,
        );
        send(&mut self.svm, &self.payer, &[&self.payer], &[fund]);
        (alt_mint, alt_buyer_ata)
    }

    fn inject_pyth_defaults(&mut self) {
        inject_pyth_account(
            &mut self.svm,
            &pyth_hnt_usd(),
            feed_id(HNT_USD_FEED_HEX),
            HNT_USD_PRICE,
            0,
        );
        inject_pyth_account(
            &mut self.svm,
            &pyth_stable_usd(),
            feed_id(USDC_USD_FEED_HEX),
            STABLE_USD_PRICE,
            0,
        );
    }

    fn set_hnt_balance(&mut self, amount: u64) {
        inject_account(
            &mut self.svm,
            &self.escrow_hnt,
            &token_prog(),
            token_account_bytes(&hnt_mint(), &self.contract, amount),
        );
    }

    /// Model the bundled Jupiter leg that the real flow assumes: before
    /// `finalize_pro_rata`, the swap consumed `earned_stable` from the
    /// escrow's stablecoin ATA (delivering HNT into `escrow_hnt_ata`).
    /// Re-inject the stable ATA at its post-swap balance (the refund).
    fn simulate_swap_consumes_earned(&mut self, f_micros: u32) {
        let earned = (self.price as u128 * f_micros as u128 / 1_000_000) as u64;
        let refund = self.price - earned;
        inject_account(
            &mut self.svm,
            &self.escrow_stable,
            &token_prog(),
            token_account_bytes(&self.mint, &self.contract, refund),
        );
    }

    fn inject_pyth_hnt(&mut self, feed: [u8; 32], price: i64, publish_time: i64) {
        inject_pyth_account(&mut self.svm, &pyth_hnt_usd(), feed, price, publish_time);
    }

    fn inject_pyth_stable(&mut self, feed: [u8; 32], price: i64, publish_time: i64) {
        inject_pyth_account(&mut self.svm, &pyth_stable_usd(), feed, price, publish_time);
    }

    fn token_balance(&self, key: &Pubkey) -> u64 {
        let acct = self
            .svm
            .get_account(key)
            .unwrap_or_else(|| panic!("no token account at {key}"));
        u64::from_le_bytes(acct.data[64..72].try_into().unwrap())
    }

    fn config_authority(&self) -> Pubkey {
        let acct = self.svm.get_account(&self.config).unwrap();
        Pubkey::new_from_array(acct.data[8..40].try_into().unwrap())
    }

    fn finalize_tx(&self, sa: &Keypair, f_micros: u32) -> Transaction {
        let ix = finalize_ix(
            &sa.pubkey(),
            &self.config,
            &self.contract,
            &self.escrow_stable,
            &self.buyer_stable,
            &self.escrow_hnt,
            &self.seller_hnt,
            f_micros,
        );
        Transaction::new_signed_with_payer(
            &[ix],
            Some(&sa.pubkey()),
            &[sa],
            self.svm.latest_blockhash(),
        )
    }

    fn finalize_stub_tx(&self, sa: &Keypair, f_micros: u32) -> Transaction {
        let ix = finalize_stub_ix(
            &sa.pubkey(),
            &self.config,
            &self.contract,
            &self.escrow_stable,
            &self.buyer_stable,
            &self.seller_stable,
            f_micros,
        );
        Transaction::new_signed_with_payer(
            &[ix],
            Some(&sa.pubkey()),
            &[sa],
            self.svm.latest_blockhash(),
        )
    }

    fn cancel_tx(&self) -> Transaction {
        let ix = cancel_ix(
            &self.payer.pubkey(),
            &self.contract,
            &self.escrow_stable,
            &self.buyer_stable,
        );
        Transaction::new_signed_with_payer(
            &[ix],
            Some(&self.payer.pubkey()),
            &[&self.payer],
            self.svm.latest_blockhash(),
        )
    }
}

// ---------- Plumbing ------------------------------------------------------

/// Send a transaction that is expected to succeed.
fn send(svm: &mut LiteSVM, payer: &Keypair, signers: &[&Keypair], ixs: &[Instruction]) {
    let tx = Transaction::new_signed_with_payer(
        ixs,
        Some(&payer.pubkey()),
        signers,
        svm.latest_blockhash(),
    );
    match svm.send_transaction(tx) {
        Ok(_) => {}
        Err(f) => panic!(
            "unexpected tx failure: {:?}\nlogs: {:#?}",
            f.err, f.meta.logs
        ),
    }
}
/// Assert the transaction failed with exactly the given escrow error code.
fn expect_custom(result: TransactionResult, expected: EscrowError) {
    let err = match result {
        Ok(_) => panic!("transaction unexpectedly succeeded; expected {expected:?}"),
        Err(f) => {
            eprintln!("logs:\n{:#?}", f.meta.logs);
            f.err
        }
    };
    let expected_code: u32 = expected.into();
    match err {
        TransactionError::InstructionError(_, InstructionError::Custom(code)) => {
            assert_eq!(
                code, expected_code,
                "custom error code mismatch (expected {expected_code} = {expected:?})"
            );
        }
        TransactionError::InstructionError(_, other) => panic!(
            "unexpected instruction error {other:?}; expected custom code {expected_code} ({expected:?})"
        ),
        other => panic!(
            "unexpected transaction error {other:?}; expected custom code {expected_code} ({expected:?})"
        ),
    }
}

/// Assert the transaction failed with *some* error (used where the exact
/// failure is an Anchor runtime error rather than one of our custom
/// codes — e.g. re-initialising an existing PDA).
fn expect_tx_error(result: TransactionResult) {
    match result {
        Ok(_) => panic!("transaction unexpectedly succeeded"),
        Err(f) => {
            eprintln!("logs:\n{:#?}", f.meta.logs);
        }
    }
}

// ---------- Production finalize: adversarial -----------------------------

#[test]
fn finalize_rejects_non_settlement_authority() {
    let mut h = Harness::new();
    let attacker = Keypair::new();
    h.svm.airdrop(&attacker.pubkey(), 1_000_000_000).unwrap();
    let res = h.svm.send_transaction(h.finalize_tx(&attacker, 1_000_000));
    expect_custom(res, EscrowError::NotSettlementAuthority);
}

#[test]
fn finalize_rejects_fraction_out_of_range() {
    let mut h = Harness::new();
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_001));
    expect_custom(res, EscrowError::FractionOutOfRange);
}

#[test]
fn finalize_rejects_second_finalize() {
    let mut h = Harness::new();
    h.svm
        .send_transaction(h.finalize_tx(&h.payer, 1_000_000))
        .unwrap();
    // Fresh blockhash so the second tx isn't an exact replay (which would
    // return AlreadyProcessed, masking the program's real answer).
    h.svm.expire_blockhash();
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::AlreadyFinal);
}

#[test]
fn finalize_stale_hnt_feed_reverts() {
    let mut h = Harness::new();
    // publish_time far in the past relative to the genesis clock (t=0).
    h.inject_pyth_hnt(feed_id(HNT_USD_FEED_HEX), HNT_USD_PRICE, -1_000);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::PythStale);
}

#[test]
fn finalize_stale_stable_feed_reverts() {
    let mut h = Harness::new();
    h.inject_pyth_stable(feed_id(USDC_USD_FEED_HEX), STABLE_USD_PRICE, -1_000);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::PythStale);
}

#[test]
fn finalize_mismatched_feed_id_reverts() {
    let mut h = Harness::new();
    // HNT/USD account actually holds the USDC feed id.
    h.inject_pyth_hnt(feed_id(USDC_USD_FEED_HEX), HNT_USD_PRICE, 0);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::PythStale);
}

#[test]
fn finalize_swap_underdelivery_reverts() {
    let mut h = Harness::new();
    // One atomic unit short of the Pyth-derived minimum.
    h.set_hnt_balance(DEFAULT_HNT_ESCROW - 1);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::SwapBelowMinimum);
}

#[test]
fn finalize_nonpositive_price_reverts() {
    let mut h = Harness::new();
    h.inject_pyth_hnt(feed_id(HNT_USD_FEED_HEX), 0, 0);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::BadOraclePrice);
}

#[test]
fn finalize_overflow_math_reverts() {
    // price = u64::MAX with a full completion and normal oracle prices.
    // expected_hnt_atomic must exceed u64::MAX → MathOverflow fires before
    // any balance check or transfer.
    let mut h = Harness::new_with(u64::MAX, u64::MAX);
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::MathOverflow);
}

#[test]
fn pay_zero_price_reverts() {
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let pay = pay_ix(
        &h.payer.pubkey(),
        &h.seller.pubkey(),
        &h.mint,
        &h.buyer_stable,
        &h.escrow_stable,
        &h.contract,
        h.job_id,
        0,
    );
    let tx = Transaction::new_signed_with_payer(
        &[pay],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::ZeroPrice);
}

#[test]
fn pay_same_job_id_twice_fails() {
    // §2.2b: the second deposit for an existing job_id must fail (the
    // contract PDA already exists).
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    h.pay(PAY_PRICE);
    let pay = pay_ix(
        &h.payer.pubkey(),
        &h.seller.pubkey(),
        &h.mint,
        &h.buyer_stable,
        &h.escrow_stable,
        &h.contract,
        h.job_id,
        PAY_PRICE,
    );
    let tx = Transaction::new_signed_with_payer(
        &[pay],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    let res = h.svm.send_transaction(tx);
    expect_tx_error(res);
}

#[test]
fn pay_rejects_buyer_ata_with_wrong_mint() {
    // §2.2e: buyer ATA's mint must match the contract's stablecoin mint.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let (alt_mint, alt_buyer_ata) = h.add_alt_mint();
    let pay = pay_ix(
        &h.payer.pubkey(),
        &h.seller.pubkey(),
        &h.mint,        // correct contract mint…
        &alt_buyer_ata, // …but buyer ATA denominated in the alt mint
        &h.escrow_stable,
        &h.contract,
        h.job_id,
        PAY_PRICE,
    );
    let tx = Transaction::new_signed_with_payer(
        &[pay],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::WrongMint);
    let _ = alt_mint;
}

#[test]
fn pay_rejects_buyer_ata_with_wrong_owner() {
    // §2.2f: buyer ATA must be owned by the signing buyer.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let attacker = Keypair::new();
    let attacker_ata = ata(&attacker.pubkey(), &h.mint);
    inject_account(
        &mut h.svm,
        &attacker_ata,
        &token_prog(),
        token_account_bytes(&h.mint, &attacker.pubkey(), BUYER_MINT_AMOUNT),
    );
    let pay = pay_ix(
        &h.payer.pubkey(),
        &h.seller.pubkey(),
        &h.mint,
        &attacker_ata, // ATA owned by attacker, not the buyer signer
        &h.escrow_stable,
        &h.contract,
        h.job_id,
        PAY_PRICE,
    );
    let tx = Transaction::new_signed_with_payer(
        &[pay],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::WrongOwner);
}

#[test]
fn finalize_rejects_seller_hnt_ata_wrong_owner() {
    // §2.2f: seller HNT ATA must be owned by the contract's seller_payout.
    let mut h = Harness::new();
    let attacker = Keypair::new();
    inject_account(
        &mut h.svm,
        &h.seller_hnt,
        &token_prog(),
        token_account_bytes(&hnt_mint(), &attacker.pubkey(), 0),
    );
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::WrongOwner);
}

#[test]
fn cancel_after_finalize_fails() {
    // §2.2h: cancel_before_start after finalize → AlreadyFinal.
    let mut h = Harness::new();
    h.svm
        .send_transaction(h.finalize_tx(&h.payer, 1_000_000))
        .unwrap();
    let res = h.svm.send_transaction(h.cancel_tx());
    expect_custom(res, EscrowError::AlreadyFinal);
}

#[test]
fn finalize_rounds_tiny_fraction_consistently() {
    // §2.2j: price = 1, f_micros = 1 → earned truncates to 0, refund = 1;
    // no overflow, buyer gets everything back.
    let mut h = Harness::new_with(1, BUYER_MINT_AMOUNT);
    h.svm.send_transaction(h.finalize_tx(&h.payer, 1)).unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.buyer_stable), BUYER_MINT_AMOUNT);
    assert_eq!(h.token_balance(&h.seller_hnt), 0);
    assert_eq!(h.token_balance(&h.escrow_hnt), DEFAULT_HNT_ESCROW);
}

// ---------- Production finalize: valid flows -----------------------------

#[test]
fn production_finalize_happy_path() {
    let mut h = Harness::new();
    // The real bundle is `[jupiter swap → finalize_pro_rata]`: the swap
    // drains the earned slice from the escrow stable ATA before finalize
    // runs. Model that leg, then verify finalize distributes what's left.
    h.simulate_swap_consumes_earned(1_000_000);
    let meta = h
        .svm
        .send_transaction(h.finalize_tx(&h.payer, 1_000_000))
        .unwrap();
    // Anchor 0.30 `emit!` surfaces as a `Program data:` log line carrying
    // the `event:JobFinalized` discriminator + payload in base64.
    let dg: [u8; 32] = Sha256::digest(b"event:JobFinalized").into();
    let mut event_disc = [0u8; 8];
    event_disc.copy_from_slice(&dg[..8]);
    assert!(
        meta.logs
            .iter()
            .any(|l| l.starts_with("Program data:") && decode_b64_prefix(l) == Some(event_disc)),
        "expected JobFinalized event in logs: {:#?}",
        meta.logs
    );
    assert_eq!(h.token_balance(&h.escrow_hnt), 0);
    assert_eq!(h.token_balance(&h.seller_hnt), SELLER_HNT_EARNED);
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(
        h.token_balance(&h.buyer_stable),
        BUYER_MINT_AMOUNT - PAY_PRICE
    );
}

#[test]
fn finalize_fraction_zero_refunds_buyer() {
    let mut h = Harness::new();
    h.svm.send_transaction(h.finalize_tx(&h.payer, 0)).unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.buyer_stable), BUYER_MINT_AMOUNT);
    assert_eq!(h.token_balance(&h.seller_hnt), 0);
    assert_eq!(h.token_balance(&h.escrow_hnt), DEFAULT_HNT_ESCROW);
}

#[test]
fn finalize_eur_feed_fallback_succeeds() {
    let mut h = Harness::new();
    // The stablecoin/USD account holds the EUR feed; the USDC attempt
    // fails (mismatched id), the EUR fallback must succeed.
    h.inject_pyth_stable(feed_id(EUR_USD_FEED_HEX), STABLE_USD_PRICE, 0);
    h.svm
        .send_transaction(h.finalize_tx(&h.payer, 1_000_000))
        .unwrap();
    assert_eq!(h.token_balance(&h.seller_hnt), SELLER_HNT_EARNED);
    assert_eq!(h.token_balance(&h.escrow_hnt), 0);
}

// ---------- Settlement authority (MAINNET-CHECKLIST §3.5) -----------------

#[test]
fn settlement_authority_can_rotate() {
    let mut h = Harness::new();
    assert_eq!(h.config_authority(), h.payer.pubkey());

    // Non-authority can't rotate.
    let attacker = Keypair::new();
    h.svm.airdrop(&attacker.pubkey(), 1_000_000_000).unwrap();
    let bad = update_settlement_authority_ix(&attacker.pubkey(), &h.config, &h.seller.pubkey());
    let tx = Transaction::new_signed_with_payer(
        &[bad],
        Some(&attacker.pubkey()),
        &[&attacker],
        h.svm.latest_blockhash(),
    );
    expect_custom(
        h.svm.send_transaction(tx),
        EscrowError::NotSettlementAuthority,
    );

    // Current authority rotates to the seller.
    let good = update_settlement_authority_ix(&h.payer.pubkey(), &h.config, &h.seller.pubkey());
    let tx = Transaction::new_signed_with_payer(
        &[good],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    h.svm.send_transaction(tx).unwrap();
    assert_eq!(h.config_authority(), h.seller.pubkey());

    // Old authority can no longer finalize…
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::NotSettlementAuthority);

    // …and the new one can.
    h.svm
        .send_transaction(h.finalize_tx(&h.seller, 1_000_000))
        .unwrap();
    assert_eq!(h.token_balance(&h.seller_hnt), SELLER_HNT_EARNED);
}

// ---------- Stub finalize (devnet smoke path) -----------------------------

#[test]
fn stub_finalize_rejects_non_settlement_authority() {
    let mut h = Harness::new();
    let attacker = Keypair::new();
    h.svm.airdrop(&attacker.pubkey(), 1_000_000_000).unwrap();
    let res = h
        .svm
        .send_transaction(h.finalize_stub_tx(&attacker, 1_000_000));
    expect_custom(res, EscrowError::NotSettlementAuthority);
}

#[test]
fn stub_finalize_happy_path() {
    let mut h = Harness::new();
    h.svm
        .send_transaction(h.finalize_stub_tx(&h.payer, 1_000_000))
        .unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.seller_stable), PAY_PRICE);
    assert_eq!(
        h.token_balance(&h.buyer_stable),
        BUYER_MINT_AMOUNT - PAY_PRICE
    );
}

#[test]
fn stub_finalize_rejects_fraction_out_of_range() {
    let mut h = Harness::new();
    let res = h
        .svm
        .send_transaction(h.finalize_stub_tx(&h.payer, 1_000_001));
    expect_custom(res, EscrowError::FractionOutOfRange);
}

// ---------- Buyer unilateral cancel ---------------------------------------

#[test]
fn cancel_before_start_refunds_buyer() {
    let mut h = Harness::new();
    h.svm.send_transaction(h.cancel_tx()).unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.buyer_stable), BUYER_MINT_AMOUNT);

    // After cancel the contract is finalized; finalize must refuse.
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::AlreadyFinal);
}

#[test]
fn cancel_before_start_rejects_non_buyer() {
    let mut h = Harness::new();
    let attacker = Keypair::new();
    h.svm.airdrop(&attacker.pubkey(), 1_000_000_000).unwrap();
    let ix = cancel_ix(
        &attacker.pubkey(),
        &h.contract,
        &h.escrow_stable,
        &h.buyer_stable,
    );
    let tx = Transaction::new_signed_with_payer(
        &[ix],
        Some(&attacker.pubkey()),
        &[&attacker],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::WrongOwner);
}
