# Phase 3 — Cluster-Aware Settlement Design

**Date:** 2026-09-27
**Status:** Approved for planning (revision 2)
**Parent spec:** [`2026-09-26-a2a-marketplace-design.md`](2026-09-26-a2a-marketplace-design.md) §11 (Configuration & environments), §12 (Rollout, Phase 3)

---

## 1. Scope

Phase 3 of the parent spec reads: *"mainnet-beta launch; token-registry governance process for additional stablecoins."* That sentence bundles four independent subsystems, so Phase 3 is decomposed and sequenced:

| Workstream | Disposition |
|---|---|
| **Cluster-aware settlement** | **This document.** |
| Token-registry governance (admin surface, audit trail) | Deferred to its own spec and cycle. |
| Postgres store (parent spec §11 promises it for production) | Deferred to its own spec and cycle. |
| A2A `tasks/*` lifecycle and conformance (parent spec §10) | Deferred; unrelated to Solana. |

This document makes the cluster a first-class value so the service cannot settle against a token that does not exist on the chain it is connected to, and so a mainnet-beta deploy is deliberate rather than a mistyped string.

## 2. Motivation

### 2.1 The governed EURC mint does not exist

`internal/tokens` ships `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc` as the governed EURC mint. Queried on 2026-09-27 (mainnet slot 450860693) against **two independent providers** — `api.mainnet-beta.solana.com` and `solana-rpc.publicnode.com`:

```
getAccountInfo HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc -> {"value": null}   <-- both providers
getAccountInfo HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr -> SPL mint, 6 decimals, owner Tokenkeg...
```

The shipped constant is a lookalike: it shares a **30-character** prefix with the real address and diverges at position 31. A 30-character match is computationally infeasible to grind, so this is a transcription or generation error rather than an attack — which means the impact is liveness, not safety: no private key exists for the bogus address, so nobody can create a token there. It also means every other hardcoded base58 constant from the same source is suspect (§12.5). This is precisely the spoofing case the `tokens` package documents itself as existing to prevent — *"Token identity is by mint address only — never by symbol — because symbols are not unique across issuers and are trivially spoofed."* Any trade priced in that "EURC" could never settle, and any agent card advertising it names a token that does not exist.

### 2.2 The default registry is not cluster-aware

`tokens.Default()` returns one fixed pair of mints regardless of which chain the process is connected to. The USDC constant is mainnet's, so a devnet or local-validator deployment advertises and prices in a mainnet token. The parent spec's §11 table requires devnet mints in dev and mainnet mints in production; the code cannot express that distinction.

### 2.3 Nothing identifies the cluster

An operator points `VTESSERA_RPC_URL` at a host and the service trusts it. A devnet URL on a mainnet deployment — or a typo'd host, or a proxy pointing somewhere unexpected — is indistinguishable from the intended configuration until trades dispute. A single boot-time check is also not enough: a proxy behind a fixed URL can repoint without a restart (§9.6).

## 3. Verified on-chain constants

Read from public RPC on 2026-09-27. These are inputs to the implementation and are pinned by a recorded-snapshot test (§9.4).

**Provenance.** Mainnet values were confirmed against two independent providers and are treated as verified. Devnet values rest on a single provider (`api.devnet.solana.com`); a second devnet provider could not be reached during design. Re-deriving the devnet row from an independent endpoint is a pre-merge action item (§12.5).

### 3.1 Governed mints

| Cluster | Token | Address | Mint authority | Decimals |
|---|---|---|---|---|
| mainnet-beta | USDC | `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v` | `BJE5MMbqXjVwjAF7oxwPYXnTXDyspzZyt4vwenNw5ruG` | 6 |
| mainnet-beta | EURC | `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr` | `Hy9168u7b2Toujh8SJKtKK8DWbyaBbXPzxm337R4o4XY` | 6 |
| devnet | USDC | `4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU` | `GrNg1XM2ctzeE2mXxXCfhcTUbejM8Z4z4wNVTy2FjMEz` | 6 |
| devnet | EURC | `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr` | `DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99` | 6 |
| localnet | *(none governed)* | — | — | — |

**EURC is one address on two chains but two different accounts.** The address is shared across mainnet-beta and devnet, while the mint authority differs per cluster (as it must — the ledgers are independent). An implementation that deduplicates EURC by address, or that verifies address alone, is wrong. Both properties are asserted by test.

