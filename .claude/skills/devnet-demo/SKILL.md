---
name: devnet-demo
description: Run Vtessera's Solana devnet end-to-end flows — the x402 paid-job demo, offer-index demo, settlement demo, and the soak runner. Use whenever the user wants to demo, verify, or debug the pay→run→settle→split flow, mentions x402, escrow payments, devnet, soak testing, or asks "does the whole flow still work". Explains the Solana-side setup (keypairs, airdrops, env vars) — do not assume the user knows Solana tooling.
---

# Vtessera devnet end-to-end flows

Four scripted flows, all against **Solana devnet** (mainnet is explicitly
deferred — see MAINNET-CHECKLIST.md; never point anything at mainnet).

The escrow program is deployed at
`6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` (devnet).

## The demos (repo root)

```bash
./scripts/x402-demo.sh          # agent pays escrow via x402, submits a paid job
./scripts/offer-index-demo.sh   # two nodes, FCFS claims, MCP discover
./scripts/settlement-demo.sh    # node → signed receipt → vtessera-settle
```

`x402-demo.sh` reuses a node already answering on `127.0.0.1:8402` (e.g. the
Flatpak GUI's) or builds and starts its own. Useful env overrides:
`VTESSERA_NODE_URL`, `VTESSERA_OFFER_MODE` (free|paid), `VTESSERA_BACKEND`
(noop-cpu|local-cpu), `VTESSERA_JOB_SECONDS`.

## Soak runner

`crates/devnet-demo` is **excluded from the workspace** (it pins the Solana
1.18 SDK); build and run it from its own directory:

```bash
cd crates/devnet-demo
VTESSERA_PAYER=~/.config/solana/id.json cargo run --bin soak -- --iterations 10
```

- `VTESSERA_PAYER` points at a funded devnet keypair (CI uses the
  `DEVNET_PAYER_KEYPAIR` secret instead; hourly soak lives in
  `.github/workflows/soak-devnet.yml` — 100 iters, parallel 3).
- Transient `Blockhash not found` errors are retried with backoff by design
  (commit cdbcfa7) — a few retries in the output are normal, not failures.

## Solana prerequisites (when things fail before the demo even starts)

- A local keypair: `solana-keygen new` → `~/.config/solana/id.json`.
- Point the CLI at devnet: `solana config set --url devnet`.
- Fund it: `solana airdrop 2` (devnet SOL is free; airdrops rate-limit —
  retry or use https://faucet.solana.com).
- Paid flows also need devnet EURC/USDC test tokens in the payer's
  associated token account; the demo scripts print what's missing.
- Every paid transaction also pays the flat protocol fee
  (100,000 lamports SOL), so the payer needs SOL even for stablecoin jobs.

## Debugging

- Inspect any transaction/account on devnet:
  `https://explorer.solana.com/?cluster=devnet` (paste the signature or
  address the script printed).
- `x402-client` is also excluded from the workspace — build it in its own
  directory like devnet-demo.
