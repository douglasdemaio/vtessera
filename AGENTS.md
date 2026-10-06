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
make quickstart # runs both quickstart guides against a throwaway sandbox

make preflight-live   # verify a real cluster without starting the service
```

`make quickstart` runs `quickstart/python/agent.py` and
`quickstart/typescript/agent.ts` against a throwaway sandbox, one marketplace
each. It is not wired into `all`, and that is deliberate: it needs `python3` with
`cryptography` plus node 22.18 or later, and constraint 1 means the hermetic
suite must not start depending on two more runtimes. The guides are the API's
front door, so if a change moves an endpoint, an attestation field or a receipt
claim, this is the check that catches it. It moves no value: the sandbox refuses
on-chain settlement and no chain is configured.

`make mcp-test` runs the MCP module's suite, which is a separate module and so is
invisible to `./...`. `make smoke` is hermetic. `make preflight-live` is the only
thing in this list that touches the network. It
is the §11.13 acceptance check and it is worth running after any change to a
base58 constant:

```bash
make preflight-live CLUSTER=devnet
make preflight-live CLUSTER=mainnet-beta RPC_URL=https://solana-rpc.publicnode.com MAINNET_ACK=1
```

It opens no database and creates no signing key, so it is safe to point at a host
you are only inspecting. Never edit a pin to make it pass — see constraint 6.

Validator-backed tests are behind a build tag and need a running local validator:

```bash
make validator   # start a test validator
make test-solana # ~8.5 minutes, 5 scenarios
make validator-off   # STOP IT — see below

make test-devnet # touches the network, moves no value

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

**The one sanctioned exception is `mcp/`**, the MCP server. It is a real module
with its own `go.mod` because it depends on the official MCP SDK and nothing else,
and it must stay importable by someone who wants only the client. It re-resolves
one dependency graph, not the service's, and it may not import this module at all:
it talks to the service's public HTTP API, the same endpoints any agent reads with
`curl`, and it holds no key. `make mcp-test` and friends are wired into `all`,
because a nested module that nothing builds rots quietly. Do not add a second one.

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

### 4. The live deployment settles on mainnet-beta

**Changed 2026-10-01.** This used to say the opposite. Phase 3 (cluster
awareness, endpoint pinning, on-chain mint verification) is implemented, the
live app at `https://vtessera.fly.dev` now settles against **mainnet-beta**, and
`/healthz` reports `cluster` and `genesisHash`. On-chain settlement is on and
real value can move.

The endpoint is `https://solana-rpc.publicnode.com`, pinned by genesis hash
`5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d`, and it was verified against the
live chain before being enabled. The three variables are secrets — not in
`fly.toml` — so a cluster or endpoint change is one auditable `fly secrets set`
rather than an image rebuild, and so the public endpoint can later be swapped for
a dedicated one carrying an API key:

```
VTESSERA_CLUSTER=mainnet-beta
VTESSERA_RPC_URL=https://solana-rpc.publicnode.com
VTESSERA_MAINNET_ACK=1
```

What has not changed, and is why constraint 6 still governs:

- The constants came from an untrusted process. Phase 3 protects against a
  repeat, it does not certify the originals.
- **Never edit a genesis or authority pin to clear a failure.** That converts a
  detectable incident into an undetectable compromise.
- `VTESSERA_MAINNET_ACK=1` is required on mainnet-beta and rejected off it. It is
  the acknowledgement that settlement moves real value.
- `VTESSERA_FEE_LAMPORTS` and `VTESSERA_FEE_WALLET` must stay unset on
  mainnet-beta; the service refuses to start otherwise.

Run `make preflight-live CLUSTER=mainnet-beta RPC_URL=https://solana-rpc.publicnode.com MAINNET_ACK=1`
before changing any of this. `docs/deploy.md` is the runbook.

Turning it back off is unsetting `VTESSERA_RPC_URL` and `VTESSERA_CLUSTER`
together, which leaves a working off-chain marketplace: `/healthz` stops
reporting `cluster` and `genesisHash`, and on-chain trades are refused with
`501 ONCHAIN_UNAVAILABLE`.

### 5. `vtessera` is deployed at `https://vtessera.fly.dev`

**Live as of 2026-09-29.** One Fly machine (`shared-cpu-1x`, 256 MB, `fra`) with
a 1 GB volume mounted at `/data`, and both kept warm. `fly.toml` and a verified
`Containerfile` are in the repository; the runbook is
[`docs/deploy.md`](docs/deploy.md).

Settlement was enabled against mainnet-beta on 2026-10-01 and has been serving
since — see constraint 4. The live `verificationKey` is
`5LRpM9wpvPfRYuQAC7oNdyaQa6sakpMcnZeR9FS5CgjB`, and that is the identity to
protect across any deploy that touches settlement.

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

### 7. Spending caps are on by default, and pricing is operator-declared