### 3.2 Token programs

| Program | Address |
|---|---|
| SPL Token (classic) | `TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA` |

Every governed mint above is owned by the classic SPL Token program. The check is hardcoded to it, which means a future Token-2022 stablecoin (PYUSD and others) will fail preflight. This is a known limitation, not an oversight; §12.6 records it for the deferred governance spec, which will need a per-entry program field before it can onboard such a token.

### 3.3 Cluster identity by genesis hash

| Cluster | Genesis hash |
|---|---|
| mainnet-beta | `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d` |
| devnet | `EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG` |
| testnet | `4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY` |

`testnet` is recorded specifically so an accidental testnet deploy fails loudly with a clear message instead of passing as devnet.

### 3.4 Fee policy

| Item | Value |
|---|---|
| Amount | `1000` lamports |
| Destination | `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh` |
| Rent-exempt minimum, 0-byte account | **queried at runtime**, not hardcoded (see below) |

### 3.5 Production RPC endpoints

| Cluster | Documented endpoint |
|---|---|
| mainnet-beta | `https://solana.publicnode.com/` |
| devnet | `https://api.devnet.solana.com` |
| localnet | operator-supplied loopback endpoint |

`https://solana.publicnode.com/` is the documented mainnet-beta endpoint and the target of `make preflight-live`. It also served as the independent second provider for the §3.1 verification.

It is a third-party public provider, which makes it a sound default and a poor long-term dependency. Production deployments should prefer a dedicated or self-operated endpoint. The genesis-hash pin (§3.3) is what makes provider choice safe rather than risky: any endpoint reporting the mainnet-beta genesis hash is acceptable, and a third-party endpoint cannot silently substitute another chain. A private endpoint therefore needs no code change, only configuration.

**What any endpoint must provide**, self-operated or public:

- The JSON-RPC methods the preflight and settlement paths use: `getGenesisHash`, `getAccountInfo`, `getLatestBlockhash`, `getFeeForMessage`, `getSignatureStatuses`, `getTransaction`, `getBalance`, `getMinimumBalanceForRentExemption`.
- Distinguishable rate-limit and timeout behaviour. The service depends on the split in §5.4 between "the mint is wrong" and "the node could not be reached"; an endpoint that reports a throttled request as a missing account silently converts a transient 429 into `MINT_UNVERIFIED` and a spurious outage.
- No method-level restrictions on the accounts the service reads.

### 3.6 Operating a mainnet node

If the operator's intent is to run their own mainnet-beta RPC node, that is **infrastructure, not service configuration**, and it changes nothing in this design: the service validates whichever endpoint it is given exactly as it validates a public one. Running a mainnet validator carries its own hardware, stake and operational requirements that this document does not address and does not need to.

The only thing the marketplace requires of a node is the list in §3.5. Everything else — whether the endpoint is a public provider, a dedicated node, or a node the operator runs — is a deployment-topology choice behind `VTESSERA_RPC_URL`.

**The invariant is rent exemption, and it is enforced by the runtime, not by the instruction.** This is the single most easily mis-derived claim in this document, so it was settled empirically against a local test validator (Agave 3.1.14) on 2026-09-27. Amounts below are in SOL because that is what `solana transfer` accepts, with the lamport equivalent in brackets:

| Amount sent | Result |
|---|---|
| 0.00001 SOL (10,000 lamports) | **rejected**, RPC `-32002` `Transaction results in an account (1) with insufficient funds for rent` |
| 0.00089079 SOL (890,879 lamports) | **rejected**, identical error — one lamport under the minimum |
| 0.00089088 SOL (890,880 lamports) | **accepted**, finalized with `err: null`; account created at `lamports: 890880, space: 0, rentEpoch: u64::MAX` |
| 0.00100000 SOL (1,000,000 lamports) | **accepted** — a round control well clear of the boundary; it is *not* the minimum |

Two details make this counter-intuitive, and each one alone leads to the wrong answer:

- The System Program reports **success**. The log reads `Program 11111111111111111111111111111111 invoke [1]` / `success`, because `transfer_verified` only checks that the sender has enough lamports. Reading the program log, or reading `transfer_verified`, tells you the transfer succeeded. It did not.
- The rejection comes from the runtime's **post-transaction** account sanitization, which validates that every account left standing by the transaction is rent-exempt. That check is `TransactionError::InsufficientFundsForRent`, a transaction-level error, not a program error. A non-existent account does load with `rent_epoch = u64::MAX`, but that only means it *would* be rent-exempt — the post-transaction check runs first and rejects the underfunded result, so the `u64::MAX` is never reached.

