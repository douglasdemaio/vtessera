//! Vtessera devnet demo — end-to-end on-chain escrow exercise.
//!
//! Runs against Solana devnet. In one process it:
//!
//! 1. Creates a test SPL token mint (acts as the "stablecoin" — devnet
//!    USDC works the same way; using our own mint lets the demo run
//!    without depending on a faucet being up).
//! 2. Funds a buyer keypair with that token.
//! 3. Generates a fresh seller keypair.
//! 4. Calls `pay_for_compute` on the deployed escrow program — buyer's
//!    tokens move into a program-owned escrow PDA, flat SOL fee
//!    transfers to the fee wallet.
//! 5. Runs the (no-op) executor for one job to produce metering.
//! 6. Computes the completion fraction `f` via the settlement crate.
//! 7. Calls `finalize_pro_rata` — escrow splits by `f`: earned slice to
//!    seller, refund to buyer.
//! 8. Prints final on-chain balances + transaction signatures.
//!
//! No mainnet exposure. Total devnet cost is a small fraction of one
//! SOL (mint creation, three ATAs, two transactions).

use std::env;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::Duration;

use borsh::BorshSerialize;
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signer},
    system_instruction, system_program,
    sysvar::rent,
    transaction::Transaction,
};
use spl_associated_token_account::{
    get_associated_token_address, instruction::create_associated_token_account,
};
use spl_token::state::{Account as TokenAccount, Mint};

// The host crates `vtessera-executor` and `vtessera-settlement` aren't
// depended on here because their ed25519-dalek 2 ecosystem conflicts
// with solana-sdk 1.18's old curve25519-dalek (see Cargo.toml). The
// pieces of their behaviour this demo needs are tiny — a synthetic
// metering generator and the f = clamp(used/agreed, 0, 1) computation
// — and are reproduced inline so the demo can run while leaving the
// host crates as the source of truth for everything else.

#[derive(Debug, Clone, Copy)]
struct InlineMetering {
    cpu_seconds: f64,
    elapsed_secs: u64,
    ok: bool,
}

/// Mirror of `vtessera_executor::NoopCpuExecutor` for the demo: pretends
/// to run a job and returns synthetic metering equal to the requested
/// vCPU count (one CPU-second per vCPU).
fn noop_run(vcpus: u32) -> InlineMetering {
    InlineMetering {
        cpu_seconds: vcpus as f64,
        elapsed_secs: 1,
        ok: true,
    }
}

/// Mirror of `vtessera_settlement::settle` for the demo.
fn settle_f(device_seconds: f64, agreed_device_seconds: u64) -> f64 {
    if agreed_device_seconds == 0 {
        return 0.0;
    }
    (device_seconds / agreed_device_seconds as f64).clamp(0.0, 1.0)
}

/// Devnet program ID — see ROADMAP.md §0, programs/Anchor.toml.
const PROGRAM_ID_STR: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
/// DRAFT fee wallet from the roadmap. Drives a real lamport transfer on
/// devnet so the IX exercises every account in the production graph.
const FEE_WALLET_STR: &str = "9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD";
const DEVNET_RPC: &str = "https://api.devnet.solana.com";

/// `pay_for_compute` IX args, encoded with borsh after the 8-byte
/// Anchor discriminator (= first 8 bytes of `sha256("global:pay_for_compute")`).
#[derive(BorshSerialize)]
struct PayForComputeArgs {
    job_id: [u8; 32],
    price_micros: u64,
}

#[derive(BorshSerialize)]
struct FinalizeProRataArgs {
    f_micros: u32,
}

