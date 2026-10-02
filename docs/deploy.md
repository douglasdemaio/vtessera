# Deploying vtessera

vtessera is a single Go binary with no runtime dependencies beyond the kernel.
This document turns that into a running service; it does not provision the host
or the DNS record, because those need the maintainer's account.

## What the service needs

Three things, and the first two are the ones that bite:

| Need | Why | Failure if forgotten |
| --- | --- | --- |
| A persistent volume at `/data` | Holds the database and the Ed25519 marketplace signing key | Every restart is a new marketplace. Registered agents, trades and the ledger are gone, and the verification key changes, so every previously issued tessera stops verifying. |
| `VTESSERA_SESSION_SECRET`, ≥32 bytes | Signs session tokens | The service refuses to start. |
| `VTESSERA_PUBLIC_BASE_URL` | Advertised in the agent card so agents know where to reach the gateway | The card omits `url`, so agents cannot discover the gateway. The service logs a warning at startup. |

Generate the secret once and store it in the deployment's secret store, not in a
unit file on disk:

```bash
openssl rand -hex 32
```

## Build

```bash
make image                     # podman; IMAGE=vtessera:local by default
```

The image is a multi-stage build: a Go compile stage, then Alpine with
`ca-certificates` and `tzdata`. It runs as uid 10001, never root. It contains no
key and no database — `.containerignore` excludes `data/`, so the marketplace
identity cannot be baked into a layer.

## Run

```bash
podman run -d --name vtessera --restart=unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v vtessera-data:/data \
  -e VTESSERA_SESSION_SECRET="$(openssl rand -hex 32)" \
  -e VTESSERA_PUBLIC_BASE_URL="https://<host>" \
  vtessera:local
```

Bind to `127.0.0.1`, not `0.0.0.0`, and put a TLS terminator in front. The
service speaks plain HTTP by design; it should not be the thing facing the
internet. The agent card must nonetheless advertise the public `https://` URL,
because that is what agents will call.

`--restart=unless-stopped` plus the container `HEALTHCHECK` is the whole
supervision story: the healthcheck probes `/healthz`, which is cheap and does
not touch the database.

### Without a container

```bash
make build
VTESSERA_SESSION_SECRET=... VTESSERA_PUBLIC_BASE_URL=https://<host> \
  ./bin/vtessera --addr 127.0.0.1:8080 --db /var/lib/vtessera/vtessera.db \
                 --signer-key /var/lib/vtessera/signer.key
```

Use a systemd unit with `Restart=on-failure` and the secret in
`LoadCredential=` or an `EnvironmentFile=` that is mode 0600 and outside git.

## Fly.io

`fly.toml` is checked in: one machine, one volume mounted at `/data`, and the
`/healthz` check. It is deliberately a single instance — two machines would be
two marketplaces with two signing keys, and a tessera issued by one would not
verify against the other. Phase 3 does not change that: it made the chain a
verified value, not the store a shared one, so the Postgres workstream remains
deferred and multi-instance deployment stays out of scope.

The image is built by Fly from the same `Containerfile` used locally, so what
ships is what was tested.

### One-time setup

```bash
fly auth login

# The app name is global across Fly; if "vtessera" is taken, pick another and
# update the `app` line in fly.toml to match.
fly apps create vtessera

# The volume must exist before the first deploy, or the mount fails and the
# machine will not start. 1 GB is ample; the database is small and WAL is the
# only growth.
fly volumes create vtessera_data --region fra --size 1

# Signs session tokens. Generate a fresh one; never reuse the dev value.
fly secrets set VTESSERA_SESSION_SECRET="$(openssl rand -hex 32)"

# The agent card must advertise a URL that actually reaches the service. Fly
# provides one, so going live needs no DNS record at all; a custom domain can be
# added later with `fly certs add`.
fly secrets set VTESSERA_PUBLIC_BASE_URL="https://<app>.fly.dev"
```

### Deploy

```bash
make fly-deploy          # fly deploy
```

Then verify it is real rather than merely green:

```bash
curl -fsS https://<app>.fly.dev/healthz
fly ssh console -C 'ls /data'      # expect vtessera.db, -wal, -shm, signer.key
```

The `verificationKey` in the health output is the marketplace identity. It must
be identical after every deploy. If it ever changes, the volume is gone and so
is every tessera the service has issued.

### Backup on Fly

`fly ssh console` in and use SQLite's `.backup` (see [Backup](#backup) for why
copying the `.db` alone is not enough), or snapshot the volume with
`fly volumes snapshots`. The signing key is the part that cannot be recreated —
consider retrieving it once and storing it somewhere other than the volume.

## Settlement: on when a cluster is configured, off otherwise

Settlement is switched by configuration and by nothing else. This deployment
has been enabled against **mainnet-beta** since 2026-10-01; the same binary
serves the disabled state without any other change.

**Enabled** — `VTESSERA_CLUSTER` and `VTESSERA_RPC_URL` both set, plus
`VTESSERA_MAINNET_ACK=1` on mainnet-beta. `/healthz` reports `cluster` and
`genesisHash`, on-chain trades are accepted, and a boot that cannot verify the
cluster it was told to expect refuses to serve.

