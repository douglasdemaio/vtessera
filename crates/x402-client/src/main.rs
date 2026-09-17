//! Vtessera x402 client — the AI-agent side of the loop.
//!
//! Plays the buyer against a running `vtessera-node` over HTTP:
//!
//! 1. `GET /offer` — pulls the seller's signed offer (price is
//!    per_device_second_micros).
//! 2. `POST /jobs` without a payment proof — gets the 402 x402
//!    challenge body (network, escrow account, offer).
//! 3. Pays on Solana devnet: the buyer's stablecoin moves into the
//!    escrow program's per-job contract PDA via `pay_for_compute`.
//! 4. `POST /jobs` again with an `x-payment` proof header — the node
//!    verifies the SPL transfer on-chain, accepts the job, executes it and
//!    returns a signed receipt (200).
//! 5. `finalize_pro_rata` (f = 1.0) drains the escrow to the seller's
//!    stablecoin ATA in the contract's mint. The finalize signer must be
//!    the **per-contract settlement authority recorded at pay time** —
//!    the client records this payer, so finalize always succeeds once the
//!    money moved (no dependency on who initialized the shared `Config`).
//!
//! By default the client mints its own test stablecoin so the whole loop
//! runs with no faucet dependency (devnet-demo's approach). Pass
//! `--mint <addr>` to pay a real devnet stablecoin (e.g. Circle's devnet
//! USDC `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU`) — the payer must
//! already hold that token, since only Circle can mint it.
//!
//! The node's `VerifyAndRun` branch verifies the payment proof on-chain and
//! executes the paid job (no 501 on current nodes). This crate's `--check`
//! mode runs the same diagnostics the session previously had to do by hand
//! (offer payout, challenge escrow, config PDA fee setup, funds).
//!
//! Standalone crate (excluded from the host workspace): pins the solana 3.x
//! line (see Cargo.toml).

use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use borsh::BorshSerialize;
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
// Solana 3.x split several `solana_sdk` modules into standalone crates.
use solana_commitment_config::CommitmentConfig;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signer},
    transaction::Transaction,
};
use solana_system_interface::{instruction as system_instruction, program as system_program};
use spl_associated_token_account::{
    get_associated_token_address, instruction::create_associated_token_account_idempotent,
};
// spl-token 9.x moved `state`/`instruction` builders into spl-token-interface.
use spl_token_interface::state::{Account as TokenAccount, Mint};

/// Devnet escrow program ID — see ROADMAP.md §0, programs/Anchor.toml.
const PROGRAM_ID_STR: &str = "D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn";
/// Protocol fee wallet from the design spec. Drives a real lamport
/// transfer on devnet so the IX exercises every account in the production
/// graph. The program validates it against the wallet pinned in `Config`.
const FEE_WALLET_STR: &str = "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh";
/// 0.0001 SOL per agent↔node transaction, charged on pay, finalize and
/// cancel to fund protocol infrastructure.
const FEE_LAMPORTS: u64 = 100_000;
/// Circle's devnet USDC mint (verified on-chain, 6 decimals).
const DEVNET_USDC_MINT: &str = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU";
const DEVNET_RPC: &str = "https://api.devnet.solana.com";
const DEFAULT_NODE: &str = "http://127.0.0.1:8402";
const DEFAULT_SECONDS: u64 = 60;
/// `Config` account size: 8-byte Anchor discriminator + config authority
/// (32) + fee_wallet (32) + fee_lamports (8) + bump (1).
const CONFIG_LEN: usize = 8 + 32 + 32 + 8 + 1;

const READ_TIMEOUT: Duration = Duration::from_secs(20);

/// `pay_for_compute` instruction data after the 8-byte Anchor discriminator
/// (= first 8 bytes of `sha256("global:pay_for_compute")`): `job_id`, then
/// `price_micros`, then `settlement_authority` — the per-contract key
/// allowed to finalize (the client records this payer, so finalize is
/// always authorized). Encoded by hand because borsh's `Pubkey` impl lives
/// on the 0.10 line while this crate derives borsh 1.8 (payload layout is
/// still the raw 32 bytes either way).
fn encode_pay_args(
    out: &mut Vec<u8>,
    job_id: [u8; 32],
    price_micros: u64,
    settlement_authority: Pubkey,
) {
    out.extend_from_slice(&job_id);
    out.extend_from_slice(&price_micros.to_le_bytes());
    out.extend_from_slice(&settlement_authority.to_bytes());
}

#[derive(BorshSerialize)]
struct FinalizeProRataArgs {
    f_micros: u32,
}

struct HttpResponse {
    status: u16,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
}

struct Args {
    node: String,
    mint: Option<Pubkey>,
    seconds: u64,
    seller: Option<Pubkey>,
    program: Option<Pubkey>,
    check: bool,
    verbose: bool,
}

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-x402-client [--node <url>] [--mint <addr>] \
         [--seconds <n>] [--seller <pubkey>] [--program <addr>] [--check] [--verbose]"
    );
    eprintln!("  --node    vtessera-node base URL (default {DEFAULT_NODE})");
    eprintln!("  --mint    pay a real devnet stablecoin mint instead of minting a test one");
    eprintln!("  --seconds agreed device-seconds for the job (default {DEFAULT_SECONDS})");
    eprintln!(
        "  --seller  where the seller's earned slice lands — on PAID hops must equal the offer's \n\
         \x20          payout_id or the run refuses to pay (a fresh keypair would never be seen by \n\
         \x20          the seller's operator)"
    );
    eprintln!(
        "  --program escrow program ID to pay (default {PROGRAM_ID_STR}); finalize is per-contract, \
         so any deployment works once paid"
    );
    eprintln!(
        "  --check   pre-flight diagnostics (offer payout, x402 challenge, config PDA, payer \
         funds, seller ATA) and exit without paying"
    );
    eprintln!("  --verbose extra detail during the paid flow");
    std::process::exit(2);
}

