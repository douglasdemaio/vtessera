# Signing-key rotation

**Status:** Implemented on `feat/signing-key-rotation`, awaiting review. The
open questions were settled as recorded below; every behaviour named in the
*Tests* section is covered by the suite.

Written 2026-10-09, after the threat model named the signing key as the one
catastrophic, unrecoverable asset:

> **The marketplace signing key is a single point of failure with no rotation
> path.** It is on the Fly volume, was created on first boot, and cannot be
> regenerated without invalidating every previously issued tessera.
> — `docs/specs/2026-10-04-settlement-auth-threat-model.md`

## What the key does today

One Ed25519 private key, `data/signer.key` (`--signer-key`), is loaded at boot by
`ledger.LoadOrCreateSigner`. It is the *same* key for three jobs, deliberately
(`internal/ledger/signer.go`):

1. It signs each tessera, a JWT (`EdDSA`) issued when a trade settles
   (`ledger.Record`).
2. It signs marketplace attestations — the card attestation, probe reports — as a
   detached `attest.Signature` whose `KeyID` is the public key (`Signer.AttestationSigner`,
   `registry.Service.market`).
3. Its public half is the identity the service publishes: `verificationKey` in
   `/healthz`, in every tessera response, and in the ledger listing.

Verification is pinned to the single current key:

- `ledger.Verify` parses a tessera with `l.signer.PublicKey()` and nothing else.
- `registry` verifies every marketplace attestation against
  `s.market.PublicKeyBase58()` (`VerifyOfferAttestation`, `VerifyProbe`,
  `httpapi` card attestation).
- `tesseraPayload` returns `led.VerificationKey()` — the *current* key — for a
  receipt **whatever key actually signed it**.

The last point is the sharp edge. Today there is one key, so "the current key"
and "the key that signed this receipt" are always the same string. The moment a
second key exists they are not, and a receipt fetched after a rotation would
advertise a key that does not verify it.

## Goals

- Replace the signing key without invalidating already-issued tesserae or already
  published marketplace attestations.
- Keep exactly **one active signing key** at a time. Rotation is a swap, not a
  quorum.
- Publish the full set of keys a verifier must accept, so a client can check an
  old signature and a new one with the same document.
- Keep it a deliberate, auditable operator action, not something that happens by
  accident on restart.

## Non-goals

- **Recovery from compromise.** This is the important one, and it is not a
  limitation to fix later. Retired public keys must stay valid to verify old
  receipts. If the current private key was stolen, the thief can still mint
  receipts under it *after* it is retired, and keeping it trusted to preserve old
  receipts keeps the forgery valid too. Rotation is for planned replacement and
  for key loss; a compromise forces a choice between invalidating old receipts
  and accepting forgeries, and this design does not pretend to square that
  circle. The threat model keeps compromise listed as catastrophic.
- **Online rotation under load.** No admin route generates a key. Generating a
  signing key is an offline operator action followed by a restart.
- **A second signing identity.** Cards, offers and probes keep sharing the one
  active key; nothing here splits them.

## Design

### Keyring

Replace the single key with a keyring held in memory:

- **current** — the only private key present; signs everything.
- **retired** — zero or more public keys, verify-only. Never a private key.

`attest.Verify` already takes the expected key from the caller and refuses a
signature from any other key. That is the right primitive; the change is that a
caller passes the whole ring instead of one ID. Add `attest.VerifyAny(payload,
sig, keyIDs)` (or have callers loop) so a marketplace signature is accepted when
its `KeyID` is any trusted key. A tessera selects its key by the `kid` header
described below.

### Signing and the tessera header

New content is always signed by **current**. Tessera JWTs gain a `kid` header
holding the current key's base58 public key:

```go
token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
token.Header["kid"] = l.signers.CurrentBase58()
signed, err := token.SignedString(l.signers.CurrentPrivate())
```

`ledger.Verify` reads `kid`, looks it up in the ring, and verifies against that
key. A receipt signed before this change has no `kid`; it is verified against the
single legacy key, which the migration keeps as current (see *Migration*).

### Publishing the key set

`/healthz` keeps `verificationKey` = the **current** key, so nothing that reads
today breaks, and gains `verificationKeys` — the full ordered set, current first.
The tessera response changes to advertise the key that *signed this receipt*
(from `kid`, falling back to the sole key), so a stored receipt fetched after a
rotation still verifies against the key the response names. The ledger listing
advertises the set the same way.

A verifier's rule becomes: check the signature against `keyId` as named, and
require `keyId` to be a member of `verificationKeys`.

### File format

`data/signer.key` becomes a JSON document:

