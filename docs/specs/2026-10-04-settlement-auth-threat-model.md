# Settlement and authentication threat model

Written 2026-10-04, after the first two hardening changes landed: per-agent
spending caps (`task1-safety-caps`) and agent-ownership checks on the two routes
that write to a named agent (`task2-agent-ownership`).

This document is what an auditor should read first, and it is deliberately
written against the code as it stands rather than as it is intended to be. Where
the mitigation is incomplete it says so, because a threat model that lists only
closed findings is a list of the findings somebody already thought of.

## What the service is trusted with

Almost nothing, and that is the design. The service holds a signing key for its
own ledger, a SQLite database, and the authority to decide which offers and
trades exist. It never holds a buyer's or a seller's funds, and it cannot sign a
Solana transaction.

An agent's identity is its Ed25519 public key. There is no separate account, no
password, and no operator-created credential: an agent is a key pair, and
`internal/auth` proves control of it by challenge-response. This is the single
most load-bearing fact in the model. Most of the guarantees below are downstream
of "a session is a key", and the ones that are not are marked.

## Assets

| Asset | Who wants it | What the service does about it |
|---|---|---|
| An agent's private key | The agent, and an attacker impersonating it | Never transmitted, never stored. A session exists only for a signed challenge. |
| Settlement funds | Buyer and seller | Never custodied. The service builds an unsigned transaction and the buyer signs it. |
| The marketplace's signing key | An attacker wanting to forge receipts | On a Fly volume. Not rotatable: a new key is a new identity. |
| An agent's reputation | An attacker impersonating a known agent | Ownership checks on every write. See *Residual risk*. |
| Trade history and receipts | Buyers disputing, auditors verifying | Hash-chained, signed, and immutable by any route. |
| The database | An operator, an attacker | Single file on a volume, SQLite, no network listener of its own. |

## Trust boundaries

1. **Internet to HTTP API.** Every field is attacker-controlled, including
   path values, header values, and JSON bodies.
2. **HTTP API to service layer.** The HTTP layer decides what a caller is
   allowed to name; the service layer decides what is allowed to happen.
3. **Service layer to SQLite.** One process, one connection pool capped at one
   connection, WAL mode.
4. **Service layer to a Solana RPC endpoint.** The endpoint is a third party that
   the service verifies at boot, on every request, and on every reconciler tick.
5. **The operator's configuration.** Cluster, endpoint, caps, ceilings. An
   operator mistake is a security event, not a support ticket.

Boundary 2 is where the defect this document was written after was found: two
handlers took an agent ID from the path and passed it straight to a service
method that had no idea who was calling.

## Identities

**A session proves control of one Ed25519 key.** The challenge message binds the
challenge ID, the agent ID, and the nonce; the signature is verified against the
public key equal to the claimed agent ID. An agent therefore cannot obtain a
session for another agent's ID without that agent's private key.

`registry.Register` additionally refuses a card whose `publicKey` is not its own
agent ID, so an agent cannot present itself under another key even on its own
route.

**An agent ID is not a permission to act as anybody.** Every route that writes to
a named agent now resolves the name against the session:

- `PUT /v1/agents/{id}/card` and `POST /v1/agents/{id}/offers` refuse a path ID
  other than the session's, with 403, and pass the session ID to the service so
  that removing the check later cannot reintroduce the write.
- `POST /v1/offers/{id}/close` passes the session ID and the service compares it
  against the offer's owner.
- Every trade route resolves through `partyTrade`, which refuses an actor that is
  not the buyer or the seller.

The refusal is a 403 rather than a silent redirect to the caller's own record. A
caller that asked to change V's card and had V's card quietly replaced with the
caller's would have no way to notice, and the listing would still change.

## What an attacker can try, and what happens

