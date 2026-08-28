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
//!    refuses with 501 while execution is not wired (v0). That's honest:
//!    the payment is real and sitting in the escrow, so step 5 still
//!    exercises the money path end to end.
//! 5. `finalize_pro_rata` (f = 1.0) drains the escrow to the seller's
//!    stablecoin ATA in the contract's mint. On devnet the settlement
//!    authority is the payer.
//!
//! By default the client mints its own test stablecoin so the whole loop
//! runs with no faucet dependency (devnet-demo's approach). Pass
//! `--mint <addr>` to pay a real devnet stablecoin (e.g. Circle's devnet
//! USDC `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU`) — the payer must
//! already hold that token, since only Circle can mint it.
//!
//! The node's `VerifyAndRun` branch returns 501 Not Implemented until an
//! executor and an on-chain verifier are wired in (crates/node-api). The
//! proof is sent anyway so the wire contract is exercised end to end.
//!
//! Standalone crate (excluded from the host workspace): same pinned
//! solana-sdk 1.18 tree as `crates/devnet-demo`.

use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

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
    transaction::Transaction,
};
use spl_associated_token_account::{
    get_associated_token_address, instruction::create_associated_token_account_idempotent,
};
use spl_token::state::{Account as TokenAccount, Mint};

/// Devnet escrow program ID — see ROADMAP.md §0, programs/Anchor.toml.
const PROGRAM_ID_STR: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
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

const READ_TIMEOUT: Duration = Duration::from_secs(20);

/// `pay_for_compute` IX args, borsh-encoded after the 8-byte Anchor
/// discriminator (= first 8 bytes of `sha256("global:pay_for_compute")`).
#[derive(BorshSerialize)]
struct PayForComputeArgs {
    job_id: [u8; 32],
    price_micros: u64,
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
}

fn usage_and_exit() -> ! {
    eprintln!(
        "usage: vtessera-x402-client [--node <url>] [--mint <addr>] \
         [--seconds <n>] [--seller <pubkey>]"
    );
    eprintln!(
        "  --node    vtessera-node base URL (default {DEFAULT_NODE})"
    );
    eprintln!(
        "  --mint    pay a real devnet stablecoin mint instead of minting a test one"
    );
    eprintln!("  --seconds agreed device-seconds for the job (default {DEFAULT_SECONDS})");
    eprintln!("  --seller  where the seller's earned slice lands (default: fresh keypair)");
    std::process::exit(2);
}

