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
//! fee config, `EscrowError` codes). `spl-token` / ATA instructions are
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
//! | 2.2c fraction out of range | `finalize_rejects_fraction_out_of_range` |
//! | 2.2d double finalize | `finalize_rejects_second_finalize` |
//! | 2.2e buyer ATA wrong mint | `pay_rejects_buyer_ata_with_wrong_mint` |
//! | 2.2f wrong owner | `pay_rejects_buyer_ata_with_wrong_owner`, `finalize_rejects_seller_ata_wrong_owner` |
//! | 2.2g cancel by non-buyer | `cancel_before_start_rejects_non_buyer` |
//! | 2.2h cancel after finalize | `cancel_after_finalize_fails` |
//! | 2.2j tiny-fraction rounding | `finalize_rounds_tiny_fraction_consistently` |
//! | 2.2k finalize by non-authority | `finalize_rejects_non_settlement_authority` |
//! | §2.4 happy path (seller paid) | `finalize_happy_path` |
//! | §2.4 fraction = 0 (refund only) | `finalize_fraction_zero_refunds_buyer` |
//! | fee on pay | `pay_for_compute_charges_sol_fee` |
//! | fee on finalize | `finalize_charges_sol_fee` |
//! | fee on cancel | `cancel_charges_sol_fee` |
//! | zero fee disables | `zero_fee_disables_fee` |
//! | wrong fee wallet | `pay_rejects_wrong_fee_wallet` |
//! | config init + immutability | `init_config_sets_fee_fields`, `config_immutable_after_init` |
//! | config rotation (update_config) | `update_config_rotates_settlement_authority`, `update_config_edits_fee_fields`, `update_config_rejects_non_settlement_authority` |
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
    /// Kept to mirror the program's variant order for the error-code
    /// drift guard; the new finalize math (u128 intermediates) can no
    /// longer overflow, so it is never exercised by a test.
    #[allow(dead_code)]
    MathOverflow,
    WrongFeeWallet,
}

const ERROR_CODE_OFFSET: u32 = 6000;

impl From<EscrowError> for u32 {
    fn from(e: EscrowError) -> u32 {
        ERROR_CODE_OFFSET + e as u32
    }
}

// ---------- Pinned addresses (mainnet-beta canonical) ---------------------

const PROGRAM_ID_STR: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
const TOKEN_PROGRAM_STR: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const ATA_PROGRAM_STR: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
/// Protocol fee wallet from the spec — drives the real lamport transfer.
const FEE_WALLET_STR: &str = "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh";

// ---------- Test fixture numbers ------------------------------------------

/// 0.0001 SOL = 100_000 lamports, the per-transaction protocol fee.
const FEE_LAMPORTS: u64 = 100_000;

const STABLE_DECIMALS: u8 = 6;
const PAY_PRICE: u64 = 2_000_000; // 2.000000 stablecoin
const BUYER_MINT_AMOUNT: u64 = 10_000_000;

// --------------------------------------------------------------------------

fn pk(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}

fn prog() -> Pubkey {
    pk(PROGRAM_ID_STR)
}

fn token_prog() -> Pubkey {
    pk(TOKEN_PROGRAM_STR)
}

fn ata_prog() -> Pubkey {
    pk(ATA_PROGRAM_STR)
}