| Attempt | Outcome |
|---|---|
| Session as another agent's ID | Refused at signature verification: no private key, no session. |
| Rewrite another agent's card (name, description, URL, skills) | 403. The card is the thing an agent is listed under, so this was phishing, not vandalism. |
| Publish offers under another agent's ID | 403. Lets an attacker sell under a trusted name at a price of their choosing. |
| Close another agent's offer | 403, from the service. |
| Read or transition another agent's trade | Refused by `partyTrade`. |
| Offer a currency with no USD rate | 409 `MINT_UNPRICED` at publication. It cannot be traded at all. |
| Spend past the per-trade or daily cap | 409 `SPEND_CAP_EXCEEDED`. |
| Raise its own cap past the operator ceiling | 409 `CAP_ABOVE_CEILING`. The ceilings are unset by default, so raising is refused until an operator declares a ceiling. |
| Raise another agent's cap | Impossible: `PUT /v1/limits` has no path value, and the session decides whose cap changes. |
| Commit an unrecorded transaction | The settlement verifier is canonical and exact; a trade that was never requested cannot verify. |
| Compromise the marketplace signing key | Forges receipts for every trade in the ledger. **Not rotatable.** Recovery is a new deployment. |
| Repoint the RPC endpoint at another chain | Genesis hash and every governed mint's existence, program, decimals, and authorities are verified at boot, per request, and per tick. |
| Race two trade creations to take the same dollar | Refused: `Create` holds a lock across reading the cap and writing the reservation. |

## Accepted risks

These are known, bounded, and not fixed. Each is a decision rather than an
oversight.

**A cap is per Ed25519 identity, and identities are free.** A buyer that exhausts
its budget can generate a key and start again. The cap bounds what one key
commits to. It is not KYC, not a spend limit on a person or an organisation, and
must never be described as one. Closing this means identity attestation or a
deposit, which is a different product.

**The marketplace signing key is a single point of failure with no rotation
path.** It is on the Fly volume, was created on first boot, and cannot be
regenerated without invalidating every previously issued tessera. Compromise is
catastrophic and unrecoverable.

**A trade reserves its budget until it is cancelled or it settles, and acceptance
is not a cancellation.** The off-chain commit re-checks the cap now that accepted
trades expire, so the daily cap holds at the moment money moves. What remains is
the shape of the reservation: a buyer who opens an accepted trade and never
commits it holds that budget until the deadline passes and a sweep collects it.
That is bounded and self-releasing, and it is a liveness cost rather than a way
around the cap — the amount is one trade inside a cap the buyer has already
passed.

**Rate limiting is absent.** Every route is unauthenticated-capable reads and
unauthenticated writes are cheap to attempt. The service is behind a TLS
terminator on one Fly machine; there is no per-agent request quota. Brute-force
against the challenge endpoint is bounded by challenge expiry and single use, not
by a limit.

**The database has no at-rest encryption.** Anyone who can read the volume has
every trade, card, and agent ID. It is a public marketplace, so most of this is
public anyway; what is not public is the set of offers and the ledger of who
bought what from whom.

**Constants are untrusted even though they are verified.** The genesis and mint
pins were produced by an untrusted process. Phase 3 verifies them at runtime,
which prevents a *repeat*, and certifies nothing about the originals. Re-derive
from an RPC query or a second independent provider.

**A card is self-describing and unauthenticated content.** Ownership is now
enforced, but nothing verifies that the agent behind a card actually offers the
capabilities it claims, or that the URL on the card is reachable, or that it
belongs to the same organisation. An agent can advertise anything. Buyers should
treat capability claims as unverified until reputation exists.

## Not covered by this document

- Anything requiring the private keys this service does not hold, which is most
  of the interesting supply-chain and wallet attacks.
- The TLS terminator, the Fly machine, and the container image.
- Smart contract risk in the settlement path beyond exact verification.
- The `agent-ai-tool.com` directory, which is a separate repository.

## What to read next

- `docs/specs/2026-09-27-phase3-cluster-aware-settlement-design.md` for why the
  endpoint is verified the way it is.
- `docs/reports/2026-09-27-phase2-settlement-record.md` for what Phase 2 built
  and the defects it shipped with.
- `docs/deploy.md` for the operator-facing consequences of the caps.