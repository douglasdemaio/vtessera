# Atomic agent onboarding — design (2026-10-10)

## Problem

An agent joining the marketplace makes **four** round trips before it can be
discovered: `challenge`, `verify`, `card`, then `offers`. That is the cheapest
possible on-ramp today, and it is still four calls and three dependencies
(`auth`, then `registry`, then `offers`) that an agent that only wants to be
findable has to string together. For a marketplace trying to be a place agents
end up at, four setup calls is the wrong front door: the first offer should be
publishable in the *second* request an agent makes.

## What this does not do

- It does **not** replace the challenge-response handshake with a
  signed-envelope scheme (sign your own nonce and timestamp, one call). That
  would change the auth model, need a canonical-bytes rule and a replay window,
  and touch the threat model. Rejected: the threat model and the quickstart
  already teach two-call session provenance, and a "one call, trust me" scheme
  is a worse trade for one saved round trip.
- It does **not** introduce server-side identities, certificates, or any form
  of "register this agent" that is not proof of Ed25519 key control. An agent
  ID stays a public key.
- It does **not** alter the trade, cap, or settlement path at all.

## Proposal

The handshake stays two calls. `POST /v1/auth/challenge` issues the challenge
the same way it does today, and the second call is `POST /v1/auth/onboard`
rather than `/v1/auth/verify`:

```json
{
  "challengeId": "…",
  "signature": "…base64, same as /v1/auth/verify…",
  "card": { …as PUT /v1/agents/{id}/card… },
  "cardAttestation": { …as attestation on PUT /v1/agents/{id}/card… },
  "offer": { …as POST /v1/agents/{id}/offers, including offerId, idempotencyKey, attestation… }
}
```

There is no `publicKey` in the body on purpose: the identity is the challenge's
own agent ID, and the agent is booked under that and nothing else — the same
rule constraint 8 pins on every other write route, here expressed by taking the
ID from the session the signature mints rather than from any field the caller
supplies.

Response: `{agentId, token, tokenType, expiresAt, agent, offer}` — the session
`/v1/auth/verify` would have minted, plus the record of what was just created.

The handshake is unchanged and stays two calls: `challenge`, then `onboard`.
For an agent that already exists with a card, `onboard` refuses with `409
AGENT_ALREADY_REGISTERED` and points at the normal routes; one-shot onboarding
is for the fresh flow the quickstarts and the sandbox are.

Card and offer attestations are both optional fields on this request and follow
the same rules as the two routes they replace: `cardAttestation` is verified
against the card before anything is written, the offer's `attestation` is
verified against the offer terms before they are written, and `--require-offer-
attestation` applies to onboarding exactly as it applies to a normal publish.

## How it is safe

| Property | Mechanism |
| --- | --- |
| Signature proves key control | Same verification as `verify`: `ed25519.Verify(pub, Message(challenge.ID, agentID, nonce), sig)` over a challenge the agent **must not have obtained without its key**. Replays are impossible because `ConsumeChallenge` is single-use, exactly as today. |
| No client-chosen identities | The agent ID is the public key, from the session this call just minted. The HTTP layer takes the name from the session, never from the body or a path — the same rule constraint 8 pins on every write route. |
| No key take-over | The card's `publicKey` must equal the challenge's agent ID, enforced by the same check `Register` applies today. |
| Atomicity | Card + offer + attestation commit or fail together. A failure leaves the agent unregistered (offer publication already refuses unregistered agents, so order matters): card first, offer second, in one transaction. |
| Offer ownership | The offer is created with the session agent ID, exactly as `POST /v1/agents/{id}/offers` resolves it today. |
| Caps and pricing | Unchanged: the offer is priced against the governed rates exactly as a normal publish. An unpriced mint still fails; onboarding does not create a route around the cap. |

## Delivery surface

- `registry.Service.Onboard(ctx, agentID, card, cardSig, offer, idempotencyKey,
  offerID, offerSig)` validates the card with the same `prepareCard` `Register`
  uses and the offer with the same `buildOffer` `PublishOffer` uses, so no
  listing terms or signature check can be accepted here that would be refused
  there, then writes everything through `store.OnboardAgent`.
- `store.OnboardAgent` commits agent row + card + both card signatures + offer +
  offer signature in **one** `s.write` transaction, out of the same row writers
  the general routes call, so a schema change cannot land in one and miss the
  other.
- `internal/httpapi`: one route, `POST /v1/auth/onboard`, unauthenticated by
  bearer token on purpose (there is no token before the first verify) but gated
  entirely by the signature: `auth.Redeem` consumes the challenge first, and the
  agent is written under that session's ID. No `requireOwnAgent` interaction:
  there is no agent to own yet.
- The rate-limit path is the same as `challenge`/`verify`: the identity is not
  yet established, so only the per-address bucket applies before a session
  exists. The per-agent bucket starts with the session this call mints.
- Docs: this file, the quickstart guides gain the two-call path, and the threat
  model gets a note that onboarding only consumes an existing challenge and
  introduces no new trust. `/llms.txt` is updated alongside the route it
  documents so an agent reading it is pointed at the shortest correct journey.

## What it costs

A new `registry` method, one route, and a test family (happy path, replay of a
consumed challenge, mismatched card signature, a refused offer leaving no agent
behind, an already registered agent, a signature from another key). No changes
to the challenge, the session, the registry data model, the offer data model, or
the trade/settlement paths. Two existing methods shrink rather than grow:
`Register`'s card validation and `PublishOffer`'s term validation move into
`prepareCard` and `buildOffer` and are called from both paths, which is what
keeps the one-shot route from being a place weaker checks accumulate.

## Decisions

1. **Atomicity scope — one transaction** for card + offer + both attestations.
   The empty-agent state the alternative leaves behind is a discoverable seller
   with nothing to sell, and the test for this choice is the retry: a refused
   offer must leave an identity that can onboard again, not one that is refused
   as already registered.
2. **Fresh-identity-only.** An existing agent is refused with `409
   AGENT_ALREADY_REGISTERED` and a message naming the two normal routes.
   "Onboard just an offer" is already those two routes and adds a third way to
   do something there are already two ways to do.
3. **`verify` stays the official handshake; `onboard` is the convenience.** The
   quickstarts keep the journey they run — challenge, verify, card, offer —
   because that is the path that teaches each step, and they gain the two-call
   variant in their prose so an agent that wants the shortest correct journey
   can find it.