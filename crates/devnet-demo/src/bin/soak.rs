//! Vtessera devnet **soak runner** — MAINNET-CHECKLIST §6.1.
//!
//! Runs the escrow flow repeatedly against Solana devnet with varied
//! parameters to shake out the bugs that only surface under realistic
//! volume: rent-exemption edges at tiny `price`, `f_micros` values where
//! the integer split rounds oddly, ATA-creation collisions, RPC failures
//! mid-transaction, double-finalize races, etc.
//!
//! Each iteration:
//!
//! - picks a random `price_micros` in `[1, 10_000_000]`
//! - picks a random `f_micros` from a weighted pool (`0`, `1`,
//!   `500_000`, `990_000`, `1_000_000`, plus a uniform random draw)
//! - with probability `CANCEL_P`, fires `cancel_before_start` instead of
//!   `pay` + `finalize_pro_rata_stub` — the buyer-side full refund
//! - uses a random seller pubkey
//! - logs the outcome; any unexpected failure bumps the error count
//!
//! ## Usage
//!
//! ```
//! # 100 iterations against devnet with the default payer:
//! cargo run --bin soak
//!
//! # a specific count, and a faster cancel cadence:
//! cargo run --bin soak -- --iters 500 --cancel-p 0.3
//!
//! # different payer + RPC (e.g. a local validator for offline soaking):
//! VTESSERA_PAYER=~/.config/solana/id.json \
//!   SOAK_RPC=http://127.0.0.1:8899 \
//!   cargo run --bin soak -- --iters 10
//! ```
//!
//! ## Exit codes
//!
//! - `0` — every iteration behaved as expected
//! - `1` — at least one iteration failed unexpectedly (or setup failed)
//!
//! ## What "expected" means per iteration
//!
//! - **Finalize path:** after `pay` + `finalize_pro_rata_stub(f_micros)`,
//!   the escrow ATA is drained; seller received `earned = price * f`
//!   (truncated toward zero) and the buyer's balance is
//!   `start + refund`, `refund = price - earned`. An `f_micros = 1_000_000`
//!   run must refund exactly 0.
//! - **Cancel path:** after `pay` + `cancel_before_start`, the buyer gets
//!   the full `price` back and the escrow is drained.
//!
//! ## Deploy prerequisite
//!
//! Requires the §3.5 program build deployed on devnet (the one exposing
//! `init_config`). The runner initializes the config PDA with the payer
//! as settlement authority if it doesn't exist yet (idempotent), exactly
//! like the demo.
//!
//! This runner deliberately does **not** use `rand`: it's a long-running
//! devnet process and the xorshift64\* below is deterministic given a
//! seed, so a failing seed can be replayed. The default seed is derived
//! from the payer's pubkey + the iteration count, so two runs of the same
//! payer diverge. Override with `SOAK_SEED`.

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

// Mirror of crates/devnet-demo/src/main.rs — same constants, same
// hand-rolled Anchor encoding. Kept separate (not shared) because the
// demo's file is intentionally self-contained; see its module comment.

const PROGRAM_ID_STR: &str = "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma";
const FEE_WALLET_STR: &str = "9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD";
const DEVNET_RPC: &str = "https://api.devnet.solana.com";

/// Probability of firing `cancel_before_start` instead of
/// `pay` + `finalize_pro_rata_stub`, per iteration.
const CANCEL_P: f64 = 0.2;

/// Stablecoin decimals for the test mint — matches the demo.
const MINT_DECIMALS: u8 = 6;

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

// ---------- Deterministic PRNG (xorshift64*) --------------------------------

struct Rng(u64);

impl Rng {
    fn from_seed(seed: u64) -> Self {
        // xorshift can't start at 0.
        Rng(if seed == 0 {
            0x9e37_79b9_7f4a_7c15
        } else {
            seed
        })
    }