fn fee_wallet() -> Pubkey {
    pk(FEE_WALLET_STR)
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

// ---------- Escrow instruction builders -----------------------------------

fn init_config_ix(authority: &Pubkey, config: &Pubkey) -> Instruction {
    let mut data = disc("init_config").to_vec();
    data.extend_from_slice(&authority.to_bytes());
    data.extend_from_slice(&fee_wallet().to_bytes());
    data.extend_from_slice(&FEE_LAMPORTS.to_le_bytes());
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

fn update_config_ix(
    sa: &Pubkey,
    config: &Pubkey,
    new_sa: &Pubkey,
    new_fee_wallet: &Pubkey,
    new_fee_lamports: u64,
) -> Instruction {
    let mut data = disc("update_config").to_vec();
    data.extend_from_slice(&new_sa.to_bytes());
    data.extend_from_slice(&new_fee_wallet.to_bytes());
    data.extend_from_slice(&new_fee_lamports.to_le_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*sa, true),
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
    config: &Pubkey,
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
            AccountMeta::new_readonly(*config, false),
            AccountMeta::new_readonly(token_prog(), false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
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
    seller_stable: &Pubkey,
    f_micros: u32,
) -> Instruction {
    let mut data = disc("finalize_pro_rata").to_vec();
    data.extend_from_slice(&f_micros.to_le_bytes());
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*sa, true),
            AccountMeta::new_readonly(*config, false),
            AccountMeta::new(*contract, false),
            AccountMeta::new(*escrow_stable, false),
            AccountMeta::new(*buyer_stable, false),
            AccountMeta::new(*seller_stable, false),
            AccountMeta::new(fee_wallet(), false),
            AccountMeta::new_readonly(token_prog(), false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
        ],
        data,
    }
}

fn cancel_ix(
    buyer: &Pubkey,
    contract: &Pubkey,
    escrow_stable: &Pubkey,
    buyer_stable: &Pubkey,
    config: &Pubkey,
) -> Instruction {
    Instruction {
        program_id: prog(),
        accounts: vec![
            AccountMeta::new(*buyer, true),
            AccountMeta::new(*contract, false),
            AccountMeta::new(*escrow_stable, false),
            AccountMeta::new(*buyer_stable, false),
            AccountMeta::new(fee_wallet(), false),
            AccountMeta::new_readonly(*config, false),
            AccountMeta::new_readonly(token_prog(), false),
            AccountMeta::new_readonly(solana_system_interface::program::id(), false),
        ],
        data: disc("cancel_before_start").to_vec(),
    }
}

// ---------- Harness -------------------------------------------------------

/// A funded, paid ledger ready to run `finalize_pro_rata`.
///
/// State built:
/// 1. stablecoin mint (6 decimals, mint authority = payer/buyer)
/// 2. buyer/seller/escrow ATAs for the stablecoin mint
/// 3. `BUYER_MINT_AMOUNT` minted to the buyer
/// 4. `init_config` with settlement authority = payer, the protocol fee
///    wallet + `FEE_LAMPORTS`
/// 5. `pay_for_compute(price)` into the contract PDA
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
}

fn job_id() -> [u8; 32] {
    Sha256::digest(b"vtessera-adversarial:job").into()
}

impl Harness {
    fn new() -> Self {
        Self::new_with(PAY_PRICE, BUYER_MINT_AMOUNT)
    }

    fn setup_only(buyer_balance: u64) -> Self {
        Self::setup_only_with_fee(buyer_balance, FEE_LAMPORTS)
    }

