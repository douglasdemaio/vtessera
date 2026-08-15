# Vtessera

**AI-agent compute settled in EURC/USDC.** An opt-in layer for GNU/Linux
machine owners to rent out CPU and GPU capacity to AI workloads, with
sellers settling in the **same stablecoin the buyer pays** — **EURC or
USDC** — plus a flat SOL protocol fee. There is **no Vtessera token** —
the protocol is technology, not a token.

> **Status.** v0 (`vtesserad`) is a read-only metering daemon: it samples
> `/proc`, writes signed Ed25519 receipts to a state directory, opens no
> sockets, and runs no third-party code. The escrow program is built
> (Anchor) and deployed live on **Solana devnet**; `vtessera-node`
> advertises a signed offer and negotiates paid jobs over **x402**
> (`HTTP 402 Payment Required`), demonstrated end-to-end by
> `crates/x402-client` + `scripts/x402-demo.sh`. The remaining modules
> are in active build under separate workspace crates (see `ROADMAP.md`).

## What Vtessera is

- **Technology, not a token.** No mint, no reserve, no DAO, no treasury,
  no custodian. Sellers earn the stablecoin the buyer paid, directly
  through on-chain settlement.
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
  the seller's earned slice is paid out in the same stablecoin mint; the
  unearned slice is refunded to the buyer in the original stablecoin.
  **No human ever holds the funds.**

## How a job flows

```
agent finds node      ──▶  via MCP (signed offer: GPU, VRAM, price OR free)
agent contracts node  ──▶  job contract; price OR free
   ↓
 free path  ─▶  HTTP 200, job runs, no transaction
 paid path  ─▶  HTTP 402 (x402) → agent signs stablecoin payment → retries

paid path on confirmation:
   buyer EURC/USDC  ─▶  escrow PDA (program-owned, no human withdraw)
   flat fee         ─▶  protocol fee wallet (100,000 lamports SOL)
   job runs         ─▶  per-job signed receipts (Ed25519, vtesserad)
   settlement       ─▶  completion fraction f ∈ [0, 1]
   on finalize:
      f × price     ─▶  SELLER (same stablecoin mint — no swap)
      (1−f) × price ─▶  refund BUYER in original stablecoin
```

## Repository layout (Cargo workspace)

```
vtessera/
├── README.md                       # this file
├── ROADMAP.md                      # modules 1–5, build order, milestones
├── BUILD.md                        # v0 daemon's authoritative build spec
├── MAINNET-CHECKLIST.md            # pre-flight items before mainnet deploy
├── LICENSE                         # Apache-2.0
├── Cargo.toml                      # workspace root (host crates)
├── rust-toolchain.toml             # pinned Rust toolchain + musl target
├── deny.toml                       # cargo-deny policy
├── crates/
│   ├── vtesserad/                  # v0 metering daemon (this README's quickstart)
│   ├── vtessera-offer/             # signed-offer types (canonical bytes + Ed25519)
│   ├── vtessera-node-api/          # agent-facing HTTP server: offer, jobs, 402/x402
│   ├── vtessera-executor/          # job execution backends (Module 1: noop-cpu, local-cpu)
│   ├── vtessera-settlement/        # receipt verification + settlement (Module 3, skeleton)
│   ├── vtessera-gui/               # GTK4 desktop app (Flatpak-packaged)
│   ├── devnet-demo/                # excluded: exercises the devnet escrow end-to-end
│   └── x402-client/                # excluded: agent that pays the escrow and submits a job
├── programs/
│   └── vtessera-escrow/            # Anchor escrow program — live on Solana devnet
├── packaging/                      # RPM spec, systemd unit, AppArmor, example config, Flatpak
├── scripts/
│   └── x402-demo.sh                # one-command agent demo against devnet
├── docs/
│   └── DESIGN.md                   # design index
└── .github/workflows/ci.yml
```

`devnet-demo` and `x402-client` are excluded from the host workspace: they
pin the Solana SDK 1.18.x toolchain, whose crypto dep tree conflicts with
the host crates' newer ed25519-dalek 2. Each builds standalone with its own
`Cargo.lock` (see the file headers).

## Where Vtessera fits on Solana

Vtessera is **technology, not a token**: it settles directly in the
stablecoins buyers already hold. Every **paid** Vtessera job escrows the
buyer's EURC/USDC and pays the seller the earned slice in the **same
stablecoin mint** — no swap, no oracle, no conversion. A flat SOL
protocol fee (100,000 lamports to
`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`) funds protocol
infrastructure. Free jobs don't touch a chain. The protocol adds an
escrow program and a discovery layer; nothing else.

## Currencies

- **Buyer pays:** EURC (default — ECB-anchored price stability) or USDC.
- **Seller earns:** the same EURC/USDC the buyer paid, in the same mint.
- **Protocol fee:** flat SOL fee of 100,000 lamports (0.0001 SOL) to
  `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`, charged on
  `pay_for_compute`, `finalize_pro_rata`, and `cancel_before_start`,
  stored in `Config` at `init_config` (immutable after). See `ROADMAP.md`
  §0.

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

CI additionally gates the module crates (incl. `vtessera-node-api` with
`--features serve`) and the GUI crate's formatting — see
`.github/workflows/ci.yml`. Note `cargo fmt --check` checks the whole
workspace, so the GUI code must be formatted even on machines without the
GTK4 dev libraries.

## Quickstart — smoke test (no systemd)

The fastest way to confirm the v0 daemon works on your box:

