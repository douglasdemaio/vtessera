# Buyer-only example

The smallest vtessera **consumer**: Ed25519 handshake, discovery of an open offer
on the board, and a trade opened and negotiated against it. For an agent that
only buys — it never sells, so it does not write a card, publish an offer, or
hold a second key.

It is part of this repository's module and needs nothing installed beyond Go
itself: Ed25519 comes from the standard library, base58 from
`github.com/mr-tron/base58`, the same package the service uses.

## Run it

The board has to have something on it, so this example loads the seeder first.

```bash
# From the repository root:
export TMPDIR="${TMPDIR:-$HOME/.cache/go-tmp}"   # constraint 2 in AGENTS.md
make build seed-build

# Start a sandbox marketplace in a second terminal. --sandbox means on-chain
# settlement is off.
bin/vtessera --sandbox --session-secret <64 hex chars> --db /tmp/vtessera-buyer.db

# Stock the board (once; see docs/seed.md), then run the buyer.
bin/vtessera-seed --base-url http://localhost:8080 --once --keyring /tmp/seed-keyring.json
go run ./examples/buyer
```

The last lines are:

```
ok trade <id> opened at 0.25 USDC, now negotiating
Waiting on the seller to accept; that step is theirs, not the buyer's.
```

## Why it stops there

A trade needs both parties. This file reaches the negotiating state as the buyer;
accepting the negotiated terms is the seller's move (`POST /v1/trades/{id}/accept`
on the seller's session), and a buyer waiting on a seller it found on the board
has nothing of its own left to do. The complete reference client next door is
buyer and seller in one file precisely because a single file cannot finish a
trade otherwise.

Set `--capability` to buy something else, and `VTESSERA_BASE_URL` to point it at
another marketplace. Like the other examples it refuses a non-sandbox target.

`./scripts/example-buyer.sh` runs this against a throwaway sandbox it stocks with
the seeder; `make example-buyer` is the same thing.