fn parse_args() -> Args {
    let mut node = DEFAULT_NODE.to_string();
    let mut mint: Option<Pubkey> = None;
    let mut seconds = DEFAULT_SECONDS;
    let mut seller: Option<Pubkey> = None;
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

    let program_id = Pubkey::from_str(PROGRAM_ID_STR)?;
    let fee_wallet = Pubkey::from_str(FEE_WALLET_STR)?;
    let rpc = RpcClient::new_with_commitment(DEVNET_RPC.to_string(), CommitmentConfig::confirmed());
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
    println!(
        "agreed work: {} device-seconds → price = {} micros",
        args.seconds, price_micros
    );

    // --- 2. POST /jobs → 402 x402 challenge -----------------------------
    println!("\n--- 2. POST {}/jobs (no payment proof) ---", args.node);
    // Build a minimal job spec so the node can parse the body.
    let job_id_hex = {
        let mut h = Sha256::new();
        h.update(payer.pubkey().as_ref());
        h.update(format!("{}", std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()));
        hex_string(h.finalize().as_slice())
    };
    let job_spec = format!(
        "{{\"job_id\":\"{}\",\"image\":\"busybox\",\"command\":[\"echo\",\"x402 test\"],\
         \"env\":[],\"devices\":{{\"class\":{{\"kind\":\"cpu\"}},\"vcpus\":1,\"mem_kb\":65536,\"min_vram_mb\":0}},\
         \"network\":\"none\",\"max_duration_secs\":{}}}",
        job_id_hex, args.seconds
    );
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
    if chall_escrow != PROGRAM_ID_STR {
        println!("  NOTE: challenge escrow differs from local program ID {PROGRAM_ID_STR}");
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
            println!("creating test stablecoin mint (no faucet needed): {}", kp.pubkey());
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
        ixs.insert(1, spl_token::instruction::initialize_mint(
            &spl_token::id(),
            &mint_pk,
            &authority,
            None,
            6,
        )?);
        ixs.push(spl_token::instruction::mint_to(
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

    // --- 3b. init_config: settlement authority + fee config (devnet) ----
    // Finalize IXs require the signer to match the config's settlement
    // authority; the fee wallet + amount are pinned here too. On mainnet
    // this is the Squads vault PDA (§3.5); the devnet flow pins it to the
    // payer. Idempotent across runs (the PDA may already be initialized).
    // A config left over from the pre-stablecoin program (41-byte layout)
    // must be cleared before redeploy, or this account deserialize fails.
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
    const CONFIG_LEN: usize = 8 + 32 + 32 + 8 + 1; // disc + Config::LEN
    match rpc.get_account(&config_pda) {
        Ok(acct) if acct.data.len() == CONFIG_LEN => {
            println!("config PDA {config_pda} already initialized; skipping init_config")
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
        Ok(acct) if acct.lamports >= rpc.get_minimum_balance_for_rent_exemption(acct.data.len())? => {}
        _ => {
            let sig = rpc.request_airdrop(&fee_wallet, 1_000_000_000)?;
            rpc.confirm_transaction(&sig)?;
            println!("fee wallet {fee_wallet} funded on devnet (rent-exempt)");
        }
    }

    let pay_disc = anchor_disc("pay_for_compute");
    let pay_args = PayForComputeArgs {
        job_id,
        price_micros,
    };
    let mut pay_data = pay_disc.to_vec();
    pay_data.extend_from_slice(&pay_args.try_to_vec()?);

    // Anchor account order in PayForCompute (programs/vtessera-escrow):
    //   buyer (signer, mut), seller_payout, stablecoin_mint,
    //   buyer_stablecoin_ata (mut), escrow_stablecoin_ata (mut),
    //   contract (init, mut), fee_wallet (mut), config,
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
            AccountMeta::new_readonly(config_pda, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
        ],
        data: pay_data,
    };
    println!(
        "pay_for_compute: {price_micros} micros into escrow (fee wallet {fee_wallet})"
    );
    let pay_sig = send_tx(&rpc, &[pay_ix], &[&payer], &payer, "pay_for_compute")?;

    let escrow_balance = token_balance(&rpc, &escrow_ata)?;
    println!("escrow balance after deposit: {escrow_balance} micros");
    if escrow_balance != price_micros {
        return Err(format!(
            "escrow balance {escrow_balance} != price {price_micros}"
        )
        .into());
    }

    // Wait for the transaction to finalize (devnet can take 30+ seconds).
    println!("waiting for transaction to finalize...");
    let pay_sig_parsed = solana_sdk::signature::Signature::from_str(&pay_sig)?;
    for attempt in 1..=30 {
        let statuses = rpc.get_signature_statuses(&[pay_sig_parsed])?;
        if let Some(Some(status)) = statuses.value.first() {
            if status.err.is_some() {
                return Err("pay_for_compute tx failed on-chain".into());
            }
            let confirmed = format!("{:?}", status.confirmation_status);
            if confirmed.contains("Finalized") {
                println!("  finalized after {attempt} attempts");
                break;
            } else if attempt == 30 {
                return Err(format!("transaction did not finalize in time, status: {confirmed}").into());
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
    if let Some((_, err)) = accept_resp.headers.iter().find(|(k, _)| k == "x-payment-error") {
        println!("  payment error: {err}");
    }
    match accept_resp.status {
        200 => println!(
            "  (paid job accepted and executed — full x402 flow succeeded)"
        ),
        501 => println!(
            "  (expected in v0: execution is not wired, so the node refuses. \
             The payment is in the escrow; finalize releases it.)"
        ),
        other => {
            return Err(format!(
                "expected 200 or 501, got {other} — is vtessera-node up to date?"
            )
            .into());
        }
    }

    // --- 5. finalize_pro_rata (f = 1.0) ---------------------------------
    // The production finalize: seller is paid the earned slice in the
    // contract's stablecoin mint, buyer gets the refund. Devnet has no
    // HNT/Pyth feeds — none are needed now. The agent ran the job to
    // completion → f = 1.0, seller gets the whole escrow.
    println!("\n--- 5. finalize_pro_rata (f = 1.0) ---");
    let fin_disc = anchor_disc("finalize_pro_rata");
    let fin_args = FinalizeProRataArgs { f_micros: 1_000_000 };
    let mut fin_data = fin_disc.to_vec();
    fin_data.extend_from_slice(&fin_args.try_to_vec()?);

    // Anchor account order in FinalizePro (programs/vtessera-escrow):
    //   settlement_authority (signer, mut), config,
    //   contract (mut), escrow_stablecoin_ata (mut),
    //   buyer_stablecoin_ata (mut), seller_stablecoin_ata (mut),
    //   fee_wallet (mut), token_program, system_program
    let fin_ix = Instruction {
        program_id,
        accounts: vec![
            AccountMeta::new(payer.pubkey(), true),
            AccountMeta::new_readonly(config_pda, false),
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
    println!("seller ATA:  {seller_after} micros  (expected {price_micros}, f=1.0)");
    println!(
        "agent SOL:   {:.6} (started {:.6})",
        post_lamports as f64 / 1e9,
        pre_lamports as f64 / 1e9,
    );

    assert_eq!(escrow_after, 0, "escrow should be drained after finalize");
    assert_eq!(seller_after, price_micros, "seller should get the full price at f=1.0");

    println!("\nsuccess: paid {price_micros} micros into the escrow program, node accepted the job, seller paid out.");
    println!("explorer:");
    println!("  https://explorer.solana.com/tx/{pay_sig}?cluster=devnet");
    println!("  https://explorer.solana.com/tx/{fin_sig}?cluster=devnet");
    Ok(())
}

// --- HTTP client -------------------------------------------------------

fn http_request(
    base: &str,
    method: &str,
    path: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> Result<HttpResponse, String> {
    let url = base.strip_prefix("http://").ok_or("base URL must be http://")?;
    let (host, port) = match url.split_once(':') {
        Some((h, p)) => (h, p.parse::<u16>().map_err(|e| format!("bad port: {e}"))?),
        None => (url, 80u16),
    };
    let mut stream = TcpStream::connect((host, port))
        .map_err(|e| format!("connect {host}:{port}: {e}"))?;
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
    stream.write_all(body).map_err(|e| format!("write body: {e}"))?;

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

fn anchor_disc(ix_name: &str) -> [u8; 8] {
    let mut h = Sha256::new();
    h.update(format!("global:{ix_name}").as_bytes());
    let d = h.finalize();
    let mut out = [0u8; 8];
    out.copy_from_slice(&d[..8]);
    out
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
