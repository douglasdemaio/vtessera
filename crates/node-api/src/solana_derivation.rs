//! Minimal, self-contained Solana address derivation used by the node's
//! on-chain x402 payment verifier.
//!
//! Implements exactly the parts of solana-sdk / spl-token that the verifier
//! needs (program-derived-address search and associated-token-account
//! derivation) *without* pulling solana-sdk into the host workspace, whose
//! dependency tree must stay on ed25519-dalek 2 (see `Cargo.toml`). The
//! algorithms below are transcribed from solana-program 1.18
//! (`Pubkey::find_program_address`/`create_program_address`) and
//! spl-associated-token-account 2.3 (`get_associated_token_address`), and
//! must match them bit-for-bit — a mismatch that *accepts* the wrong PDA is
//! a payment bug, so the unit tests pin against vectors produced by the real
//! solana-sdk.
//!
//! What a "program-derived address" is: `create_program_address` hashes the
//! seeds plus the program id, then rejects the result if it happens to land
//! on the ed25519 (Twisted Edwards) curve — PDAs by construction must be off
//! curve. `find_program_address` searches bump seeds 255..=0 for the first
//! hash that is off curve.

use curve25519_dalek::edwards::CompressedEdwardsY;
use sha2::{Digest, Sha256};

/// Trailing marker bytes appended to the seed hash in `create_program_address`.
const PDA_MARKER: &[u8; 21] = b"ProgramDerivedAddress";

const MAX_SEEDS: usize = 16;
const MAX_SEED_LEN: usize = 32;

/// `spl_token::id()` — the legacy SPL Token program.
const SPL_TOKEN_PROGRAM: [u8; 32] = [
    6, 221, 246, 225, 215, 101, 161, 147, 217, 203, 225, 70, 206, 235, 121, 172, 28, 180, 133, 237,
    95, 91, 55, 145, 58, 140, 245, 133, 126, 255, 0, 169,
];

/// `spl_associated_token_account::id()` — the ATA program.
const ATA_PROGRAM: [u8; 32] = [
    140, 151, 37, 143, 78, 36, 137, 241, 187, 61, 16, 41, 20, 142, 13, 131, 11, 90, 19, 153, 218, 255,
    16, 132, 4, 142, 123, 216, 219, 233, 248, 89,
];

/// True if `sha256(seeds || program_id || PDA_MARKER)` does **not** decompress
/// to a valid Edwards point — i.e. the resulting bytes are a valid PDA.
/// Mirrors `solana_program::pubkey::bytes_are_curve_point` inverted by
/// `create_program_address`.
fn hash_is_off_curve(seeds: &[&[u8]], program_id: &[u8; 32]) -> bool {
    let mut h = Sha256::new();
    for s in seeds {
        h.update(*s);
    }
    h.update(program_id);
    h.update(PDA_MARKER);
    let digest = h.finalize();
    let mut bytes = [0u8; 32];
    bytes.copy_from_slice(&digest);
    CompressedEdwardsY(bytes).decompress().is_none()
}

/// The raw derived PDA bytes for a set of seeds, or `None` if the result
/// lands on the curve (invalid). Mirrors `Pubkey::create_program_address`.
fn create_program_address(seeds: &[&[u8]], program_id: &[u8; 32]) -> Option<[u8; 32]> {
    if seeds.len() > MAX_SEEDS || seeds.iter().any(|s| s.len() > MAX_SEED_LEN) {
        return None;
    }
    if !hash_is_off_curve(seeds, program_id) {
        return None;
    }
    let mut h = Sha256::new();
    for s in seeds {
        h.update(*s);
    }
    h.update(program_id);
    h.update(PDA_MARKER);
    let digest = h.finalize();
    let mut out = [0u8; 32];
    out.copy_from_slice(&digest);
    Some(out)
}