    /// Same as `setup_only` but pins the given protocol fee in `Config`
    /// (0 disables the fee, which is itself a tested configuration).
    fn setup_only_with_fee(buyer_balance: u64, fee_lamports: u64) -> Self {
        let mut svm = LiteSVM::new();
        svm.add_program_from_file(prog(), program_so_path())
            .unwrap();

        let payer = Keypair::new();
        let seller = Keypair::new();
        svm.airdrop(&payer.pubkey(), 5_000_000_000).unwrap();
        svm.airdrop(&seller.pubkey(), 5_000_000_000).unwrap();
        // The program pays a fixed lamport fee to `fee_wallet` on every
        // deposit/finalize/cancel. On mainnet that wallet pre-exists
        // (operator-funded); create it here rent-exempt, or the pay tx
        // would fail with InsufficientFundsForRent (0-byte account needs
        // (128 + 0) * 6960 = 890,880 lamports; FEE_LAMPORTS is only
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
        let (config, _) = Pubkey::find_program_address(&[b"vtessera_config_v2"], &prog());
        let mut data = disc("init_config").to_vec();
        data.extend_from_slice(&payer.pubkey().to_bytes());
        data.extend_from_slice(&fee_wallet().to_bytes());
        data.extend_from_slice(&fee_lamports.to_le_bytes());
        let cfg = Instruction {
            program_id: prog(),
            accounts: vec![
                AccountMeta::new(payer.pubkey(), true),
                AccountMeta::new(config, false),
                AccountMeta::new_readonly(solana_system_interface::program::id(), false),
            ],
            data,
        };
        send(&mut svm, &payer, &[&payer], &[cfg]);

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
        }
    }

    fn new_with(price: u64, buyer_balance: u64) -> Self {
        let mut h = Self::setup_only(buyer_balance);
        h.price = price;
        h.pay(price);
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
            &self.config,
            self.job_id,
            price,
        );
        send(&mut self.svm, &self.payer, &[&self.payer], &[pay]);
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

    fn token_balance(&self, key: &Pubkey) -> u64 {
        let acct = self
            .svm
            .get_account(key)
            .unwrap_or_else(|| panic!("no token account at {key}"));
        u64::from_le_bytes(acct.data[64..72].try_into().unwrap())
    }

    fn sol_balance(&self, key: &Pubkey) -> u64 {
        self.svm.get_balance(key).unwrap_or(0)
    }

    fn config_authority(&self) -> Pubkey {
        let acct = self.svm.get_account(&self.config).unwrap();
        Pubkey::new_from_array(acct.data[8..40].try_into().unwrap())
    }

    fn config_fee_wallet(&self) -> Pubkey {
        let acct = self.svm.get_account(&self.config).unwrap();
        Pubkey::new_from_array(acct.data[40..72].try_into().unwrap())
    }

    fn config_fee_lamports(&self) -> u64 {
        let acct = self.svm.get_account(&self.config).unwrap();
        u64::from_le_bytes(acct.data[72..80].try_into().unwrap())
    }

    fn finalize_tx(&self, sa: &Keypair, f_micros: u32) -> Transaction {
        let ix = finalize_ix(
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
            &self.config,
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

// ---------- Config: init + immutability -----------------------------------

#[test]
fn init_config_sets_fee_fields() {
    let h = Harness::setup_only(BUYER_MINT_AMOUNT);
    assert_eq!(h.config_authority(), h.payer.pubkey());
    assert_eq!(h.config_fee_wallet(), fee_wallet());
    assert_eq!(h.config_fee_lamports(), FEE_LAMPORTS);
}

#[test]
fn config_immutable_after_init() {
    // No governance instructions exist; the only setup path is init_config,
    // which fails once the PDA already exists (there is nothing to rotate).
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let cfg = init_config_ix(&h.payer.pubkey(), &h.config);
    let tx = Transaction::new_signed_with_payer(
        &[cfg],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    expect_tx_error(h.svm.send_transaction(tx));
    // Config untouched by the failed re-init.
    assert_eq!(h.config_fee_lamports(), FEE_LAMPORTS);
}

#[test]
fn update_config_rotates_settlement_authority() {
    // The current settlement authority can rotate the pinned authority,
    // so a wrong `init_config` value is recoverable on-chain.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let new_sa = Keypair::new();
    h.svm.airdrop(&new_sa.pubkey(), 1_000_000_000).unwrap();

    let ix = update_config_ix(
        &h.payer.pubkey(),
        &h.config,
        &new_sa.pubkey(),
        &fee_wallet(),
        FEE_LAMPORTS,
    );
    let tx = Transaction::new_signed_with_payer(
        &[ix],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    h.svm.send_transaction(tx).unwrap();

    assert_eq!(h.config_authority(), new_sa.pubkey());
    assert_eq!(h.config_fee_wallet(), fee_wallet());
    assert_eq!(h.config_fee_lamports(), FEE_LAMPORTS);
    // The old authority can no longer finalize; the new one can.
    let mut old = |sa: &Keypair| {
        h.svm
            .send_transaction(Transaction::new_signed_with_payer(
                &[finalize_ix(
                    &sa.pubkey(),
                    &h.config,
                    &h.contract,
                    &h.escrow_stable,
                    &h.buyer_stable,
                    &h.seller_stable,
                    0,
                )],
                Some(&sa.pubkey()),
                &[sa],
                h.svm.latest_blockhash(),
            ))
    };
    expect_custom(old(&h.payer), EscrowError::NotSettlementAuthority);
    old(&new_sa).unwrap();
}

#[test]
fn update_config_edits_fee_fields() {
    // Fee wallet and per-transaction fee can be changed by the current
    // settlement authority.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let new_wallet = Keypair::new().pubkey();

    let ix = update_config_ix(
        &h.payer.pubkey(),
        &h.config,
        &h.payer.pubkey(),
        &new_wallet,
        0,
    );
    let tx = Transaction::new_signed_with_payer(
        &[ix],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    h.svm.send_transaction(tx).unwrap();

    assert_eq!(h.config_fee_wallet(), new_wallet);
    assert_eq!(h.config_fee_lamports(), 0);
}

#[test]
fn update_config_rejects_non_settlement_authority() {
    // Anyone other than the current settlement authority is rejected.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let attacker = Keypair::new();
    h.svm.airdrop(&attacker.pubkey(), 1_000_000_000).unwrap();

    let ix = update_config_ix(
        &attacker.pubkey(),
        &h.config,
        &h.payer.pubkey(),
        &fee_wallet(),
        FEE_LAMPORTS,
    );
    let tx = Transaction::new_signed_with_payer(
        &[ix],
        Some(&attacker.pubkey()),
        &[&attacker],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::NotSettlementAuthority);
    // Config untouched.
    assert_eq!(h.config_authority(), h.payer.pubkey());
}

// ---------- Protocol fee --------------------------------------------------

#[test]
fn pay_for_compute_charges_sol_fee() {
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let buyer_before = h.sol_balance(&h.payer.pubkey());
    let wallet_before = h.sol_balance(&fee_wallet());
    h.pay(PAY_PRICE);
    assert_eq!(h.sol_balance(&fee_wallet()), wallet_before + FEE_LAMPORTS);
    // The buyer also pays SOL tx fees; the protocol fee is on top.
    assert!(
        h.sol_balance(&h.payer.pubkey()) <= buyer_before - FEE_LAMPORTS,
        "buyer SOL must drop by at least the fee"
    );
}

#[test]
fn finalize_charges_sol_fee() {
    let mut h = Harness::new();
    let sa_before = h.sol_balance(&h.payer.pubkey());
    let wallet_before = h.sol_balance(&fee_wallet());
    h.svm
        .send_transaction(h.finalize_tx(&h.payer, 1_000_000))
        .unwrap();
    assert_eq!(h.sol_balance(&fee_wallet()), wallet_before + FEE_LAMPORTS);
    assert!(
        h.sol_balance(&h.payer.pubkey()) <= sa_before - FEE_LAMPORTS,
        "settlement authority SOL must drop by at least the fee"
    );
}

#[test]
fn cancel_charges_sol_fee() {
    let mut h = Harness::new();
    let buyer_before = h.sol_balance(&h.payer.pubkey());
    let wallet_before = h.sol_balance(&fee_wallet());
    h.svm.send_transaction(h.cancel_tx()).unwrap();
    assert_eq!(h.sol_balance(&fee_wallet()), wallet_before + FEE_LAMPORTS);
    assert!(
        h.sol_balance(&h.payer.pubkey()) <= buyer_before - FEE_LAMPORTS,
        "buyer SOL must drop by at least the fee even on cancel"
    );
}

#[test]
fn zero_fee_disables_fee() {
    // A config pinned with fee_lamports = 0 means the fee is skipped.
    let mut h = Harness::setup_only_with_fee(BUYER_MINT_AMOUNT, 0);
    let wallet_before = h.sol_balance(&fee_wallet());
    h.pay(PAY_PRICE);
    assert_eq!(h.sol_balance(&fee_wallet()), wallet_before);
    assert_eq!(h.config_fee_lamports(), 0);
}

#[test]
fn pay_rejects_wrong_fee_wallet() {
    // The passed fee-wallet account must match the wallet pinned in Config.
    let mut h = Harness::setup_only(BUYER_MINT_AMOUNT);
    let impostor = Keypair::new();
    h.svm.airdrop(&impostor.pubkey(), 1_000_000_000).unwrap();
    let pay = {
        let mut data = disc("pay_for_compute").to_vec();
        data.extend_from_slice(&h.job_id);
        data.extend_from_slice(&PAY_PRICE.to_le_bytes());
        Instruction {
            program_id: prog(),
            accounts: vec![
                AccountMeta::new(h.payer.pubkey(), true),
                AccountMeta::new_readonly(h.seller.pubkey(), false),
                AccountMeta::new_readonly(h.mint, false),
                AccountMeta::new(h.buyer_stable, false),
                AccountMeta::new(h.escrow_stable, false),
                AccountMeta::new(h.contract, false),
                AccountMeta::new(impostor.pubkey(), false),
                AccountMeta::new_readonly(h.config, false),
                AccountMeta::new_readonly(token_prog(), false),
                AccountMeta::new_readonly(solana_system_interface::program::id(), false),
            ],
            data,
        }
    };
    let tx = Transaction::new_signed_with_payer(
        &[pay],
        Some(&h.payer.pubkey()),
        &[&h.payer],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::WrongFeeWallet);
}

// ---------- Pay: adversarial ----------------------------------------------

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
        &h.config,
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
        &h.config,
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
        &h.config,
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
        &h.config,
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

// ---------- Finalize: adversarial -----------------------------------------

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
fn finalize_rejects_seller_ata_wrong_owner() {
    // §2.2f: seller stablecoin ATA must be owned by the contract's
    // seller_payout.
    let mut h = Harness::new();
    let attacker = Keypair::new();
    inject_account(
        &mut h.svm,
        &h.seller_stable,
        &token_prog(),
        token_account_bytes(&h.mint, &attacker.pubkey(), 0),
    );
    let res = h.svm.send_transaction(h.finalize_tx(&h.payer, 1_000_000));
    expect_custom(res, EscrowError::WrongOwner);
}

#[test]
fn finalize_rounds_tiny_fraction_consistently() {
    // §2.2j: price = 1, f_micros = 1 → earned truncates to 0, refund = 1;
    // no overflow, buyer gets everything back.
    let mut h = Harness::new_with(1, BUYER_MINT_AMOUNT);
    h.svm.send_transaction(h.finalize_tx(&h.payer, 1)).unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.buyer_stable), BUYER_MINT_AMOUNT);
    assert_eq!(h.token_balance(&h.seller_stable), 0);
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

// ---------- Finalize: valid flows -----------------------------------------

#[test]
fn finalize_happy_path() {
    // Seller paid the whole escrow in the contract's stablecoin mint;
    // buyer gets nothing back at f = 1.0.
    let mut h = Harness::new();
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
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.seller_stable), PAY_PRICE);
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
    assert_eq!(h.token_balance(&h.seller_stable), 0);
}

#[test]
fn finalize_half_pays_half_refunds() {
    // f = 0.5 with an even price: half to the seller, half refunded.
    let mut h = Harness::new_with(2_000_000, BUYER_MINT_AMOUNT);
    h.svm
        .send_transaction(h.finalize_tx(&h.payer, 500_000))
        .unwrap();
    assert_eq!(h.token_balance(&h.escrow_stable), 0);
    assert_eq!(h.token_balance(&h.seller_stable), 1_000_000);
    assert_eq!(
        h.token_balance(&h.buyer_stable),
        BUYER_MINT_AMOUNT - 1_000_000
    );
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
        &h.config,
    );
    let tx = Transaction::new_signed_with_payer(
        &[ix],
        Some(&attacker.pubkey()),
        &[&attacker],
        h.svm.latest_blockhash(),
    );
    expect_custom(h.svm.send_transaction(tx), EscrowError::WrongOwner);
}

// ---------- Fuzz: randomized finalize + cancel paths -----------------------

/// Deterministic xorshift64* PRNG — no external deps. Seeds from
/// `test::black_box` to avoid optimizer folding the loop.
struct FuzzRng(u64);

impl FuzzRng {
    fn next_u64(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.0 = x;
        x.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    fn range(&mut self, lo: u64, hi: u64) -> u64 {
        if lo >= hi {
            return lo;
        }
        lo + self.next_u64() % (hi - lo + 1)
    }
}

/// Run 1000 iterations with random `(price, f_micros)` through finalize
/// and verify the on-chain split matches the expected math. Every 10th
/// iteration forces an edge: `price = 1` (rounding) or
/// `f_micros = 1_000_000` (zero refund).
#[test]
fn fuzz_finalize_random_split() {
    let mut rng = FuzzRng(0xDEAD_BEEF_CAFE_1234);
    let iterations = std::hint::black_box(1000);

    for i in 0..iterations {
        // Edge cadence: force rounding / zero-refund edges.
        let price = if i % 10 == 0 {
            1
        } else {
            rng.range(1, 10_000_000)
        };
        let f_micros: u32 = if i % 10 == 5 {
            1_000_000
        } else if i % 10 == 0 {
            1
        } else {
            rng.range(0, 1_000_000) as u32
        };

        // Each iteration needs a fresh harness (unique contract PDA).
        let mut h = Harness::new_with(price, BUYER_MINT_AMOUNT);
        h.svm
            .send_transaction(h.finalize_tx(&h.payer, f_micros))
            .unwrap();

        // Verify the math: earned = price * f_micros / 1_000_000
        let earned = (price as u128)
            .checked_mul(f_micros as u128)
            .unwrap()
            .checked_div(1_000_000)
            .unwrap() as u64;
        let refund = price - earned;

        assert_eq!(
            h.token_balance(&h.escrow_stable),
            0,
            "iter {i}: escrow not drained (price={price} f={f_micros})"
        );
        assert_eq!(
            h.token_balance(&h.seller_stable),
            earned,
            "iter {i}: seller balance wrong (price={price} f={f_micros} earned={earned})"
        );
        assert_eq!(
            h.token_balance(&h.buyer_stable),
            BUYER_MINT_AMOUNT - earned,
            "iter {i}: buyer balance wrong (price={price} f={f_micros} refund={refund})"
        );
    }
}

/// Run 1000 iterations with random prices through cancel and verify
/// the full refund. Every 10th iteration uses `price = 1` (minimum).
#[test]
fn fuzz_cancel_random_price() {
    let mut rng = FuzzRng(0x1234_5678_9ABC_DEF0);
    let iterations = std::hint::black_box(1000);

    for i in 0..iterations {
        let price = if i % 10 == 0 {
            1
        } else {
            rng.range(1, 10_000_000)
        };

        let mut h = Harness::new_with(price, BUYER_MINT_AMOUNT);
        h.svm.send_transaction(h.cancel_tx()).unwrap();

        assert_eq!(
            h.token_balance(&h.escrow_stable),
            0,
            "iter {i}: escrow not drained (price={price})"
        );
        assert_eq!(
            h.token_balance(&h.buyer_stable),
            BUYER_MINT_AMOUNT,
            "iter {i}: buyer not fully refunded (price={price})"
        );
    }
}
