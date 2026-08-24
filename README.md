# Vtessera

**AI-agent compute settled in EURC/USDC.** An opt-in layer for GNU/Linux
machine owners to rent out CPU and GPU capacity to AI workloads, with
sellers settling in the **same stablecoin the buyer pays** — **EURC or
USDC** — plus a flat SOL protocol fee. There is **no Vtessera token** —
the protocol is technology, not a token.

> **Status.** `vtesserad` v0 is a read-only metering daemon: it samples
> `/proc`, writes signed Ed25519 receipts to a state directory, opens no
> sockets, and runs no third-party code. The escrow program is built
> (Anchor) and deployed live on **Solana devnet**; `vtessera-node`
> advertises a signed offer and negotiates paid jobs over **x402**
> (`HTTP 402 Payment Required`), demonstrated end-to-end by
> `crates/x402-client` + `scripts/x402-demo.sh`. **Module 1 shipped:**
> `--backend cloud-hypervisor` runs each job in a disposable Cloud
> Hypervisor microVM (CPU-only, no guest network). **Module 2 shipped:**
> MCP offer discovery, offer index with FCFS claims, HTTP + x402 job
> submission, off-chain Solana payment verification. **Module 3 shipped:**
> per-job signed receipts and settlement service computing completion
> fraction `f`. **Module 4 shipped:** escrow program live on devnet, full
> pay→run→settle→split flow exercised end-to-end. See `ROADMAP.md`.

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
 executor   ─▶  --backend cloud-hypervisor boots a disposable microVM

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
├── SECURITY.md                     # security policy + vulnerability reporting
├── LICENSE                         # Apache-2.0
├── Cargo.toml                      # workspace root (host crates)
├── rust-toolchain.toml             # pinned Rust toolchain + musl target
├── deny.toml                       # cargo-deny policy
├── crates/
│   ├── vtesserad/                  # v0 metering daemon (this README's quickstart)
│   ├── offer/                      # signed-offer types (canonical bytes + Ed25519)
│   ├── node-api/                   # agent-facing HTTP server: offer, jobs, 402/x402
│   ├── executor/                   # job execution backends (Module 1: noop-cpu, local-cpu, cloud-hypervisor)
│   ├── settlement/                 # receipt verification + settlement (Module 3)
│   ├── offer-index/                # Module 2a: central offer index (verify + serve)
│   ├── mini-http/                  # Module 2: shared HTTP/1.1 server primitives
│   ├── vtessera-gui/               # GTK4 desktop app (Flatpak-packaged)
│   ├── marketplace-server/         # reference marketplace server (Axum)
│   ├── vtessera-config/            # config wizard for private/enterprise deployments
│   ├── metering-sidecar/           # Kata container guest-side metering
│   ├── devnet-demo/                # excluded: exercises the devnet escrow end-to-end
│   └── x402-client/                # excluded: agent that pays the escrow and submits a job
├── programs/
│   └── vtessera-escrow/            # Anchor escrow program — live on Solana devnet
├── tests/
│   └── adversarial/                # excluded: fuzz + adversarial test suite for escrow
├── packaging/                      # RPM spec, systemd unit, AppArmor, example config, Flatpak
├── scripts/
│   ├── local-stack.sh              # one-command dev stack: start|stop|status
│   ├── x402-demo.sh                # one-command agent demo against devnet
│   ├── build-initramfs.sh          # builds the CH executor's guest initramfs
│   ├── offer-index-demo.sh         # end-to-end: two nodes, claims, MCP discover
│   ├── settlement-demo.sh          # end-to-end: node → signed receipt → settle
│   ├── agent-smoke-test.sh         # Flatpak agent smoke test (7 checks)
│   └── kata-setup.sh               # provisions fresh nodes for Kata backend
├── docs/
│   ├── DESIGN.md                   # design index
│   ├── CONSENT.md                  # consent & disclosure spec (UI + copy rules)
│   └── superpowers/                # specs + plans (historical; specs are canonical)
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

## Consent & disclosure

Vtessera is an opt-in compute node for AI agents, settled in EURC/USDC. It
asks your permission before it does anything: **metering consent on first
run**, and a separate, **off-by-default switch** before it accepts workloads
from other agents. Jobs run through your chosen backend — simulated by
default, executed on this machine **with no sandbox** if you say so, or
run inside a disposable **Cloud Hypervisor microVM** (CPU-only, no guest
network) for real isolation. You
can stop everything with one button and uninstall completely at any time.
Vtessera never starts itself, never restarts itself, and never runs code you
didn't approve. There is no token, and settlement is a flat SOL protocol fee
on top of the stablecoin the buyer pays.

In short:

- **Two consent gates, both explicit.** Nothing runs without the first-run
  "Enable metering" gate; nothing accepts jobs until "Accept workloads from
  others" is turned on (it defaults off).
- **One-action stop.** Stop halts metering and job acceptance together. No
  silent resume.
