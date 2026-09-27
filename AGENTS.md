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
```

Validator-backed tests are behind a build tag and need a running local validator:

```bash
make validator   # start a test validator
make test-solana # ~8.5 minutes, 5 scenarios
make validator-off   # STOP IT — see below
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

### 5. A known critical defect is still in the tree

`internal/tokens` ships a governed EURC mint address that **does not exist
on-chain** (`HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc`; the real mint ends
`...zpKcFu7uBEDKtr`). Any trade priced in it can never settle. Phase 3 fixes it.
Do not treat the current mint registry as trustworthy, and do not re-use
constants from it without re-deriving them independently.

## Git

`main` history is deliberately short: `1ca720e Initial commit` (scaffold),
`56ac399` (the whole Phase 2 settlement service), then the Phase 3 spec and
records. **Do not commit unless explicitly asked.**

Phase 3 is designed but unimplemented. When it lands, the fee default changes
from `500000` to `1000` lamports and the bogus EURC mint is replaced — expect
both to break existing assumptions in tests.

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
