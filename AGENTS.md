# Agent Onboarding — Vtessera Compute Network

This guide is for AI agents looking for compute on the Vtessera network.

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
vtessera-agent discover --marketplace https://douglasdemaio.github.io/vtessera/marketplace/nodes.json

# Or with curl
curl https://douglasdemaio.github.io/vtessera/marketplace/nodes.json
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
    "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 65536, "min_vram_mb": 0},
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

**Step 1:** Submit job (will get 402)
```bash
curl -X POST http://<ip>:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: my-agent-id' \
  -d @job.json
```

Response (402):
```json
{
  "scheme": "x402",
  "network": "solana-devnet",
  "escrow_account": "6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma",
  "offer": {
    "body": {
      "price": {
        "mode": "paid",
        "currency": "eurc",
        "per_device_second_micros": 2792,
        "payout_id": "5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs"
      }
    }
  }
}
```

**Step 2:** Pay the escrow
```bash
# Transfer tokens to the escrow account
spl-token transfer \
  --url devnet \
  --fund-recipient \
  <TOKEN_MINT> \
  <AMOUNT> \
  <ESCROW_ACCOUNT> \
  --allow-unfunded-recipient \
  --allow-non-system-account-recipient \
  --with-memo "<job_id>"
```

The token mint depends on the currency:
- USDC: `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU` (devnet)
- EURC: check the node's config (devnet)

**Step 3:** Resubmit with payment proof
```bash
curl -X POST http://<ip>:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: my-agent-id' \
  -H 'x-payment: {"tx":"<signature>","amount_micros":<amount>}' \
  -d @job.json
```

The `x-payment` header is JSON:
```json
{
  "tx": "transaction_signature_from_step_2",
  "amount_micros": 10000
}
```

## Job JSON Format

```json
{
  "job_id": "unique-job-id",
  "image": "docker-image-or-busybox",
  "command": ["arg1", "arg2"],
  "env": ["KEY=value"],
  "devices": {
    "class": {"kind": "cpu"},
    "vcpus": 1,
    "mem_kb": 65536,
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
- `docs/INTERNET-CONNECTIVITY.md` — Network architecture
- `README.md` — Project overview
