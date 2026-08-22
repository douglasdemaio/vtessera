---
name: escrow-program
description: Build, test, and deploy the vtessera-escrow Anchor program (programs/vtessera-escrow) on Solana devnet, and run its adversarial/fuzz suite. Use for ANY change under programs/ or tests/adversarial, or when the user mentions Anchor, the escrow, PDAs, program deploys, IDL, or the reproducible-build (solana-verify) gate. On-chain code is high-stakes — always run the adversarial suite before a deploy.
---

# vtessera-escrow (Anchor program)

One Solana Anchor program: buyer's EURC/USDC enters a program-owned escrow
PDA; on finalize it splits by completion fraction `f` — earned slice to the
seller in the same stablecoin mint, remainder refunded to the buyer. Flat
protocol fee: 100,000 lamports SOL. There is no Vtessera token.

Program ID (devnet + localnet):
`6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`.

## Layout quirks — read before running anything

- The Anchor root is `programs/` (NOT the repo root): `Anchor.toml`,
  its own `Cargo.toml` and `Cargo.lock` live there. Run all `anchor`
  commands from `programs/`.
- The program tree is **excluded from the host workspace** because it pins
  the Solana 1.18 SDK (curve25519-dalek 3) which conflicts with the host
  crates' ed25519-dalek 2. Root-level cargo commands never touch it.
- `Anchor.toml` lists `members = ["vtessera-escrow"]` explicitly — Anchor
  0.30 silently finds nothing otherwise. Keep it listed.
- Provider defaults: cluster `devnet`, wallet `~/.config/solana/id.json`.

## Build / test / deploy

```bash
cd programs
anchor build                    # or: cargo build-sbf
anchor test                     # ts-mocha tests (needs yarn deps installed)
anchor deploy --provider.cluster devnet
```

Deploys cost devnet SOL from the provider wallet; `solana airdrop 2` if
short. **Never deploy to mainnet** — mainnet is deferred behind
MAINNET-CHECKLIST.md.

## Adversarial suite — run before any deploy

`tests/adversarial/` (own Cargo.lock, excluded from workspace):

```bash
cd tests/adversarial
cargo test
```

A change to instruction handlers, account constraints, or the split math
without a corresponding adversarial test is a review flag, not a nit.

## Reproducible build gate

CI's `reproducible-build.yml` verifies the deployed program with
`solana-verify` — the on-chain bytes must match a clean build of the
committed source. If you change program code, the deployed devnet program
and the repo drift apart until the next deploy; say so in the PR.

## After changing the program interface

The IDL consumed by clients changes too: rebuild (`anchor build`
regenerates the IDL) and check `crates/devnet-demo`, `crates/x402-client`,
and `crates/settlement` for instruction/account layout assumptions that
must move in lockstep.