**Disabled** — neither set. `/healthz` omits `cluster` and `genesisHash`
entirely rather than reporting them empty, and on-chain trades are refused with
`501 ONCHAIN_UNAVAILABLE`: no cluster is configured there, so retrying cannot
help. Discovery, negotiation, the hash-chained ledger and signed virtual tessera
are all unaffected, which makes that state a complete off-chain marketplace
rather than a degraded one. An endpoint set without a cluster, or the reverse,
is a startup error rather than either state.

The live deployment's three settings are Fly secrets, not `fly.toml`:

```
VTESSERA_CLUSTER=mainnet-beta
VTESSERA_RPC_URL=https://solana-rpc.publicnode.com
VTESSERA_MAINNET_ACK=1
```

Settlement needs the cluster named as well as the endpoint: a service that
inferred its cluster from the URL would have nothing to check that URL against.

Phase 3 is implemented, and the service verifies the chain it is pointed at:
the genesis hash and every governed mint are checked at boot, on every
settlement request and on every reconciler tick. A boot that verifies logs
`preflight passed` followed by `on-chain settlement enabled`. A boot that does
not verify exits before it opens the database or creates the signing key, so a
misconfigured deploy never half-serves; once running, a chain that drifts later
is reported as `503 ONCHAIN_UNAVAILABLE` and confirmations are halted for that
tick.

The last recorded boot verified both governed mints, genesis
`5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`, and a fee wallet holding
39,724,828 lamports against a 650,240 rent minimum. The fee is 1,000 lamports
per settlement, paid by the buyer to `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`.
That wallet had no transaction history on mainnet-beta as of this writing, which
is expected: no settlement had been driven through to completion.

Turning settlement on, or changing cluster or endpoint, is a deliberate act:

1. `make preflight-live CLUSTER=mainnet-beta RPC_URL=https://solana-rpc.publicnode.com MAINNET_ACK=1`.
   Read the report. Every mint must say `verified`, the genesis hash must be
   `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`, and the fee wallet must hold
   more than the reported rent minimum. This step creates no database and no
   signing key, so it is safe to run against a host you are only inspecting.
2. Set `VTESSERA_CLUSTER=mainnet-beta`, `VTESSERA_RPC_URL`, and
   `VTESSERA_MAINNET_ACK=1` with one `fly secrets set`, not in `fly.toml`. The
   acknowledgement is required because settlement there moves real value, and
   keeping them out of the image means a cluster or endpoint change is an
   auditable secret change rather than a rebuild — which is also what lets the
   public endpoint be replaced later by a dedicated one carrying an API key.
3. Deploy, then confirm `curl /healthz` reports the cluster and genesis hash you
   verified in step 1, and that `verificationKey` is unchanged.

```bash
fly secrets set --app vtessera \
  VTESSERA_CLUSTER=mainnet-beta \
  VTESSERA_RPC_URL=https://solana-rpc.publicnode.com \
  VTESSERA_MAINNET_ACK=1
make fly-deploy
curl -fsS https://vtessera.fly.dev/healthz
```

If the machine will not boot or `/healthz` does not report the cluster you
verified, roll the switch back — `fly secrets unset --app vtessera
VTESSERA_CLUSTER VTESSERA_RPC_URL VTESSERA_MAINNET_ACK`, then `make fly-deploy`
— and read `fly logs --app vtessera` before trying again. That leaves the
disabled state described above, which is a working marketplace.

On mainnet-beta `VTESSERA_FEE_LAMPORTS` and `VTESSERA_FEE_WALLET` must be
**unset**; the service refuses to start if either is set, because a different fee
destination is a different marketplace rather than a variant of this one.

To turn settlement back off, unset `VTESSERA_RPC_URL` and `VTESSERA_CLUSTER`
together and redeploy. That is the correct state for a deployment with no trades
to settle.

**Do not edit the genesis pin to make a mismatch disappear.** A changed genesis
hash is either a provider incident or a DNS hijack, and updating the pin converts
a detectable incident into an undetectable compromise. Stop and investigate; see
the Phase 3 design §12.4.

A governed mint whose authority has rotated stops the deploy as well. That is
intentional: it is a governance change on a token the marketplace prices, and a
human should re-derive the pin from a second provider rather than have a deploy
re-pin it automatically.

## Verify a deployment

```bash
curl -fsS https://<host>/healthz
curl -fsS https://<host>/.well-known/agent-card.json | \
  python3 -m json.tool | head -20
```

The health output carries `verificationKey`. Record it: it is the marketplace
identity, and it must be identical after every deploy and restart. If it changes,
`/data` was not persisted. The container image was verified against exactly that
failure — stop, restart, confirm the key is unchanged.

When settlement is enabled, `/healthz` also carries `cluster` and `genesisHash`.
Those are the chain this deployment settled on, and a tessera is only meaningful
relative to one. They are absent rather than empty when settlement is
unconfigured, which is how a caller tells the two states apart without guessing
from the status code alone. The live app currently reports `mainnet-beta` and
`5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`.