/// Search bump seeds 255..=0 for the first off-curve address. Mirrors
/// `Pubkey::find_program_address`. Returns `(pda, bump)`.
pub fn find_program_address(seeds: &[&[u8]], program_id: &[u8; 32]) -> Option<([u8; 32], u8)> {
    let mut bump: u8 = 255;
    loop {
        let bump_seed = [bump];
        let seeds_with_bump: Vec<&[u8]> = seeds
            .iter()
            .copied()
            .chain(std::iter::once(bump_seed.as_slice()))
            .collect();
        if let Some(pda) = create_program_address(&seeds_with_bump, program_id) {
            return Some((pda, bump));
        }
        if bump == 0 {
            return None;
        }
        bump -= 1;
    }
}

/// The associated token account for `owner` of `mint`, via the legacy SPL
/// Token program. Mirrors `spl_associated_token_account::get_associated_token_address`.
pub fn get_associated_token_address(owner: &[u8; 32], mint: &[u8; 32]) -> [u8; 32] {
    find_program_address(&[owner, &SPL_TOKEN_PROGRAM, mint], &ATA_PROGRAM)
        .map(|(a, _)| a)
        .unwrap_or([0u8; 32])
}

/// The escrow contract PDA for a job. Mirrors the escrow program's
/// `CONTRACT_SEED` derivation (`programs/vtessera-escrow`, seed `b"contract"`).
pub fn contract_pda(job_id: &[u8; 32], program_id: &[u8; 32]) -> Option<([u8; 32], u8)> {
    find_program_address(&[b"contract", job_id], program_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    // Pubkey strings and expected vectors below were produced by the real
    // solana-sdk (standalone `veccheck` against solana-sdk 1.18 + spl-token
    // 4 + spl-associated-token-account 2.3). They pin the vendored derivation
    // exactly to Solana's. Bytes are the decoded base58 of each address.
    fn escrow_program() -> [u8; 32] {
        [85,33,75,86,190,213,51,181,150,15,160,251,3,216,231,4,251,108,235,172,165,175,110,134,44,242,212,115,49,38,126,9]
    }
    fn usdc_mint() -> [u8; 32] {
        [59,68,44,179,145,33,87,241,58,147,61,1,52,40,45,3,43,95,254,205,1,162,219,241,183,121,6,8,223,0,46,167]
    }
    fn buyer() -> [u8; 32] {
        [69,65,206,234,127,223,72,124,40,27,21,142,229,229,236,188,213,45,163,33,226,113,172,50,78,130,241,1,51,244,237,134]
    }
    fn exp_buyer_ata() -> [u8; 32] {
        [58,56,53,191,165,10,140,43,196,2,242,206,125,59,36,179,99,191,56,103,196,199,15,63,175,241,229,68,232,250,191,170]
    }
    fn exp_contract() -> [u8; 32] {
        [227,225,181,18,35,209,168,73,158,80,161,31,117,84,106,207,14,198,114,172,174,249,163,81,248,90,153,142,180,88,206,135]
    }
    fn exp_escrow_ata() -> [u8; 32] {
        [207,180,84,129,115,41,124,180,222,217,94,210,159,12,204,249,184,88,100,75,229,20,110,79,239,169,178,160,86,231,84,170]
    }

    #[test]
    fn buyer_ata_matches_solana() {
        let ata = get_associated_token_address(&buyer(), &usdc_mint());
        assert_eq!(ata, exp_buyer_ata());
    }

    #[test]
    fn contract_pda_matches_solana() {
        let job = [0u8; 32];
        let (pda, bump) = contract_pda(&job, &escrow_program()).expect("valid pda");
        assert_eq!(pda, exp_contract());
        assert_eq!(bump, 255);
    }

    #[test]
    fn escrow_ata_matches_solana() {
        let job = [0u8; 32];
        let (pda, _) = contract_pda(&job, &escrow_program()).unwrap();
        let ata = get_associated_token_address(&pda, &usdc_mint());
        assert_eq!(ata, exp_escrow_ata());
    }
}
