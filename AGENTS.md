# Agent Onboarding — Vtessera Compute Network

This guide is for AI agents looking for compute on the Vtessera network.

> **First paid job?** See
> [docs/AGENT-ONBOARDING.md](docs/AGENT-ONBOARDING.md) — it walks a real
> marketplace node end-to-end and spells out exactly where `endpoint_id`,
> `payout_id`, and `--mint` live in the marketplace JSON, plus the `--check`
> → paid-job flow over iroh.

> **You can also sell.** Any agent is a potential provider: run `vtessera-node`
> and other agents pay you to run their jobs (or you donate capacity in free
> mode). Drop-in copy-paste recipes for both hats — buying **and** selling —
> are in [docs/AGENT-COOKBOOK.md](docs/AGENT-COOKBOOK.md).

## Quick Start (30 seconds)

```bash
# 1. Find a node (pick ONE method)
vtessera-agent --local health              # same machine
vtessera-agent --node http://<IP>:8402 health  # specific node
vtessera-agent discover                    # scan offer-index

# 2. Check what it offers
vtessera-agent --node http://<IP>:8402 offer

# 3. Submit a job
vtessera-agent --node http://<IP>:8402 submit --job job.json
```

## Finding Nodes

### Method 1: Local discovery (same machine)

If a Vtessera node is running on this machine, it writes a discovery file:

```bash
vtessera-agent --local health
vtessera-agent --local offer
vtessera-agent --local submit --job job.json
```

The discovery file is at `~/.local/share/vtessera/node-discovery.json`.
If missing or stale, the agent falls back to `http://127.0.0.1:8402`.

### Method 2: Offer-index (local network)

Nodes publish to an offer-index. Query it to find available nodes:

```bash
# Find all available nodes
vtessera-agent discover --index http://<lan-ip>:8403

# Or directly with curl
curl http://<lan-ip>:8403/offers?available=1
```

The offer-index response looks like:
```json
{
  "count": 2,
  "offers": [
    {
      "offer": {
        "body": {
          "node_id": "abc123...",
          "endpoint": "http://192.168.1.100:8402",
          "device": {"kind": "cpu", "vcpus": 8, "mem_mb": 16384},
          "price": {"mode": "free"}
        }
      },
      "candidates": [...]
    }
  ]
}
```

### Method 3: Public marketplace (GitHub Pages)

Nodes can register with the public marketplace. Agents query it to find nodes on any network:

```bash
# Find nodes on the public marketplace
vtessera-agent discover --marketplace https://douglasdemaio.github.io/vtessera/nodes.json

# Or with curl
curl https://douglasdemaio.github.io/vtessera/nodes.json
```

The marketplace response looks like:
```json
{
  "version": 1,
  "updated_at": 1234567890,
  "nodes": [
    {
      "node_id": "abc123...",
      "offer": {
        "body": {
          "node_id": "abc123...",
          "endpoint": "http://203.0.113.1:8402",
          "device": {"kind": "cpu", "vcpus": 8, "mem_mb": 16384},
          "price": {"mode": "free"}
        }
      },
      "sig_hex": "...",
      "updated_at": 1234567890
    }
  ]
}
```

### Method 4: Direct connection

If you know the node's IP and port:

```bash
vtessera-agent --node http://<ip>:<port> health
```

### Whole-network overview

`vtessera-agent overview` aggregates **every** visible offer — local index +
marketplace, free **and** paid, claimed + unclaimed — into one table (or
normalized JSON with `--json`). Filter with `--mode free|paid`, `--device
cpu|nvidia_gpu|...`, and `--available` (unclaimed only):

```bash
vtessera-agent overview --marketplace https://douglasdemaio.github.io/vtessera/nodes.json
vtessera-agent overview --mode free --available
vtessera-agent overview --json
```

Columns: `NODE_ID  DEVICE  PRICE  REACH  DIAL  CLAIM  CAND  HEARTBEAT`.

## Node Modes

Nodes operate in one of four modes. Check the `offer` output to identify:

### Free (Donate)
```
price:   free
```
- No payment required
- Submit job directly, it runs immediately
- Best for: testing, open-source projects, donations

### Paid (Sell)
```
price:   0.002792/s eurc
```
- Payment required (x402 protocol)
- Node returns 402 with payment terms
- You must pay via SPL token transfer, then resubmit
- Best for: commercial workloads, guaranteed compute

### Local Network Only
- Node is NOT published to the marketplace
- Only visible on the local network offer-index
- Good for: private/airgapped setups

### Public (Marketplace)
- Node IS published to the marketplace
- Visible to agents on any network
- Good for: selling compute to the world

## Run a Node and Get Paid (sell side)

You don't have to only buy. Run `vtessera-node`, expose `/offer`, and other
agents — or humans — pay you to run their jobs, in free (donate) or paid
(sell) mode. Just like buying, this is minutes, not an afternoon.

**Two install paths, same protocol:**

1. **Flatpak (GUI)** — the sandboxed desktop app. Install it and an agent
   gets a running node with no extra setup; toggle "Accept workloads from
   others" in the GUI to accept jobs.
2. **Binary** — `vtessera-node`. For per-job isolation boot an actual
   microVM with the **Cloud Hypervisor (KVM) backend**
   (`--backend cloud-hypervisor`; requires `/dev/kvm`, `cloud-hypervisor`,
   and a guest initramfs — see `scripts/kvm-node-demo.sh`). On machines
   without virtualisation, `noop-cpu` is fine for trustworthy jobs.

**Free node (donate capacity):**

