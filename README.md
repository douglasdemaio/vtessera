# Vtessera

**AI-agent compute for the HNT ecosystem.** An opt-in layer for GNU/Linux
machine owners to rent out CPU and GPU capacity to AI workloads, with
sellers settling in **HNT** and buyers (AI agents) paying in **EURC or
USDC**. There is **no Vtessera token** — the protocol is technology that
plugs into the existing Helium / HNT economy.

> **Status.** v0 (`vtesserad`) is a read-only metering daemon: it samples
> `/proc`, writes signed Ed25519 receipts to a state directory, opens no
> sockets, and runs no third-party code. The compute-execution, discovery,
> settlement, and escrow layers described below are in active build under
> separate workspace crates (see `ROADMAP.md`).

## What Vtessera is

- **Technology, not a token.** No mint, no reserve, no DAO, no treasury,
  no custodian. Sellers earn HNT directly through on-chain settlement.
- **Agent-native.** The buyer is software. Discovery, contracting, and
  payment happen machine-to-machine — no signups, no API keys, no
  dashboards. Sellers advertise their machine to other AIs through
  **MCP**-shaped resources, and paid endpoints negotiate via **x402**
  (`HTTP 402 Payment Required`).
- **Free or paid, the seller decides.** A node can serve compute for
  **free** (no transaction, no escrow, no fee — it just runs the job) or
  charge in EURC/USDC. The choice is a single flag in the seller's
  config.
- **Non-custodial settlement.** When a job is paid, buyer funds enter a
  program-owned escrow PDA on Solana. They leave only by on-chain rules:
  the seller's earned slice is swapped to HNT (Jupiter, Pyth-guarded) and
  paid out; the unearned slice is refunded to the buyer in the original
  stablecoin. **No human ever holds the funds.**

## How a job flows

```
agent finds node      ──▶  via MCP (signed offer: GPU, VRAM, price OR free)
agent contracts node  ──▶  job contract; price OR free
   ↓
 free path  ─▶  HTTP 200, job runs, no transaction
 paid path  ─▶  HTTP 402 (x402) → agent signs stablecoin payment → retries

paid path on confirmation:
   buyer EURC/USDC  ─▶  escrow PDA (program-owned, no human withdraw)
   flat fee         ─▶  protocol fee address (DRAFT, see below)
   job runs         ─▶  per-job signed receipts (Ed25519, vtesserad)
   settlement       ─▶  completion fraction f ∈ [0, 1]
   on finalize:
      f × price     ─▶  swap to HNT (Jupiter / Pyth guard) → burn slice → SELLER (HNT)
      (1−f) × price ─▶  refund BUYER in original stablecoin
```

## Repository layout (Cargo workspace)

```
vtessera/
├── README.md                       # this file
├── ROADMAP.md                      # modules 1–5, build order, milestones
├── BUILD.md                        # v0 daemon's authoritative build spec
├── LICENSE                         # Apache-2.0
├── Cargo.toml                      # workspace root
├── rust-toolchain.toml             # pinned Rust toolchain + musl target
├── deny.toml                       # cargo-deny policy
├── crates/
│   └── vtesserad/                  # v0 metering daemon (this README's quickstart)
│       ├── Cargo.toml
│       └── src/                    # main, config, metrics, receipt, sign, spool, submit
├── programs/
│   └── vtessera-escrow/            # (planned) Solana Anchor escrow program — Module 4
├── packaging/                      # RPM spec, systemd unit, AppArmor profile, example config
├── docs/
│   └── DESIGN.md                   # design index
└── .github/workflows/ci.yml
```

New module crates (`executor`, `offer`, `node-api`, `settlement`) land under
`crates/` as they come online; see `ROADMAP.md` for status.

## Where Vtessera fits in the HNT ecosystem

