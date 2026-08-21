# vtessera-escrow — Anchor program (Module 4)

The escrow leg of Vtessera. One on-chain program holds buyer
stablecoin in a program-owned PDA, transfers a flat SOL fee to the
protocol fee wallet, then on finalize splits the escrow by the
completion fraction `f` produced by the settlement crate (Module 3):

- `f × price` → paid directly to the seller's ATA in the same
  stablecoin mint (no swap, no burn).
- `(1 − f) × price` → refund to buyer in the original stablecoin.

**No human ever holds the funds.** See `ROADMAP.md` §4.

## Fee and Config

The fee and settlement config live in a single on-chain `Config` account
(seed `CONFIG_SEED`), created once by
`init_config(settlement_authority, fee_wallet, fee_lamports)` right
after deploy. `Config` is **immutable** after `init_config` — there are
no update or governance instructions.

| Field | Value |
| ----- | ----- |
| `settlement_authority` | The operator's key, pinned at deploy; signs `finalize_pro_rata` (a functional gate so no arbitrary caller can finalize with a fabricated `f`) |
| `fee_wallet` | `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh` |
| `fee_lamports` | `100_000` (0.0001 SOL) |
| `bump` | PDA bump for the `Config` account |

The flat fee is charged on **every agent↔node transaction** — on
`pay_for_compute` (buyer), `finalize_pro_rata` (settlement authority),
and `cancel_before_start` (buyer) — even when a contract never
completes. It is skipped when `fee_lamports == 0`; `init_config` is not
charged (bootstrap).

## Why this crate is outside the host workspace

Anchor programs build under the **Solana BPF toolchain**
(`anchor build` / `cargo build-sbf`), not the host's Rust toolchain.
Including this crate in the workspace would force every plain
`cargo build` to drag in the BPF dep tree and a pinned Solana version.
The root `Cargo.toml` therefore lists this crate in `workspace.exclude`,
and the v0 daemon's CI doesn't touch it.

To build:

```
# Install Anchor: https://www.anchor-lang.com/docs/installation
anchor build
```

## Reproducible build + verification (MAINNET-CHECKLIST §5)

The program is built deterministically with
[`solana-verify`](https://github.com/solana-foundation/solana-verifiable-build)
inside a pinned Docker image, so anyone can prove the on-chain bytecode
matches this source. CI runs this on every change under `programs/`
(`.github/workflows/reproducible-build.yml`) and again as a gate at
release (`release.yml`): **two clean builds whose `.so` SHA-256 must
match**, using `solana-verify` 0.5.1 and the image pinned to the
`Cargo.lock` solana-program version (1.18.26).

Locally (requires Docker):

```
cargo install solana-verify --version 0.5.1 --locked
cd programs
solana-verify build
sha256sum target/deploy/vtessera_escrow.so
```

**`programs/Cargo.lock` must stay at lockfile version 3.** The pinned
build image for Solana 1.18.26 ships Rust 1.75, which cannot parse
version-4 lockfiles; a newer `cargo` will rewrite the header on the
next `cargo generate-lockfile`/`update`. If that happens, change the
header back to `version = 3` (registry-only lockfiles are identical in
v3 and v4) and re-run the job.

**Every crate in the lockfile must be parseable and buildable with
Rust 1.75.** Cargo 1.75 rejects any crate published with
`edition = "2024"` (`feature edition2024 is required`) — including
crates that never compile on this platform, because `cargo build-sbf`
runs `cargo metadata` over the whole graph — and the pinned `rustc`
1.75 cannot compile crates that declare a higher MSRV. The lockfile
pins the transitive downgrades that keep the tree 1.75-compatible; a
`cargo update` that floats them back up will break the build, so
preserve them:

```
blake3            = 1.8.2      (>= 1.8.3 needs edition 2024; pulls digest 0.10, not 0.11)
borsh             = 1.5.7      (1.6+/1.7 need Rust 1.77; ^1.5 satisfies every spl-* pin)
jobserver         = 0.1.32     (0.1.34 pulls getrandom 0.3.4 -> wasip2 -> wit-bindgen 0.57)
proc-macro-crate  = 3.4.0      (3.5.0 pulls toml_edit 0.25 + toml_parser 1.1, both edition 2024)
toml_edit         = 0.23.5     (0.23.9+ declares Rust 1.76)
toml_parser       = 1.0.2      (1.1.x needs edition 2024; 1.0.4+ declares Rust 1.76)
toml_datetime     = 0.7.1      (0.7.2+ declares Rust 1.76)
indexmap          = 2.11.4     (2.12+ declares Rust 1.82; 2.14.0 pulls hashbrown 0.17)
hashbrown         = 0.16.1     (0.17.1 is edition 2024)
rayon             = 1.10.0     (1.11+ declares Rust 1.80)
rayon-core        = 1.12.1     (1.13.0 declares Rust 1.80)
unicode-segmentation = 1.12.0  (1.13.3 declares Rust 1.85)
zeroize_derive    = 1.4.3      (1.5.0 is edition 2024)
```

`cargo update` picks newer semver-compatible versions, so if a
dependency bump demands one of these, verify with
`cargo +1.75.0 metadata --locked` before committing.

The reproducible SHA-256 of the deployed program is committed at
`DEPLOYED_SHA256.txt` (§5.3 — filled at mainnet deploy with the deploy
date, program ID, and commit). Anyone can verify the deployed program
against this repo:

```
solana-verify verify-from-repo https://github.com/douglasdemaio/vtessera \
  --program-id <PROGRAM_ID> --url <devnet|mainnet-beta> \
  --commit-hash <DEPLOYED_COMMIT> --library-name vtessera_escrow \
  --mount-path programs --bpf
```

(`--bpf` matches how Anchor-built programs are hashed on-chain; the
`programs/` mount path points at the workspace whose `Cargo.toml`
contains the `vtessera_escrow` library.)

## Program ID

Devnet: **`6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`**

Generated by `anchor build` / `cargo build-sbf` — the keypair lives at
`programs/target/deploy/vtessera_escrow-keypair.json`. The same address
is declared in `src/lib.rs` (`declare_id!`) and `programs/Anchor.toml`.
On first mainnet deploy a fresh keypair is generated and these three
sites get updated together.

## Forked from

[`douglasdemaio/forkit`](https://github.com/douglasdemaio/forkit) — the
recommended escrow starting point for Vtessera. This skeleton diverges
on the pro-rata release path; the basic "deposit → release" structure
stays close to forkit.

## Status

The program compiles under Anchor 0.30 and is **live on Solana devnet**
at `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`. The production
path is `finalize_pro_rata`: the seller is paid in the contract's
stablecoin mint, the buyer is refunded in the same mint, and the SOL
fee is charged from the settlement authority. The old devnet stub is
deleted — there is no swap and no burn. `Config` is immutable after
`init_config`, and the settlement authority is the operator's key pinned
at deploy — see `ROADMAP.md` §4d. Full end-to-end pay→run→settle→split
flow exercised via `crates/devnet-demo` soak runner (20+ successful
finalizations, 0% failure rate). See `tests/adversarial/` for the fuzz +
adversarial test suite.
