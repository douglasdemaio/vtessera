# Repointing a paid vtessera-node at the D4iX escrow deployment

Operational notes and commands for switching a paid `vtessera-node` so its
x402 flow settles instead of failing `finalize` with `NotSettlementAuthority`.

> **Superseded by the per-contract settlement-authority rework.**
> Once a program built from the reworked source (`Contract` records its
> own `settlement_authority` at `pay_for_compute`; `finalize_pro_rata` gates
> on that, never on `Config`) is deployed, a buyer can settle through **any**
> deployment that accepted their payment — the config PDA owner is
> irrelevant. The repoint below only matters for the **currently deployed
> bytes** (`6jK6…`, `D4iX…`), which still gate on the (seized) config
> authority. After that redeploy, nodes can keep pointing at whatever
> program they already verify payments against.

## Why

The shared escrow program `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` has a
config PDA whose settlement authority is `Dtb4KYwzrEUomtWTcBJ1DziTzHbfHDyp9RPmRbjKuGVA`
(a throwaway CI soak key that nobody holds). `finalize_pro_rata` requires
`config.settlement_authority == settlement_authority.key()`
(programs/vtessera-escrow/src/lib.rs:426), so no run through `6jK6…` can ever
settle from a laptop.

The clean-deploy path is already done: escrow program
`D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn` was deployed to devnet and its
config PDA `3CHz4ruzxTJaK1Vkt4rRgxjabdqGRzFxe4gRE7MvmDQx` was `init_config`'d
with settlement authority **`34Wxj37y8yCynoxsqkvZ5o2Wj3xH36XFkQ1AVUpawCZB`**
(our client payer). Finalize through it succeeds — verified on-chain
(`Program D4iX… Instruction: FinalizeProRata`, escrow drained, seller credited).

The only thing still pointing at the old program is each **node's**
`--escrow` argument: the node challenges buyers with that account and verifies
the on-chain payment landed there (vtessera_node.rs:942 derives the expected
escrow ATA and checks the transfer; the 402 challenge echoes
`escrow_account`). No key rotation and no redeploy are involved — the node is
stateless about escrow beyond this one flag.

## Key addresses (devnet)

| Thing | Address |
| --- | --- |
| New escrow program | `D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn` |
| New config PDA (authority `34Wxj…`) | `3CHz4ruzxTJaK1Vkt4rRgxjabdqGRzFxe4gRE7MvmDQx` |
| Old escrow program (keep for rollback) | `6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma` |
| Client payer / buyer | `34Wxj37y8yCynoxsqkvZ5o2Wj3xH36XFkQ1AVUpawCZB` |
| Seller payout (node offer `payout_id`) | `5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs` |
| USDC mint (devnet) | `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU` |
| Fee wallet | `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh` |

## Repoint the node

1. Find the node's current command line. If it runs under systemd / Flatpak /
   the daemon, read the unit/launcher; otherwise check `pgrep -af vtessera-node`
   on the box. You want the exact `--offer`, `--key`, `--state-dir`, and any
   `--marketplace`/`--publish` arguments — those stay unchanged.

2. Stop the node.

3. Start it again with exactly the same arguments, but replace `--escrow
   6jK6oEaLt…` with `--escrow D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn`.

   Example (paths are the other laptop's real ones):

   ```bash
   vtessera-node \
     --bind 0.0.0.0:8402 \
     --offer "$CFG/offer.json" \
     --escrow D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn \
     --network solana-devnet \
     --backend local-cpu \
     --key "$CFG/identity.key" \
     --state-dir "$DATA/vtessera" \
     --max-concurrent-jobs 1
   ```

   No other argument changes. The node's signed offer (including
   `payout_id 5fML…`) stays as-is.

## Verify

Run these from the operator laptop (the x402-client build lives there; the
target node must be reachable over the LAN).

```bash
# 1. Node is up and still advertises the paid offer with the same payout.
curl -s http://192.168.178.82:8402/offer | head -c 400

# 2. The 402 challenge now carries the new escrow program.
curl -s -X POST http://192.168.178.82:8402/jobs \
  -H 'Content-Type: application/json' \
  -H 'x-agent-id: ops-check' \
  -d '{"job_id":"repoint-verify","image":"busybox","command":["echo","hi"],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":65536,"min_vram_mb":0},"max_duration_secs":1}' \
  | grep escrow_account
#   → escrow_account: D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn

# 3. Pre-flight from the x402-client (PR #114 build) — all five checks PASS.
cd crates/x402-client
VTESSERA_PAYER=$HOME/.config/solana/payer.json \
  target/debug/vtessera-x402-client \
  --node http://192.168.178.82:8402 \
  --check \
  --program D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller 5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs
#   → RESULT: READY — safe to submit the paid job.

# 4. Full paid run: pay → node accepts + executes (200) → finalize settles.
VTESSERA_PAYER=$HOME/.config/solana/payer.json \
  target/debug/vtessera-x402-client \
  --node http://192.168.178.82:8402 \
  --program D4iXSnHJfW8qh1Zh4AK7rh4mXC8G6RNcSmkvR6vrmcCn \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller 5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs \
  --seconds 1 --verbose
```

The run is a success when it prints, in order: `escrow balance after deposit:
<job price> micros`, `POST /jobs → 200`, `finalize_pro_rata` with no
`0x1770`/`NotSettlementAuthority` hint, `escrow ATA: 0 micros`, `seller ATA: …
(was …, delta <job price> micros)`, and `success: …` (`exit 0`).

## Expected failure if the repoint didn't take

`--check` check [2] fails:

```
[2] x402 challenge
    FAIL  402 challenge escrow_account 6jK6… != program D4iX… (network solana-devnet)
```

…and the full run dies at step 5 with the `NotSettlementAuthority` hint (the
same signature as a node still running the old value).

## Rollback

Relaunch with `--escrow 6jK6…` again. Repointing touches nothing persistent —
no state migration, job queue untouched, signed offer unchanged. Both programs
stay deployed on devnet; you can flip the flag back and forth at will.

## Notes

- The parked escrow balances on the old `6jK6…` program (e.g. the two
  unreconciled jobs) are **not** touched by this repoint. They cannot be
  drained until someone rotates `6jK6`'s config authority away from the
  throwaway `Dtb4KY…` key via `update_config` (lib.rs:92) — out of scope here.
- `--check` verifies the offer `payout_id` matches `--seller`, so keep the
  node's payout at `5fML…` (already the case).
- The client always skips `init_config` against `D4iX…` because the config PDA
  is already initialized (idempotent skip in
  crates/x402-client/src/main.rs) — that's the state from the original deploy,
  not something to redo.
- Node verification requires the devnet RPC to be reachable from the node's
  host (same requirement as before the repoint).