Helium's HNT is the protocol's settlement asset. Every **paid** Vtessera job
results in an on-market HNT buy (Jupiter swap from the seller's earned
stablecoin) and a small burn, then HNT to the seller. Free jobs don't touch
HNT, an oracle, or any chain. The protocol consumes the existing HNT mint,
on-chain Pyth feeds, and Jupiter routing — it adds an escrow program and a
discovery layer; nothing else.

## Currencies

- **Buyer pays:** EURC (default — ECB-anchored price stability) or USDC.
- **Seller earns:** HNT.
- **Protocol fee:** flat SOL fee. **DRAFT** until the escrow program is
  deployed and operating end-to-end — both the wallet address and the
  amount are subject to change before mainnet. See `ROADMAP.md` §0 for the
  current draft values.

## Prerequisites (v0 daemon)

You need a Rust toolchain. The Rust version and (optionally) the musl
target are pinned by `rust-toolchain.toml` and installed automatically
on first `cargo` invocation.

Install `rustup` if you don't have it:

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
. "$HOME/.cargo/env"
```

For the static / RPM build path you need the musl **Rust target**:

```bash
rustup target add x86_64-unknown-linux-musl
```

That's it for the **default build** — vtesserad's deps are pure Rust
and rustup's `x86_64-unknown-linux-musl` target ships everything
needed to link statically. You do **not** need a system musl-gcc, and
on openSUSE Tumbleweed there is no `musl` / `musl-devel` package in
the default repos to install anyway.

You only need an external musl cross-compiler if you also enable the
optional `submit` feature, which pulls in `rustls` → `ring` (C code)
and so wants `x86_64-linux-musl-gcc` at link time:

| Distro | Command (only for `--features submit`) |
| --- | --- |
| Debian / Ubuntu            | `sudo apt install musl-tools`            |
| Fedora / RHEL              | `sudo dnf install musl-gcc`              |
| openSUSE Tumbleweed / Leap | install `x86_64-linux-musl-gcc*` from the `devel:tools:cross` OBS repo; no default-repo package today |

You can skip musl entirely if you only want a local glibc build for
testing — see "Build (quick, glibc)" below.

## Build

### Build (quick, glibc) — for local testing

```bash
cargo build -p vtesserad --release
```

Binary lands at `target/release/vtesserad`.

### Build (static musl) — for production / RPM

```bash
cargo build -p vtesserad --release --locked --target x86_64-unknown-linux-musl
```

Binary lands at `target/x86_64-unknown-linux-musl/release/vtesserad`. This
is the artifact CI publishes and what the RPM ships.

### All checks (v0)

`cargo audit` and `cargo deny` aren't included in a default Rust
install. CI installs them on every run; for a local check run, install
them once:

```bash
cargo install cargo-audit cargo-deny --locked
```

Then the full v0 check suite is:

```bash
cargo fmt --check
cargo clippy -p vtesserad --all-targets -- -D warnings
cargo test -p vtesserad --locked
cargo audit
cargo deny check
```

## Quickstart — smoke test (no systemd)

The fastest way to confirm the v0 daemon works on your box:

```bash
# 1. Build
cargo build -p vtesserad --release

# 2. Drop a config into place
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
# Choose your editor. Edit payout_id to your own Solana wallet address.
gedit /vtessera/vtessera.toml
# Choose your editor. Edit let mut price_micros: Option<u64> = 0.0005; (0.0005 is per second so that's about 1.80 USDC an hour)
gedit /vtessera/crates/node-api/src/bin/gen_offer.rs