The failure is **atomic**. After the rejected 1,000-lamport attempt the destination still returned `value: null`, and the sender's balance was bit-for-bit unchanged — no transfer, and no transaction fee either. So the fee wallet is drained by nothing and the buyer's lamports are not burned; the settlement simply cannot complete until the wallet is funded.

The threshold must be queried from the node and never hardcoded, and the reason is now concrete rather than theoretical: `getMinimumBalanceForRentExemption(0)` returns **650,240** on both mainnet-beta and devnet, but **890,880** on a local test validator — a 37% difference. A constant derived locally would be wrong in production, and a constant derived from production would be wrong under `make test-solana`. The fee destination only ever *receives* — it never pays transaction fees — so no forward-looking buffer is justified either.

Consequence for mainnet: an unfunded fee wallet blocks every on-chain settlement, and it blocks them *silently* from the service's point of view, because the failure surfaces to the buyer who submits the transaction, not to the operator. Hence the fatal severity in §6.3.

## 4. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Verify mints against RPC at startup **and** on use, with a short TTL. | The shipped fake EURC would have been caught on the first boot. Address-only trust cannot catch a decimals or authority lookalike. |
| D2 | Require an explicit cluster declaration plus a separate mainnet acknowledgement. | "mainnet-beta" and "devnet" differ by one string — exactly the mistake a launch-phase service should make impossible rather than merely discouraged. |
| D3 | Fail-closed startup preflight, with fee policy pinned to defaults on mainnet-beta. | Prevents settling into a lookalike token and prevents settling into a fee wallet that would absorb the fee as rent. |
| D4 | localnet-only mint allowlist for the validator E2E suite, plus a loopback or explicitly allowed host. | Keeps the governed set closed in production while letting E2E exercise the real preflight path, including when the validator is a separate container. |
| D5 | Leave stale fake-EURC rows untouched; reject them at settlement time. | They are already unsellable once the mint is no longer advertised, and no destructive migration means nothing new to get wrong. |
| D6 | Cluster is a first-class value threaded through the system, and re-checked per reconciler tick. | A cluster used only at startup cannot protect in-flight requests from a later config flip, nor from a proxy that repoints without one. |
| D7 | Versioned migrations tracked by a `schema_migrations` table. | The store has no version tracking today, and the deferred Postgres workstream needs a mechanism that ports. |
| D8 | Keep the memo format unchanged. | Phase 2 signatures are bound to a recent blockhash and expire within minutes, so there is no durable body of signatures to invalidate; the load-bearing argument is that canonical comparison already pins every instruction, mints and fee transfer included. A cluster in the memo would add no security while changing an on-chain format. |
| D9 | Record the cluster as a **signed claim** on receipts issued from Phase 3 onward. | Otherwise a devnet receipt is cryptographically indistinguishable from a mainnet one — the same spoofing class this document exists to close. Absence of the claim means "pre-Phase-3", not "invalid", so existing receipts stay verifiable. |
| D10 | The reconciler **expires** cluster-mismatched requests; it does not merely skip them. | A mismatched `issued` request holds the per-trade partial unique index, so leaving it in place permanently wedges the trade: no replacement request can be issued. Expiry frees it and leaves the trade pending, which is exactly the existing behaviour for an unfillable request. |
| D11 | Mint verification distinguishes hard invariants from authority pins, and distinguishes "wrong" from "could not check". | A legitimate issuer mint-authority rotation must not become a self-inflicted settlement outage, and a rate-limited RPC must not be reported as a bad mint. |

## 5. Architecture

### 5.1 `internal/cluster` (new)

The only place a cluster is defined.

```go
type Cluster string

const (
    MainnetBeta Cluster = "mainnet-beta"
    Devnet      Cluster = "devnet"
    Localnet    Cluster = "localnet"
)

func Parse(s string) (Cluster, error)
func (c Cluster) GenesisHash() string          // "" for localnet
func (c Cluster) IsProduction() bool
func (c Cluster) AllowsExtraMints() bool       // true only for localnet
func (c Cluster) RequiresLoopback() bool       // true only for localnet
```

