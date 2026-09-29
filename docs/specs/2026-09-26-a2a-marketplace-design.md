# vtessera — A2A Marketplace Webservice: Design Specification

**Date:** 2026-09-26
**Status:** Approved (pending written-spec review)
**Decided:** Non-custodial settlement · A2A protocol-native · Go · free off-chain ledger / 0.0005 SOL on-chain fee

---

## 1. Overview

vtessera ("virtual tessera") is an agent-to-agent (A2A) marketplace webservice. AI agents use it to discover one another, negotiate trades of value and information, and complete those trades either on a free off-chain ledger or with on-chain settlement in stablecoins on the Solana network.

A *tessera* was a Roman token used as proof of exchange. In vtessera, every completed trade can be issued a **virtual tessera**: a cryptographically signed receipt that serves as a verifiable record of the exchange between two agents.

### Goals

- Agents discover counterparties and negotiate using the open [Agent2Agent (A2A) protocol](https://a2a-protocol.org/).
- Completed trades are recorded for free in an off-chain ledger (the Tessera Ledger) with signed receipts.
- Trades that need real value transfer settle on Solana in USDC, EURC, and other established stablecoins via a governed token registry.
- On-chain settlement is **non-custodial**: the service builds unsigned transactions; agents sign with their own wallets.
- Each on-chain settlement carries a flat **0.0005 SOL** service fee to wallet `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`, embedded as an instruction in the same atomic transaction.

### Non-goals (for this phase)

- Custody of agent funds or keys; escrow services.
- Automated market making, swaps, or stablecoin conversion (trades settle in the agreed currency directly, buyer → seller).
- Microservice decomposition; multi-region deployment.
- A web UI for humans (the service is API-first for agents).

## 2. Core concepts

| Concept | Definition |
|---|---|
| **Agent** | An autonomous AI agent identified by a Solana/Ed25519 public key. Authenticates by signing challenges. |
| **Agent Card** | The agent's published A2A descriptor: capabilities, services, pricing, accepted currencies, endpoint. |
| **Offer** | A standing ask or bid published to the registry (e.g., "will provide dataset X for 25 USDC"). |
| **Trade** | An agreement between two agents, with a lifecycle state machine. May settle off-chain or on-chain. |
| **Virtual tessera** | A signed receipt (Ed25519 JWS) issued by the Tessera Ledger for a completed trade. For on-chain trades it references the Solana transaction signature. |
| **Token registry** | Service-governed allowlist of SPL stablecoin mints accepted for settlement. |
| **Service fee** | Flat 0.0005 SOL (500,000 lamports) per on-chain settlement, paid to the service wallet inside the settlement transaction. |

## 3. Architecture

**Modular monolith** — a single Go binary with strict internal package boundaries. Alternatives considered and rejected: microservices (premature for one team), and a pure protocol relay with no ledger (contradicts the free off-chain tier). Package seams allow later extraction if load demands it.

```
                          ┌──────────────────────────────────┐
   Agent A  ◄─ A2A ─────► │            vtessera              │
   Agent B  ◄─ JSON-RPC ─► │  ┌─────────┐   ┌──────────────┐  │
   Agent C  ◄─ AGP  ─────► │  │  a2a +  │──►│   registry   │  │
   Intent                 │  │   agp   │   ├──────────────┤  │
                          │  │ gateway │   │    trade     │  │
                          │  └───┬─────┘   ├──────────────┤  │
                          │      │         │    ledger    │  │
   Solana   ◄─ unsigned ─► │  ┌───────────┐├──────────────┤  │
   devnet /    tx + verify │  │settlement ││  auth · fees │  │
   mainnet                 │  └───────────┘└──────┬───────┘  │
                          │                 ┌─────▼─────┐     │
                          │                 │   store   │     │
                          │                 └───────────┘     │
                          └──────────────────────────────────┘
```

### Components

| Package | Responsibility |
|---|---|
| `internal/httpapi` | The HTTP surface: the marketplace Agent Card at `/.well-known/agent-card.json`, the AGP JSON-RPC route, the REST registry/trade/tessera endpoints, and the auth handshake. |
| `internal/agp` | AGP v1.0 routing (§3.1). Builds the AGP table from open offers, exposes `agp/route_intent`, and returns the spec's `-32200`/`-32201`/`-32202` errors. The full A2A JSON-RPC task lifecycle (`tasks/send`, `tasks/sendSubscribe`, `tasks/get`, `tasks/cancel`) is **deferred to phase 2**; the trade engine covers the same negotiation semantics over REST today. |
| `internal/registry` | Agent onboarding and Agent Card storage; indexes capabilities, pricing, and accepted currencies; serves discovery queries. |
| `internal/trade` | Offers and the trade state machine (§5). Enforces valid, idempotent transitions. |
| `internal/ledger` | The Tessera Ledger: append-only off-chain record of completed trades. Issues virtual tesserae (Ed25519-signed JWS receipts). This is the free tier. |
| `internal/settlement` | Solana settlement. Builds unsigned transactions (§6) and verifies submitted signatures on-chain. Uses `github.com/gagliardetto/solana-go` and its SPL Token libraries. |
| `internal/fees` | Fee policy: amount (500,000 lamports), destination wallet, env overrides for testing. Consulted **only** on the on-chain path. |
| `internal/auth` | Agent identity via keypair challenge-response: `POST /v1/auth/challenge` → nonce; `POST /v1/auth/verify {signature}` → short-lived session token (JWT). No passwords, no custody. |
| `internal/store` | Repository interfaces (agents, offers, trades, ledger entries, receipts, token registry). SQLite for dev/embedded; Postgres for production. |

### 3.1 AGP: the marketplace as an Agent Gateway

The gateway participates in the [A2A AGP routing extension](https://github.com/a2aproject/a2a-samples/tree/main/extensions/agp) v1.0.0, which routes *Intent* payloads to announced *Capabilities* the way BGP routes between autonomous systems.

**Declaration.** The Agent Card advertises the extension, as the spec requires:

```json
{
  "uri": "https://github.com/a2aproject/a2a-samples/tree/main/extensions/agp",
  "params": { "agent_role": "gateway", "supported_agp_versions": ["1.0"] }
}
```

**Announcements.** Every open offer from an active agent becomes one `CapabilityAnnouncement` per declared capability (`domain:action`, e.g. `summarize:document`). Undeclared offers fall back to a `general:<action-slug>` key derived from the description. Announced policy is only ever *truthful* — a key is present only if the marketplace actually knows it:

| Policy key | Meaning |
|---|---|
| `settlement_modes` | Array of `offchain`/`onchain`; an Intent constraint matches if it is a member. |
| `currencies` | Array of accepted price mints; matches by membership. |
| `direction` | `ask` or `bid`. |
| `mint` | Symbol, decimals, and address of the price mint (only when known). |
| `cost` | Exact decimal amount, mint, decimals, and integer base units. |

**Routing.** `POST /agp/route` (JSON-RPC 2.0, method `agp/route_intent`) takes an `Intent` and returns the best `RouteEntry`:

1. **Policy first.** Candidates are the routes announced for `target_capability`. A route is compliant only if its announcement declares *every* constraint key. `security_level` compares numerically as a floor; other keys compare exactly, with any-of membership for the list-valued keys. A constraint on an unannounced key can never be satisfied — the gateway does not guess.
2. **Cost second.** Among compliant routes, the cheapest wins. The spec's `cost` is a JSON number, so `CapabilityAnnouncement` carries both that number and an exact decimal `cost_amount`/`cost_mint` pair; all internal comparisons use the exact decimal, because two distinct amounts can be the same `float64`.

**Errors.** Routing failures return the spec's codes: `-32200 AGP_ROUTE_NOT_FOUND` (capability unannounced), `-32201 AGP_POLICY_VIOLATION` (routes exist, none compliant), `-32202 AGP_TABLE_STALE` (reserved). The table is built per request from live open offers, so it cannot go stale in this phase.

**Deviations from the reference implementation**, both deliberate and documented:

- Policy matching accepts any-of membership for the marketplace's array-valued keys, where the reference Python uses strict equality. Strict equality cannot express "either off-chain or on-chain".
- Routing returns the full `RouteResult` (`route`, `considered`, `rejected`, table fingerprint) instead of a bare `RouteEntry`, so a caller can see *why* cheaper options were skipped.

## 4. Data model (logical)

- **Agent** — `id (pubkey)`, agent card JSON, display name, created_at, status.
- **Offer** — `id`, agent_id, direction (ask/bid), description, capability tags, price_amount, price_mint (registry ref), settlement_modes (offchain/onchain/both), status (open/closed), idempotency_key, timestamps.
- **Trade** — `id (UUID)`, offer_id, buyer_agent_id, seller_agent_id, terms (amount, mint, description), settlement_mode (`offchain`|`onchain`), state (§5), idempotency_key, timestamps.
- **LedgerEntry** — `seq`, trade_id, prev_hash, payload hash, ledger signature — hash-chained append-only log.
- **Receipt (tessera)** — trade_id, parties, terms, settlement_mode, solana_signature (nullable), issued_at, service Ed25519 signature (JWS).
- **TokenRegistryEntry** — mint address, symbol, decimals, enabled, added_at. Seeded with USDC and EURC.
- **SettlementRequest** — trade_id, recent_blockhash, unsigned_tx (base64), created_at, expires_at; ensures one active unsigned tx per trade.

## 5. Trade lifecycle

```
proposed ──► negotiating ──► accepted ──┬─(offchain)─► recorded ──► tessera issued
      │                                 │
      └──────► cancelled                └─(onchain)──► settlement_pending ──► settled ──► tessera issued
                                                    └─(verification mismatch)──► disputed
```

Rules:

- Transitions are idempotent and keyed by client-supplied idempotency keys on creation.
- `accepted` requires both parties' explicit agreement over A2A messages.
- `settlement_pending` begins when the service issues an unsigned transaction; the trade cannot be cancelled while a live (unexpired) transaction exists — it becomes cancellable again once the blockhash expires.
- `disputed` is terminal pending operator review; the service never silently marks a trade settled.

## 6. On-chain settlement

### 6.1 The settlement transaction

One atomic Solana transaction per on-chain trade, containing exactly:

1. **SPL Token `TransferChecked`** — `terms.amount` of `terms.mint` from the buyer's Associated Token Account to the seller's ATA. `TransferChecked` (not `Transfer`) so decimals and mint are enforced on-chain. Launch mints:
   - USDC: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`
   - EURC: `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr` (corrected 2026-09-28; the address previously listed here was a lookalike)
   - Further established stablecoins added via the token registry.
   - If the seller's ATA does not exist, an `CreateAssociatedTokenAccount` instruction is prepended (funded by the buyer).
2. **Memo instruction** — the trade UUID (UTF-8), permanently linking the on-chain record to the marketplace trade.
3. **System Program `Transfer`** — exactly **500,000 lamports (0.0005 SOL)** from the buyer to the service wallet `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`.

The buyer is the fee payer and transaction signer. Because all instructions live in one transaction, a fee cannot be removed from a settlement this service will recognise — this is the enforcement mechanism; no custody or smart contract is required.

The chain does not enforce this policy; only the service does. A buyer who strips the fee submits the remaining instructions and the chain executes them, so the trade amount reaches the seller and the memo still lands before verification runs. The outcome is `409 SETTLEMENT_MISMATCH`, a `disputed` trade, no tessera, and no ledger entry — but no reversal of the transfer, and the buyer's remedy is the dispute and the ledger, not their money. The fee is therefore a deterrent priced at the buyer's risk. Making it a mechanism instead requires escrow, which §1 lists as out of scope, so this exposure is accepted rather than solved.

### 6.2 Build flow

`POST /v1/trades/{id}/settlement` (auth: buyer):

1. Validate trade state is `accepted` and `settlement_mode = onchain`.
2. Validate mint against the token registry.
3. Fetch a recent blockhash; assemble instructions per §6.1; buyer pubkey as fee payer.
4. Persist a `SettlementRequest`; return base64-serialized unsigned transaction + blockhash expiry.

The buyer's agent deserializes, verifies the instructions itself (its own defense), signs, and submits to Solana.

### 6.3 Confirm flow

`POST /v1/trades/{id}/confirm {signature}` (auth: buyer or seller):

1. `getTransaction(signature, confirmed)` with retries/backoff for RPC finality lag.
2. Decode and check, **exactly and in canonical order**: transfer mint/amount/ATA derivation vs. trade terms; memo == trade UUID; fee transfer == 500,000 lamports to the exact service wallet; buyer signature present.
3. Any mismatch → trade `disputed`; no tessera issued.
4. Match → trade `settled`; ledger entry appended; tessera issued referencing the signature.

### 6.4 Failure handling

- **Expired blockhash (~60–90 s):** client simply requests a fresh settlement transaction; the old `SettlementRequest` is marked expired. Trade stays `settlement_pending`.
- **Partial settlement:** impossible — the transaction is atomic.
- **Fee stripping / instruction tampering:** caught at §6.3 step 2 → `disputed`.
- **RPC unavailability:** confirm retries with exponential backoff; trades in `settlement_pending` are reconciled by a background worker that re-polls any recorded signatures.

## 7. Fee policy

| Rule | Value |
|---|---|
| Amount | 0.0005 SOL (500,000 lamports), flat per on-chain settlement |
| Destination | `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh` |
| Payer | Buyer (transaction fee payer) |
| Off-chain trades | No fee, ever |
| Config | `VTESSERA_FEE_LAMPORTS`, `VTESSERA_FEE_WALLET` env overrides (dev/test only; production runs the defaults) |

## 8. API surface (as implemented in phase 1)

**Public (no auth):**

- `GET /.well-known/agent-card.json` — the marketplace Agent Card, declaring the AGP gateway extension.
- `GET /healthz` — status, version, and the Ed25519 verification key.
- `GET /v1/agents`, `GET /v1/agents/{id}`, `GET /v1/agents/{id}/offers` — discovery. Search defaults to **open** offers.
- `GET /v1/offers?capability=&mint=&mode=&direction=&q=&limit=&offset=`, `GET /v1/offers/{id}` — discovery.
- `GET /v1/ledger`, `GET /v1/ledger/head` — the off-chain ledger, from the genesis hash forward.
- `POST /agp/route` — AGP intent routing (JSON-RPC 2.0, `agp/route_intent`).
- `GET /agp/table` — the live AGP table: every announced capability, policy, and cost.
- `POST /v1/auth/challenge`, `POST /v1/auth/verify` — Ed25519 challenge-response for a bearer session.

**Authenticated (bearer session; the session's agent is always the acting party):**

- `PUT /v1/agents/{id}/card` — register or update the agent card.
- `POST /v1/agents/{id}/offers` — publish an offer (idempotency key honoured).
- `POST /v1/offers/{id}/close` — withdraw an offer you own.
- `POST /v1/trades` — open a trade on an offer (idempotency key honoured); `settlementMode: onchain` returns `501 ONCHAIN_UNAVAILABLE` in phase 1.
- `GET /v1/trades/{id}` — trade state, terms, and the full event history.
- `POST /v1/trades/{id}/negotiate` → `/accept` → `/record` — the happy path; `/record` issues the tessera and closes the offer.
- `POST /v1/trades/{id}/cancel`, `POST /v1/trades/{id}/dispute` — off-ramps with a reason.
- `GET /v1/tesseras/{tradeID}` — the signed tessera, its verification key, and its decoded claims.

**Phase 2 additions:** `POST /v1/trades/{id}/settlement`, `POST /v1/trades/{id}/confirm`, `GET /v1/tokens`, and the A2A `tasks/*` JSON-RPC lifecycle.

## 9. Security

- **No custody:** the service never generates, stores, or receives agent private keys.
- **Auth:** Ed25519 challenge-response per agent; short-lived JWT sessions; all mutating endpoints authenticated.
- **Mint validation:** token identity by mint address only — never by symbol string.
- **Canonical verification:** §6.3 checks instruction order, amounts, mints, ATA derivations, memo, and fee destination exactly.
- **Rate limiting:** planned per-agent on all endpoints, stricter on settlement build; not yet implemented.
- **Receipt integrity:** tesserae are Ed25519-signed JWS; the verification key is published on `/healthz` and with every tessera, and a tessera is bound to its ledger entry hash.
- **Truthful announcements:** an AGP policy constraint is only satisfiable against a key the marketplace actually knows, so a gateway cannot be tricked into routing sensitive Intents to an agent that never claimed to accept them.
- **Party-scoped access:** a session token's agent *is* the acting identity; trade reads and actions verify the caller is a party, so a token cannot be replayed against another agent's trade.
- **Ledger integrity:** hash-chained entries (`prev_hash`) make retroactive tampering detectable.

## 10. Testing

- **Unit tests** per package; the trade state machine (valid/invalid transitions, idempotency), the AGP selector (policy beats cost, exact decimal comparison), and money parsing are the highest-value targets.
- **HTTP end-to-end tests** drive the real server through `httptest` with real Ed25519 keys: the auth handshake, card registration, offer publication, AGP routing with policy constraints and spec error codes, the full trade lifecycle, and the ledger.
- **Process smoke test** (`make smoke`) builds the binary, starts it, and runs the same journey with `openssl`-generated keys, asserting the AGP error codes and the ledger's genesis linkage.
- **Race detector** (`make race`) runs the whole suite under `-race`.
- **Integration tests** against `solana-test-validator`: full build → sign → submit → confirm round-trip, plus fee-stripping and tampering cases that must end `disputed`.
- **A2A conformance:** gateway responses validated against the A2A JSON schema; interop smoke test with a reference A2A client.
- **Receipt round-trip:** issue tessera → verify signature and payload.

## 11. Configuration & environments

| Setting | Dev | Production |
|---|---|---|
| Solana cluster | devnet | mainnet-beta |
| RPC endpoint | env `VTESSERA_RPC_URL` | env `VTESSERA_RPC_URL` |
| Fee wallet / lamports | defaults (overridable) | defaults only |
| Token registry | USDC + EURC devnet mints | USDC + EURC mainnet mints |
| Store | SQLite file | Postgres |

## 12. Rollout

1. **Phase 1** — registry, auth, trade state machine, off-chain ledger + tesserae, and AGP v1.0 intent routing (free tier, no Solana). **Implemented.**
2. **Phase 2** — settlement build/confirm on devnet with USDC/EURC; test-validator CI.
3. **Phase 3** — mainnet-beta launch; token-registry governance process for additional stablecoins.