Landed on `task1-safety-caps`, retuned to $10/$10 by operator decision. Ten per
trade and ten per rolling day per buyer, equal on purpose so one maximum-size
trade exhausts the day. Governed stablecoins at par, no oracle. Three things
follow that are not
negotiable without a design discussion:

- **An unpriced mint cannot be traded.** No declared rate means `409
  MINT_UNPRICED` at offer publication and at trade creation, and the service
  refuses to boot if a governed mint on the configured cluster has no rate. A cap
  that quietly did not apply to some currencies would be a way around the cap.
- **Ceilings default to closed.** `--spend-cap-max-*` unset means `PUT
  /v1/limits` is refused. Do not add a default ceiling; an agent that can raise
  itself without an operator declaring the ceiling is a cap the operator did not
  choose.
- **A cap is per Ed25519 identity.** It bounds what one key commits to. It is not
  KYC, and anyone can mint another identity. Do not describe it as a spend limit
  on a person or an organisation.

An accepted trade carries a deadline (`--trade-accept-ttl`, default `72h`) and can
be cancelled only once it passes. This is what lets the off-chain commit re-check
the cap: without somewhere to go, a refused commit would strand the buyer holding
a trade they can neither complete nor abandon. A sweep cancels expired trades so
their reserved budget comes back. The service refuses to boot without a deadline,
because running with a cap that cannot be enforced at the commit is a weaker cap
than the one the operator configured, reached by a setting they never touched.

What is left is bypassable by registering a new agent, and documented in
`docs/deploy.md`. Closing that means identity attestation or a deposit.

`make test-devnet` completes a full off-chain trade against the public devnet
cluster and asserts the cap refusals there. It is the only test besides
`preflight-live` that touches the network, and like the preflight it must not be
made to pass by editing a pin.

### 8. An agent ID is a public key, and a session is the only identity

A route that writes to a named agent must take that name from the session, not
from the path. `requireOwnAgent` in `internal/httpapi/server.go` is the check,
and both routes it guards then pass `agentFrom(r)` to the service so that
removing the check later cannot reintroduce the write. Do not read
`r.PathValue("id")` for a write target anywhere in this service; it took two
routes and a long time to find the second one.

Trade routes resolve the actor through `partyTrade`, which refuses anybody who
is not the buyer or the seller. Closing an offer is checked in the service,
because the offer names its owner in a column.

The threat model is `docs/specs/2026-10-04-settlement-auth-threat-model.md`. It
lists what is still open, and the four that matter are a cap per identity when
identities are free, no rate limiting at all, a marketplace signing key with no
rotation path, and an operator retirement token with no identity behind it.

### 9. The retirement routes take a token, not an agent session

`POST /v1/admin/agents/{id}/retire` is the only route that removes a principal's
ability to trade without that principal asking. It is gated on
`--admin-token` / `VTESSERA_ADMIN_TOKEN`, compared in constant time, and
deliberately does not accept an agent session: an agent that could authenticate
there could withdraw every other agent. With no token configured the routes are
not registered at all, and answer `404` rather than `403`.

Do not add a default token, a fallback to the session secret, or an agent-session
path into `requireAdmin`. Do not retire an agent to make a test pass, and do not
retire one on the live deployment without saying so first. `RetireAgent` is
transactional because a retired agent whose offers are still open is a listing
the marketplace promised to hide and did not, and it refuses while a trade is
live because that buyer is existing business rather than future business.

## Git

`main` history is deliberately short: `d6fda1f Initial commit` (scaffold),
`20edf8f` (the whole Phase 2 settlement service), `440aebc` (Phase 3: cluster
awareness, endpoint pinning, on-chain mint verification), then the Phase 3
records. **Do not commit unless explicitly asked.**

Phase 3 landed on 2026-09-30 in `440aebc`. The fee default is now `1000`
lamports, the bogus EURC mint is replaced, and the governed table is
cluster-scoped — all three break assumptions in tests that predate them, which
is why the fee and mint tests assert the new values rather than the old.

## Documentation

| Path | What it is |
|---|---|
| `docs/specs/2026-09-26-a2a-marketplace-design.md` | Authoritative product spec. |
| `docs/specs/2026-09-27-phase3-cluster-aware-settlement-design.md` | Approved Phase 3 design (revision 2). Read before touching settlement. |
| `docs/specs/2026-10-04-settlement-auth-threat-model.md` | Threat model, including what is still open. Read before touching auth or settlement. |
| `docs/reports/2026-09-27-phase2-settlement-record.md` | What Phase 2 actually built, plus its known defects. Read before claiming Phase 2 works. |
| `docs/quickstart/python.md` | The five-minute journey, in Python. |
| `docs/quickstart/typescript.md` | The same journey, in TypeScript, with no install step. |

When changing behaviour, update the relevant document in the same change.

## Conventions

- No comments unless asked. This codebase is unusually well-commented already;
  match the surrounding density rather than adding more.
- Test names state the behaviour, not the function under test.
- Prefer refusing loudly to degrading quietly: an operator who mistypes a setting
  should find out at boot.