Errors: `ErrUnknownCluster`, `ErrClusterMismatch`, `ErrEndpointNotAllowed`, `ErrClusterUnsupported`.

`testnet` is a recognised but unsupported cluster: `Parse("testnet")` returns `ErrClusterUnsupported` and the message names `testnet` and the supported set. It is deliberately *not* a `Cluster` constant, so it cannot be declared by accident, and it is never silently treated as devnet.

### 5.2 `internal/tokens` (changed)

- `Token` gains `Cluster`, `MintAuthority` and `FreezeAuthority` (the latter two observational pins, §5.3).
- `Default()` is **deleted**. `ForCluster(cluster, opts)` is the only constructor, so every call site fails to compile until it names a cluster. The bug in §2.1 was possible precisely because a default existed.
- `ErrUngoverned` is distinct from `ErrNotRegistered` (not a pubkey at all) and `ErrDisabled` (governed, switched off). "Valid address, not governed on this cluster" is a different operational situation from either.
- Governed sets are per §3.1. `localnet` starts empty and requires the operator-supplied extras.

### 5.3 `internal/preflight` (new)

Fail-closed startup gate.

```go
// RPC is a narrow interface owned by preflight. It is deliberately separate
// from the settlement RPC interface: preflight needs reads the settlement
// path never performs, and widening that interface would force every existing
// test fake to implement them.
type RPC interface {
    GetGenesisHash(ctx context.Context) (string, error)
    GetAccountInfo(ctx context.Context, addr solana.PublicKey) (*AccountInfo, error)
    GetBalance(ctx context.Context, addr solana.PublicKey) (uint64, error)
    GetMinimumBalanceForRentExemption(ctx context.Context, dataLen uint64) (uint64, error)
}

type Deps struct {
    Cluster  cluster.Cluster
    RPC      RPC
    Mints    []tokens.Token
    FeePolicy fees.Policy
}

type Report struct {
    Cluster     cluster.Cluster
    GenesisHash string
    Mints       []MintCheck
    FeeWallet   string
    FeeBalance  uint64
    RentMinimum uint64
}

func Check(ctx context.Context, d Deps) (Report, error)
```

Checks, in order, each failure producing a typed error naming the specific check:

1. **Cluster identity** — `getGenesisHash` equals the declared cluster's pin. For `localnet`, the resolved endpoint address must be loopback, or listed in `VTESSERA_LOCALNET_ALLOW_HOST` (§6.2).
2. **Governed mints**, split into two classes:
   - **Hard invariants** — the account exists, its owner is the configured token program, and `decimals` matches the configured value. A failure is fatal at startup **and** on use: these are the properties settlement correctness actually depends on, and none of them can change without the token ceasing to be the token we priced.
   - **Governance pins** — `mintAuthority` and `freezeAuthority`. A failure is **fatal at startup**, where a human is present to decide whether to re-pin (§12.2), but is a **warning plus a counter on use**, where failing closed would turn a legitimate issuer rotation into a settlement outage (D11). A changed `freezeAuthority` is reported at both levels, since counterparties care about it more than about mint authority.
3. **Fee wallet** — exists, and holds at least `getMinimumBalanceForRentExemption(0)`. There is no fee buffer; see §3.4.

Nothing is cached at startup. A wrong mint must never be able to look right because it was right at boot.

`Report` is logged as a single structured line and is the return value tests assert on. `--preflight-only` runs `Check` and exits without serving; this is additive to the always-on startup gate, letting an operator validate an endpoint before a deploy.

### 5.4 On-use verification (`internal/settlement`)

Settlement dependencies carry a `Cluster`. Every settlement request records the cluster whose mint set was used to compile its frozen terms.

**The memo is unchanged** (`tessera:v1:<tradeUUID>`), per D8.

Verification of the price mint happens when a request is built, subject to a **30-second TTL cache keyed by (cluster, address), with single-flight so a burst of concurrent builds issues one RPC call.** The TTL is the correct balance: it still bounds staleness far below the blockhash lifetime, so a mint cannot "look right because it was right at boot", while collapsing the read amplification that an uncached per-build check would cause against a rate-limited public endpoint.

Outcomes are distinguished, because conflating them is how a transient 429 becomes an outage:

