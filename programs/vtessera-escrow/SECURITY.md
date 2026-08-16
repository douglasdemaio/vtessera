# Vtessera Escrow — Security Policy

Program: `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` (devnet)
On-chain security.txt: Program Metadata seed `security` (metadata PDA
`42YbtUqT4w2u2rECYvL5daaaZM7ANkqCsqXG6sH8wvCg`)
Source: <https://github.com/douglasdemaio/vtessera>

## Reporting a vulnerability

Email the project owner at `douglasdemaio@gmail.com`. Include the
program ID, the instruction involved, and a minimal repro. We will
acknowledge within 48h and triage against the trust model below.

## Scope

One Anchor program (`programs/vtessera-escrow`). It escrows a buyer's
stablecoin (EURC or USDC, whichever mint the buyer's offer specifies)
in a program-owned PDA and distributes it by on-chain rules:

- `init_config` — creates the single immutable `Config` account
  (settlement authority, fee wallet, fee lamports).
- `pay_for_compute(job_id, price_micros)` — deposits price into the
  escrow PDA; charges the flat SOL fee (default 100,000 lamports).
- `finalize_pro_rata(f_micros)` — pays seller `f × price` and refunds
  buyer `(1 − f) × price`, both in the contract's mint; charges fee.
- `cancel_before_start` — buyer reclaims full escrow at `f = 0` before
  finalize; charges fee (per-transaction fee even on never-completed
  contracts).

There is **no swap, no price oracle, no burn, and no governance token**.
The program never mints or holds a token of its own; stablecoin flows
only between the buyer's ATA, the escrow PDA, and the seller's ATA.

## Trust model

**Assumed honest:**
- **Settlement authority** — the single key pinned in `Config`. It
  signs `finalize_pro_rata` and is the only party that can pick `f`.
  `Config` is written once and has no update instructions. A dishonest
  settlement authority can finalize any escrow at any `f` (e.g. refund
  the buyer and pay the seller nothing) or simply never finalize.
  This is a deliberate single-operator design; mainnet plan pins a
  Squads vault as the authority (MAINNET-CHECKLIST §3).
- **Upgrade authority** — until the program is made immutable (mainnet
  decision: Option A, `set-upgrade-authority --final`), whoever holds
  this keypair can replace the on-chain bytecode with anything. On
  devnet this key is the throwaway CI key for automation isolation, but
  the *deploy* key is the operator's laptop keypair; it must never
  enter CI.

**Not defended against (accepted risk):**
- **Node off-chain metering** — `f` is produced off-chain by the
  settlement crate and signed by the settlement authority. A compromised
  node cannot finalize anything by itself; it can only propose a value
  that the authority signs. Metering fraud is out of the program's
  trust boundary.
- **Stablecoin issuer freeze/blacklist** — Circle's USDC or the EURC
  issuer may freeze or seize a specific address per their own rules;
  the program cannot prevent this and does not try.
- **Validator censorship / chain reorgs** — normal Solana assumptions.
- **Deploy-key compromise before `--final`** — pre-freeze, the operator
  keypair is an implicit custodian. The immutable freeze (§3.3) is the
  mitigation.

## Attack surface (what the adversarial suite pins)

Each case asserts the exact error code (drift-guard test pins codes
6000–6007 in `lib.rs`); rejecting for the wrong reason is a bug.
Coverage in `tests/adversarial/tests/adversarial.rs`:

- `pay_for_compute(price_micros = 0)` → `ZeroPrice`
- duplicate `job_id` → second pay fails (PDA already exists)
- `finalize_pro_rata(f_micros > 1_000_000)` → `FractionOutOfRange`
- double `finalize_pro_rata` → `AlreadyFinal`
- buyer ATA of the wrong mint → `WrongMint`
- seller ATA owned by someone other than `contract.seller_payout` →
  `WrongOwner`
- `finalize_pro_rata` signed by a non-authority → `NotSettlementAuthority`
- `cancel_before_start` by a non-buyer → signer check
- `cancel_before_start` after finalize → `AlreadyFinal`
- fee charged on pay / finalize / cancel, `fee_lamports = 0` disables it
- math: `price = u64::MAX, f = 999_999` (no silent overflow) and
  `price = 1, f = 1` (consistent rounding) — u128 checked arithmetic
- config immutability: no update instruction exists

## Known limitations

- **No on-chain timeout.** If a seller starts a job but never finishes
  and the settlement authority never finalizes, the buyer can still
  reclaim the escrow at any time with `cancel_before_start` (full
  refund, fee paid). There is no automatic trigger.
- **Single-operator finalize.** A lost settlement authority means
  escrows can no longer finalize (buyers can still cancel).
- **Immutable config.** Changing the fee or the settlement authority
  requires a redeploy to a new program ID.
- **SPL token program only.** Token-2022 mints (or any mint that needs
  the Token-2022 program) are unsupported.
- **`init_config` front-running.** The config PDA is derivable from the
  program ID, so on a fresh program ID a griefer could call
  `init_config` first. Mitigation: initialize in the same block as
  deploy. Worst case on mainnet is a DoS of finalize, not fund theft.

## Deploy procedure

Follow MAINNET-CHECKLIST §3.3 (immutable runbook): deploy the
reproducible `.so` (§5), `init_config` with the settlement authority,
run one small end-to-end flow, then
`solana program set-upgrade-authority <PROGRAM_ID> --final` and verify
`Authority: None`. `--final` is irreversible.

## Host-side hardening (node software)

The program is one side; the software that meters and serves jobs is the
other. v0 (`vtesserad`) is built with `#![forbid(unsafe_code)]`, opens **no
sockets** in its default build (pinned by `tests/no_socket.rs`; unit
restriction `RestrictAddressFamilies=AF_UNIX` in `BUILD.md` §5), and ships a
hardened systemd unit (`DynamicUser=yes`, `ProtectSystem=strict`,
`NoNewPrivileges`, empty capability set — `packaging/vtesserad.service`) plus
an AppArmor profile (`packaging/vtessera.apparmor`) that denies `/dev`,
`/sys`, and `/proc` writes. The consent model that gates job acceptance lives
in `docs/CONSENT.md` (two consent gates, no autostart, one-action stop).
Any new privileged component (executor, dispatch API) must re-run
`systemd-analyze security` before it ships (ROADMAP §5).

## Review / verification

- Build: `anchor build` (or the CI-equivalent pinned toolchain).
- Tests: program unit tests + drift guard; `tests/adversarial/`
  standalone LiteSVM suite (`cargo test --locked`).
- Soak: hourly devnet soak (`.github/workflows/soak-devnet.yml`).
- Host invariants: `tests/no_socket.rs` (v0 metering opens no sockets);
  `systemd-analyze security` on the unit in CI (best-effort).
- Reproducibility: `solana-verify build` (0.5.1) — two clean builds in
  CI (`.github/workflows/reproducible-build.yml`, plus a `release.yml`
  gate) whose `.so` SHAs must match; SHA committed to
  `DEPLOYED_SHA256.txt` (§5).