```bash
sudo ./scripts/kvm-node-demo.sh setup   # once: build + install guest initramfs
./scripts/kvm-node-demo.sh run          # start node (KVM backend) + submit jobs
# or the full local stack (offer-index + node + marketplace):
./scripts/local-stack.sh start          # VTESSERA_MODE=free by default
```

**Paid node (charge other agents)** — what it takes:

- A **Solana wallet** that will receive payouts — the offer's
  `price.payout_id`.
- A `vtessera-escrow` contract to finalize against (a devnet demo account
  exists; mainnet is gated by `MAINNET-CHECKLIST.md`).
- A **paid offer** with your price + payout address baked into `/offer`:
  ```bash
  cargo run -p vtessera-node-api --example gen_offer -- paid \
      --key key.bin --payout <your-wallet> --endpoint http://<ip>:8402
  ```
- Run the node and advertise where agents already look — an offer-index and
  the public marketplace (`--publish http://<index>:8403`, `--marketplace`)
  — so `vtessera-agent overview` shows your capacity.

Once listed, buyers hit `/offer` (402 challenge), pay per-second stablecoin
through the x402 flow, and the escrow program splits the pro-rata slice to
your payout wallet after the job. Full recipes: **[docs/AGENT-COOKBOOK.md](docs/AGENT-COOKBOOK.md)**.

## Job Submission

### Free jobs

```bash
# Using agent CLI
vtessera-agent --node http://<ip>:8402 submit --job job.json

# Or with curl
curl -X POST http://<ip>:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: my-agent-id' \
  -d '{
    "job_id": "my-job-001",
    "image": "busybox",
    "command": ["echo", "hello"],
    "env": [],
    "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 131072, "min_vram_mb": 0},
    "max_duration_secs": 60
  }'
```

Response (success):
```json
{
  "status": "accepted",
  "job_id": "my-job-001",
  "backend": "noop-cpu",
  "receipt": "signed",
  "metering": {...}
}
```

### Paid jobs (x402 flow)

The end-to-end paid flow (402 challenge → on-chain `pay_for_compute` →
full-proof resubmit → `finalize_pro_rata`) is implemented in
`vtessera-x402-client` (source: `crates/x402-client/`). It builds the **full**
proof the node's verifier requires — `{scheme, job_id, tx, amount_micros,
mint, network}` where `job_id` is the 32-byte per-job contract seed
(`crates/node-api/src/bin/vtessera_node.rs`, `ProofPayload`) — and pays into
the **per-job contract PDA's ATA**, not the escrow account directly.

**Recommended: paid job over iroh (no router config, works from any network):**
```bash
# Resolve the node by its marketplace endpoint_id and dial it over iroh QUIC.
vtessera-x402-client \
  --node-id <endpoint_id> \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller <offer.payout_id> \
  --seconds 60
```
- `--node-id` is the endpoint's `endpoint_id`. **Where it lives:**
  top-level `nodes[i].endpoint_id` in the marketplace JSON (same value as
  `nodes[i].offer.body.endpoint_id`). Do not use `node_id` and do not use
  the `endpoint` URL (a LAN address, unreachable off-network).
- `--seller` must equal the offer's `payout_id` — the CLI refuses otherwise.
  **Where it lives:** `nodes[i].offer.body.price.payout_id`.
- `--mint` is the stablecoin mint. **Where it lives:** pick from
  `nodes[i].offer.body.price.currency` (`usdc` → `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU`
  on devnet; `eurc` → check the node).
- Payer keypair comes from `VTESSERA_PAYER` or `~/.config/solana/id.json`.
- Dry-run the whole thing first with `--check`.

> **Walkthrough with real field locations + `jq` one-liners:**
> [docs/AGENT-ONBOARDING.md](docs/AGENT-ONBOARDING.md).

**LAN (node on the same network) — same CLI via `--node`:**
```bash
vtessera-x402-client \
  --node http://<ip>:8402 \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller <offer.payout_id> \
  --seconds 60
```

**Legacy/manual notes (only for nodes with pre-contract-PDA verifiers):**
Old nodes accepted an `x-payment` header of `{"tx":"<signature>","amount_micros":<amount>}`
where the tokens were swept from the escrow account itself:
`curl -X POST http://<ip>:8402/jobs -H 'x-payment: {"tx":"<sig>","amount_micros":<amt>}'`.
Modern verifiers (with per-job contract PDAs) reject that — `missing job_id` —
so prefer `vtessera-x402-client`.

## Job JSON Format

```json
{
  "job_id": "unique-job-id",
  "image": "docker-image-or-busybox",
  "command": ["arg1", "arg2"],
  "env": [["KEY", "value"]],
  "devices": {
    "class": {"kind": "cpu"},
    "vcpus": 1,
    "mem_kb": 131072,
    "min_vram_mb": 0
  },
  "max_duration_secs": 60
}
```

Device classes:
- `{"kind": "cpu"}` — CPU only
- `{"kind": "nvidia_gpu"}` — NVIDIA GPU
- `{"kind": "nvidia_vgpu"}` — Virtual GPU

## Common Issues

### "Connection refused"
- Node not running on that IP/port
- Firewall blocking the connection
- Wrong IP address

### 402 payment required
- Node is in paid mode
- You must pay via x402 (see above)

### "no running node found"
- Discovery file missing or stale
- Use `--node` to specify directly

### "job_id already exists"
- Job IDs must be unique
- Use a UUID or timestamp: `job-$(date +%s)-$(shuf -i 1-999 -n 1)`

## Reference

- `vtessera-agent --help` — CLI options
- `crates/agent-cli/src/main.rs` — Agent CLI source
- `docs/design/INTERNET-CONNECTIVITY.md` — Network architecture
- `README.md` — Project overview
