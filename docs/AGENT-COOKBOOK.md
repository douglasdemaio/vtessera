# Agent Cookbook — Buy and Sell Vtessera Compute

Drop-in recipes for AI agents. **Buying** — find compute, run free or paid
jobs. **Selling** — run a node yourself (Flatpak or KVM) and charge other
agents, or donate capacity. Every recipe is a complete copy-paste block.

Context: [AGENTS.md](../AGENTS.md) is the tour guide;
[docs/AGENT-ONBOARDING.md](AGENT-ONBOARDING.md) walks a real paid job with
exact field locations. This file is the "just paste it" reference.

---

## R1 — Find compute

```bash
# Same machine, node running locally
vtessera-agent --local health
vtessera-agent --local offer

# Whole network — free + paid, claimed + unclaimed, one table
vtessera-agent overview \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json

# Only free, unclaimed capacity
vtessera-agent overview --mode free --available

# Only GPU boxes (if any), machine-readable
vtessera-agent overview --device nvidia_gpu --json
```

Columns: `NODE_ID  DEVICE  PRICE  REACH  DIAL  CLAIM  CAND  HEARTBEAT`.

Picking a node by hand from the raw marketplace:

```bash
curl -s https://douglasdemaio.github.io/vtessera/nodes.json
```

Each `nodes[i]` is one machine. `price.mode` is `free` or `paid`; a node with
no `candidates` array is not dialable over iroh (LAN-only reach).

---

## R2 — Run a free job

Jobs are just JSON. Submit with curl or the agent CLI:

```bash
# job.json
cat > /tmp/job.json <<'EOF'
{
  "job_id": "agent-$(date +%s)",
  "image": "busybox",
  "command": ["echo", "hello", "from", "vtessera"],
  "env": [["MY_AGENT_ID", "agent-7"]],
  "devices": {"class": {"kind": "cpu"}, "vcpus": 1, "mem_kb": 131072, "min_vram_mb": 0},
  "max_duration_secs": 60
}
EOF

vtessera-agent --node http://<ip>:8402 submit --job /tmp/job.json
# or, same thing over HTTP:
curl -X POST http://<ip>:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: agent-7' \
  -d @/tmp/job.json
```

Response: `status: accepted`, `job_id`, `backend`, signed `receipt`, and
`metering`. (If `backend` is `noop-cpu`, that node has no isolation; if it is
`cloud-hypervisor`, your job ran inside a real microVM.)

> `job_id` must be unique per node. Use a timestamp/UUID — see the "already
> exists" tip in `AGENTS.md`.

---

## R3 — Run a paid job (x402, over iroh — works from any network)

Needs: a Solana devnet wallet with USDC (payer), and the marketplace JSON.

<table>
<tr><th>CLI flag</th><th>Where to read it from the marketplace</th></tr>
<tr><td><code>--node-id</code></td><td><code>nodes[i].endpoint_id</code> (top-level; NOT <code>node_id</code>)</td></tr>
<tr><td><code>--seller</code></td><td><code>nodes[i].offer.body.price.payout_id</code></td></tr>
<tr><td><code>--mint</code></td><td><code>usdc</code> → <code>4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU</code> (devnet); <code>eurc</code> → ask the node</td></tr>
</table>

```bash
# Dry-run first — refuses to proceed unless READY
crates/x402-client/target/debug/vtessera-x402-client \
  --node-id <endpoint_id> \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller <payout_id> \
  --seconds 60 \
  --check

# Then the real thing
crates/x402-client/target/debug/vtessera-x402-client \
  --node-id <endpoint_id> \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller <payout_id> \
  --seconds 60
```

Payer keypair: `VTESSERA_PAYER` or `~/.config/solana/id.json`. On the same
LAN you can substitute `--node http://<ip>:8402` instead of `--node-id`.

---

## R4 — Sell: free node in ~60 seconds (Flatpak GUI)

Installing the Flatpak gets you a node with zero setup. Toggle **"Accept
workloads from others"** in the GUI and agents on your LAN (or anywhere, if
you also publish) can submit jobs to you for free.

```bash
# Local build: packaging/flatpak/vtessera.flatpak (see packaging/flatpak/README.md)
flatpak install --user ./vtessera.flatpak
flatpak run io.github.douglasdemaio.Vtessera      # toggle "Accept workloads"
# CLI equivalent, if you prefer no GUI:
flatpak run --command=vtessera-node io.github.douglasdemaio.Vtessera \
  --bind 127.0.0.1:8402 --offer offer.json --key key.bin \
  --escrow 8UJy6B2ZX3swc6XLgGzWeEfcrP7ujZyBcFrKfp5YkA47 --network solana-devnet \
  --state-dir ~/.local/share/vtessera
```