| Observation | Result |
|---|---|
| Account missing, wrong owner, or wrong decimals | `503 MINT_UNVERIFIED` — the token is not the one we priced. |
| `mintAuthority` or `freezeAuthority` differs from the pin | Request proceeds; warning logged, counter incremented. |
| RPC error, timeout, or rate limit | `503 ONCHAIN_UNAVAILABLE` with retry semantics — **not** `MINT_UNVERIFIED`. The mint's status is unknown, and reporting "wrong" would send an operator chasing a mint problem that does not exist. |

### 5.5 Receipts

For receipts issued from Phase 3 onward, `cluster` is a signed JWS claim (§8, D9). The verifier treats a receipt without the claim as pre-Phase-3: still valid, asserting no cluster. A receipt whose claim disagrees with the verifying service's cluster is reported as such rather than accepted silently.

## 6. Configuration

Settlement is enabled by `VTESSERA_RPC_URL` **and** `VTESSERA_CLUSTER` together.

- Neither set: settlement disabled, `501 ONCHAIN_UNAVAILABLE` (unchanged from Phase 2).
- RPC set, cluster missing: **startup error.** A half-configured deploy must not silently fall back to devnet mints.

### 6.1 Settings

| Setting | mainnet-beta | devnet | localnet |
|---|---|---|---|
| `VTESSERA_CLUSTER` | required | required | required |
| `VTESSERA_RPC_URL` | required | required | required |
| `VTESSERA_MAINNET_ACK` | **must be `1`** | ignored | ignored |
| `VTESSERA_FEE_LAMPORTS` | **must be unset** | allowed | allowed |
| `VTESSERA_FEE_WALLET` | **must be unset** | allowed | allowed |
| `VTESSERA_LOCALNET_MINTS` | rejected | rejected | allowed |
| `VTESSERA_LOCALNET_ALLOW_HOST` | rejected | rejected | allowed |
| `VTESSERA_BLOCKHASH_TTL` | required to parse | required to parse | required to parse |

**One rule for emptiness, applied uniformly:** a variable that is absent, present-and-empty, or containing only whitespace is *unset*, everywhere, for every setting in this table. A deploy template that exports an empty string must not fail to boot. *Must be unset* means the variable resolves to unset; any non-empty value is a startup error on mainnet-beta. *Rejected* means any non-empty value is a startup error. *Required to parse* means unset falls back to the default, but any value that is set and cannot be parsed is a startup error on every cluster.

That last row is a deliberate change from Phase 2, which silently fell back to the default on an unparseable value. Phase 3 is fail-closed everywhere else, and a mistyped settlement setting should surface at boot rather than be discovered later. It is the only behavioural regression risk in this configuration table, so its test asserts the error rather than the fallback.

Fee policy is pinned to spec defaults on mainnet-beta, per the parent spec §11 row "defaults only". It stays overridable on devnet and localnet, which the local-validator E2E depends on.

`VTESSERA_LOCALNET_MINTS` is a comma-separated list of `address:symbol:decimals:mintAuthority`. Its entries are verified by preflight exactly like governed mints.

### 6.2 Localnet endpoint rules

`localnet` is host-constrained because a disposable local chain has no meaningful genesis pin. Two rules, both needed:

- **Resolve, then check the address.** The endpoint host is resolved and the resulting IP is tested for loopback. The literal string `localhost` is not trusted, because it can resolve to a non-loopback address via `/etc/hosts`.
- **Explicit opt-in for containerised validators.** When the validator is a separate Compose service the endpoint is `validator:8899`, not loopback, and a string-only check would break CI. `VTESSERA_LOCALNET_ALLOW_HOST` lists hosts exempt from the loopback requirement. It is rejected on every non-localnet cluster, under the same rule as `VTESSERA_LOCALNET_MINTS`, so the escape hatch cannot widen anything in production.

### 6.3 Fee-wallet funding severity

| Cluster | Severity | Reason |
|---|---|---|
| mainnet-beta | **fatal** | An unfunded fee wallet does not reject transfers; it absorbs every fee as stranded dust while the trade completes normally. The revenue disappears with no failed transaction and no error anywhere. |
| devnet | warn | A dev first-run should not require a pre-funded wallet. |
| localnet | warn | The E2E harness funds its own. |

## 7. Error codes

Extending the family Phase 2 established (`ONCHAIN_UNAVAILABLE` 501, `SETTLEMENT_PENDING` 202, `SETTLEMENT_MISMATCH` 409, `SETTLEMENT_IN_PROGRESS` 409, `INVALID_SIGNATURE` 400):

