# AGENTS.md

Working notes for coding agents on this repository. These are **standing
constraints**, not suggestions — they were set deliberately by the maintainer.

## Project

`vtessera` is an A2A agent marketplace. Agents offer services, buyers negotiate,
and trades settle either off-chain (Ed25519-signed receipts in a Tessera Ledger)
or on-chain via a non-custodial Solana settlement the buyer signs themselves. The
service never custodies funds.

Go 1.27. Single module. SQLite via `modernc.org/sqlite`.

## Commands

```bash
make            # fmt, vet, test, build
make test       # hermetic suite, no validator needed
make race       # whole suite under -race
make vet fmt
make build      # -> bin/vtessera
make smoke      # builds the binary and runs a process-level journey

make preflight-live   # verify a real cluster without starting the service
```

`make preflight-live` is the only thing in this list that touches the network. It
is the §11.13 acceptance check and it is worth running after any change to a
base58 constant:

```bash
make preflight-live CLUSTER=devnet
make preflight-live CLUSTER=mainnet-beta RPC_URL=https://solana.publicnode.com/
```

It opens no database and creates no signing key, so it is safe to point at a host
you are only inspecting. Never edit a pin to make it pass — see constraint 6.

Validator-backed tests are behind a build tag and need a running local validator:

```bash
make validator   # start a test validator
make test-solana # ~8.5 minutes, 5 scenarios
make validator-off   # STOP IT — see below

make image      # -> vtessera:local, from the checked-in Containerfile
make image-run  # run that image locally against a volume
make fly-deploy # deploy to the live Fly app (see constraint 5)
make fly-verify # print the marketplace verificationKey
```

## Hard constraints

### 1. Do not add toolchains

No npm, no Rust, no new language runtime, and **no throwaway Go modules**. Use the
repository's existing Go toolchain and its existing dependency set. If a
measurement needs a program that does not belong in the repo, write it as a
temporary test in the repo, run it, and delete it — do not create a second module
that re-resolves the dependency graph.

### 2. `TMPDIR` is required

`/tmp` is a tmpfs and counts against RAM. Before any build or test command:

```bash
export TMPDIR="${TMPDIR:-$HOME/.cache/go-tmp}"
```

### 3. The test validator is transient — always stop it

`solana-test-validator` holds roughly 1 GB of RAM and writes gigabytes of ledger
to disk. It must be running **only** while a test suite is executing, and stopped
immediately afterwards. It is not a background service and should never be left
up between sessions.

```bash
# start, pinned to a cache dir so it is easy to find and delete
solana-test-validator --ledger "$HOME/.cache/solana-ledger" --reset --quiet &

# ... run make test-solana ...

# stop it — ALWAYS, including after a failed or interrupted run
make validator-off
```

Note `make validator` does not pin a ledger path, so the validator will place it
wherever its default takes it. Pass `--ledger` explicitly as above when you need
to find or delete it.

To check whether one is running, use `pgrep -f solana-test-validator`. Two traps
here, both of which have bitten this session already:

- `pgrep -x` and `pkill -x` **cannot match this process at all.** Linux truncates
  process names to 15 characters, so the comm name is `solana-test-val`, and
  `-x` demands an exact match against the full name.
- `pkill -f` does match, but the pattern then also matches the command line of
  the shell running the command, so it kills its own shell and takes `make` with
  it.

`make validator-off` sidesteps both: it enumerates the `pgrep -f` matches and
skips its own PID.

Delete the ledger when finished if disk matters: it is disposable, and
`--reset` rebuilds it from genesis on the next run.

### 4. The chain is never mainnet, by default

Phase 3 (cluster awareness, endpoint pinning, on-chain mint verification) is
**designed but not implemented**. Until it lands, this service has no concept of
which cluster it is connected to. Do not point it at mainnet-beta or at a live
devnet expecting production behaviour. `VTESSERA_RPC_URL` unset is the correct
state: settlement stays off and returns `501 ONCHAIN_UNAVAILABLE`.

The documented mainnet-beta RPC endpoint for the future is
`https://solana.publicnode.com/`, pinned by genesis hash
`5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`.

### 5. `vtessera` is deployed at `https://vtessera.fly.dev`

**Live as of 2026-09-29.** One Fly machine (`shared-cpu-1x`, 256 MB, `fra`) with
a 1 GB volume mounted at `/data`, and both kept warm. `fly.toml` and a verified
`Containerfile` are in the repository; the runbook is
[`docs/deploy.md`](docs/deploy.md).