---

## R5 — Sell: hardened KVM node (microVM per job)

Per-job isolation. Requirements: `/dev/kvm`, `cloud-hypervisor`,
`busybox-static`, virtiofs+fuse kernel modules (all present on openSUSE
Tumbleweed by default). One-time sudo for the initramfs + state dir, then
everything runs unprivileged.

```bash
sudo ./scripts/kvm-node-demo.sh setup    # once — builds guest initramfs
./scripts/kvm-node-demo.sh run           # node serving /offer on :8402 (KVM backend)
```

`run` boots a real VM for each demo job (`echo`, exit-code, timeout),
then prints the signed receipts. On machines without KVM, `noop-cpu`
handles trusted workloads:

```bash
./scripts/local-stack.sh start           # offer-index + node + marketplace, free mode
```

---

## R6 — Sell: charge other agents (paid node)

To earn stablecoin instead of giving it away:

1. **A payout wallet** — Solana public key (base58). This becomes your
   offer's `price.payout_id`; buyers settle into it.
2. **An escrow contract** — devnet: use the demo account
   `8UJy6B2ZX3swc6XLgGzWeEfcrP7ujZyBcFrKfp5YkA47` (program deployed; see
   `MAINNET-CHECKLIST.md` before anything, well, mainnet).
3. **A paid offer** with your price + payout:
   ```bash
   openssl rand 32 > key.bin && chmod 600 key.bin
   cargo run -p vtessera-node-api --example gen_offer -- paid \
       --key key.bin \
       --payout <your-wallet> \
       --endpoint http://<your-ip>:8402 \
       > offer.json
   ```
   (gen_offer bakes a default price of 100 µ-tokens/second; the offer JSON
   you edit if you want a different one.)
4. **Run the node** and advertise where agents already look:
   ```bash
   cargo build -p vtessera-node-api --bin vtessera-node --features serve
   ./target/debug/vtessera-node \
       --bind 0.0.0.0:8402 \
       --offer offer.json \
       --key key.bin \
       --escrow 8UJy6B2ZX3swc6XLgGzWeEfcrP7ujZyBcFrKfp5YkA47 \
       --network solana-devnet \
       --backend cloud-hypervisor \
       --state-dir ~/.local/share/vtessera \
       --publish http://<index-ip>:8403 --publish-interval 30 \
       --marketplace
   ```
   `--publish` registers with your offer-index (LAN agents); `--marketplace`
   registers with the public GitHub Pages marketplace (any agent, with an
   iroh-relayed endpoint). After that:

   ```bash
   vtessera-agent overview --mode paid --available   # ...should list YOU
   ```

5. **The money path** (what happens per job): buyer hits `/offer` → node
   challenges with x402 terms → buyer pays per-second stablecoin into the
   per-job escrow → job runs → `finalize_pro_rata` splits the escrow: your
   earned slice to `payout_id`, remainder refunded to the buyer. No you→us
   custody; the stablecoin is the same mint the buyer paid.

Full walkthrough of the buyer side: `docs/AGENT-ONBOARDING.md`.

---

## R7 — Meet agents where they look (discoverability)

Agents find this network three ways — register with all of them:

| Where agents look | How your node shows up |
| --- | --- |
| **Offer-index** (per-network) | node `--publish http://<index>:8403`; `vtessera-agent discover --index …` |
| **Public marketplace** (any network) | node `--marketplace` → `vtessera.douglasdemaio.workers.dev/register` → `nodes.json` |
| **MCP server** (`vtessera-mcp`) | `vtessera-agent` toolserver users can `discover`, `offer`, `submit_job` without writing HTTP |

A free node that also publishes is a donation; a paid node that publishes is
a business. Both cost the same to stand up.

---

## R8 — Debugging quick table

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `connection refused` | node not up / wrong port | `curl http://<ip>:8402/healthz` |
| `402 payment required` | node is paid mode | walk R3 (x402 client) |
| `no running node found` | stale discovery file | use `--node http://…` explicitly |
| `job_id already exists` | reused job id | `job-$(date +%s)-$(shuf -i 1-999 -n 1)` |
| `missing job_id` on proof | legacy `x-payment` header | use `vtessera-x402-client` (builds full proof) |
| KVM node won't boot | `/var/lib/vtessera` perms | `sudo ./scripts/kvm-node-demo.sh setup` |