| Code | Status | Meaning |
|---|---|---|
| `SETTLEMENT_CLUSTER_MISMATCH` | 409 | A signed request exists for a different cluster than the one running. Refuse to confirm; **do not** dispute — the trade is fine and the operator's configuration is wrong. |
| `MINT_UNGOVERNED` | **409** | The price mint is not governed on the active cluster. This is the rejection path for the stale fake-EURC offers (D5). 409 rather than 400: the offending mint was chosen by the seller's offer and the buyer cannot correct it, so this is a conflict with server state rather than a malformed request. Agent clients branch on status class, so the distinction matters. |
| `MINT_UNVERIFIED` | 503 | The mint is governed but its account no longer matches on identity or scale — absent, wrong owner, or wrong decimals. |
| `ONCHAIN_UNAVAILABLE` | 503 | The chain could not be consulted: RPC error, timeout, rate limit, or genesis mismatch. Reused from Phase 2 so a transient node problem is never reported as a mint or cluster fault. |

## 8. Data model

`migrate()` currently executes one `schema.sql` blob with no version tracking. It gains a `schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)` table and an ordered Go list; `schema.sql` remains the idempotent base schema, and each migration runs in its own transaction and records its version. Re-opening is a no-op.

**Migration 1:** `ALTER TABLE settlement_requests ADD COLUMN cluster TEXT NOT NULL DEFAULT 'pre-phase-3'`.

**Column semantics.** `settlement_requests.cluster` records *the cluster whose mint set was used to compile the request's frozen terms* — the chain the request is valid for. It does not record where the request was observed to land, which was never captured.

**Why the sentinel is `pre-phase-3` and not `mainnet-beta`.** Labeling legacy rows `mainnet-beta` would be both ineffective and dishonest. Ineffective, because on a mainnet-beta deploy the recorded and running clusters would match and the mismatch guard would never fire, leaving exactly the rows most likely to reference the fake EURC fully confirmable. Dishonest, because those terms were compiled with a flat registry that named mainnet's USDC address regardless of the chain the request was actually built against; recording `mainnet-beta` asserts an execution context that was never observed. The sentinel says what is actually known: nothing, because the terms predate cluster-awareness. It is not a declarable `Cluster` value, so it can never be reached except by migration.

Consequence, on **every** cluster including mainnet-beta: a `pre-phase-3` request can never be confirmed, because it can never match the running cluster. The reconciler expires it (D10), the trade stays pending, and an operator issues a fresh request on the correct cluster. A human decides, not a code path, and no legacy request is confirmable by accident.

`cluster` values are validated in Go via `cluster.Parse` on read and write — treating the sentinel as a distinct non-cluster value rather than an error — with no SQL `CHECK` constraint, so adding a cluster in a future release requires no data migration.

**Receipts.** `cluster` is added to the signed claims for receipts issued from Phase 3 onward. The ledger entry also stores it for querying, but the entry is not the security boundary; the signature is. Existing receipts lack the claim and are treated as pre-Phase-3 (§5.5), so no receipt is invalidated and the JWS payload of an old receipt is not rewritten.

**Reconciler.** Each tick, before doing any work, the reconciler re-reads the genesis hash and compares it to the declared cluster (D6). On mismatch it logs an error, reports `ONCHAIN_UNAVAILABLE`, and **halts confirmations** for that tick — it neither disputes nor expires anything, because a cluster that has changed underneath the process says nothing about the trades. Requests whose recorded cluster differs from the running one are **expired**, not skipped, and counted as `ExpiredClusterMismatch`; the trade remains pending and can be re-requested.

## 9. Testing

### 9.1 `internal/cluster`
Parse (valid, invalid, case, whitespace, known-but-unsupported testnet); the three genesis pins; `IsProduction`; loopback detection for localnet via **resolved IP**, including a case where `localhost` resolves to a non-loopback address.

### 9.2 `internal/tokens`
Each cluster's set contains exactly the §3.1 tuples. An explicit test asserts EURC has the **same address but a different authority** across clusters, so a future "deduplication" fails loudly. `Default()` is gone. `ForCluster` rejects extras off localnet. `ErrUngoverned`, `ErrNotRegistered` and `ErrDisabled` are distinct.