```json
{
  "version": 1,
  "current": "<base64 Ed25519 private key>",
  "retired": ["<base58 public key>", "..."]
}
```

The loader stays backward compatible: a file that is not JSON is decoded as the
legacy raw base64 private key and treated as `{current: that key, retired: []}`.
The first write by `rotate` upgrades it in place. The file keeps mode `0600`;
the directory keeps `0700`.

Retired keys are public and are kept **indefinitely**, because an off-chain
tessera never expires and must verify for as long as anyone relies on it. There
is no pruning in this design; a future one could prune only with a sunset for the
receipts that depend on the key.

### Rotation procedure

An offline subcommand:

```bash
vtessera key rotate --signer-key data/signer.key
```

It reads the keyring, appends the current public key to `retired`, generates a
new current, and writes the file atomically (write a temp file in the same
directory, `fsync`, `rename`, `fsync` the directory). The old private key is
**not** written anywhere — after the rename it exists only in memory of the short
lived command, and the operator is told to confirm the new `verificationKey`.

Runbook (`docs/deploy.md`):

1. Stop the machine (or accept that the running process has not yet switched).
2. `vtessera key rotate ...`; note the printed new `verificationKey`.
3. Deploy/restart. `/healthz` shows the new `verificationKey` and both keys in
   `verificationKeys`.
4. Confirm an old tessera still verifies (it names its `kid`) and a new one is
   signed by the new key.

Rotation does **not** touch `/data`'s database. The service still refuses to start
without the keyring, and a new deployment with no key still creates one.

## Migration and compatibility

- Legacy single-key file: loads as current, retired empty. No action required.
- Old tesserae with no `kid`: verified against the key that was current when they
  were issued, which is the legacy key until the first rotation, after which it
  is retired but still trusted. Behaviour is unchanged.
- Old marketplace attestations carry `KeyID`; accepted while that key is in the
  ring, which for a legacy deployment is the current one.
- A reader that only knows `verificationKey` (older clients) keeps working for
  current signatures and must read `verificationKeys` to check a retired one.
  This is additive: the field is new, and its absence has not changed.

## What must not happen

- **Do not drop the retired keys to make `/healthz` tidy.** That is the same
  class of mistake as editing a genesis pin: it converts a detectable,
  explainable old receipt into an apparently forged one.
- **Do not add a default or an online rotation trigger.** An operator rotates on
  purpose, and a mistake there is a security event.
- **Do not keep retired private keys.** Only public keys are retained, and the
  rotate command must overwrite and not archive the old secret.

## Tests

- A tessera signed before rotation verifies after it, against the `kid` it names.
- A new tessera after rotation is signed by the new current key and verifies.
- A marketplace card attestation signed by a retired key still verifies while
  that key is in the ring, and is refused once it is not.
- A signature under a key that is not in the ring is refused.
- A legacy raw-base64 key file loads as a single current key with no retired
  keys, and the first rotation upgrades it in place without losing the old key.
- `/healthz` reports `verificationKey` equal to `verificationKeys[0]` and lists
  the retired keys.
- `tesseraPayload` advertises the key named by the receipt's `kid`, not the
  current key, once the two differ.

## Decisions recorded at implementation

1. **Field names.** `verificationKeys` — ordered, current first — alongside the
   existing `verificationKey`, which stays the current key so nothing that reads
   it today breaks. A verifier's rule is: check the signature against the key the
   signature names, and require that key to be a member of `verificationKeys`.
2. **Rotation trigger.** Offline `vtessera key rotate --signer-key PATH`, run
   against a stopped service. It upgrades a legacy raw-base64 file to the JSON
   keyring on the first rotation. There is no online trigger and no default.
3. **Pruning.** Retired keys are kept indefinitely. An off-chain tessera never
   expires and must verify for as long as anyone relies on it; pruning waits for
   a receipt-sunset decision.
4. **Scope.** The `kid`/receipt-response fix lands with the keyring in the same
   change. The "advertises the current key for an old receipt" defect is closed
   by the same commit, not shipped separately.

## Implemented shape

The signing key file is now a JSON keyring (`{version, current, retired}`) with a
legacy raw-base64 fallback. `ledger.Verify` resolves a receipt by its `kid`
header against the ring, `attest.VerifyAny` accepts a marketplace signature made
by any trusted key, `/healthz`, the tessera response and the ledger listing all
publish `verificationKeys`, and the tessera response advertises the key the
receipt actually names. The `registry` trusts the retired keys too, so a card or
probe attested before a rotation keeps verifying.

Runbook: `docs/deploy.md`. Threat model updated: `docs/specs/2026-10-04-settlement-auth-threat-model.md`.
