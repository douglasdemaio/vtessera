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

## Settlement: leave the RPC endpoint unset

Do **not** set `VTESSERA_RPC_URL` for this deployment. Phase 3 is not
implemented: the service has no cluster awareness and performs no on-chain mint
verification, so it must not be pointed at mainnet-beta or a live devnet. Leave
it unset and on-chain settlement is refused with `501 ONCHAIN_UNAVAILABLE`, while
off-chain settlement — the hash-chained ledger and signed virtual tessera — works
fully. That is the correct state, not a degraded one.

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

## Pointing the directory at it

Once the service answers at a public URL, do two things together, in the
`agent-ai-tool` repository:

1. Set the `VTESSERA_BASE_URL` repository secret to the public URL. The nightly
   `refresh-live.yml` job then starts publishing the live agent list and
   `delivered` counts, which are currently absent.
2. Fill in the `vtessera` entry's `mcp_endpoint_url` in
   `content/entries/vtessera.json` and update its summary, which currently says
   the service is not deployed.

Do not do either before the URL actually answers. The directory's health check
will withhold a dead endpoint, but the honest state is to leave it `null` until
it is real.

## Why this is not already done

The missing pieces are external: a host to run on, and a DNS record. Everything
in this repository is ready for them.