### 9.3 `internal/preflight`
Against a fake RPC: genesis mismatch; missing mint (`null` account); non-token-program owner; decimals mismatch; authority mismatch (fatal at startup); missing fee wallet; fee wallet below the queried rent minimum; and the happy path. Each asserts *which* check failed, not merely that an error occurred. A test asserts the rent minimum is taken from the node's answer, with no literal in the source.

### 9.4 Recorded snapshot
The shipped mainnet table is asserted against a `testdata` snapshot file holding each governed mint's address, symbol, decimals, mint authority, freeze authority and owning program, plus the RPC endpoints and date it was taken from. Any future constant edit therefore appears as a reviewable diff rather than a quiet correction. This is the direct regression guard for §2.1.

### 9.5 `internal/config`, `internal/store`, `internal/trade`, `internal/httpapi`
- Config: asymmetric RPC/cluster cases, mainnet ack, fee overrides on mainnet, localnet settings off localnet, the uniform empty-equals-unset rule for **every** setting including `VTESSERA_LOCALNET_MINTS`, and the preserved `501` path.
- Store: migration applies once, re-open is a no-op, sentinel backfill value, cluster round-trip.
- Trade: cluster-mismatch confirm yields `SETTLEMENT_CLUSTER_MISMATCH` and neither disputes nor confirms; a stale fake-EURC offer fails at build with `MINT_UNGOVERNED` at 409.
- On-use verification: TTL cache hit issues no second RPC call; TTL expiry re-fetches; single-flight collapses a concurrent burst to one call; a hard-invariant drift returns `MINT_UNVERIFIED`; an authority-only drift returns success and increments the counter; an RPC error returns `ONCHAIN_UNAVAILABLE` and **not** `MINT_UNVERIFIED`.
- Reconciler: a mismatched `issued` request is expired, the trade stays pending, a replacement request can then be issued (proving the unique index is no longer wedged), and a genesis mismatch halts the tick without disputing or expiring.
- HTTP: the new codes with correct status; `/v1/tokens` returns only the active cluster's mints.
- Receipts: a Phase 3 receipt's `cluster` claim verifies; a receipt lacking the claim is accepted as pre-Phase-3; a receipt whose claim disagrees with the verifying cluster is reported.

### 9.6 Build-tagged `solana` E2E
The five existing scenarios must pass **unchanged** after the refactor — the real compatibility proof. Added:

- Preflight passes against the local validator when the localnet mint is configured, and fails when the list is empty.
- A request created under `localnet` is refused after a restart declaring `devnet`, proving the mismatch guard end to end, and is then expired by the reconciler so a fresh request succeeds.
- The harness mint is pinned at 6 decimals to match real stablecoins, removing decimal divergence between tests and production.

### 9.7 Live-cluster checks
Network calls do not belong in `go test`. The preflight report is exercised once by hand against the documented endpoints in §3.5 — `https://solana.publicnode.com/` for mainnet-beta and `https://api.devnet.solana.com` for devnet — during implementation, and `make preflight-live` runs it on demand behind an explicit env var. Passing this is an acceptance criterion (§11.13), because a verification path that has only ever run against a fake RPC has not been verified.

## 10. Out of scope

Postgres portability; the token-registry governance admin surface and audit trail; A2A `tasks/*`; Token-2022 mints; any change to the on-chain memo format or transaction shape.

## 11. Acceptance criteria

1. The lookalike address `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc` appears in no non-test Go source and no `testdata` file. It remains permitted in this document and in the parent spec, which record it as the documented failure. This criterion is checked with `git grep -n 'HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc' -- '*.go' 'testdata/*' 'internal/**/testdata/*'`, which returns nothing.
2. The governed set is cluster-scoped and cannot be widened by configuration on mainnet-beta or devnet.
3. The service refuses to start against a cluster whose genesis hash contradicts the declared cluster, and re-checks that hash on every reconciler tick.
4. The service refuses to start on mainnet-beta without `VTESSERA_MAINNET_ACK=1`, rejects fee-policy overrides there, and rejects an unparseable `VTESSERA_BLOCKHASH_TTL` on every cluster.
5. A governed mint that is absent, wrongly owned, or wrongly scaled prevents startup. An authority-pin drift also prevents startup, but only warns at settlement time.
6. A settlement request records its cluster; confirming it under a different cluster returns `SETTLEMENT_CLUSTER_MISMATCH` and leaves the trade untouched. The reconciler subsequently expires it, and the trade can be re-requested.
7. An offer priced in a non-governed mint fails at build with `MINT_UNGOVERNED` and status 409.
8. An RPC failure during mint verification reports `ONCHAIN_UNAVAILABLE`, never `MINT_UNVERIFIED`.
9. `/v1/tokens` advertises only the active cluster's mints.
10. Receipts issued from Phase 3 onward carry a signed `cluster` claim; receipts without one are accepted as pre-Phase-3.
11. All five existing `solana`-tagged E2E scenarios pass unchanged.
12. `gofmt`, `go build ./...`, `go vet ./...`, `go test ./...` and `go test -race ./...` are clean.
13. `make preflight-live` succeeds against `https://solana.publicnode.com/`, reporting the mainnet-beta genesis hash and all governed mints verified.