- **Legible activity.** The Status tab shows the state (Off / Metering only /
  Accepting jobs), the settlement authority, and the per-job receipts written
  to `<state-dir>/job-receipts/`.
- **No autostart, complete uninstall.** Installing never starts the app;
  uninstalling removes the service, config, key, and state (see below).
- **v0 metering opens no network sockets** — pinned by a test
  (`tests/no_socket.rs`). Only `vtessera-node` binds (loopback by default)
  and only when accepting workloads.
- **Honest claims.** What the settlement authority can and cannot do, and
  what the fee is, are documented — see `docs/CONSENT.md` for the
  do-not-say / say-instead rules and the full consent spec.

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

### Integration tests (Cloud Hypervisor CPU backend)

These boot a real VM and require `/dev/kvm`, `cloud-hypervisor`,
`virtiofsd`, and an initramfs built by `scripts/build-initramfs.sh`:

```bash
VTESSERA_CH_INTEGRATION=1 CH_INITRAMFS=/var/lib/vtessera/initramfs.cpio.gz \
  cargo test -p vtessera-executor --features cloud-hypervisor --test ch_cpu_integration
```

The initramfs must be built first (requires `busybox-static` and the
host kernel's `fuse.ko`/`virtiofs.ko` modules):

```bash
sudo scripts/build-initramfs.sh
```

## Local dev stack (one command)

The fastest way to run the full stack locally — offer-index, vtessera-node,
and marketplace-server — with one command. Mirrors the Flatpak GUI's
"Start" button from the CLI:

```bash
./scripts/local-stack.sh start    # start all services
./scripts/local-stack.sh status   # check which are running
./scripts/local-stack.sh stop     # stop all services
```

What it does automatically:
- **Auto-detects LAN IP** for the advertised endpoint (same as the GUI's
  `detect_lan_ip()`) so agents on the same network can reach the node
- **Generates or reuses** an Ed25519 identity key
- **Auto-registers** the node pubkey in the marketplace key registry
  (hex → base58, same as the GUI's `register_node_in_marketplace()`)
- **Writes a discovery file** to `~/.local/share/vtessera/node-discovery.json`
  so `vtessera-agent --local` works out of the box

Environment overrides:

| Variable | Default | Effect |
| --- | --- | --- |
| `VTESSERA_LOCAL_ONLY=1` | `0` | Bind everything to `127.0.0.1` |
| `VTESSERA_MODE=free` | `free` | `free` or `paid` |
| `VTESSERA_PORT=8402` | `8402` | Node HTTP port |

Test the running stack:

```bash
# Health check
curl http://127.0.0.1:8402/healthz

# Fetch the signed offer
curl http://127.0.0.1:8402/offer

# Submit a free job
curl -X POST http://127.0.0.1:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: my-agent' \
  -d '{
    "job_id": "test-001",
    "image": "busybox",
    "command": ["echo", "hello"],
    "env": [],
    "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 65536, "min_vram_mb": 0},
    "max_duration_secs": 60
  }'

# Discover nodes via the local offer-index
curl http://127.0.0.1:8403/offers?available=1

# Use the agent CLI with local discovery
vtessera-agent --local health
vtessera-agent --local offer
vtessera-agent --local submit --job job.json
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
`local-cpu` for unisolated host execution). Paid jobs verify the x402
payment proof via off-chain Solana RPC (`--rpc-url` on `vtessera-node`)
before running — confirming transaction finalization, escrow account
involvement, and sufficient token transfer amount. The demo finalizes
the split after the job runs, proving the escrow money path end to end.
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

## For agents

The node serves an HTTP API on `127.0.0.1:8402` (default). An agent
installing the Flatpak gets a running node with no extra setup — just
toggle "Accept workloads from others" in the GUI.

### Quick start with vtessera-agent

```bash
# Build the agent CLI
cargo build -p vtessera-agent

# Auto-discover a running node on the same machine
vtessera-agent --local health
vtessera-agent --local offer
vtessera-agent --local submit --job job.json

# Or point to a specific node
vtessera-agent --node http://192.168.1.100:8402 health

# Discover nodes via an offer-index
vtessera-agent discover --index http://127.0.0.1:8403
```

### Endpoints

| Method | Path | What it does |
| ------ | ---- | ------------ |
| `GET` | `/healthz` | Returns `ok` if the node is alive |
| `GET` | `/offer` | Returns the signed machine-readable offer (JSON) |
| `GET` | `/.well-known/agent.json` | A2A agent card |
| `POST` | `/mcp` | MCP 2024-11-05 JSON-RPC (tools: `discover`, `submit_job`) |
| `POST` | `/jobs` | Submit a job (HTTP API) |
| `GET` | `/jobs/<job_id>` | Get job status |

### Submit a free job (curl)

```bash
curl -X POST http://127.0.0.1:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: my-agent' \
  -d '{
    "job_id": "test-001",
    "image": "busybox",
    "command": ["echo", "hello from agent"],
    "env": [],
    "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 65536, "min_vram_mb": 0},
    "network": "none",
    "max_duration_secs": 60
  }'
```

On a free node, this returns `200` with the job result. On a paid node,
it returns `402` with x402 payment terms.

### Submit a job via MCP

```bash
# Discover available nodes
curl -X POST http://127.0.0.1:8402/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"discover","arguments":{}}}'

# Submit a job
curl -X POST http://127.0.0.1:8402/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"submit_job","arguments":{"job_id":"test-002","image":"busybox","command":["echo","hello"],"vcpus":1,"mem_kb":65536}}}'
```

Or use the stdio MCP binary directly (from the Flatpak or a local build):

```bash
flatpak run --command=vtessera-mcp io.github.douglasdemaio.Vtessera
```

### Node discovery

The node publishes to a central index when started with `--publish`:

```bash
# Agent: find available nodes
curl http://127.0.0.1:8403/offers?available=1

# Agent: claim a node (FCFS, 60s lease)
curl -X POST http://127.0.0.1:8403/offers/<node_id>/claim \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"my-agent"}'
```

See `scripts/offer-index-demo.sh` for a full end-to-end demo with
two nodes, claims, and MCP discovery.

### Local discovery (same machine)

When the GUI starts a node, it writes a discovery file to
`~/.local/share/vtessera/node-discovery.json` containing the endpoint,
node ID, and index URL. The `vtessera-agent` CLI reads this file with
`--local`:

```bash
# Auto-discover the running node
vtessera-agent --local health
vtessera-agent --local offer
vtessera-agent --local submit --job job.json
```

If the file is missing or the node process has exited, the agent falls
back to `http://127.0.0.1:8402`. The file is removed when the node
stops.

### Paid jobs (x402)

For paid nodes, the agent must pay via x402 — the node returns `402`
with payment terms, the agent signs a stablecoin transfer, and retries.
See `scripts/x402-demo.sh` for a working example against devnet.

## Marketplace server (reference implementation)

A standalone Axum-based marketplace server that stores signed receipts
and serves them via an authenticated REST API. Used for internal
/private deployments where nodes submit receipts to a central store.

```bash
# Build
cargo build -p marketplace-server

# Run (requires config.toml + key registry)
cargo run -p marketplace-server -- path/to/server.toml

# POST a signed receipt
curl -X POST http://127.0.0.1:8443/receipts \
  -H 'Content-Type: application/json' \
  -d '{"job_id":"...","pubkey":"...","signature":"...","received_at":...}'
```

See `scripts/local-stack.sh` which starts the marketplace server
alongside the offer-index and node.

## Observability (metrics + Grafana)

Every service exposes a Prometheus-compatible `/metrics` endpoint:

```bash
curl http://127.0.0.1:8402/metrics   # node
curl http://127.0.0.1:8403/metrics   # offer-index
curl http://127.0.0.1:8443/metrics   # marketplace-server
```

Metric types (zero-dep, implemented in `crates/vtessera-metrics/`):
- **Counter** — `# HELP` / `# TYPE` + per-label lines (requests, settle_ok, etc.)
- **Gauge** — `jobs_running`, `jobs_claimed`, `cache_hits`, etc.

To spin up a full observability stack with Prometheus + Grafana:

```bash
cd packaging/observability
podman-compose up -d
# or: docker compose up -d
# Grafana at http://localhost:3000 (admin/admin)
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

## Uninstall

Vtessera never starts itself, so removing it is straightforward and leaves
nothing running. The GUI (Flatpak) and the RPM daemon are independent
install paths; remove the one you installed.

**Flatpak (GUI):**

```bash
flatpak uninstall io.github.douglasdemaio.Vtessera
# optional: remove the per-user state it left behind
rm -rf ~/.var/app/io.github.douglasdemaio.Vtessera
```

**Flatpak (CLI — node + MCP):**

```bash
flatpak run --command=vtessera-node io.github.douglasdemaio.Vtessera \
  --bind 127.0.0.1:8402 --offer offer.json --key key.bin \
  --state-dir ~/.local/share/vtessera --backend cloud-hypervisor
```

**RPM (daemon, if installed):**

```bash
sudo systemctl disable --now vtesserad      # if you enabled/started it
sudo zypper remove vtessera                 # or rpm -e vtessera
sudo rm -rf /var/lib/vtessera               # receipts + identity key
sudo rm -rf /etc/vtessera                   # config (contains payout_id)
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
- **`SECURITY.md`** — Security policy, vulnerability reporting, and
  security design references.
- **`docs/DESIGN.md`** — Design index pointing at the documents above.
- **`docs/CONSENT.md`** — The consent & disclosure spec: behavioural
  invariants, the GUI consent flow, copy rules, the claims-precision
  table, and the anti-misclassification checklist.

## License

Apache-2.0
