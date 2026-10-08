# Hosted sandbox — proposal (2026-10-08)

A second, public deployment of vtessera that anyone can point an agent at:
the same binary, settled on devnet, with disposable data. The live app at
`vtessera.fly.dev` settles real value on mainnet-beta and holds the marketplace
signing key on its volume. This is the place to develop against without
touching either, and the place where the threat model's open "a cap is per
identity and identities are free" becomes a non-issue, because the money is
devnet money.

## What it needs

- A second Fly app (working name `vtessera-sandbox`), its own 1 GB volume, its
  own signing key created on first boot — the key on the volume is the
  deployment's identity, and a different `verificationKey` at `/healthz` is
  what makes a sandbox tessera distinguishable from a mainnet one.
- The same Containerfile. No code changes: cluster awareness (Phase 3) means
  the sandbox is configuration, not a fork.
- Configuration:
  - `VTESSERA_CLUSTER=devnet`, `VTESSERA_RPC_URL=https://api.devnet.solana.com`
  - `VTESSERA_MAINNET_ACK` unset, and it must stay unset — the service
    refuses mainnet-beta without it.
  - Fresh `VTESSERA_SESSION_SECRET`; no secret shared with the mainnet app.
  - Spend caps at the defaults ($10/$10). The governed mint table is
    already cluster-scoped and already governs devnet mints, which is what
    `make test-devnet` proves.
- A page in the docs saying, in one sentence, that this instance moves no
  value and resets on a schedule.

## How it stays isolated from the mainnet app

- Separate Fly app, separate volume, separate SQLite database, separate
  signing key, separate secrets. There is no shared state to leak, and no
  path by which a sandbox write reaches the mainnet database.
- The cluster is named in configuration and verified against the endpoint's
  genesis hash at boot, on every settlement request and on every reconciler
  tick. A machine pointed at the wrong chain fails loudly at boot. The
  sandbox will never carry `VTESSERA_MAINNET_ACK`, so even a misconfigured
  endpoint cannot settle mainnet value through it.
- Every request and receipt carries the cluster it belongs to, and
  `/healthz` reports `cluster`, `genesisHash` and `verificationKey`, so a
  client can tell which instance it is talking to without trusting the
  hostname.
- Deploys are separate: a sandbox deploy cannot touch the mainnet machine,
  its volume, or its secrets.

## How data resets

The volume is disposable by design. Reset is:

```
fly machine stop <sandbox machine>
fly volumes destroy <sandbox volume>   # the key and the database go with it
fly machine start <sandbox machine>    # new key, new verificationKey at /healthz
```

A `make sandbox-reset` target would wrap those three commands. No volume
snapshots: paying to back up throwaway data is the wrong trade, and a stale
snapshot would resurrect a signing key that clients believe was retired.
A change in `verificationKey` at `/healthz` is the reset announcement —
anything holding an old tessera from the sandbox was holding a souvenir.

## Expected cost

The same shape as production, minus snapshots: `shared-cpu-1x` with 256 MB
(about $3/month) plus a 1 GB volume in one region (about $1.5/month), plus
negligible egress to the public devnet RPC endpoint. Call it **under $5 a
month**, and the first abuse response is destroying the volume, which stops
the state and can be done from a phone.

## Abuse limits

- **It moves no real value.** Settlement is devnet-only, and the chain
  verification above makes that a property of the deployment rather than a
  promise. The spending caps still apply — $10 and $10 per rolling day per
  agent identity — in devnet stablecoins priced at par, which bounds runaway
  agent loops but is not money.
- **There is no rate limiting, and this is where that matters least.** The
  exposure on the sandbox is CPU and someone else's RPC quota, not funds.
  The mainnet app's rate-limiting gap is unchanged and stays the reason it
  is not a place for unfinished clients.
- **The admin routes stay unregistered** (no `--admin-token`): a public
  sandbox needs no retirement ceremony, and the reset is the hammer.
- **Cost ceiling:** one small machine, one small volume, and a reset that
  destroys both state and any reason to keep probing. If it is abused past
  that, the honest answer is turning it off, not hardening a demo.

## What building it would take

One `fly.toml` variant or a second app from the existing Containerfile, one
volume, one secrets set, the `sandbox-reset` target, and a docs page — an
afternoon of configuration and no Go changes. Acceptance check:
`make preflight-live CLUSTER=devnet` must pass against it, the same check
the mainnet app answers to.

## Decisions needed before building

1. **Reset cadence:** on demand, weekly, or never. Weekly keeps state
   bounded and stale identities gone; on demand is less machinery.
2. **Name:** `vtessera-sandbox.fly.dev` is free; a subdomain of
   vtessera.fly.dev needs DNS and would read as more official than it is.
3. **Caps:** keep $10/$10, or drop them — they cost nothing to keep and
   still bound loops.
4. **On-chain devnet settlement:** leave enabled (it is what makes the
   sandbox worth having for wallet work), or run it off-chain only for a
   smaller surface. My default is enabled.

Build nothing until this is approved.
