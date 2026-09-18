# Agent Onboarding — Running Your First Paid Job

This is the step-by-step for an AI agent (or human) that wants to **find a
paid node and run a job on it** — even when the node is behind a home NAT,
with **no router configuration** on either side. It is the reference version
of the Quick Start in [AGENTS.md](../AGENTS.md).

**Worked example uses a real node from the live marketplace.** Every value
in the examples is taken from the public registry that exists today, so you
can follow along end-to-end. Substitute your own picks where it says so.

---

## 0. What you need before you start

| Thing | Where it comes from |
| --- | --- |
| A Solana devnet wallet with USDC | `~/.config/solana/id.json`, or `VTESSERA_PAYER` (`vtessera-x402-client` reads one of these) |
| The marketplace URL | `https://douglasdemaio.github.io/vtessera/nodes.json` |
| The `vtessera-x402-client` binary | prebuilt at `crates/x402-client/target/debug/vtessera-x402-client`, or build it with `cargo build --manifest-path crates/x402-client/Cargo.toml` |

> Use `--check` first on any paid job — it dry-runs every check (offer,
> payout, challenge, config PDA, funds, seller ATA) and refuses to proceed
> unless `READY`.

If you want to see the wallet's balances before you pay:

```bash
# SOL (for transaction fees)
spl-token balance <MINT> --owner "$(solana address)"
# wallet must be the payer VTESSERA_PAYER/~/config/solana/id.json resolves to,
# and must hold devnet SOL + devnet USDC of the --mint you pass.
```

---

## 1. Fetch the marketplace and pick a node

```bash
curl -s https://douglasdemaio.github.io/vtessera/nodes.json
```

The file is a JSON object with a `nodes` array. **Each entry in `nodes` is
one machine.** Pick one. For the worked example we use this entry
(the first node in the list today):

```json
{
  "node_id": "793754d2844bfc814b0494560dd1c750",
  "offer": {
    "body": {
      "device": { "kind": "cpu", "mem_mb": 5651, "vcpus": 8 },
      "endpoint": ["http://192.168.178.82:8402"],
      "endpoint_id": "13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240",
      "node_id": "793754d2844bfc814b0494560dd1c750",
      "price": {
        "mode": "paid",
        "currency": "usdc",
        "per_device_second_micros": 3903,
        "payout_id": "5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs"
      }
    }
  },
  "endpoint_id": "13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240",
  "candidates": [
    { "addr": "https://euc1-1.relay.n0.iroh.link./", "kind": "relayed", "transport": "iroh_quic" },
    { "addr": "192.168.178.82:44839", "kind": "host", "transport": "iroh_quic" },
    { "addr": "217.61.144.128:44839", "kind": "host", "transport": "iroh_quic" }
  ]
}
```

---

## 2. Copy the three values you must pass to the CLI

The CLI takes four inputs: `--node-id`, `--marketplace`, `--mint`, and
`--seller`. **Three of them you read out of this one JSON entry.** Here is
exactly where each one lives:

| CLI flag | JSON path in the marketplace entry | Worked example value |
| --- | --- | --- |
| `--node-id` | `nodes[i].endpoint_id` (top-level) — same value as `offer.body.endpoint_id` | `13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240` |
| `--seller`  | `offer.body.price.payout_id` | `5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs` |
| `--mint`    | from `offer.body.price.currency` → the table below | `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU` (USDC devnet) |

Mapping `price.currency` → `--mint` (stablecoin account formats):

| `price.currency` | `--mint` value |
| --- | --- |
| `usdc` | `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU` (devnet) |
| `eurc` | check the node — EURC devnet mint (varies by deployment) |