A second check that the volume is real, not just present:

```bash
podman exec vtessera ls /data     # expect vtessera.db, -wal, -shm, signer.key
```

## Backup

The signing key **is** the marketplace. Losing it invalidates every tessera the
service has ever issued, and it cannot be regenerated — a new key is a new
identity. Back up `/data/signer.key` separately, and treat it as a secret.

The database runs in WAL mode, so it is three files: `vtessera.db`,
`vtessera.db-wal`, `vtessera.db-shm`. Copying only `vtessera.db` while the
service runs can silently drop committed transactions that are still in the WAL.
Use SQLite's own backup — `sqlite3 /data/vtessera.db ".backup /backup/vtessera.db"`
— or stop the service first.

### The Fly volume snapshot is the backup

Snapshot retention was set to 5 days at deploy time, but the first *scheduled*
snapshot did not exist until hours later. Retention is a policy, not a backup.
The first snapshot was therefore taken by hand, and the volume was quiesced
first so the WAL was checkpointed rather than captured mid-write:

```bash
flyctl machine stop 7845403b399e38 --app vtessera      # quiesce; WAL drains to 0
flyctl volumes snapshots create vol_vdew3gmm6lljye14 --app vtessera
flyctl machine start 7845403b399e38 --app vtessera
```

| | |
|---|---|
| Volume | `vtessera_data` / `vol_vdew3gmm6lljye14`, 1 GB, encrypted, `fra` |
| Restore point | `vs_Byvj0n8vAKpMs0GByn23OP6j` (33 MiB, 5-day retention) |
| `verificationKey` at that point | `5LRpM9wpvPfRYuQAC7oNdyaQa6sakpMcnZeR9FS5CgjB` |
| `signer.key` sha256 | `22dd5280d2982c3821ae58b9a8a8a790754d595ee067d56db84e9ce89b84d882` |

`make fly-verify` prints the live key; if it stops matching the value above, the
volume is gone and this is a new marketplace, not a recovery.

**Recovery is untested** — the snapshot has not been restored, deliberately, since
restoring it would be the one way to lose the deployment. `flyctl` has no
`restore` verb; you clone the snapshot into a new volume and swap the machine
over:

```bash
# 1. Confirm what you are cloning, and that the volume is not attached.
flyctl volumes snapshots list vol_vdew3gmm6lljye14 --app vtessera
# 2. Free the name. A volume cannot be replaced while it is attached, and the
#    snapshot must be cloned with the same size and region.
flyctl machine stop 7845403b399e38 --app vtessera
flyctl machine destroy 7845403b399e38 --app vtessera
flyctl volumes destroy vol_vdew3gmm6lljye14 --app vtessera
# 3. Clone, then bring up a machine mounting it. -v takes <name>:/path.
flyctl volumes create vtessera_data --app vtessera --region fra --size 1 \
  --snapshot-id vs_Byvj0n8vAKpMs0GByn23OP6j --snapshot-retention 5
flyctl machine run --app vtessera -v vtessera_data:/data
# 4. The one check that matters. A changed key means the clone did not carry
#    /data/signer.key and the recovery is not a recovery.
make fly-verify
```

If step 4 prints a different `verificationKey`, stop and redeploy. Do not carry
on to republish the directory: `agent-ai-tool` would list a marketplace that
cannot verify a single tessera it has already handed out.

## Pointing the directory at it

**Done 2026-09-29** for `https://vtessera.fly.dev`. Kept here because it is what
you re-do if the service ever moves.

Once the service answers at a public URL, do two things together, in the
`agent-ai-tool` repository:

1. Set the `VTESSERA_BASE_URL` repository secret to the public URL. The nightly
   `refresh-live.yml` job then publishes the live agent list and `delivered`
   counts, and the site renders its live section.
2. Set the entry's `url` to the service and `agent_card_url` to
   `<url>/.well-known/agent-card.json`, and rewrite the summary to describe what
   is actually served.

Do not do either before the URL actually answers. The directory's health check
will withhold a dead endpoint, but the honest state is to leave the fields
alone until it is real.

`mcp_endpoint_url` stays `null` for this service. It is an A2A gateway with an
AGP JSON-RPC route, not an MCP server, and that field is a promise that an agent
can speak MCP to the URL in it. `agent_card_url` is the honest machine endpoint
and the health check probes it like one.

## What is deployed

`https://vtessera.fly.dev` — one `shared-cpu-1x` machine at 256 MB in `fra`, a
1 GB encrypted volume at `/data`, Fly keeping five daily volume snapshots, and
on-chain settlement enabled against `mainnet-beta` through Fly secrets.
`verificationKey` is `5LRpM9wpvPfRYuQAC7oNdyaQa6sakpMcnZeR9FS5CgjB`.

The registry is not empty: the write path has been exercised, so there are
registered agents and at least one open offer priced in USDC, and the public
agent list at `GET /v1/agents` is the honest source for the current counts.

A custom domain is the only piece of the original plan still outstanding:
`vtessera.com` does not resolve, so nothing has been claimed that would be
wrong. `fly certs add your.domain` once you own one.