fn parse_args() -> Args {
    let mut node = DEFAULT_NODE.to_string();
    let mut mint: Option<Pubkey> = None;
    let mut seconds = DEFAULT_SECONDS;
    let mut seller: Option<Pubkey> = None;
    let mut program: Option<Pubkey> = None;
    let mut check = false;
    let mut verbose = false;
    let mut it = env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--node" => node = it.next().unwrap_or_else(|| usage_and_exit()),
            "--mint" => {
                let raw = it.next().unwrap_or_else(|| usage_and_exit());
                mint = Some(Pubkey::from_str(&raw).unwrap_or_else(|e| {
                    eprintln!("invalid --mint pubkey: {e}");
                    std::process::exit(2);
                }));
            }
            "--seconds" => {
                seconds = it
                    .next()
                    .unwrap_or_else(|| usage_and_exit())
                    .parse()
                    .unwrap_or_else(|e| {
                        eprintln!("invalid --seconds: {e}");
                        std::process::exit(2);
                    })
            }
            "--seller" => {
                let raw = it.next().unwrap_or_else(|| usage_and_exit());
                seller = Some(Pubkey::from_str(&raw).unwrap_or_else(|e| {
                    eprintln!("invalid --seller pubkey: {e}");
                    std::process::exit(2);
                }));
            }
            "--program" => {
                let raw = it.next().unwrap_or_else(|| usage_and_exit());
                program = Some(Pubkey::from_str(&raw).unwrap_or_else(|e| {
                    eprintln!("invalid --program pubkey: {e}");
                    std::process::exit(2);
                }));
            }
            "--check" => check = true,
            "--verbose" => verbose = true,
            "--help" | "-h" => usage_and_exit(),
            other => {
                eprintln!("unknown argument: {other}");
                usage_and_exit();
            }
        }
    }
    Args {
        node,
        mint,
        seconds,
        seller,
        program,
        check,
        verbose,
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args = parse_args();

    let payer_path: PathBuf = env::var("VTESSERA_PAYER")
        .map(PathBuf::from)
        .unwrap_or_else(|_| {
            let home = env::var("HOME").expect("HOME unset");
            PathBuf::from(format!("{home}/.config/solana/id.json"))
        });
    let payer = read_keypair_file(&payer_path)
        .map_err(|e| format!("read payer {}: {e}", payer_path.display()))?;
    println!("agent (payer/buyer): {}", payer.pubkey());

    let program_id = args
        .program
        .unwrap_or_else(|| Pubkey::from_str(PROGRAM_ID_STR).expect("PROGRAM_ID_STR valid"));
    let program_id_str = program_id.to_string();
    let fee_wallet = Pubkey::from_str(FEE_WALLET_STR)?;
    let rpc = RpcClient::new_with_commitment(DEVNET_RPC.to_string(), CommitmentConfig::confirmed());

    if args.check {
        let ready = preflight(&args, &payer, &program_id, &rpc)?;
        std::process::exit(if ready { 0 } else { 1 });
    }

    let pre_lamports = rpc.get_balance(&payer.pubkey())?;
    println!("agent devnet SOL: {:.6}", pre_lamports as f64 / 1e9);

    // --- 1. GET /offer --------------------------------------------------
    println!("\n--- 1. GET {}/offer ---", args.node);
    let offer_resp = http_request(&args.node, "GET", "/offer", &[], b"")?;
    if offer_resp.status != 200 {
        return Err(format!("GET /offer returned {}", offer_resp.status).into());
    }
    let offer = parse_offer(&String::from_utf8(offer_resp.body)?)?;
    let price_micros = match offer.per_device_second_micros {
        Some(per) => per.saturating_mul(args.seconds),
        None => {
            println!("offer is FREE — nothing to pay; POST /jobs directly");
            let accept = http_request(&args.node, "POST", "/jobs", &[], b"")?;
            println!("POST /jobs → {}", accept.status);
            return Ok(());
        }
    };
    let node_id = &offer.node_id;
    println!(
        "seller node {node_id} advertises {:.0} vCPU @ {:.0} MiB, price {} micros/device-sec (currency {:?})",
        offer.vcpus, offer.mem_mb, offer.per_device_second_micros.unwrap_or(0), offer.currency
    );
    // Seller payout must land where the offer advertises (the offer's
    // payout_id). If it would land anywhere else — a wrong --seller or, worse,
    // no --seller at all (a fresh keypair) — the seller's operator never sees
    // the funds, so refuse to pay. Hard error, not a note: this is a PAID hop.
    match &offer.payout_id {
        Some(payout) => {
            let ok = match args.seller {
                Some(s) => s.to_string() == *payout,
                None => false,
            };
            if !ok {
                let given = args
                    .seller
                    .map(|s| format!("--seller {s}"))
                    .unwrap_or_else(|| "--seller <unset, fresh keypair>".to_string());
                return Err(format!(
                    "offer pays out to {payout} but {given} would receive the escrow instead — \
                     the node operator would never see the funds. Re-run with --seller {payout}."
                )
                .into());
            }
            println!(
                "offer payout_id: {payout} (matches --seller {})",
                args.seller.unwrap()
            );
        }
        None => {
            return Err(
                "paid offer carries no payout_id — cannot verify where the seller's earnings \
                 land; refusing to pay."
                    .into(),
            )
        }
    }
    println!(
        "agreed work: {} device-seconds → price = {} micros",
        args.seconds, price_micros
    );

    // --- 2. POST /jobs → 402 x402 challenge -----------------------------
    println!("\n--- 2. POST {}/jobs (no payment proof) ---", args.node);
    // Build a minimal job spec the node can parse (used for the 402 challenge
    // and the paid submission).
    let job_spec = build_job_spec(&args, &payer);
    let chall_resp = http_request(&args.node, "POST", "/jobs", &[], job_spec.as_bytes())?;
    if chall_resp.status != 402 {
        return Err(format!("expected 402, got {}", chall_resp.status).into());
    }
    let chall_body = String::from_utf8(chall_resp.body)?;
    let (chall_network, chall_escrow) = parse_challenge(&chall_body)?;
    println!("x402 challenge received:");
    println!("  scheme: x402");
    println!("  network: {chall_network}");
    println!("  escrow_account: {chall_escrow}");
    if chall_escrow != program_id_str {
        println!("  NOTE: challenge escrow differs from local program ID {program_id_str}");
    }

    // --- 3. Stablecoin setup + pay_for_compute --------------------------
    println!("\n--- 3. on-chain payment (network: {chall_network}) ---");
    let (mint_pk, mint_kp, mint_authority) = match args.mint {
        Some(m) => {
            let tag = if m == Pubkey::from_str(DEVNET_USDC_MINT).unwrap() {
                " (Circle devnet USDC)"
            } else {
                ""
            };
            println!("using supplied stablecoin mint{tag}: {m}");
            (m, None, None)
        }
        None => {
            let kp = Keypair::new();
            println!(
                "creating test stablecoin mint (no faucet needed): {}",
                kp.pubkey()
            );
            (kp.pubkey(), Some(kp), Some(payer.pubkey()))
        }
    };

    // Buyer is the agent (payer). Create/find their ATA and, for the test
    // mint, fund them 10.000000.
    let buyer_ata = get_associated_token_address(&payer.pubkey(), &mint_pk);
    let mut ixs: Vec<Instruction> = vec![create_associated_token_account_idempotent(
        &payer.pubkey(),
        &payer.pubkey(),
        &mint_pk,
        &spl_token::id(),
    )];
    if let Some(authority) = mint_authority {
        let mint_rent = rpc.get_minimum_balance_for_rent_exemption(Mint::LEN)?;
        ixs.insert(
            0,
            system_instruction::create_account(
                &payer.pubkey(),
                &mint_pk,
                mint_rent,
                Mint::LEN as u64,
                &spl_token::id(),
            ),
        );
        ixs.insert(
            1,
            spl_token_interface::instruction::initialize_mint(
                &spl_token::id(),
                &mint_pk,
                &authority,
                None,
                6,
            )?,
        );
        ixs.push(spl_token_interface::instruction::mint_to(
            &spl_token::id(),
            &mint_pk,
            &buyer_ata,
            &authority,
            &[],
            10_000_000,
        )?);
    }
    let mut signers: Vec<&Keypair> = vec![&payer];
    if let Some(kp) = &mint_kp {
        signers.push(kp);
    }
    send_tx(&rpc, &ixs, &signers, &payer, "buyer ATA + fund stablecoin")?;

    // Seller: CLI pubkey or a fresh one (the program only ever credits the
    // seller; it never requires their signature).
    let seller_pubkey = args.seller.unwrap_or_else(|| Keypair::new().pubkey());
    let seller_ata = get_associated_token_address(&seller_pubkey, &mint_pk);
    send_tx(
        &rpc,
        &[create_associated_token_account_idempotent(
            &payer.pubkey(),
            &seller_pubkey,
            &mint_pk,
            &spl_token::id(),
        )],
        &[&payer],
        &payer,
        "create seller ATA",
    )?;
    println!("seller: {seller_pubkey}  ATA: {seller_ata}");
    if args.verbose {
        for (label, ata) in [("buyer", &buyer_ata), ("seller", &seller_ata)] {
            match token_account_state(&rpc, ata)? {
                Some(_info) => println!("  {label} ATA {ata}: initialized"),
                None => println!("  {label} ATA {ata}: not initialized yet"),
            }
        }
    }

    // job_id is agent-chosen entropy; the contract PDA is derived from it.
    // Mix in the current time (micros since epoch) so every run derives a
    // fresh contract PDA — the escrow program cannot re-allocate an
    // already-initialised contract account from a prior run.
    let job_id: [u8; 32] = {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_micros())
            .unwrap_or(0);
        let mut h = Sha256::new();
        h.update(b"vtessera-x402-client:");
        h.update(payer.pubkey().as_ref());
        h.update(mint_pk.as_ref());
        h.update(buyer_ata.as_ref());
        h.update(nonce.to_le_bytes());
        let d = h.finalize();
        let mut out = [0u8; 32];
        out.copy_from_slice(&d);
        out
    };
    let (contract_pda, _bump) = Pubkey::find_program_address(&[b"contract", &job_id], &program_id);
    let escrow_ata = get_associated_token_address(&contract_pda, &mint_pk);
    println!("job_id (hex): {}", hex_string(&job_id));
    println!("contract PDA: {contract_pda}");
    println!("escrow ATA:   {escrow_ata}");

    send_tx(
        &rpc,
        &[create_associated_token_account_idempotent(
            &payer.pubkey(),
            &contract_pda,
            &mint_pk,
            &spl_token::id(),
        )],
        &[&payer],
        &payer,
        "create escrow ATA",
    )?;

    // --- 3b. init_config: config authority + fee config (devnet) ----
    // The config authority only rotates the protocol fee config; finalize
    // no longer depends on it (each contract records its own settlement
    // authority at pay time). On mainnet this is the Squads vault PDA
    // (§3.5); the devnet flow pins it to the payer. Idempotent across
    // runs (the PDA may already be initialized). A config left over from
    // the pre-stablecoin program (41-byte layout) must be cleared before
    // redeploy, or this account deserialize fails.
    let (config_pda, _config_bump) =
        Pubkey::find_program_address(&[b"vtessera_config_v2"], &program_id);
    let cfg_disc = anchor_disc("init_config");
    let mut cfg_data = cfg_disc.to_vec();
    cfg_data.extend_from_slice(&payer.pubkey().to_bytes());
    cfg_data.extend_from_slice(&fee_wallet.to_bytes());
    cfg_data.extend_from_slice(&FEE_LAMPORTS.to_le_bytes());
    let cfg_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(payer.pubkey(), true),
            AccountMeta::new(config_pda, false),
            AccountMeta::new_readonly(system_program::id(), false),
        ],
        data: cfg_data,
    };
    // Disc + Config::LEN (see CONFIG_LEN const).
    match rpc.get_account(&config_pda) {
        Ok(acct) if acct.data.len() == CONFIG_LEN => {
            println!("config PDA {config_pda} already initialized; skipping init_config");
            if args.verbose {
                if let Ok(cfg) = parse_config_info(config_pda, &acct.data) {
                    println!("  config_authority: {}", cfg.config_authority);
                    println!("  fee_wallet: {}", cfg.fee_wallet);
                    println!("  fee_lamports: {}", cfg.fee_lamports);
                }
            }
        }
        Ok(acct) => {
            return Err(format!(
                "config PDA {config_pda} exists with {} bytes (expected {CONFIG_LEN}) — stale \
                 layout from the pre-stablecoin program; clear it (or redeploy the program) \
                 before running against the new program",
                acct.data.len()
            )
            .into());
        }
        Err(_) => {
            send_tx(&rpc, &[cfg_ix], &[&payer], &payer, "init_config")?;
        }
    }

    // The protocol fee lands in `fee_wallet` on every pay/finalize/cancel.
    // The runtime refuses a SOL transfer that leaves the receiver below
    // rent exemption (0-byte account needs 890,880 lamports), so on devnet
    // top it up if it doesn't exist yet. On mainnet the operator funds it.
    match rpc.get_account(&fee_wallet) {
        Ok(acct)
            if acct.lamports >= rpc.get_minimum_balance_for_rent_exemption(acct.data.len())? => {}
        _ => {
            let sig = rpc.request_airdrop(&fee_wallet, 1_000_000_000)?;
            rpc.confirm_transaction(&sig)?;
            println!("fee wallet {fee_wallet} funded on devnet (rent-exempt)");
        }
    }

    let seller_before = token_balance(&rpc, &seller_ata)?;
    let pay_disc = anchor_disc("pay_for_compute");
    let mut pay_data = pay_disc.to_vec();
    encode_pay_args(&mut pay_data, job_id, price_micros, payer.pubkey());

    // Anchor account order in PayForCompute (programs/vtessera-escrow):
    //   buyer (signer, mut), seller_payout, stablecoin_mint,
    //   buyer_stablecoin_ata (mut), escrow_stablecoin_ata (mut),
    //   contract (init, mut), fee_wallet (mut),
    //   token_program, system_program
    let pay_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(payer.pubkey(), true),
            AccountMeta::new_readonly(seller_pubkey, false),
            AccountMeta::new_readonly(mint_pk, false),
            AccountMeta::new(buyer_ata, false),
            AccountMeta::new(escrow_ata, false),
            AccountMeta::new(contract_pda, false),
            AccountMeta::new(fee_wallet, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
        ],
        data: pay_data,
    };
    println!("pay_for_compute: {price_micros} micros into escrow (fee wallet {fee_wallet})");
    let pay_sig = send_tx(&rpc, &[pay_ix], &[&payer], &payer, "pay_for_compute")?;

    let escrow_balance = token_balance(&rpc, &escrow_ata)?;
    println!("escrow balance after deposit: {escrow_balance} micros");
    if escrow_balance != price_micros {
        return Err(format!("escrow balance {escrow_balance} != price {price_micros}").into());
    }

    // Wait for the transaction to finalize (devnet can take 30+ seconds).
    println!("waiting for transaction to finalize...");
    let pay_sig_parsed = solana_sdk::signature::Signature::from_str(&pay_sig)?;
    for attempt in 1..=30 {
        let statuses = rpc.get_signature_statuses(&[pay_sig_parsed])?;
        if let Some(Some(status)) = statuses.value.first() {
            if let Some(err) = &status.err {
                let err_str = err.to_string();
                let hint = anchor_error_hint_str(&err_str);
                return Err(format!(
                    "pay_for_compute tx failed on-chain: {err_str}{}",
                    if hint.is_empty() {
                        String::new()
                    } else {
                        format!("\n  hint: {hint}")
                    }
                )
                .into());
            }
            let confirmed = format!("{:?}", status.confirmation_status);
            if confirmed.contains("Finalized") {
                println!("  finalized after {attempt} attempts");
                break;
            } else if attempt == 30 {
                return Err(
                    format!("transaction did not finalize in time, status: {confirmed}").into(),
                );
            } else {
                if attempt % 5 == 0 {
                    println!("  status: {confirmed} (waiting...)");
                }
                std::thread::sleep(Duration::from_secs(2));
            }
        } else {
            if attempt == 30 {
                return Err("transaction signature not found".into());
            }
            std::thread::sleep(Duration::from_secs(2));
        }
    }

    // --- 4. Retry POST /jobs with the x-payment proof -------------------
    println!("\n--- 4. POST {}/jobs with x-payment proof ---", args.node);
    let proof = format!(
        "{{\"scheme\":\"x402\",\"job_id\":\"{}\",\"tx\":\"{}\",\
         \"amount_micros\":{price_micros},\"mint\":\"{}\",\"network\":\"{chall_network}\"}}",
        hex_string(&job_id),
        pay_sig,
        mint_pk,
    );
    let accept_resp = http_request(
        &args.node,
        "POST",
        "/jobs",
        &[("x-payment", &proof)],
        job_spec.as_bytes(),
    )?;
    println!("POST /jobs → {} (proof submitted)", accept_resp.status);
    let accept_body = String::from_utf8(accept_resp.body)?;
    if !accept_body.is_empty() {
        println!("  body: {accept_body}");
    }
    if let Some((_, err)) = accept_resp
        .headers
        .iter()
        .find(|(k, _)| k == "x-payment-error")
    {
        println!("  payment error: {err}");
    }
    match accept_resp.status {
        200 => println!("  (paid job accepted and executed — full x402 flow succeeded)"),
        501 => println!(
            "  (legacy node without an executor: proof accepted at the wire level, payment is \
             in the escrow; finalize releases it.)"
        ),
        other => {
            return Err(
                format!("expected 200 or 501, got {other} — is vtessera-node up to date?").into(),
            );
        }
    }

    // --- 5. finalize_pro_rata (f = 1.0) ---------------------------------
    // The production finalize: seller is paid the earned slice in the
    // contract's stablecoin mint, buyer gets the refund. The payer signs
    // as the per-contract settlement authority recorded at pay_for_compute
    // time. The agent ran the job to completion → f = 1.0, seller gets
    // the whole escrow.
    println!("\n--- 5. finalize_pro_rata (f = 1.0) ---");
    let fin_disc = anchor_disc("finalize_pro_rata");
    let fin_args = FinalizeProRataArgs {
        f_micros: 1_000_000,
    };
    let mut fin_data = fin_disc.to_vec();
    fin_data.extend_from_slice(&fin_args.try_to_vec()?);

    // Anchor account order in FinalizePro (programs/vtessera-escrow):
    //   settlement_authority (signer, mut),
    //   contract (mut), escrow_stablecoin_ata (mut),
    //   buyer_stablecoin_ata (mut), seller_stablecoin_ata (mut),
    //   fee_wallet (mut), token_program, system_program
    let fin_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(payer.pubkey(), true),
            AccountMeta::new(contract_pda, false),
            AccountMeta::new(escrow_ata, false),
            AccountMeta::new(buyer_ata, false),
            AccountMeta::new(seller_ata, false),
            AccountMeta::new(fee_wallet, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
        ],
        data: fin_data,
    };
    let fin_sig = send_tx(&rpc, &[fin_ix], &[&payer], &payer, "finalize_pro_rata")?;

    // --- 6. Final balances + assertions ---------------------------------
    let escrow_after = token_balance(&rpc, &escrow_ata)?;
    let seller_after = token_balance(&rpc, &seller_ata)?;
    let post_lamports = rpc.get_balance(&payer.pubkey())?;

    println!("\n=== FINAL ON-CHAIN STATE ===");
    println!("escrow ATA:  {escrow_after} micros  (expected 0)");
    println!(
        "seller ATA:  [REDACTED] (balance change detected: {})",
        seller_after != seller_before,
    );
    println!(
        "agent SOL:   {:.6} (started {:.6})",
        post_lamports as f64 / 1e9,
        pre_lamports as f64 / 1e9,
    );

    assert_eq!(escrow_after, 0, "escrow should be drained after finalize");
    assert_eq!(
        seller_after - seller_before,
        price_micros,
        "seller delta should equal price at f=1.0"
    );

    println!("\nsuccess: paid {price_micros} micros into the escrow program, node accepted the job, seller paid out.");
    println!("explorer:");
    println!("  https://explorer.solana.com/tx/{pay_sig}?cluster=devnet");
    println!("  https://explorer.solana.com/tx/{fin_sig}?cluster=devnet");
    Ok(())
}