## 12. Runbook

### 12.1 Fee wallet unfunded or drained
Preflight is fatal on mainnet-beta, so this is caught at deploy. In flight the transaction is rejected by the runtime with `InsufficientFundsForRent` (§3.4), which is **atomic**: the destination is never created and the buyer's fee is not charged. The service's symptom is therefore an absence, not an error — settlements stop confirming, and requests age out through the reconciler as the buyer retries. If your provider surfaces the submit error to the buyer, the buyer sees a concrete RPC failure and the operator sees nothing at all. The check is to compare the count of settled trades against settled trade signatures with a fee transfer, and reconcile any gap before it grows. Then fund the wallet to at least the queried rent-exempt minimum — which is 650,240 lamports on mainnet-beta today and 890,880 on a local validator, so query it rather than transcribing either.

### 12.2 Issuer rotates a mint authority
Startup treats this as fatal, which is intended: a deploy must not proceed on an unreviewed governance change. Re-derive the mint's `mintAuthority` and `freezeAuthority` from two independent providers, update the pin in `internal/tokens` and the `testdata` snapshot in the same commit, and note it in the changelog. At runtime the drift only warns, so settlement keeps working while an operator schedules the re-pin. If a *freeze* authority appears where there was none, treat it as an incident: counterparties cannot move funds in a frozen account, and the pin change should be reviewed as a security event.

### 12.3 Genesis changes under a running process
The reconciler detects a mismatch on its next tick, reports `ONCHAIN_UNAVAILABLE`, and halts confirmations for that tick without disputing or expiring anything. If the mismatch persists, the endpoint is not the declared cluster: stop the service, correct `VTESSERA_RPC_URL` or `VTESSERA_CLUSTER`, and restart. Trades are unaffected and resume confirming on the correct chain.

### 12.4 Public provider degrades, throttles, or repoints
On sustained rate limiting the service reports `ONCHAIN_UNAVAILABLE` and the reconciler halts confirmations for that tick without disputing or expiring anything. Move `VTESSERA_RPC_URL` to a dedicated or self-operated endpoint and restart; no trade is affected. If a provider's genesis hash ever changes, preflight fails loudly. That is correct behaviour and **must not** be worked around by editing the pin in §3.3 — a changed genesis is either a provider incident or a DNS hijack, and updating the pin to make the error disappear is the one action that converts a detectable incident into an undetectable compromise. Pin changes follow the mint re-pin procedure in §12.2.

### 12.5 Re-derive every hardcoded constant, and the devnet gap
The EURC lookalike in §2.1 was a transcription error, not an attack (§2.1), which means the *process* that produced it is untrusted rather than the individual value. So: re-derive every base58 constant in the codebase from an issuer's own documentation or a second independent provider, never from the same notes or tool that produced the originals. That set is the USDC and EURC mints, both mint authorities, the fee wallet, the SPL Token program ID, and the three genesis hashes.

**Done:** the mainnet mints, their authorities, the token program ID, and the mainnet and testnet genesis hashes are dual-provider confirmed as of 2026-09-27 (`api.mainnet-beta.solana.com` and `solana-rpc.publicnode.com`).

**Outstanding:** the devnet row of §3.1 and its two authorities rest on a single provider (`api.devnet.solana.com`); a second devnet endpoint could not be reached during design. Confirm before merge.

### 12.6 Carried into the governance spec
The token-program check is hardcoded to the classic SPL Token program (§3.2). Onboarding a Token-2022 stablecoin requires a per-entry program field, which means a schema change to the registry. That work belongs to the deferred token-registry governance spec, together with the admin surface and audit trail.
