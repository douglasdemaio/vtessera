# TypeScript reference client

The smallest complete vtessera client: Ed25519 handshake, bearer token, one
off-chain sandbox trade, and a receipt this file verifies with the marketplace's
own public key — no service in the loop at the end. From a clean machine to a
signed receipt in about five minutes.

Needs Node 22.18 or later, which runs TypeScript directly. There is nothing to
install: everything the file needs — Ed25519, SHA-256, fetch — is in the
standard library, so `package.json` declares an empty dependency set on
purpose.

## Run it

```bash
# 1. Build the marketplace (once, from the repository root).
export TMPDIR="${TMPDIR:-$HOME/.cache/go-tmp}"   # constraint 2 in AGENTS.md
make build

# 2. Start a sandbox marketplace in a second terminal. --sandbox means on-chain
#    settlement is refused and no value can move.
bin/vtessera --sandbox \
  --session-secret 0123456789abcdef0123456789abcdef \
  --db /tmp/vtessera-example.db

# 3. Run the client.
node examples/typescript/client.ts
```

The last line reads:

```
Done. 2.00 of EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v, settled offchain
```

Point it elsewhere with `VTESSERA_BASE_URL=http://host:port`. The client
refuses a marketplace that is not a sandbox, because it settles trades and has
no confirmation step.

## What it does not do

It does not sign its card (a bare card is enough to trade — the marketplace
countersigns what it stores), does not settle on-chain, and cannot be
capability-probed: a probe target has to be public HTTPS, and localhost is
refused by design. See the file header for the reasoning, and
`docs/quickstart/typescript.md` for the narrated version of the same flow with
the attestation step included.

`../../scripts/examples.sh typescript` runs this file against a throwaway
sandbox, which is what CI does.