Deployed with `VTESSERA_RPC_URL` unset, so on-chain settlement is still refused
with 501. That is required, not incidental — see constraints 4 and 6.

The things that matter when editing this repository:

- **The signing key on the volume is the marketplace.** It was created on first
  boot and cannot be regenerated: a new key is a new identity, and every
  previously issued tessera stops verifying. Do not delete `/data` to "reset"
  anything. Fly keeps 5 daily volume snapshots as the recovery path; if the key
  is ever lost, the honest response is a new deployment, not a recovery.
- **`verificationKey` in `/healthz` is the identity.** It must not change across
  a deploy. `make fly-verify` prints it. A change means the volume is gone.
- The volume is a SQLite database in WAL mode, so it is three files
  (`vtessera.db`, `-wal`, `-shm`). Copying only the `.db` while the service runs
  can silently drop committed trades.
- `agent-ai-tool.com` now fetches this service. The `VTESSERA_BASE_URL` secret is
  set in that repository and the nightly refresh job commits snapshots, so a
  build that cannot reach the service still renders from cache.
- The directory entry's `agent_card_url` points here and is probed by the site's
  health check, so a machine that is not answering causes the entry to be
  withheld and then republished on its own. `mcp_endpoint_url` is deliberately
  `null`: this is an A2A/AGP gateway, not an MCP server, and that field must
  only ever hold a URL an agent can speak MCP to.

A Fly personal account with a token is required to deploy. Do not commit a
token, and do not add a `.fly` config to the repository.

### 6. Phase 3 is implemented; the constants are still untrusted

**Landed 2026-09-30.** The service is now cluster-aware. The cluster is named in
configuration (`VTESSERA_CLUSTER`, required alongside `VTESSERA_RPC_URL`) and
verified against the endpoint: the genesis hash, and every governed mint's
existence, owning program, initialization, decimals and authorities, are checked
at boot and refused on failure. The identity is re-read on every settlement
request and every reconciler tick, so a URL repointed at runtime is caught.
Requests and receipts both carry the cluster they belong to, and the reconciler
withdraws a request compiled for another chain so the buyer can re-request.

The lookalike EURC defect is fixed. `internal/tokens` had shipped
`...c2iXXcyK85CNzz7iwQc`, which does not exist on-chain; it now ships Circle's
real mint `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr`. The two share a
30-character prefix and diverge at position 31, which is why a visual check
missed it. The governed table is now pinned to
`internal/tokens/testdata/governed-mints.json`, a snapshot of what
`make preflight-live` actually read from both public clusters, so the next
transcription error fails a test instead of a deploy.

**Still true, and the reason the constraints above stand.** The constants were
produced by an untrusted process, and the fix protects against a repeat rather
than certifying the originals. Re-derive on-chain constants from an RPC query or
a second independent provider, never from these notes or from source. And do not
edit a genesis pin or an authority pin to make a preflight failure disappear: a
changed genesis is a provider incident or a DNS hijack, and re-pinning converts
a detectable incident into an undetectable compromise.

## Git

`main` history is deliberately short: `1ca720e Initial commit` (scaffold),
`56ac399` (the whole Phase 2 settlement service), then the Phase 3 spec and
records. **Do not commit unless explicitly asked.**

Phase 3 landed on 2026-09-30, uncommitted. The fee default is now `1000`
lamports, the bogus EURC mint is replaced, and the governed table is
cluster-scoped — all three break assumptions in tests that predate them, which
is why the fee and mint tests assert the new values rather than the old.

## Documentation

| Path | What it is |
|---|---|
| `docs/specs/2026-09-26-a2a-marketplace-design.md` | Authoritative product spec. |
| `docs/specs/2026-09-27-phase3-cluster-aware-settlement-design.md` | Approved Phase 3 design (revision 2). Read before touching settlement. |
| `docs/reports/2026-09-27-phase2-settlement-record.md` | What Phase 2 actually built, plus its known defects. Read before claiming Phase 2 works. |

When changing behaviour, update the relevant document in the same change.

## Conventions

- No comments unless asked. This codebase is unusually well-commented already;
  match the surrounding density rather than adding more.
- Test names state the behaviour, not the function under test.
- Prefer refusing loudly to degrading quietly: an operator who mistypes a setting
  should find out at boot.