# 3. Run once. This generates /etc/vtessera/identity.key on first run
#    and writes one sample, then exits.
sudo ./target/release/vtesserad --config /etc/vtessera/vtessera.toml --once
```

On success you'll see `vtesserad started: ...` on stderr. `--once` exits
before finalizing a window, so no receipt is written yet — that's
expected. To see a receipt land, drop `--once` and let it run for at
least `window_size` seconds (default 60), then `Ctrl-C`:

```bash
sudo ./target/release/vtesserad --config /etc/vtessera/vtessera.toml
# wait ~60s, then Ctrl-C
sudo ls /var/lib/vtessera/   # JSON receipts appear here
```

## Install as a systemd service

The shipped unit is hardened (DynamicUser, ProtectSystem=strict, no
ambient capabilities). That hardening has one consequence worth calling
out: at runtime `/etc/vtessera` is **read-only**, so the daemon cannot
auto-generate the identity key from inside the service. You need to
bootstrap the key once before starting the service.

```bash
# 1. Install the binary where the unit expects it
sudo install -m 0755 target/release/vtesserad /usr/bin/vtesserad
#   (or substitute target/x86_64-unknown-linux-musl/release/vtesserad
#    if you built with musl)

# 2. Install the config (if you haven't already)
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
sudo "${EDITOR:-vi}" /etc/vtessera/vtessera.toml   # set payout_id

# 3. Bootstrap the identity key — run the daemon once as root so it
#    can write /etc/vtessera/identity.key while /etc is still writable.
sudo /usr/bin/vtesserad --config /etc/vtessera/vtessera.toml --once
sudo ls -l /etc/vtessera/identity.key   # should exist, mode 0600

# 4. Install and start the service
sudo cp packaging/vtesserad.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start vtesserad
sudo systemctl status vtesserad
```

Receipts land under `/var/lib/vtessera/` (systemd's DynamicUser symlinks
this to `/var/lib/private/vtessera/` — both paths work).

Watch live logs:

```bash
sudo journalctl -u vtesserad -f
```

## Troubleshooting

**`status=203/EXEC` / `Unable to locate executable '/usr/bin/vtesserad'`**
The binary isn't installed yet. Run step 1 above
(`install -m 0755 ... /usr/bin/vtesserad`).

**`error: failed to load/generate key: Read-only file system (os error 30)`**
The identity key doesn't exist yet and the hardened unit can't create it
because `/etc` is read-only inside the service sandbox. Run step 3 above
to bootstrap the key once outside systemd.

**`System call ~@resources is not known, ignoring.`**
Harmless warning on older systemd. The seccomp filter just drops that
group; the daemon still starts.

**Service stuck in restart loop**
`sudo systemctl reset-failed vtesserad` clears the rate-limit, then check
`journalctl -u vtesserad -e --no-pager -o cat` for the real error.

## Receipt format

Signed receipts are written to the state directory (default
`/var/lib/vtessera/`). Each is a JSON file containing the receipt, the
operator's Ed25519 public key, and the signature over the canonical
receipt bytes defined in `BUILD.md` §4.

There is no CLI verify subcommand in v0 — verification is library-only.
Downstream tools and the future settlement service verify receipts by
calling `sign::verify` against the canonical-byte layout. The
verification path lives in the settlement crate as it lands (see
`ROADMAP.md` §3).

## Config

See `packaging/vtessera.toml.example` for all options. Required fields:
`sample_interval_secs`, `state_dir`, `key_path`, `payout_id`.

`payout_id` is the seller's Solana base58 Ed25519 address — the wallet
that will receive HNT once the settlement and escrow modules are live.
The daemon refuses to start with an empty or malformed value.

## Design

- **`ROADMAP.md`** — Modules 0–5, build order, and milestones for the
  full HNT/AI-agent stack. **Start here** if you're trying to understand
  where Vtessera is going.
- **`MAINNET-CHECKLIST.md`** — Per-step checklist of what must hold
  before the escrow program can be deployed to Solana mainnet. The
  devnet program is live; mainnet is intentionally deferred behind
  this list.
- **`BUILD.md`** — Authoritative v0 build specification (scope, hard
  rules, module contracts, systemd hardening, CI, definition of done).
  v0 must not widen beyond this; new modules live in separate crates.
- **`docs/DESIGN.md`** — Design index pointing at the documents above.

## License

Apache-2.0
