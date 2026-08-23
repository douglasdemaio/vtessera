---
name: provider-api
description: Design and build vtessera's large-scale provider program — multi-node provider accounts, bulk capacity registration, a model catalog, and an OpenRouter-compatible /v1/models facade so big compute providers can plug in machines, infrastructure, and hosted models. Use when the user mentions providers, OpenRouter, for-providers, model catalog, /v1/models, capacity declarations, provider onboarding, or "large scale providers with API endpoints". The vendored OpenRouter Provider Monitor Schema v2.4 lives in references/.
---

# Vtessera provider API (OpenRouter-style onboarding)

Goal: today one Ed25519 key = one node, offers advertise **hardware only**
(`AdvertisedDevice`: Cpu/NvidiaGpu/NvidiaMig/AmdGpu/NvidiaVgpu), and there is
no provider entity, no model catalog, no bulk API, no OpenAPI spec. A
datacenter operator with 500 GPUs and hosted models has no sane on-ramp.
OpenRouter's provider program (references/openrouter-provider-notes.md) is
the design template: providers declare models/capacity via a schema'd
listing endpoint, the platform monitors uptime and routes accordingly.

## Hard constraints

- `OfferBody` canonical tag bytes are **append-only**
  (`crates/offer/src/lib.rs:84`) — extend the schema with new tags + bump
  `schema_ver`, never reorder or reuse tags. Old verifiers must still verify
  old offers.
- Identity stays Ed25519, no accounts/API keys in the public flow
  (ROADMAP "machine-native: no human signup, no API keys"). Provider
  identity = a provider keypair that **delegates** to node keys via signed
  certificates, not a login.
- Payments stay x402 + escrow in EURC/USDC with flat SOL fee. Do not invent
  new billing. Mainnet remains behind the documented checklist.
- Rate limiting / admission on offer-index already exists
  (`crates/offer-index/src/lib.rs:30-66`) — bulk endpoints must fit it.

## Implementation task list (for the executing model)

1. **Provider identity** (`crates/offer`): `ProviderCert { provider_pubkey,
   node_pubkey, expires_unix, sig }` — provider key signs node keys. Offers
   optionally carry the cert (new append-only tag). Offer-index verifies the
   chain and groups entries by provider.
2. **Model catalog schema** (`crates/offer`): new `AdvertisedModel { model_id,
   family, quantization, context_len, modalities_in/out, per-token or
   per-second pricing }` alongside `AdvertisedDevice`. Borrow field semantics
   from the vendored OpenRouter schema (modality-scoped pricing arrays,
   `cost` as strings, capacity per minute/hour/day windows, `is_ready`,
   `deprecation_date`). Keep it a subset — do not swallow all 64 KB of
   schema.
3. **Bulk registration API** (offer-index): `POST /providers/{pubkey}/offers`
   accepting a batch of signed offers; `GET /providers/{pubkey}` returning
   aggregate capacity + heartbeat health. Reuse existing verify + TTL logic.
4. **OpenRouter-compatible facade**: `GET /v1/models` on offer-index
   rendering the registered model catalog in Provider Monitor Schema v2.4
   shape (references/provider-monitor-schema-v2.openapi.json). This makes a
   vtessera index consumable by OpenRouter-style routers, and positions any
   vtessera provider to ALSO list on OpenRouter later (their requirements:
   /v1/models doc, ≥95% uptime for full routing priority, stream tokens
   immediately, early 429s over queueing — see notes file).
5. **Uptime/quality tracking** (offer-index): success/total per node from
   heartbeats + claim outcomes; expose in `GET /offers` ordering and in
   /metrics (observability skill). OpenRouter demotes <80% uptime to
   fallback — adopt the same tiering for claim routing.
6. **OpenAPI spec**: write `docs/api/openapi.yaml` covering node-api and
   offer-index as they exist BEFORE extending them (there is none today).
   Generated clients are the provider onboarding DX.
7. **Provider onboarding doc** `docs/PROVIDERS.md`: keygen, cert issuance to
   node keys, bulk publish, heartbeat contract (30s beat / 120s TTL), payout
   wallet (`payout_id` in PriceQuote), settlement expectations.
8. Sequence with the other two skills: fleet-management provisions the
   machines, observability proves the uptime numbers providers are ranked
   by.

## References

- `references/provider-monitor-schema-v2.openapi.json` — vendored OpenRouter
  Provider Monitor Schema v2.4 (OpenAPI 3.1), fetched 2026-08-23 from
  https://openrouter.ai/docs/assets/provider-monitor-schema-v2.openapi.json
- `references/openrouter-provider-notes.md` — distilled onboarding,
  uptime-scoring, and pricing-schema notes from
  https://openrouter.ai/docs/guides/community/for-providers

## Definition of done

A demo "provider" script registers 3 node keys under one provider cert with
2 advertised models; `GET /v1/models` on the offer-index validates against
the vendored schema; claims prefer the higher-uptime node.
