# The vtessera sandbox

`https://vtessera-sandbox.fly.dev` is a second, public deployment of the same
binary: the place to develop an agent against a live vtessera without touching
the mainnet app or its signing key. It moves no value — settlement is off-chain
only and the service refuses anything else — and its data is disposable by
design, **reset every week**.

The live app at `https://vtessera.fly.dev` settles real value on mainnet-beta
and holds the marketplace signing key on its volume. Its runbook is
[`docs/deploy.md`](deploy.md). The two deployments share no state, no secret and
no key: `make fly-deploy` deploys one, `make fly-sandbox-deploy` deploys the
other.

## What it is for

An agent pointed at the sandbox runs the whole journey — handshake, registry,
offers, negotiation, the hash-chained ledger, signed tessera receipts, AGP
intent routing — against a URL that resets weekly, so a test that leaves junk
behind stays a test. It is also where the threat model's open "a cap is per
identity and identities are free" stops mattering: there is nothing to protect,
because there is nothing to move.

## How it differs from the mainnet app

| | Mainnet app | Sandbox |
| --- | --- | --- |
| Settlement | mainnet-beta, real value | **off-chain only**; on-chain trades refused `501` |
| Chain config | `VTESSERA_CLUSTER` / `VTESSERA_RPC_URL` / `VTESSERA_MAINNET_ACK=1` secrets | **none**, and must stay none |
| `--sandbox` | no | **yes**, `/healthz` reports `"sandbox": true` |
| Spending caps | $10 / $10 per trade and day | **$1,000,000 / $1,000,000** (see below) |
| Rate limits | on by default | on by default — same flags, same defaults |
| Admin routes | `--admin-token` / `--admin-operators` | **unregistered**: no token, so the routes 404 rather than 403 |
| Machines / volume | 1 machine, 1 GB, kept warm, 5 daily snapshots | 1 machine, 1 GB, cold, **no snapshots** |
| `verificationKey` | must never change | **changes on every reset**, and that is the announcement |
| Reset | never — losing the key is a new identity | weekly, `make sandbox-reset` |

### Why the caps are $1,000,000 and not off

They cannot be off. A non-positive cap is a startup error
(`spend_cap` rejects zero; `limits.ErrCapNotPositive`), and that is deliberate:
a cap that quietly did not apply to some currencies or some callers would be a
way around the cap rather than a weaker one — see constraint 7 in `AGENTS.md`.

The sandbox therefore declares the largest figure it can instead. It is a real,
enforced, operator-declared number that sits far above anything a sandbox loop
reaches, so it never interferes with development, while the service's refusal
path still exists and still gets exercised. The mainnet app keeps $10/$10; do
not raise that one to match.

## One-time setup

Needs a Fly account and `fly` on the path. Same shape as `docs/deploy.md`, with
a separate app:

```bash
fly auth login
fly apps create vtessera-sandbox
fly secrets set VTESSERA_SESSION_SECRET="$(openssl rand -hex 32)" \
                VTESSERA_PUBLIC_BASE_URL=https://vtessera-sandbox.fly.dev
make fly-sandbox-deploy
make fly-sandbox-verify
```

Never set `VTESSERA_CLUSTER` or `VTESSERA_RPC_URL` on this app: sandbox mode
refuses to boot with a chain configured, and that refusal is the guard that
keeps a sandbox from ever settling. It also never carries
`VTESSERA_MAINNET_ACK`, so even a mis-set endpoint could not settle mainnet
value through it.

## Acceptance check

```bash
curl -fsS https://vtessera-sandbox.fly.dev/healthz | jq '{sandbox, cluster, genesisHash, verificationKey}'
```

`sandbox` must be `true`, `cluster` and `genesisHash` must be absent, and
`verificationKey` must differ from the mainnet app's (a sandbox tessera that
verified under the mainnet key would be a bug in the isolation). An on-chain
trade attempt must return `501 ONCHAIN_UNAVAILABLE`.

## How data resets

Weekly, and only weekly — the cadence that keeps state bounded and stale
identities gone without churn while someone is developing against it:

```bash
make sandbox-reset
```

It destroys every machine and volume of the sandbox app, recreates the volume,
and redeploys. The volume holds the SQLite database *and* the signing key, so
both go together: a new `verificationKey` at `/healthz` is the reset
announcement, and any tessera an agent is still holding from before is a
souvenir. Nothing is snapshotted — paying to back up throwaway data is the
wrong trade, and a restored key would resurrect an identity clients were told
had been replaced.

A reset wipes registered agents, offers, trades and the ledger. Re-handshaking
is two signed calls, which is exactly why a disposable instance is usable.

The equivalent by hand:

```bash
fly machine  destroy <id>     -a vtessera-sandbox --force
fly volumes  destroy <id>     -a vtessera-sandbox --yes
fly volumes  create vtessera_sandbox_data -a vtessera-sandbox --size 1 --region fra --yes
make fly-sandbox-deploy
```

## Abuse limits

It moves no real value, so the exposure is CPU and someone else's RPC quota
(there is none of that either, since no RPC is configured), not funds. The
first response to abuse is `make sandbox-reset`, which stops the state and can
be done from a phone. The second is destroying the app, which costs nothing to
undo.

## Expected cost

One stopped-or-cold `shared-cpu-1x` machine with 256 MB plus a 1 GB volume,
**under $5 a month**. The machine is set to stop when idle
(`auto_stop_machines`), so a sandbox nobody is using costs the volume only and
the first request of the day pays a cold start.
