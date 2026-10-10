# Hosted sandbox — proposal (2026-10-08)

**Approved 2026-10-10, and built.** The four decisions at the end are recorded
with the operator's answers, two of which overrode the defaults this proposal
argued for. The runbook is [`docs/sandbox.md`](../sandbox.md), the config is
`fly.sandbox.toml`, and the targets are `make fly-sandbox-deploy`, `make
fly-sandbox-verify` and `make sandbox-reset`.

A second, public deployment of vtessera that anyone can point an agent at: the
same binary, off-chain only, with disposable data. The live app at
`vtessera.fly.dev` settles real value on mainnet-beta and holds the marketplace
signing key on its volume. This is the place to develop against without touching
either, and the place where the threat model's open "a cap is per identity and
identities are free" becomes a non-issue, because nothing moves here at all.

## What it needs

- A second Fly app (working name `vtessera-sandbox`), its own 1 GB volume, its
  own signing key created on first boot — the key on the volume is the
  deployment's identity, and a different `verificationKey` at `/healthz` is
  what makes a sandbox tessera distinguishable from a mainnet one.
- The same Containerfile. No code changes: cluster awareness (Phase 3) means
  the sandbox is configuration, not a fork.
- Configuration:
  - **No chain, and `--sandbox`.** The operator's decision was off-chain only —
    the opposite of this proposal's original default of devnet settlement — so
    `VTESSERA_CLUSTER` and `VTESSERA_RPC_URL` are unset and stay unset, and
    `VTESSERA_SANDBOX=1` refuses on-chain settlement outright. The service
    refuses to boot with a chain configured alongside sandbox mode, so the
    isolation is a boot-time property rather than a convention.
  - `VTESSERA_MAINNET_ACK` unset, and it must stay unset — the service
    refuses mainnet-beta without it.
  - Fresh `VTESSERA_SESSION_SECRET`; no secret shared with the mainnet app.
  - Spend caps declared at $1,000,000 rather than at the defaults or off —
    see "Decisions" below: a cap cannot be zero.
- A page in the docs saying, in one sentence, that this instance moves no
  value and resets on a schedule.

## How it stays isolated from the mainnet app

- Separate Fly app, separate volume, separate SQLite database, separate
  signing key, separate secrets. There is no shared state to leak, and no
  path by which a sandbox write reaches the mainnet database.
- Sandbox mode refuses to boot alongside an RPC endpoint, and the sandbox
  carries no chain configuration at all, so it cannot settle anything. It also
  never carries `VTESSERA_MAINNET_ACK`, so even a misconfigured endpoint could
  not settle mainnet value through it.
- `/healthz` reports `sandbox: true` and no cluster or genesis hash, so a
  client can tell which instance it is talking to without trusting the
  hostname, and a mainnet tessera will not verify under a sandbox key.
- Deploys are separate: a sandbox deploy cannot touch the mainnet machine,
  its volume, or its secrets.

## How data resets

Weekly, by decision. The volume is disposable by design, and `make
sandbox-reset` destroys every machine and volume of the sandbox app, recreates
the volume and redeploys:

```
fly machine destroy <sandbox machine>  -a vtessera-sandbox --force
fly volumes destroy <sandbox volume>  -a vtessera-sandbox --yes   # key and database go
fly volumes create vtessera_sandbox_data -a vtessera-sandbox --size 1 --region fra --yes
make fly-sandbox-deploy                                   # new key, new verificationKey
```

No volume snapshots: paying to back up throwaway data is the wrong trade, and a
stale snapshot would resurrect a signing key that clients believe was retired.
A change in `verificationKey` at `/healthz` is the reset announcement — anything
holding an old tessera from the sandbox was holding a souvenir.

## Expected cost

The same shape as production, minus snapshots, minus a warm machine: a
`shared-cpu-1x` with 256 MB that stops when idle (about $3/month) plus a 1 GB
volume in one region (about $1.5/month), with no RPC egress because there is no
RPC. Call it **under $5 a month**, and the first abuse response is resetting the
volume, which stops the state and can be done from a phone.

## Abuse limits

- **It moves no real value.** Settlement is off-chain only, refused at boot, so
  the exposure is CPU, not funds. Spending caps still apply and are still
  enforced — they are declared at $1,000,000 because a cap cannot be zero — so
  they bound runaway agent loops without ever getting in the way.
- **Rate limits are on, at the same defaults as the mainnet app.** (This
  proposal originally assumed they were absent; they are not — the rate-limiting
  work landed first. It costs nothing to keep them.)
- **The admin routes stay unregistered** (no `--admin-token`): a public
  sandbox needs no retirement ceremony, and the reset is the hammer.
- **Cost ceiling:** one small machine, one small volume, and a reset that
  destroys both state and any reason to keep probing. If it is abused past
  that, the honest answer is turning it off, not hardening a demo.

## What building it took

A second Fly config (`fly.sandbox.toml`), the `fly-sandbox-deploy`,
`fly-sandbox-verify` and `sandbox-reset` targets, and a docs page — no Go
changes, exactly as predicted. The acceptance check changed with the settlement
decision: with no cluster there is nothing for `make preflight-live` to verify,
so it is instead that `/healthz` reports `sandbox: true` with no cluster and
that an on-chain trade returns `501 ONCHAIN_UNAVAILABLE`.

## Decisions (recorded 2026-10-10)

1. **Reset cadence: weekly.** Keeps state bounded and stale identities gone,
   without churn while someone is developing against it.
2. **Name:** `vtessera-sandbox`, per this proposal's working name —
   `vtessera-sandbox.fly.dev` was free, and a subdomain of vtessera.fly.dev
   would read as more official than it is.
3. **Caps: "off" — recorded as $1,000,000, because off is not a state the
   service has.** `parseCap` rejects a non-positive figure and `limits` has no
   disabled mode; that is deliberate, per constraint 7: a cap that quietly did
   not apply would be a way around the cap. The declared figure satisfies the
   operator's intent (never interfere with development) while keeping the cap a
   real enforced number. The mainnet app stays at $10/$10.
4. **On-chain devnet settlement: off.** Off-chain only, with `--sandbox`,
   which shrinks the surface (no chain, no fee wallet, no RPC quota) at the cost
   of not exercising the on-chain path here. That path is already covered by
   `make test-devnet`.
