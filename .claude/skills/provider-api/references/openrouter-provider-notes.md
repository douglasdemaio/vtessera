# OpenRouter "For Providers" — distilled notes

Source: https://openrouter.ai/docs/guides/community/for-providers (fetched 2026-08-23)
Schema: provider-monitor-schema-v2.openapi.json (v2.4, vendored in this directory)

## What a provider must implement

1. **`GET /v1/models`** returning `{ "data": [ <ModelDocumentV2>... ] }`.
   Per model (schema_version "2.4"): `id`, `name`, `created` (unix),
   `input_modalities[]`, `output_modalities[]` (each modality object owns
   its pricing + constraints), optional root `pricing`, `capacity`,
   `passthrough_parameters`, plus operational fields `is_ready`,
   `deprecation_date` (ISO 8601), `is_free`, `discount_to_user`,
   `openrouter.slug`, and infrastructure fields `datacenters` (ISO 3166-1),
   `deployment_region`, `compliance` (ZDR, HIPAA booleans).
2. **Pricing**: modality-scoped arrays. Input units: token | image |
   megapixel | second | character. Output adds `request`. Root scope:
   request | web_search only. All `cost_usd` values are **strings** (no
   float error). Conditional overrides allowed (long-context tiers,
   time-of-day rates). Zero cost = genuinely free SKU; omit SKUs you don't
   bill.
3. **Capacity declarations**: throughput limits per minute/hour/day windows.

## How providers are scored (adopt for vtessera claim routing)

- Uptime = successful / total requests. Counts against you: 401/402/404,
  5xx, mid-stream failures, error finish reasons. Does NOT count: 429, 400.
- <80% uptime → fallback-only. 80–94% → deprioritized. Public metrics:
  TTFT and throughput (output tokens / generation time).
- Tool-call quality ("Auto Exacto"): needs 100+ general and 200+ tool-call
  requests in recent windows. Prefer early 429 over queueing.
- Stream immediately; send SSE keep-alive comments on long tasks.

## Onboarding flow

provider form (openrouter.ai/how-to-list) → payment rails (auto top-up or
invoicing; OpenRouter pays the provider for inference) → submit /v1/models
document (v2.4; legacy flat format still accepted) → baseline validation
tests (`is_ready:false` skips) → dashboard access (uptime, throughput,
tool-call success, benchmarks).

## Relevance to vtessera

- The modality-scoped, capacity-windowed model listing is the template for
  `AdvertisedModel` in `crates/offer`.
- The uptime tiering maps directly onto offer-index claim ordering.
- A vtessera provider that implements the facade endpoint is most of the
  way to also listing on OpenRouter itself (remaining gap: USD invoicing
  relationship with OpenRouter vs vtessera's on-chain EURC/USDC settlement —
  these can coexist; they are different buyers).
