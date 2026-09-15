# Vtessera Escrow — Security Policy

Program: `D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn` (devnet)
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

- `init_config(config_authority, fee_wallet, fee_lamports)` — creates the
  single `Config` account (config authority, fee wallet, fee lamports),
  kept as a default/off-chain reference only.
- `update_config(…)` — the config authority can rotate the reference
  config without redeploying. `Config` neither gates finalize nor gates
  the fee: every contract commits its own fee at `pay_for_compute`.
- `pay_for_compute(job_id, price_micros, settlement_authority)` — deposits
  price into the escrow PDA and records the **per-contract settlement
  authority** (the key allowed to finalize this job) **and the
  per-contract fee** (wallet validated against `DEFAULT_FEE_WALLET`,
  amount pinned to `DEFAULT_FEE_LAMPORTS`); charges the flat SOL fee
  (100,000 lamports).
- `finalize_pro_rata(f_micros)` — pays seller `f × price` and refunds
  buyer `(1 − f) × price`, both in the contract's mint; charges the fee
  recorded on the contract. Signed by the **contract's recorded**
  settlement authority.
- `cancel_before_start` — buyer reclaims full escrow at `f = 0` before
  finalize; charges the contract's recorded fee (per-transaction fee even
  on never-completed contracts).

There is **no swap, no price oracle, no burn, and no governance token**.
The program never mints or holds a token of its own; stablecoin flows
only between the buyer's ATA, the escrow PDA, and the seller's ATA.

## Trust model

**Assumed honest:**
- **Per-contract settlement authority** — recorded in the `Contract` by
  `pay_for_compute`. It signs `finalize_pro_rata` and is the only party
  that can pick `f` for that job. A dishonest authority can finalize its
  contract at any `f` (e.g. refund the buyer and pay the seller nothing)
  or simply never finalize. This is a deliberate design: the buyer names
  the authority at payment (in the standard flow, the node's offer
  advertises who settles), so a seller should verify the recorded
  authority before running a paid job. Because the authority is
  per-contract, it is never coupled to who initialized the shared
  `Config` account — devnet deployments whose config was seized by a
  throwaway CI key still settle normally. Mainnet plan pins a Squads
  vault as the recorded authority (MAINNET-CHECKLIST §3).
- `Config` authority (`Config.settlement_authority`) — only gates
  `update_config` (the reference fee config). It has no power over
  escrowed funds and no power over the fee any contract charges (each
  contract commits its own fee at `pay_for_compute`).
- **Upgrade authority** — until the program is made immutable (mainnet
  decision: Option A, `set-upgrade-authority --final`), whoever holds
  this keypair can replace the on-chain bytecode with anything. On
  devnet this key is the throwaway CI key for automation isolation, but
  the *deploy* key is the operator's laptop keypair; it must never
  enter CI.

**Not defended against (accepted risk):**
- **Node off-chain metering** — `f` is produced off-chain by the
  settlement crate and signed by the contract's settlement authority. A
  compromised node cannot finalize anything by itself; it can only
  propose a value that the authority signs. Metering fraud is out of the
  program's trust boundary.
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
- `finalize_pro_rata` signed by a key that is not the contract's
  recorded authority → `NotSettlementAuthority`
- `pay_for_compute` records an arbitrary per-contract authority; only
  that key can finalize the contract
- config rotation to a throwaway key does **not** revoke an in-flight
  contract's recorded authority (the recorded authority still settles)
- `cancel_before_start` by a non-buyer → signer check
- `cancel_before_start` after finalize → `AlreadyFinal`
- fee charged on pay / finalize / cancel, committed **per contract** at
  `pay_for_compute` (`fee_committed_per_contract_not_config`,
  `fee_charged_ignores_later_config_rotation`) — rotating `Config` after
  payment does not change what finalize charges
- math: `price = u64::MAX, f = 999_999` (no silent overflow) and
  `price = 1, f = 1` (consistent rounding) — u128 checked arithmetic
- `update_config` signed by a non-config-authority → `NotSettlementAuthority`

## Known limitations

- **No on-chain timeout.** If a seller starts a job but never finishes
  and the contract's settlement authority never finalizes, the buyer can
  still reclaim the escrow at any time with `cancel_before_start` (full
  refund, fee paid). There is no automatic trigger.
- **Lost per-contract authority.** If the key recorded as a contract's
  settlement authority is lost, that specific job can no longer finalize
  (the buyer can still cancel). Other contracts are unaffected because
  the authority is per-contract, not global.
- **Config rotation scope.** `update_config` can change the reference fee
  wallet and amount without a redeploy, but in-flight contracts keep the
  fee they committed at `pay_for_compute`; changing the program's actual
  behavior (the `pay_for_compute` ABI, constraint logic, fee constants,
  etc.) still requires a redeploy to a new program ID.
- **SPL token program only.** Token-2022 mints (or any mint that needs
  the Token-2022 program) are unsupported.
- **`init_config` front-running.** The config PDA is derivable from the
  program ID, so on a fresh program ID a griefer could call
  `init_config` first, locking the reference fee config to their values.
  Mitigation: initialize in the same block as the deploy. Under the
  per-contract authority + per-contract fee model this no longer gates
  finalize or the fee, so the impact is limited to clobbering the
  off-chain reference, not fund theft.

## Deploy procedure

Follow MAINNET-CHECKLIST §3.3 (immutable runbook): deploy the
reproducible `.so` (§5), `init_config` with the config authority + fee
config, run one small end-to-end flow, then
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

## Module 1 executor — Cloud Hypervisor CPU backend

The executor (`crates/executor`, feature `cloud-hypervisor`) is the
privileged component that actually runs buyer-submitted jobs. It lives
**outside** the Anchor program but is the strongest attack surface in
the stack.

**Trust model (CPU backend):**

- The executor runs as root inside the hardened `vtessera-node` systemd
  unit. The `cloud-hypervisor` process is spawned as a child of the node.
- Each job boots a **disposable microVM** (host kernel + custom initramfs)
  with no guest network device (only a virtio-fs shared directory for
  `manifest.json` and `out/`).
- The **host is authoritative** for the wall-clock timeout
  (`max_duration_secs`); the guest runner enforces a best-effort cap.
- Admission requires `DeviceClass::Cpu` and `NetworkPolicy::None` — GPU
  and networking are follow-ups that will have their own threat models.

**Known limitations (documented, accepted):**

- **Plaintext manifest and env on the shared dir.** The `manifest.json`
  and environment variables are not encrypted at rest on the virtio-fs
  share. Host policy must ensure `env` contains no secrets (credentials,
  tokens, keys). A future attestation/confidential-compute path mitigates
  this.
- **Single-tenant-per-VM.** Each job gets its own microVM; there is no
  multi-tenant isolation within a VM. This is a strength (isolation) and
  a cost (per-job boot overhead).
- **No attestation yet.** A buyer cannot currently verify that the
  correct kernel/initramfs booted. Attestation hooks (Module 3 link) are
  a follow-up.

---

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