```bash
# 1. Build
cargo build -p vtesserad --release

# 2. Drop a config into place
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
# Choose your editor. Edit payout_id to your own Solana wallet address.
gedit /etc/vtessera/vtessera.toml

# 3. Run once. This generates /var/lib/vtessera/identity.key on first
#    run and writes one sample, then exits.
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

## Devnet agent demo (node + x402)

To see the agent-facing side — `vtessera-node` serving a signed offer and
negotiating a paid job against the live devnet escrow — run the one-command
demo (needs a devnet wallet; see the script header):

```bash
scripts/x402-demo.sh
```

It builds the agent (`crates/x402-client`), points a node at
`http://127.0.0.1:8402` (reusing one already running there), submits a
job, and pays the x402 challenge into the escrow. Free jobs run through
the node's wired executor (`--backend noop-cpu` by default, or
`local-cpu` for unisolated host execution). A paid job's proof is still
**not verified** — that path stays an honest 501 until the on-chain
payment verifier lands (Module 4) — so the demo finalizes the split
without running the job, proving the escrow money path end to end.
`crates/devnet-demo` is the lower-level variant that exercises
`pay_for_compute` → `finalize_pro_rata` directly.

## Offer index + claims (Module 2a)

A `vtessera-node` can register its signed offer with a central index and
enforce first-come-first-served claims from agents:

```bash
cargo build -p vtessera-offer-index --locked --bin vtessera-offer-index --features serve
./target/debug/vtessera-offer-index --bind 127.0.0.1:8403

# node side: advertise, and re-register every interval (default 60s)
./target/debug/vtessera-node --bind 127.0.0.1:8402 --offer offer.json --key key.bin \
    --state-dir /var/lib/vtessera --publish http://127.0.0.1:8403
```

- `GET /offers` lists current offers (`?available=1` filters out claimed
  ones); the index verifies each signature on register, so a node cannot
  impersonate another.
- An agent claims a node with `POST /offers/<node_id>/claim
  {"agent_id":"..."}` (HTTP 201, FCFS; 409 if taken). Claims are lease-style
  (TTL 60s, renewable by the owner) and release via
  `DELETE /offers/<node_id>/claim` with the same body.
- The **node enforces** its claim: once it publishes to an index, it
  requires an agent identity on every job. HTTP clients send `X-Agent-Id:
  <id>`; MCP `submit_job` carries an `agent_id` argument. If the node is
  claimed by someone else the job is refused (409), and if the index is
  unreachable the node fails closed (503). The MCP `discover` tool lists
  current offers with their claim state.
- One command end-to-end — two nodes (one free, one paid) publishing to an
  index, FCFS claims, node enforcement, MCP discover, release:

```bash
scripts/offer-index-demo.sh
```

## Settlement service (Module 3)

After every job, `vtessera-node` signs a per-job metering receipt with
its identity key and writes it to `<state-dir>/job-receipts/<job_id>.json`.
The node takes `--key <path>` (the same raw 32-byte Ed25519 seed format
`vtesserad` uses; mode 0600) and `--state-dir <dir>`; it refuses to start
if the key's `node_id` doesn't match the offer it's advertising.

`vtessera-settle` turns those signed receipts into completion fractions:

```bash
cargo build -p vtessera-settlement --locked --release

# single sweep (exit 0; exit 1 if any job was permanently rejected)
./target/release/vtessera-settle --state-dir /var/lib/vtessera/node --once

# or leave it watching (default 60s interval)
./target/release/vtessera-settle --state-dir /var/lib/vtessera/node
```

It scans `contracts/<job_id>.json`, verifies each job's signed receipt
(any signature/schema/node_id failure is a permanent hard reject — no
partial credit), aggregates device-seconds by the agreed device class, and
writes `settlements/<job_id>.json` with the completion fraction `f`. A
missing receipt is transient and retried next sweep. The escrow program
(Module 4) uses `f` to split the buyer's stablecoin pro-rata.

```bash
scripts/settlement-demo.sh   # end-to-end: node → signed receipt → settle
```

## Install as a systemd service

The shipped unit is hardened (DynamicUser, ProtectSystem=strict, no
ambient capabilities). No bootstrap step is needed: `StateDirectory=vtessera`
creates `/var/lib/vtessera` owned by the service's dynamic user, and the
daemon auto-generates its identity key there (`/var/lib/vtessera/identity.key`,
mode 0600) on first start. `/etc/vtessera` stays read-only — the key
never goes in `/etc` (a root-created key there could not be read by the
dynamic user anyway).

```bash
# 1. Install the binary where the unit expects it
sudo install -m 0755 target/release/vtesserad /usr/bin/vtesserad
#   (or substitute target/x86_64-unknown-linux-musl/release/vtesserad
#    if you built with musl)

# 2. Install the config (if you haven't already)
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
sudo "${EDITOR:-vi}" /etc/vtessera/vtessera.toml   # set payout_id

# 3. If you ran the manual quickstart above as root first, remove its
#    root-owned key so the service regenerates one under its own user.
#    (Only needed when /var/lib/vtessera/identity.key already exists.)
sudo rm -f /var/lib/vtessera/identity.key

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
that will receive the stablecoin payout once the settlement and escrow
modules are live. The daemon refuses to start with an empty or malformed
value.

## Design

- **`ROADMAP.md`** — Modules 0–5, build order, and milestones for the
  full Solana stablecoin / AI-agent stack. **Start here** if you're
  trying to understand where Vtessera is going.
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