**How to read it with `jq`** (so you don't eyeball it):

```bash
node_id="793754d2844bfc814b0494560dd1c750"

# Top-level endpoint_id of that node (this is --node-id)
curl -s https://douglasdemaio.github.io/vtessera/nodes.json \
  | jq -r --arg n "$node_id" '.nodes[] | select(.node_id==$n) | .endpoint_id'

# Its payout_id (this is --seller)
curl -s https://douglasdemaio.github.io/vtessera/nodes.json \
  | jq -r --arg n "$node_id" '.nodes[] | select(.node_id==$n) | .offer.body.price.payout_id'

# Its currency mode + price (sanity check: "paid", not "free")
curl -s https://douglasdemaio.github.io/vtessera/nodes.json \
  | jq -r --arg n "$node_id" '.nodes[] | select(.node_id==$n) | .offer.body.price | "\(.mode) \(.currency) \(.per_device_second_micros)/s"'
```

> **What each value means, so you trust it:**
> - `endpoint_id` is the node's **iroh identity**. The CLI dials it directly
>   over iroh QUIC — it does **not** use the `endpoint` URL (that `http://…`
>   is a LAN address; unreachable from outside the home network). The
>   `candidates` list tells iroh *how* to reach the endpoint (relay / hole-punch),
>   which is why no router port-forwarding is needed.
> - `payout_id` is the node operator's **Solana wallet** that collects your
>   payment after the job runs. The CLI refuses to pay if `--seller` ≠
>   `payout_id` — because otherwise your money would settle to the wrong
>   wallet and the operator would never see it.
> - `per_device_second_micros` × your `--seconds` = **exact price in
>   micro-units** (1,000,000 micros = 1 whole token). The example node
>   charges 3903 micros/sec; a `--seconds 60` job costs 234,180 micros
>   (≈ 0.23 USDC).

You can also discover the same fields by dialing the node directly with the
`vtessera-agent` CLI:

```bash
vtessera-agent --node-id 13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240 \
  --index http://127.0.0.1:8403 \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  offer
```

> **Caveat — check the `candidates` array.** A *dialable-over-iroh* node has
> a non-empty `candidates`. A node listed without `candidates` (or with an
> empty one) has no iroh paths and cannot be reached over iroh — pick a
> different node, or fall back to its `endpoint` URL only if you are on the
> same LAN.

---

## 3. Pre-flight check (always do this before paying)

```bash
crates/x402-client/target/debug/vtessera-x402-client \
  --node-id 13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240 \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller 5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs \
  --seconds 60 \
  --check
```

Wait for `READY — safe to submit the paid job.` If it reports `NOT READY`,
fix the listed failures — do **not** run the paid job.

## 4. Run the paid job

```bash
crates/x402-client/target/debug/vtessera-x402-client \
  --node-id 13b53df7dc9b2c5e85445ac34b36181fa8824f61d0f7d95fc26d0156e00cd240 \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller 5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs \
  --seconds 60
```

The CLI automates the whole flow and prints every step:

1. `GET /offer` — quotes the price.
2. `POST /jobs` → **402 challenge** (x402 payment terms + escrow account).
3. On-chain `pay_for_compute` — your USDC into the per-job escrow. Prints a
   Solana tx signature (copy it for the explorer links at the end).
4. `POST /jobs` again **with the full payment proof** → expect
   `200 OK` with `"status":"accepted"` and a signed receipt.
5. `finalize_pro_rata` — splits the escrow: seller gets the earned slice,
   excess refunded to you. **escrow ATA ends at 0 micros.**

**Success looks like** (final lines):

```
escrow ATA:  0 micros  (expected 0)
seller ATA:  [REDACTED] (balance change detected: true)
success: paid 234180 micros into the escrow program, node accepted the job, seller paid out.
explorer:
  https://explorer.solana.com/tx/<pay_sig>?cluster=devnet
  https://explorer.solana.com/tx/<finalize_sig>?cluster=devnet
```

---

## 5. Pitfalls and where they show up

| Symptom | What it usually means | Fix |
| --- | --- | --- |
| `no node with endpoint_id …` | wrong `--node-id`, or you pasted the `node_id` instead | copy `endpoint_id` (top-level), not `node_id` |
| `offer pays out to X but … would receive the escrow instead` | `--seller` ≠ `offer.body.price.payout_id` | pass the exact `payout_id` as `--seller` |
| `--node` times out / `connection refused` | you're using the LAN `http://… endpoint` URL from outside the home network | switch to `--node-id` (iroh) |
| preflight `NOT READY` on payer funds | wallet has no devnet SOL or no devnet USDC of that mint | fund `~/.config/solana/id.json` wallet; airdrop/have USDC minted |
| `missing job_id` on proof | legacy pay path (`x-payment: {"tx",…}` only) doesn't satisfy the modern verifier | use `vtessera-x402-client` (builds the full proof) |

---

## Where the flow lives in the code

- CLI / full proof build / `pay_for_compute`: `crates/x402-client/src/main.rs`
- iroh candidate resolution + QUIC dial (no router config):
  `crates/x402-client/src/main.rs` (`resolve_node_candidates`,
  `http_request_iroh`), `crates/transport/src/iroh_sidecar.rs`
- Node-side verifier (which proof fields it requires): `crates/node-api/src/bin/vtessera_node.rs`
- Escrow program (per-job PDA, payout split): `programs/vtessera-escrow/`