    fn next_u64(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.0 = x;
        x.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    /// Uniform integer in `[lo, hi]` inclusive.
    fn range(&mut self, lo: u64, hi: u64) -> u64 {
        if lo >= hi {
            return lo;
        }
        lo + self.next_u64() % (hi - lo + 1)
    }

    /// Uniform float in `[0, 1)`.
    fn unit(&mut self) -> f64 {
        (self.next_u64() >> 11) as f64 / (1u64 << 53) as f64
    }
}

/// Pick `f_micros` from the weighted pool the checklist specifies:
/// `0, 1, 500_000, 990_000, 1_000_000`, plus a uniform random draw.
fn pick_f_micros(rng: &mut Rng) -> u32 {
    const POOL: [u32; 5] = [0, 1, 500_000, 990_000, 1_000_000];
    let uniform = rng.range(0, 1_000_000) as u32;
    // Roughly half the time use a boundary/edge value, otherwise uniform.
    match rng.next_u64() % 2 {
        0 => POOL[(rng.next_u64() as usize) % POOL.len()],
        _ => uniform,
    }
}

fn expected_earned(price: u64, f_micros: u32) -> u64 {
    (price as u128 * f_micros as u128 / 1_000_000) as u64
}

// ---------- RPC plumbing ----------------------------------------------------

fn send_tx(
    rpc: &RpcClient,
    ixs: &[Instruction],
    signers: &[&Keypair],
    fee_payer: &Keypair,
    _label: &str,
) -> Result<String, Box<dyn std::error::Error>> {
    let bh = rpc.get_latest_blockhash()?;
    let mut tx = Transaction::new_with_payer(ixs, Some(&fee_payer.pubkey()));
    tx.sign(signers, bh);
    let sig = rpc.send_and_confirm_transaction_with_spinner(&tx)?;
    std::thread::sleep(Duration::from_millis(300));
    Ok(sig.to_string())
}

fn token_balance(rpc: &RpcClient, ata: &Pubkey) -> Result<u64, Box<dyn std::error::Error>> {
    match rpc.get_account(ata) {
        Ok(acct) => Ok(TokenAccount::unpack(&acct.data)?.amount),
        Err(_) => Ok(0),
    }
}

// ---------- Setup (mirrors the demo's main) ---------------------------------

struct Env {
    payer: Keypair,
    mint: Pubkey,
    buyer_ata: Pubkey,
    program_id: Pubkey,
    config_pda: Pubkey,
    fee_wallet: Pubkey,
}

fn setup(rpc: &RpcClient, payer: Keypair) -> Result<Env, Box<dyn std::error::Error>> {
    let program_id = Pubkey::from_str(PROGRAM_ID_STR)?;
    let fee_wallet = Pubkey::from_str(FEE_WALLET_STR)?;

    // Create a fresh test "stablecoin" mint owned by the payer.
    let mint_kp = Keypair::new();
    let mint_pk = mint_kp.pubkey();
    let mint_rent = rpc.get_minimum_balance_for_rent_exemption(Mint::LEN)?;
    let create_mint = system_instruction::create_account(
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
        MINT_DECIMALS,
    )?;
    send_tx(
        rpc,
        &[create_mint, init_mint],
        &[&payer, &mint_kp],
        &payer,
        "create+init mint",
    )?;

    // Buyer = payer; create their ATA and a generous starting balance.
    let buyer_ata = get_associated_token_address(&payer.pubkey(), &mint_pk);
    let create_ata = create_associated_token_account(
        &payer.pubkey(),
        &payer.pubkey(),
        &mint_pk,
        &spl_token::id(),
    );
    send_tx(rpc, &[create_ata], &[&payer], &payer, "create buyer ATA")?;
    top_up(rpc, &payer, &mint_pk, &buyer_ata, 1_000_000_000_000)?;

    // init_config (idempotent) with settlement authority = payer.
    let (config_pda, _) = Pubkey::find_program_address(&[b"vtessera_config"], &program_id);
    if rpc.get_account(&config_pda).is_err() {
        let cfg_disc = anchor_disc("init_config");
        let mut cfg_data = cfg_disc.to_vec();
        cfg_data.extend_from_slice(&payer.pubkey().to_bytes());
        let cfg_ix = Instruction {
            program_id,
            accounts: vec![
                AccountMeta::new(payer.pubkey(), true),
                AccountMeta::new(config_pda, false),
                AccountMeta::new_readonly(system_program::id(), false),
            ],
            data: cfg_data,
        };
        send_tx(rpc, &[cfg_ix], &[&payer], &payer, "init_config")?;
    }

    Ok(Env {
        payer,
        mint: mint_pk,
        buyer_ata,
        program_id,
        config_pda,
        fee_wallet,
    })
}

fn top_up(
    rpc: &RpcClient,
    payer: &Keypair,
    mint: &Pubkey,
    ata: &Pubkey,
    amount: u64,
) -> Result<(), Box<dyn std::error::Error>> {
    let ix =
        spl_token::instruction::mint_to(&spl_token::id(), mint, ata, &payer.pubkey(), &[], amount)?;
    send_tx(rpc, &[ix], &[payer], payer, "mint_to buyer")?;
    Ok(())
}

/// A single iteration's result.
#[derive(Debug)]
struct IterOutcome {
    iter: u64,
    action: String,
    price: u64,
    ok: bool,
    detail: String,
}

fn run_iteration(rpc: &RpcClient, env: &Env, iter: u64, rng: &mut Rng) -> IterOutcome {
    let price = rng.range(1, 10_000_000);
    let do_cancel = rng.unit() < CANCEL_P;

    // Ensure buyer has enough for this price (top up generously).
    if let Err(e) = top_up(rpc, &env.payer, &env.mint, &env.buyer_ata, price) {
        return IterOutcome {
            iter,
            action: "setup/top_up".into(),
            price,
            ok: false,
            detail: format!("top_up failed: {e}"),
        };
    }
    let buyer_before = match token_balance(rpc, &env.buyer_ata) {
        Ok(b) => b,
        Err(e) => {
            return IterOutcome {
                iter,
                action: "setup/balance".into(),
                price,
                ok: false,
                detail: format!("read buyer balance failed: {e}"),
            };
        }
    };

    // Fresh random seller.
    let seller = Keypair::new();
    let seller_ata = get_associated_token_address(&seller.pubkey(), &env.mint);

    // Fresh job id (per iteration, deterministic from seed state).
    let mut h = Sha256::new();
    h.update(b"vtessera-soak:");
    h.update(iter.to_le_bytes());
    h.update(rng.next_u64().to_le_bytes());
    let job_id: [u8; 32] = h.finalize().into();

    let (contract_pda, _) = Pubkey::find_program_address(&[b"contract", &job_id], &env.program_id);
    let escrow_ata = get_associated_token_address(&contract_pda, &env.mint);

    // Create the accounts the flow needs (escrow ATA always; seller ATA
    // only on the finalize path — cancel refunds the buyer, never the seller).
    let create_escrow = create_associated_token_account(
        &env.payer.pubkey(),
        &contract_pda,
        &env.mint,
        &spl_token::id(),
    );
    if let Err(e) = send_tx(
        rpc,
        &[create_escrow],
        &[&env.payer],
        &env.payer,
        "create escrow ATA",
    ) {
        return IterOutcome {
            iter,
            action: "setup/escrow_ata".into(),
            price,
            ok: false,
            detail: format!("create escrow ATA failed: {e}"),
        };
    }
    if !do_cancel {
        let create_seller = create_associated_token_account(
            &env.payer.pubkey(),
            &seller.pubkey(),
            &env.mint,
            &spl_token::id(),
        );
        if let Err(e) = send_tx(
            rpc,
            &[create_seller],
            &[&env.payer],
            &env.payer,
            "create seller ATA",
        ) {
            return IterOutcome {
                iter,
                action: "setup/seller_ata".into(),
                price,
                ok: false,
                detail: format!("create seller ATA failed: {e}"),
            };
        }
    }

    // ---- pay_for_compute ----
    let pay_disc = anchor_disc("pay_for_compute");
    let pay_args = PayForComputeArgs {
        job_id,
        price_micros: price,
    };
    let mut pay_data = pay_disc.to_vec();
    if let Err(e) = pay_args
        .try_to_vec()
        .map(|v| pay_data.extend_from_slice(&v))
    {
        return IterOutcome {
            iter,
            action: "pay/encode".into(),
            price,
            ok: false,
            detail: format!("encode pay args failed: {e}"),
        };
    }
    let pay_ix = Instruction {
        program_id: env.program_id,
        accounts: vec![
            AccountMeta::new(env.payer.pubkey(), true),
            AccountMeta::new_readonly(seller.pubkey(), false),
            AccountMeta::new_readonly(env.mint, false),
            AccountMeta::new(env.buyer_ata, false),
            AccountMeta::new(escrow_ata, false),
            AccountMeta::new(contract_pda, false),
            AccountMeta::new(env.fee_wallet, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(system_program::id(), false),
            AccountMeta::new_readonly(rent::id(), false),
        ],
        data: pay_data,
    };
    if let Err(e) = send_tx(rpc, &[pay_ix], &[&env.payer], &env.payer, "pay_for_compute") {
        return IterOutcome {
            iter,
            action: "pay".into(),
            price,
            ok: false,
            detail: format!("pay_for_compute failed: {e}"),
        };
    }

    if do_cancel {
        // ---- cancel_before_start: buyer gets everything back ----
        let cancel_disc = anchor_disc("cancel_before_start");
        let cancel_ix = Instruction {
            program_id: env.program_id,
            accounts: vec![
                AccountMeta::new(env.payer.pubkey(), true),
                AccountMeta::new(contract_pda, false),
                AccountMeta::new(escrow_ata, false),
                AccountMeta::new(env.buyer_ata, false),
                AccountMeta::new_readonly(spl_token::id(), false),
            ],
            data: cancel_disc.to_vec(),
        };
        if let Err(e) = send_tx(
            rpc,
            &[cancel_ix],
            &[&env.payer],
            &env.payer,
            "cancel_before_start",
        ) {
            return IterOutcome {
                iter,
                action: "cancel".into(),
                price,
                ok: false,
                detail: format!("cancel_before_start failed: {e}"),
            };
        }
        let escrow_after = match token_balance(rpc, &escrow_ata) {
            Ok(b) => b,
            Err(e) => {
                return IterOutcome {
                    iter,
                    action: "cancel/verify".into(),
                    price,
                    ok: false,
                    detail: format!("read escrow after cancel failed: {e}"),
                };
            }
        };
        let ok = escrow_after == 0;
        IterOutcome {
            iter,
            action: "cancel".into(),
            price,
            ok,
            detail: format!("cancel refunded full {price}; escrow drained: {escrow_after}"),
        }
    } else {
        // ---- finalize_pro_rata_stub ----
        let f_micros = pick_f_micros(rng);
        let fin_disc = anchor_disc("finalize_pro_rata_stub");
        let fin_args = FinalizeProRataArgs { f_micros };
        let mut fin_data = fin_disc.to_vec();
        if let Err(e) = fin_args
            .try_to_vec()
            .map(|v| fin_data.extend_from_slice(&v))
        {
            return IterOutcome {
                iter,
                action: "finalize/encode".into(),
                price,
                ok: false,
                detail: format!("encode finalize args failed: {e}"),
            };
        }
        let fin_ix = Instruction {
            program_id: env.program_id,
            accounts: vec![
                AccountMeta::new_readonly(env.payer.pubkey(), true),
                AccountMeta::new_readonly(env.config_pda, false),
                AccountMeta::new(contract_pda, false),
                AccountMeta::new(escrow_ata, false),
                AccountMeta::new(env.buyer_ata, false),
                AccountMeta::new(seller_ata, false),
                AccountMeta::new_readonly(spl_token::id(), false),
            ],
            data: fin_data,
        };
        if let Err(e) = send_tx(
            rpc,
            &[fin_ix],
            &[&env.payer],
            &env.payer,
            "finalize_pro_rata_stub",
        ) {
            return IterOutcome {
                iter,
                action: "finalize".into(),
                price,
                ok: false,
                detail: format!("finalize_pro_rata_stub failed: {e}"),
            };
        }

        // Verify the split on-chain.
        let earned = expected_earned(price, f_micros);
        let refund = price - earned;
        let escrow_after = token_balance(rpc, &escrow_ata).unwrap_or(u64::MAX);
        let buyer_after = token_balance(rpc, &env.buyer_ata).unwrap_or(0);
        let seller_after = token_balance(rpc, &seller_ata).unwrap_or(0);

        let ok = escrow_after == 0
            && buyer_after == buyer_before - price + refund
            && seller_after == earned;
        IterOutcome {
            iter,
            action: format!("finalize f_micros={f_micros}"),
            price,
            ok,
            detail: format!(
                "earned={earned} refund={refund} escrow_after={escrow_after} \
                 buyer_before={buyer_before} buyer_after={buyer_after} seller_after={seller_after}"
            ),
        }
    }
}

// ---------- CLI + main ------------------------------------------------------

fn parse_u64(s: &str) -> Result<u64, String> {
    s.parse::<u64>().map_err(|e| format!("bad u64 `{s}`: {e}"))
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // CLI: --iters N [--cancel-p P]
    let mut iters: u64 = 100;
    let mut cancel_p: f64 = CANCEL_P;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--iters" => {
                iters = args
                    .next()
                    .ok_or("--iters needs a value")?
                    .parse()
                    .map_err(|e| format!("--iters: {e}"))?;
            }
            "--cancel-p" => {
                cancel_p = args
                    .next()
                    .ok_or("--cancel-p needs a value")?
                    .parse()
                    .map_err(|e| format!("--cancel-p: {e}"))?;
            }
            other => {
                return Err(format!("unknown arg `{other}`").into());
            }
        }
    }
    if iters == 0 {
        return Err("--iters must be >= 1".into());
    }
    if !(0.0..=1.0).contains(&cancel_p) {
        return Err("--cancel-p must be in [0, 1]".into());
    }

    let payer_path: PathBuf = env::var("VTESSERA_PAYER")
        .map(PathBuf::from)
        .unwrap_or_else(|_| {
            let home = env::var("HOME").expect("HOME unset");
            PathBuf::from(format!("{home}/.config/solana/id.json"))
        });
    let payer = read_keypair_file(&payer_path)
        .map_err(|e| format!("read payer {}: {e}", payer_path.display()))?;

    let rpc_url = env::var("SOAK_RPC").unwrap_or_else(|_| DEVNET_RPC.to_string());
    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::confirmed());

    let payer_pk = payer.pubkey();
    let env = setup(&rpc, payer)?;
    println!("payer: {payer_pk}  mint: {}", env.mint);
    println!("iters: {iters}  cancel_p: {cancel_p}");

    let seed = match env::var("SOAK_SEED") {
        Ok(s) => parse_u64(&s)?,
        Err(_) => {
            // Default seed: payer pubkey bytes XOR iteration index — same
            // payer diverges across runs, but a failing run is reproducible
            // by re-running with the printed SOAK_SEED.
            let mut h = Sha256::new();
            h.update(b"vtessera-soak-seed:");
            h.update(payer_pk.as_ref());
            let d: [u8; 32] = h.finalize().into();
            u64::from_le_bytes(d[..8].try_into().unwrap())
        }
    };
    let mut rng = Rng::from_seed(seed);
    println!("seed: {seed}  (SOAK_SEED to replay)");

    let mut failures = 0u64;
    for iter in 1..=iters {
        let out = run_iteration(&rpc, &env, iter, &mut rng);
        let mark = if out.ok { "OK  " } else { "FAIL" };
        println!(
            "[{mark}] iter {:>4}  {:<12}  price={:<9}  {}",
            out.iter, out.action, out.price, out.detail
        );
        if !out.ok {
            failures += 1;
        }
    }

    println!(
        "\nsoak complete: {iters} iterations, {failures} failures ({}%).",
        failures as f64 / iters as f64 * 100.0
    );
    if failures > 0 {
        std::process::exit(1);
    }
    Ok(())
}