fn anchor_disc(ix_name: &str) -> [u8; 8] {
    let mut h = Sha256::new();
    h.update(format!("global:{ix_name}").as_bytes());
    let d = h.finalize();
    let mut out = [0u8; 8];
    out.copy_from_slice(&d[..8]);
    out
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let payer_path: PathBuf = env::var("VTESSERA_PAYER")
        .map(PathBuf::from)
        .unwrap_or_else(|_| {
            let home = env::var("HOME").expect("HOME unset");
            PathBuf::from(format!("{home}/.config/solana/id.json"))
        });
    let payer = read_keypair_file(&payer_path)
        .map_err(|e| format!("read payer {}: {e}", payer_path.display()))?;
    println!("payer (buyer + mint authority): {}", payer.pubkey());

    let program_id = Pubkey::from_str(PROGRAM_ID_STR)?;
    let fee_wallet = Pubkey::from_str(FEE_WALLET_STR)?;

    let rpc = RpcClient::new_with_commitment(DEVNET_RPC.to_string(), CommitmentConfig::confirmed());

    let pre_lamports = rpc.get_balance(&payer.pubkey())?;
    println!(
        "payer devnet SOL: {:.6}",
        pre_lamports as f64 / 1_000_000_000.0
    );

    // --- 1. Create a test stablecoin mint -------------------------------
    let mint_kp = Keypair::new();
    let mint_pk = mint_kp.pubkey();
    println!("\n--- creating test stablecoin mint ---");
    println!("mint: {mint_pk}");
    let mint_rent = rpc.get_minimum_balance_for_rent_exemption(Mint::LEN)?;
    let create_mint_acct = system_instruction::create_account(
        &payer.pubkey(),
        &mint_pk,
        mint_rent,
        Mint::LEN as u64,
        &spl_token::id(),
    );
    let init_mint = spl_token::instruction::initialize_mint(
        &spl_token::id(),
        &mint_pk,
        &payer.pubkey(),
        None,
        6,
    )?;
    send_tx(
        &rpc,
        &[create_mint_acct, init_mint],
        &[&payer, &mint_kp],
        &payer,
        "create+init mint",
    )?;

    // --- 2. Buyer is payer; create their ATA and mint 10 stablecoin ----
    let buyer = &payer;
    let buyer_ata = get_associated_token_address(&buyer.pubkey(), &mint_pk);
    println!("buyer ATA: {buyer_ata}");
    let mut ixs: Vec<Instruction> = vec![create_associated_token_account(
        &payer.pubkey(),
        &buyer.pubkey(),
        &mint_pk,
        &spl_token::id(),
    )];
    let mint_amount: u64 = 10_000_000; // 10.000000 of a 6-decimal mint
    ixs.push(spl_token::instruction::mint_to(
        &spl_token::id(),
        &mint_pk,
        &buyer_ata,
        &payer.pubkey(),
        &[],
        mint_amount,
    )?);
    send_tx(&rpc, &ixs, &[&payer], &payer, "create buyer ATA + mint 10")?;

    // --- 3. Fresh seller keypair + their ATA ---------------------------
    let seller = Keypair::new();
    let seller_ata = get_associated_token_address(&seller.pubkey(), &mint_pk);
    println!("seller: {} ATA: {seller_ata}", seller.pubkey());
    let create_seller_ata = create_associated_token_account(
        &payer.pubkey(),
        &seller.pubkey(),
        &mint_pk,
        &spl_token::id(),
    );
    send_tx(&rpc, &[create_seller_ata], &[&payer], &payer, "create seller ATA")?;

    // --- 4. Derive contract PDA + escrow ATA ---------------------------
    let job_id: [u8; 32] = {
        let mut h = Sha256::new();
        h.update(b"vtessera-devnet-demo:");
        h.update(payer.pubkey().as_ref());
        h.update(mint_pk.as_ref());
        // A bit of caller-supplied entropy without using SystemTime
        // (Bash-tool sleeps + RPC roundtrips already give per-run uniqueness;
        // include the buyer ATA pubkey too so two demos in the same slot still differ).
        h.update(buyer_ata.as_ref());
        let d = h.finalize();
        let mut out = [0u8; 32];
        out.copy_from_slice(&d);
        out
    };
    let (contract_pda, _bump) =
        Pubkey::find_program_address(&[b"contract", &job_id], &program_id);
    let escrow_ata = get_associated_token_address(&contract_pda, &mint_pk);
    println!("\n--- escrow accounts ---");
    println!("job_id (hex): {}", hex_string(&job_id));
    println!("contract PDA: {contract_pda}");
    println!("escrow ATA:   {escrow_ata}");

    let create_escrow_ata = create_associated_token_account(
        &payer.pubkey(),
        &contract_pda,
        &mint_pk,
        &spl_token::id(),
    );
    send_tx(&rpc, &[create_escrow_ata], &[&payer], &payer, "create escrow ATA")?;

    // --- 5. pay_for_compute -------------------------------------------
    let price: u64 = 2_000_000; // 2.000000 stablecoin
    let pay_disc = anchor_disc("pay_for_compute");
    let pay_args = PayForComputeArgs {
        job_id,
        price_micros: price,
    };
    let mut pay_data = pay_disc.to_vec();
    pay_data.extend_from_slice(&pay_args.try_to_vec()?);

    // Anchor account order in PayForCompute (lib.rs):
    //   buyer (signer, mut)
    //   seller_payout
    //   stablecoin_mint
    //   buyer_stablecoin_ata (mut)
    //   escrow_stablecoin_ata (mut)
    //   contract (init, mut)
    //   fee_wallet (mut)
    //   token_program
    //   system_program
    let pay_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(buyer.pubkey(), true),
            AccountMeta::new_readonly(seller.pubkey(), false),
            AccountMeta::new_readonly(mint_pk, false),
            AccountMeta::new(buyer_ata, false),
            AccountMeta::new(escrow_ata, false),
            AccountMeta::new(contract_pda, false),
            AccountMeta::new(fee_wallet, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
            // Anchor also needs the rent sysvar for `init` accounts under
            // some IDL versions; include defensively.
            AccountMeta::new_readonly(rent::id(), false),
        ],
        data: pay_data,
    };
    println!("\n--- pay_for_compute (price {} micros) ---", price);
    let pay_sig = send_tx(&rpc, &[pay_ix], &[&payer], &payer, "pay_for_compute")?;
    println!("pay_for_compute signature: {pay_sig}");

    // Inspect on-chain state after deposit.
    let escrow_balance = token_balance(&rpc, &escrow_ata)?;
    println!("escrow balance after deposit: {escrow_balance} micros");

    // --- 6. Run executor stub + compute f ------------------------------
    // Agreed work = 2 device-seconds. Noop executor "ran" with 1 vCPU →
    // it claims 1 CPU-second. So f should land at 0.5, splitting escrow
    // evenly between seller and refund.
    let agreed_device_seconds: u64 = 2;
    let metering = noop_run(/* vcpus = */ 1);
    println!(
        "\nexecutor ran: cpu_seconds={} elapsed={}s ok={}",
        metering.cpu_seconds, metering.elapsed_secs, metering.ok
    );
    let f = settle_f(metering.cpu_seconds, agreed_device_seconds);
    let f_micros: u32 = (f * 1_000_000.0).round() as u32;
    println!("settlement: f = {f:.4}  → f_micros = {f_micros}");

    // --- 7. finalize_pro_rata ------------------------------------------
    let fin_disc = anchor_disc("finalize_pro_rata");
    let fin_args = FinalizeProRataArgs { f_micros };
    let mut fin_data = fin_disc.to_vec();
    fin_data.extend_from_slice(&fin_args.try_to_vec()?);

    // Anchor account order in FinalizePro:
    //   settlement_authority (signer)
    //   contract (mut)
    //   escrow_stablecoin_ata (mut)
    //   buyer_stablecoin_ata (mut)
    //   seller_stablecoin_ata (mut)
    //   token_program
    let fin_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new_readonly(payer.pubkey(), true), // settlement authority = payer (devnet stub)
            AccountMeta::new(contract_pda, false),
            AccountMeta::new(escrow_ata, false),
            AccountMeta::new(buyer_ata, false),
            AccountMeta::new(seller_ata, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: fin_data,
    };
    println!("\n--- finalize_pro_rata (f_micros={f_micros}) ---");
    let fin_sig = send_tx(&rpc, &[fin_ix], &[&payer], &payer, "finalize_pro_rata")?;
    println!("finalize signature: {fin_sig}");

    // --- 8. Final balances --------------------------------------------
    let escrow_after = token_balance(&rpc, &escrow_ata)?;
    let buyer_after = token_balance(&rpc, &buyer_ata)?;
    let seller_after = token_balance(&rpc, &seller_ata)?;
    let post_lamports = rpc.get_balance(&payer.pubkey())?;

    println!("\n=== FINAL ON-CHAIN STATE ===");
    println!("escrow ATA:  {escrow_after} micros");
    println!("buyer ATA:   {buyer_after} micros  (started 10_000_000, paid {price}, expected refund (1-f)*price)");
    println!("seller ATA:  {seller_after} micros  (expected f*price)");
    println!("payer SOL:   {:.6} (started {:.6})",
        post_lamports as f64 / 1_000_000_000.0,
        pre_lamports as f64 / 1_000_000_000.0,
    );

    let expected_earned = (price as u128 * f_micros as u128 / 1_000_000) as u64;
    let expected_refund = price - expected_earned;
    assert_eq!(seller_after, expected_earned, "seller earned mismatch");
    assert_eq!(
        buyer_after,
        mint_amount - price + expected_refund,
        "buyer refund mismatch"
    );
    assert_eq!(escrow_after, 0, "escrow should be drained after finalize");

    println!("\nsuccess: split matches settlement.f exactly.");
    println!(
        "explorer:\n  https://explorer.solana.com/tx/{pay_sig}?cluster=devnet\n  https://explorer.solana.com/tx/{fin_sig}?cluster=devnet"
    );
    Ok(())
}

fn send_tx(
    rpc: &RpcClient,
    ixs: &[Instruction],
    signers: &[&Keypair],
    fee_payer: &Keypair,
    label: &str,
) -> Result<String, Box<dyn std::error::Error>> {
    let bh = rpc.get_latest_blockhash()?;
    let mut tx = Transaction::new_with_payer(ixs, Some(&fee_payer.pubkey()));
    tx.sign(signers, bh);
    let sig = rpc.send_and_confirm_transaction_with_spinner(&tx)?;
    println!("  [{label}] {sig}");
    // Brief breather so subsequent RPC reads see the confirmed state.
    std::thread::sleep(Duration::from_millis(400));
    Ok(sig.to_string())
}

fn token_balance(rpc: &RpcClient, ata: &Pubkey) -> Result<u64, Box<dyn std::error::Error>> {
    let acct = rpc.get_account(ata)?;
    let parsed = TokenAccount::unpack(&acct.data)?;
    Ok(parsed.amount)
}

fn hex_string(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}