// --- Pre-flight check mode ---------------------------------------------

/// Build a minimal job spec the node can parse (used for the 402 challenge
/// and the paid submission). Unique-per-run job_id so the queue never sees a
/// collision from a prior run.
fn build_job_spec(args: &Args, payer: &Keypair) -> String {
    let job_id_hex = {
        let mut h = Sha256::new();
        h.update(payer.pubkey().as_ref());
        h.update(format!(
            "{}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        ));
        let d = h.finalize();
        let mut out = [0u8; 32];
        out.copy_from_slice(&d);
        hex_string(&out)
    };
    format!(
        "{{\"job_id\":\"{}\",\"image\":\"busybox\",\"command\":[\"echo\",\"x402 test\"],\
         \"env\":[],\"devices\":{{\"class\":{{\"kind\":\"cpu\"}},\"vcpus\":1,\"mem_kb\":65536,\"min_vram_mb\":0}},\
         \"network\":\"none\",\"max_duration_secs\":{}}}",
        job_id_hex, args.seconds
    )
}

enum PreflightStatus {
    Pass,
    Warn,
    Fail,
}

fn report(status: PreflightStatus, msg: &str) {
    let tag = match status {
        PreflightStatus::Pass => "PASS",
        PreflightStatus::Warn => "WARN",
        PreflightStatus::Fail => "FAIL",
    };
    println!("    {tag}  {msg}");
}

fn preflight(
    args: &Args,
    payer: &Keypair,
    program_id: &Pubkey,
    rpc: &RpcClient,
) -> Result<bool, Box<dyn std::error::Error>> {
    let mut critical = 0usize;

    println!("\n=== vtessera-x402-client pre-flight ===");
    println!("payer:    {}", payer.pubkey());
    println!("program:  {}", program_id);

    // --- 1. node offer + payout ---------------------------------------
    println!("\n[1] node offer");
    let offer = match http_request(&args.node, "GET", "/offer", &[], b"") {
        Ok(resp) if resp.status == 200 => match parse_offer(&String::from_utf8(resp.body)?) {
            Ok(o) => {
                let price = match o.per_device_second_micros {
                    Some(per) => format!("paid {per} micros/device-sec (currency {})", o.currency),
                    None => "free".to_string(),
                };
                let payout = o
                    .payout_id
                    .as_deref()
                    .map(|p| format!("  payout_id {p}"))
                    .unwrap_or_default();
                report(
                    PreflightStatus::Pass,
                    &format!(
                        "{} offers {} vCPU @ {} MiB — {price}{payout}",
                        args.node, o.vcpus, o.mem_mb
                    ),
                );
                Some(o)
            }
            Err(e) => {
                report(
                    PreflightStatus::Fail,
                    &format!("could not parse /offer: {e}"),
                );
                critical += 1;
                None
            }
        },
        Ok(resp) => {
            report(
                PreflightStatus::Fail,
                &format!("GET /offer returned {}", resp.status),
            );
            critical += 1;
            None
        }
        Err(e) => {
            report(PreflightStatus::Fail, &format!("node unreachable: {e}"));
            critical += 1;
            None
        }
    };

    // payout vs --seller: verify the live offer pays out where we expect.
    // On a PAID hop the escrow pays the --seller pubkey (or a fresh keypair
    // when unset), never the offer's payout_id — so anything other than an
    // exact match means the seller's operator never receives the funds.
    if let Some(offer) = &offer {
        let is_paid = offer.per_device_second_micros.is_some();
        match (&offer.payout_id, args.seller, is_paid) {
            (Some(payout), Some(seller), _) if *payout == seller.to_string() => {
                report(
                    PreflightStatus::Pass,
                    &format!("offer payout_id {payout} matches --seller {seller}"),
                );
            }
            (Some(payout), Some(seller), true) => {
                report(
                    PreflightStatus::Fail,
                    &format!(
                        "--seller {seller} != offer payout_id {payout} — earnings would go to \
                         --seller {seller}, not the offer's payout key {payout}"
                    ),
                );
                critical += 1;
            }
            (Some(payout), None, true) => {
                report(
                    PreflightStatus::Fail,
                    &format!(
                        "paid offer pays {payout} but no --seller set — escrow would finalize to \
                         a fresh keypair the seller doesn't own; pass --seller {payout}"
                    ),
                );
                critical += 1;
            }
            (None, _, true) => {
                report(
                    PreflightStatus::Fail,
                    "paid offer carries no payout_id — cannot verify where earnings land",
                );
                critical += 1;
            }
            _ => {}
        }
    }

    // --- 2. x402 challenge (paid offers only) -------------------------
    // Only safe when the offer is paid: a free node would accept the job
    // outright (side effect), while a paid node answers 402 without admitting.
    println!("\n[2] x402 challenge");
    if let Some(offer) = &offer {
        if offer.per_device_second_micros.is_some() {
            let job_spec = build_job_spec(args, payer);
            match http_request(&args.node, "POST", "/jobs", &[], job_spec.as_bytes()) {
                Ok(resp) if resp.status == 402 => {
                    match parse_challenge(&String::from_utf8(resp.body)?) {
                        Ok((network, escrow)) => {
                            if escrow == program_id.to_string() {
                                report(
                                PreflightStatus::Pass,
                                &format!("402 challenge escrow_account {escrow} == program {program_id} (network {network})"),
                            );
                            } else {
                                report(
                                    PreflightStatus::Fail,
                                    &format!(
                                    "challenge escrow_account {escrow} != program {program_id} — \
                                     the node points at a different escrow deployment"
                                ),
                                );
                                critical += 1;
                            }
                        }
                        Err(e) => {
                            report(
                                PreflightStatus::Fail,
                                &format!("unparseable challenge: {e}"),
                            );
                            critical += 1;
                        }
                    }
                }
                Ok(resp) => {
                    report(
                        PreflightStatus::Fail,
                        &format!("expected 402 challenge, got status {}", resp.status),
                    );
                    critical += 1;
                }
                Err(e) => {
                    report(PreflightStatus::Fail, &format!("node unreachable: {e}"));
                    critical += 1;
                }
            }
        } else {
            report(
                PreflightStatus::Warn,
                "free offer — nothing to pay, no x402 challenge expected",
            );
        }
    } else {
        report(
            PreflightStatus::Fail,
            "offer could not be validated (see [1])",
        );
        critical += 1;
    }

    // --- 3. config PDA ------------------------------------------------
    println!("\n[3] config PDA");
    match inspect_config(rpc, program_id)? {
        Some(Ok(cfg)) => {
            report(
                PreflightStatus::Pass,
                &format!(
                    "{} config_authority {} fee_wallet {} fee {} lamports",
                    cfg.account, cfg.config_authority, cfg.fee_wallet, cfg.fee_lamports
                ),
            );
            report(
                PreflightStatus::Pass,
                &format!(
                    "finalize authority is recorded per contract at pay time as this payer {} — \
                     finalize succeeds even when the config PDA is owned by another wallet",
                    payer.pubkey()
                ),
            );
        }
        Some(Err(e)) => {
            report(
                PreflightStatus::Fail,
                &format!("{e} — the config PDA must be a valid {CONFIG_LEN}-byte Config account"),
            );
            critical += 1;
        }
        None => {
            report(
                PreflightStatus::Pass,
                &format!(
                    "no config PDA for {program_id} — not a blocker: the first paid run calls \
                     init_config (this payer becomes the config authority)"
                ),
            );
        }
    }

    // --- 4. payer funds -----------------------------------------------
    println!("\n[4] payer funds");
    let sol = rpc.get_balance(&payer.pubkey())?;
    report(
        PreflightStatus::Pass,
        &format!("devnet SOL balance: {:.6}", sol as f64 / 1e9),
    );
    if sol < 2 * FEE_LAMPORTS {
        report(
            PreflightStatus::Fail,
            &format!(
                "SOL {:.6} below 2× protocol fee ({} lamports/tx) — `solana airdrop 2` (devnet)",
                sol as f64 / 1e9,
                FEE_LAMPORTS
            ),
        );
        critical += 1;
    }
    let price_micros = offer.as_ref().and_then(|o| {
        o.per_device_second_micros
            .map(|per| per.saturating_mul(args.seconds))
    });
    match args.mint {
        Some(mint) => {
            let ata = get_associated_token_address(&payer.pubkey(), &mint);
            match token_account_state(rpc, &ata)? {
                Some(info) => {
                    report(
                        PreflightStatus::Pass,
                        &format!("{ata}: {} micros of {mint}", info.amount),
                    );
                    if let Some(price) = price_micros {
                        if info.amount < price {
                            report(
                                PreflightStatus::Fail,
                                &format!("balance {} < agreed price {price} micros", info.amount),
                            );
                            critical += 1;
                        }
                    }
                }
                None => {
                    report(
                        PreflightStatus::Fail,
                        &format!("no ATA for {mint} yet — fund the payer's {mint} balance"),
                    );
                    critical += 1;
                }
            }
        }
        None => report(
            PreflightStatus::Warn,
            "no --mint: a test stablecoin will be created + funded automatically",
        ),
    }

    // --- 5. seller ATA ------------------------------------------------
    println!("\n[5] seller ATA");
    match (args.seller, args.mint) {
        (Some(seller), Some(mint)) => {
            let ata = get_associated_token_address(&seller, &mint);
            match token_account_state(rpc, &ata)? {
                Some(info) => report(
                    PreflightStatus::Pass,
                    &format!(
                        "{ata}: {} micros  mint {}  owner {}",
                        info.amount, info.mint, info.owner
                    ),
                ),
                None => report(
                    PreflightStatus::Warn,
                    &format!("no ATA for {seller}/{mint} yet — created automatically on pay"),
                ),
            }
        }
        _ => report(
            PreflightStatus::Warn,
            "skip — need --seller and --mint to resolve (fresh keypair otherwise)",
        ),
    }

    println!("\n=== RESULT ===");
    if critical == 0 {
        println!("READY — safe to submit the paid job.");
    } else {
        println!("NOT READY — {critical} critical check(s) failed; fix above before paying.");
    }
    Ok(critical == 0)
}

// --- HTTP client -------------------------------------------------------

fn http_request(
    base: &str,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> Result<HttpResponse, String> {
    let url = base
        .strip_prefix("http://")
        .ok_or("base URL must be http://")?;
    let (host, port) = match url.split_once(':') {
        Some((h, p)) => (h, p.parse::<u16>().map_err(|e| format!("bad port: {e}"))?),
        None => (url, 80u16),
    };
    let mut stream =
        TcpStream::connect((host, port)).map_err(|e| format!("connect {host}:{port}: {e}"))?;
    stream.set_read_timeout(Some(READ_TIMEOUT)).ok();
    stream.set_write_timeout(Some(READ_TIMEOUT)).ok();

    let mut req = format!("{method} {path} HTTP/1.1\r\nhost: {host}\r\nconnection: close\r\n");
    req.push_str(&format!("content-length: {}\r\n", body.len()));
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str("\r\n");
    stream
        .write_all(req.as_bytes())
        .map_err(|e| format!("write request: {e}"))?;
    stream
        .write_all(body)
        .map_err(|e| format!("write body: {e}"))?;

    let mut reader = BufReader::new(stream);
    let mut status_line = String::new();
    reader
        .read_line(&mut status_line)
        .map_err(|e| format!("read status line: {e}"))?;
    let status: u16 = status_line
        .split_whitespace()
        .nth(1)
        .ok_or("malformed status line")?
        .parse()
        .map_err(|e: std::num::ParseIntError| format!("bad status: {e}"))?;

    let mut content_length: usize = 0;
    let mut resp_headers: Vec<(String, String)> = Vec::new();
    loop {
        let mut line = String::new();
        let n = reader
            .read_line(&mut line)
            .map_err(|e| format!("read header line: {e}"))?;
        if n == 0 {
            break;
        }
        let trimmed = line.trim_end();
        if trimmed.is_empty() {
            break;
        }
        if let Some(idx) = trimmed.find(':') {
            let (k, v) = trimmed.split_at(idx);
            let k = k.trim().to_string();
            let v = v[1..].trim().to_string();
            if k.eq_ignore_ascii_case("content-length") {
                content_length = v.parse().unwrap_or(0);
            } else {
                resp_headers.push((k, v));
            }
        }
    }

    let mut body_out = vec![0u8; content_length];
    if content_length > 0 {
        reader
            .read_exact(&mut body_out)
            .map_err(|e| format!("read body: {e}"))?;
    }
    Ok(HttpResponse {
        status,
        headers: resp_headers,
        body: body_out,
    })
}

// --- Offer / challenge parsing -----------------------------------------

struct OfferInfo {
    node_id: String,
    vcpus: u32,
    mem_mb: u32,
    currency: String,
    per_device_second_micros: Option<u64>,
    /// `price.payout_id` (paid offers only): the wallet the node pays earnings
    /// to. Verify it matches the intended seller before paying.
    payout_id: Option<String>,
}

/// Parse the envelope `{...}` printed by `vtessera_offer::to_json`. The
/// node binary writes fixed field order, so string scanning (matching
/// crates/node-api/src/bin/vtessera_node.rs) is enough.
fn parse_offer(s: &str) -> Result<OfferInfo, String> {
    let body_str = extract_object(s, "\"body\":").ok_or("missing offer body")?;
    let node_id = extract_string(&body_str, "\"node_id\":").ok_or("missing node_id")?;
    let device_str = extract_object(&body_str, "\"device\":").ok_or("missing device")?;
    let vcpus: u32 = extract_number(&device_str, "\"vcpus\":")
        .ok_or("missing vcpus")?
        .parse()
        .map_err(|e: std::num::ParseIntError| e.to_string())?;
    let mem_mb: u32 = extract_number(&device_str, "\"mem_mb\":")
        .ok_or("missing mem_mb")?
        .parse()
        .map_err(|e: std::num::ParseIntError| e.to_string())?;
    let price_str = extract_object(&body_str, "\"price\":").ok_or("missing price")?;
    let currency = extract_string(&price_str, "\"currency\":").unwrap_or_default();
    let payout_id = extract_string(&price_str, "\"payout_id\":");
    let per = match extract_string(&price_str, "\"mode\":").as_deref() {
        Some("paid") => Some(
            extract_number(&price_str, "\"per_device_second_micros\":")
                .ok_or("missing per_device_second_micros")?
                .parse()
                .map_err(|e: std::num::ParseIntError| e.to_string())?,
        ),
        _ => None,
    };
    Ok(OfferInfo {
        node_id,
        vcpus,
        mem_mb,
        currency,
        per_device_second_micros: per,
        payout_id,
    })
}

/// Parse the 402 body: `{"scheme":"x402","network":..,"escrow_account":..,"offer":..}`.
fn parse_challenge(s: &str) -> Result<(String, String), String> {
    let network = extract_string(s, "\"network\":").ok_or("missing network")?;
    let escrow = extract_string(s, "\"escrow_account\":").ok_or("missing escrow_account")?;
    Ok((network, escrow))
}

fn extract_string(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = s[start..].trim_start();
    if !rest.starts_with('"') {
        return None;
    }
    let body = &rest[1..];
    let end = body.find('"')?;
    Some(body[..end].to_string())
}

fn extract_number(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = s[start..].trim_start();
    let end = rest
        .find(|c: char| !(c.is_ascii_digit() || c == '-'))
        .unwrap_or(rest.len());
    if end == 0 {
        None
    } else {
        Some(rest[..end].to_string())
    }
}

/// Extract a balanced `{...}` object value following `key` in `s`.
fn extract_object(s: &str, key: &str) -> Option<String> {
    let start = s.find(key)? + key.len();
    let rest = &s[start..];
    let open = rest.find('{')?;
    let bytes = &rest.as_bytes()[open..];
    let mut depth = 0i32;
    let mut in_str = false;
    let mut esc = false;
    for (i, &b) in bytes.iter().enumerate() {
        if esc {
            esc = false;
            continue;
        }
        if in_str {
            if b == b'\\' {
                esc = true;
            } else if b == b'"' {
                in_str = false;
            }
            continue;
        }
        match b {
            b'"' => in_str = true,
            b'{' => depth += 1,
            b'}' => {
                depth -= 1;
                if depth == 0 {
                    return Some(std::str::from_utf8(&bytes[..=i]).ok()?.to_string());
                }
            }
            _ => {}
        }
    }
    None
}

// --- On-chain helpers --------------------------------------------------

/// Decoded `Config` account (`vtessera_config_v2` PDA) for the escrow
/// program. `config_authority` is the fee-config governance key (it gates
/// `update_config` unless rotated there); finalize authorization is
/// per-contract, never derived from this account.
struct ConfigInfo {
    account: Pubkey,
    config_authority: Pubkey,
    fee_wallet: Pubkey,
    fee_lamports: u64,
}

/// Parse a `Config` account blob (see `CONFIG_LEN`); the discriminator is the
/// first 8 bytes, then the three fields in init_config order.
fn parse_config_info(account: Pubkey, data: &[u8]) -> Result<ConfigInfo, String> {
    if data.len() != CONFIG_LEN {
        return Err(format!(
            "config {account} has {} bytes (expected {CONFIG_LEN}) — stale layout from the \
             pre-stablecoin program; clear it (or redeploy the program) before running against \
             the new program",
            data.len()
        ));
    }
    let mut auth = [0u8; 32];
    auth.copy_from_slice(&data[8..40]);
    let mut fee_wallet = [0u8; 32];
    fee_wallet.copy_from_slice(&data[40..72]);
    let mut lamports = [0u8; 8];
    lamports.copy_from_slice(&data[72..80]);
    Ok(ConfigInfo {
        account,
        config_authority: Pubkey::new_from_array(auth),
        fee_wallet: Pubkey::new_from_array(fee_wallet),
        fee_lamports: u64::from_le_bytes(lamports),
    })
}

/// Read and decode the program's config PDA. Returns:
/// - `Some(Ok(info))` if initialized with the current layout,
/// - `Some(Err(msg))` if the account exists but has a stale layout,
/// - `None` if the PDA does not exist yet.
fn inspect_config(
    rpc: &RpcClient,
    program_id: &Pubkey,
) -> Result<Option<Result<ConfigInfo, String>>, Box<dyn std::error::Error>> {
    let (config_pda, _) = Pubkey::find_program_address(&[b"vtessera_config_v2"], program_id);
    match rpc.get_account(&config_pda) {
        Ok(acct) => Ok(Some(parse_config_info(config_pda, &acct.data))),
        Err(_) => Ok(None),
    }
}

/// On-chain state of a token account; `None` means the account does not
/// exist yet.
struct TokenAccountInfo {
    amount: u64,
    mint: Pubkey,
    owner: Pubkey,
}

fn token_account_state(
    rpc: &RpcClient,
    ata: &Pubkey,
) -> Result<Option<TokenAccountInfo>, Box<dyn std::error::Error>> {
    match rpc.get_account(ata) {
        Ok(acct) => {
            let parsed = TokenAccount::unpack(&acct.data)?;
            Ok(Some(TokenAccountInfo {
                amount: parsed.amount,
                mint: parsed.mint,
                owner: parsed.owner,
            }))
        }
        Err(_) => Ok(None),
    }
}

fn anchor_disc(ix_name: &str) -> [u8; 8] {
    let mut h = Sha256::new();
    h.update(format!("global:{ix_name}").as_bytes());
    let d = h.finalize();
    let mut out = [0u8; 8];
    out.copy_from_slice(&d[..8]);
    out
}

fn contains_any(hay: &str, needles: &[&str]) -> bool {
    needles.iter().any(|n| hay.contains(n))
}

/// Map a failed on-chain transaction to a human remediation hint. The escrow
/// program's `EscrowError` variants encode as Anchor custom codes 6000 + index
/// (see programs/vtessera-escrow/src/lib.rs), surfaced by the RPC as either a
/// named log line or `custom program error: 0x17xx` / decimal `6000..6007`.
fn anchor_error_hint(e: &dyn std::error::Error) -> &'static str {
    anchor_error_hint_str(&format!("{e}\n{e:?}"))
}

fn anchor_error_hint_str(hay: &str) -> &'static str {
    if contains_any(
        hay,
        &[
            "NotSettlementAuthority",
            "0x1770",
            "custom program error: 6000",
        ],
    ) {
        "the finalize signer is not this contract's recorded settlement authority. The client \
         records the payer at pay_for_compute time, so finalize with the same payer key; to \
         settle under another key, that key must have been passed as pay_for_compute's \
         settlement_authority."
    } else if contains_any(
        hay,
        &["WrongFeeWallet", "0x1777", "custom program error: 6007"],
    ) {
        "fee_wallet does not match config.fee_wallet — align FEE_WALLET_STR with the config \
             pinned in the deployment, or redeploy with a matching config."
    } else if contains_any(hay, &["ZeroPrice", "0x1771", "custom program error: 6001"]) {
        "contract price must be > 0 — the node's offer price is 0; check the offer's \
             per_device_second_micros."
    } else if contains_any(
        hay,
        &["AlreadyFinal", "0x1773", "custom program error: 6003"],
    ) {
        "contract already finalized — job_id collided with a prior run; rerun (job_ids are \
             time-derived, so a fresh run gets a fresh contract PDA)."
    } else if contains_any(hay, &["WrongMint", "0x1774", "custom program error: 6004"]) {
        "token account mint mismatch — keep --mint consistent across the run and with the \
             offer's currency, or the escrow/buyer/seller ATAs span different mints."
    } else if contains_any(hay, &["WrongOwner", "0x1775", "custom program error: 6005"]) {
        "token account owner mismatch — a reused or mispointed ATA; rerun with a fresh \
             --seller/contract (ATAs are re-derived from the pubkeys)."
    } else if contains_any(
        hay,
        &[
            "FractionOutOfRange",
            "MathOverflow",
            "0x1772",
            "0x1776",
            "custom program error: 6002",
            "custom program error: 6006",
        ],
    ) {
        "program-internal arithmetic/encoding failure (fraction out of range or overflow) — \
             this should not happen for f = 1.0; capture the full raw error below."
    } else if hay.contains("InsufficientFundsForRent") || hay.contains("InsufficientFunds") {
        "payer SOL too low (devnet protocol fee is 100,000 lamports per tx and ATAs need rent) \
             — `solana airdrop 2` (devnet)."
    } else if hay.contains("Transaction simulation failed") {
        "simulation failed before send — run --check to surface payer SOL / ATA / config \
             readiness."
    } else {
        ""
    }
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
    let sig = match rpc.send_and_confirm_transaction_with_spinner(&tx) {
        Ok(sig) => sig,
        Err(e) => {
            let hint = anchor_error_hint(&e);
            let err = if hint.is_empty() {
                format!("[{label}] transaction failed: {e}")
            } else {
                format!("[{label}] transaction failed: {e}\n  hint: {hint}")
            };
            return Err(err.into());
        }
    };
    println!("  [{label}] {sig}");
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
