# Phase 2 — Settlement Implementation Record

**Date:** 2026-09-27
**Status:** Complete, **not deployable** — see §7.1
**Commit:** `56ac399` — *feat: non-custodial Solana settlement for on-chain trades*
**Parent spec:** [`../specs/2026-09-26-a2a-marketplace-design.md`](../specs/2026-09-26-a2a-marketplace-design.md) §12 (Rollout, item 2)
**Successor:** [`../specs/2026-09-27-phase3-cluster-aware-settlement-design.md`](../specs/2026-09-27-phase3-cluster-aware-settlement-design.md)

## 1. Purpose

This document records **what was actually built for Phase 2**, as distinct from what a design document intended. It exists so the work can be fact-checked without trusting anyone's memory of it, including mine.

**How to fact-check it.** Every claim below carries either a file path, a test name, or a command you can re-run. §6 is the audit checklist. §7 deliberately lists the defects I found, including one that is disqualifying for mainnet use — that section is the point of this document, not an appendix to it.

**Commit pin.** This record is pinned to `56ac399`, the commit that contains every code path, test, and build target it describes. `git show 56ac399` is the ground truth for "what was built"; this document is a claim about it that anyone can now check against a clean checkout. Where the two disagree, the commit wins and this document is the bug.

## 2. Scope

Parent spec §12, item 2: *"settlement build/confirm on devnet with USDC/EURC; test-validator CI."*

Delivered: non-custodial Solana settlement — unsigned transaction issuance, exact canonical verification, persistent settlement-request state, background reconciliation, HTTP surface, and validator-backed integration tests.

Not delivered, and not claimed: mainnet deployment readiness, cluster awareness, token-registry governance, Postgres.

## 3. What was delivered

| Area | Path | What it does |
|---|---|---|
| Frozen terms | `internal/settlement/terms.go` | Derives the canonical accounts and memo from a trade. Memo is `tessera:v1:<tradeUUID>`. |
| Canonical compiler | `internal/settlement/canonical.go` | Builds the expected `solana.Message` and compares. The same compiler is used to build the unsigned transaction *and* to verify the submitted one, so the two cannot drift apart. |
| Unsigned builder | `internal/settlement/build.go` | Emits the base64 transaction returned to the buyer. |
| Verifier | `internal/settlement/verify.go` | Exact comparison of the on-chain transaction against the expected message, with typed mismatch errors. |
| RPC adapter | `internal/settlement/client.go` | Wraps the Solana JSON-RPC client; `ErrTransactionNotFound` distinguishes "not landed" from "failed". |
| Fee policy | `internal/fees/fees.go` | Flat per-settlement fee paid by the buyer inside the same transaction, which is what makes a fee-stripped settlement one the service refuses. See §7.8 for what that does not buy. |
| Governed mints | `internal/tokens/tokens.go` | Service-side mint registry; identity by address, never by symbol. |
| Persistence | `internal/store/settlement.go`, `internal/store/schema.sql` | `settlement_requests` table with a partial unique index enforcing one issued request per trade. |
| Trade integration | `internal/trade/settlement.go`, `internal/trade/trade.go` | Issuance, signature recording, confirmation, dispute, expiry, and cancellation blocking. |
| Reconciliation | `internal/trade/reconcile.go` | Background worker so a signature that was invisible at `/confirm` still reaches a verdict. `ReconcilePolicy{Interval: 15s, Batch: 50}`. |
| Configuration | `internal/config/config.go` | `VTESSERA_RPC_URL`, `VTESSERA_FEE_LAMPORTS`, `VTESSERA_FEE_WALLET`, `VTESSERA_BLOCKHASH_TTL`. |
| HTTP surface | `internal/httpapi/settlement.go` | Build, fetch, confirm, plus public `GET /v1/tokens`. |
| Wiring | `cmd/vtessera/main.go` | Starts the reconciler with `TradeList: db`. |
| Integration tests | `internal/e2e/` | Five validator-backed scenarios behind the `solana` build tag. |
| Tooling | `Makefile`, `scripts/smoke.sh` | `validator`, `test-solana`, and a smoke that optionally uses a live RPC. |

Removed: the `internal/probe` package, replaced by `internal/e2e`.

## 4. Behavioral contract

The invariants Phase 2 was supposed to guarantee, and where each one lives:

1. **The service never holds a key that can move funds.** Agent identity is a Solana public key; the buyer signs and submits. The service *does* hold the ledger's Ed25519 receipt-signing key (`TestVerifyRejectsForeignSigner`), which can only sign tesserae and cannot move, hold or authorize lamports. The distinction matters: "the service holds no key" would be false, and is the kind of overstatement that erodes trust in the rest of the document. `internal/settlement/terms.go`, `internal/trade/settlement.go`, `internal/ledger/`.
2. **Canonical instruction order** is optional seller-ATA creation, SPL `TransferChecked`, trade-UUID memo, exact system fee transfer. `internal/settlement/canonical.go`.
3. **A fee cannot go unpaid and still settle.** The verifier compares the whole compiled message, not the token transfer, so removing or redirecting the fee leg is a mismatch and the trade ends `disputed`. The limit on that claim is §7.8: the chain still executes the stripped transaction, so the trade amount has already reached the seller when the mismatch is caught. `internal/settlement/verify.go`.
4. **A signature that has not landed is `settlement_pending`, never `disputed`.** `ErrTransactionNotFound` is not an error verdict. `internal/settlement/client.go`, `internal/trade/reconcile.go`.
5. **A mismatch is `disputed` and issues no tesserae.** `internal/trade/settlement.go`.
6. **An on-chain execution failure expires the request but leaves the trade pending**, so a fresh request can be built. `internal/trade/settlement.go`.
7. **Only the buyer may confirm, and only the seller may offer on-chain settlement.** Enforced in `internal/trade/` and covered in `internal/httpapi/settlement_test.go`.
8. **An unexpired settlement request blocks cancellation.** `internal/trade/trade.go`.
9. **One issued request per trade**, enforced in the database by a partial unique index rather than in application logic. `internal/store/schema.sql`.
10. **Unconfigured deployments stay off.** No `VTESSERA_RPC_URL` means `501 ONCHAIN_UNAVAILABLE`; the off-chain ledger path is unaffected.
11. **`/confirm` and the reconciler share one verdict path**, so the two cannot disagree about what a transaction means. `internal/trade/reconcile.go`.

## 5. Test inventory

Counts are the number of `func Test*` **declarations**, not subtests or table cases, read from the source on 2026-09-27. They are useful for "did the count change", not as a measure of coverage.

| Package | Tests | | Package | Tests |
|---|---|---|---|---|
| `settlement` | 29 | | `tokens` | 7 |
| `trade` | 38 | | `fees` | 5 |
| `httpapi` | 26 | | `ledger` | 9 |
| `config` | 14 | | `registry` | 11 |
| `store` | 14 | | `agp` | 10 |
| `auth` | 9 | | `money` | 7 |
| **Total (unit)** | **179** | | | |

Validator-backed, behind the `solana` build tag — five scenarios:

- `TestSettlementRoundTripAgainstValidator`
- `TestFeeStrippingEndsDisputed`
- `TestTamperedMemoEndsDisputed`
- `TestSettlementIsBlockedWhileTheTransactionIsLive`
- `TestConfirmOfAnUnseenSignatureIsPendingNotDisputed`

The three scenarios that matter most are the negative ones: fee stripping and memo tampering must both end `disputed`, and a signature that is not yet visible must end `pending` rather than `disputed`. The parent spec calls these out directly, and the second one is the failure mode most likely to destroy trust in a marketplace.

Receipt round-trip is covered in `internal/ledger/ledger_test.go`: `TestRecordIssuesVerifiableTessera`, `TestVerifyRejectsTamperedTessera`, `TestVerifyRejectsForeignSigner`, `TestOnchainTesseraCarriesSignature`, `TestLedgerChainLinksEntries`.

## 6. Audit checklist

Run these to reproduce the verification claims:

```bash
export TMPDIR="${TMPDIR:-$HOME/.cache/go-tmp}"   # /tmp is a constrained tmpfs on this host

gofmt -l .                     # must print nothing
go build ./...
go vet ./...
go test ./...                  # 179 Test* declarations across 12 packages
go test -race ./...

# validator-backed suite; ~8.5 minutes.
# The validator is transient by policy: start it for this run, stop it after.
solana-test-validator --ledger "$HOME/.cache/solana-ledger" --reset --quiet &
make test-solana               # 5 scenarios
# then: make validator-off   (pkill -x cannot work — see AGENTS.md)

# optional live-RPC smoke
SMOKE_RPC_URL=https://api.devnet.solana.com ./scripts/smoke.sh
```

Last full run: unit suite green; `-race` green; all five E2E scenarios green in 504.6s against a local validator; smoke passed both with and without `SMOKE_RPC_URL`.

**The rent experiment of 2026-09-27 is reproducible** and is the evidence for §7.2. It needs a validator, so apply the same transient-validator discipline as `make test-solana` — start, run, stop:

```bash
solana-test-validator --ledger "$HOME/.cache/solana-ledger-renttest" --reset --quiet &
# each of these to a *fresh, unfunded* address; the last two are expected to pass
solana --url http://127.0.0.1:8899 transfer --allow-unfunded-recipient --keypair <k> <dest> 0.00001
solana --url http://127.0.0.1:8899 transfer --allow-unfunded-recipient --keypair <k> <dest> 0.00089079
solana --url http://127.0.0.1:8899 transfer --allow-unfunded-recipient --keypair <k> <dest> 0.00089088
solana --url http://127.0.0.1:8899 transfer --allow-unfunded-recipient --keypair <k> <dest> 0.00100000
curl -s -X POST http://127.0.0.1:8899 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["<dest>"]}'
make validator-off
rm -rf "$HOME/.cache/solana-ledger-renttest"
```

Three traps, all of which cost time here:

- `solana transfer` takes **SOL, not lamports**, and writes the amount unadorned. Passing `1000` moves 1000 SOL, which fails on the sender's balance and looks like a rent problem. Every amount above is in SOL for that reason.
- `--allow-unfunded-recipient` is mandatory. Without it the CLI refuses an underfunded recipient client-side and never reaches the runtime check under test.
- Verify with `getSignatureStatuses`, not the CLI's own output. `solana transfer` prints a `Signature:` line for a transaction it has merely *submitted*. One attempt here reported a signature for a transaction that had not yet reached a slot, and reading the account immediately returned `null` — which reads as a failure and is not one. Wait for `getSlot` to pass the transaction's slot before drawing a conclusion.

## 7. Defects and gaps

This is the section that matters. Ordered by severity.

### 7.1 CRITICAL — the governed EURC mint does not exist

`internal/tokens/tokens.go:100` ships:

```
Address: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc"
```

Queried on 2026-09-27 against two independent providers, this address returns no account at all. The real EURC mint is `HzwqbKZw8HxMN6bF2yFZNrht3c2iXX**zpKcFu7uBEDKtr**`, which is a live SPL mint with 6 decimals and a 105,019,212.86 supply on mainnet-beta. The two share a **30-character** prefix and diverge at position 31 (`c` vs `z`).

Consequences: any trade priced in the governed "EURC" can never settle, and every agent card advertising that currency names a token that does not exist. This is the exact spoofing failure the `tokens` package was written to prevent, shipped inside the package that claims to prevent it. **Phase 2 is not safe to run against devnet or mainnet until this is fixed.** It is the first item in Phase 3 §2.1.

> **Resolved 2026-09-28.** `internal/tokens` now ships
> `HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr`, re-verified against mainnet
> `getAccountInfo` at slot 451247490 (owner `Tokenkeg...`, type `mint`,
> decimals 6, initialized). The five test files that pinned the lookalike were
> updated in the same change. The rest of this section stands as written, as the
> record of what Phase 2 shipped.

> **Resolved in Phase 3 (2026-09-30).** The lookalike is gone from all
> non-test source and is asserted absent by
> `TestTheLookalikeEURCAddressAppearsInNoGovernedSet`. The governed table is
> additionally pinned to `internal/tokens/testdata/governed-mints.json`, a
> snapshot of what `make preflight-live` actually read from the chain, so the
> next transcription error is caught by a test rather than by a deploy. Both
> public clusters verify clean as of 2026-09-30.

**Root cause.** A 30-character matching prefix cannot be the product of vanity-address grinding — that is computationally infeasible at this length. The value is therefore a transcription or generation error, not an attack. That distinction matters for two reasons:

- **Impact is liveness, not safety.** No private key exists for the bogus address, so nobody — including an attacker — can create a token there. The realistic outcome is unsettled trades, not a spoofed asset being accepted. Anyone treating this as a security incident would be over-reading it.
- **The process that produced it is untrusted.** If one base58 constant was mistranscribed, every other constant produced the same way is suspect, whether or not it has been checked yet. That set includes the USDC mint, the fee wallet, the token program ID, and — added in Phase 3 — the genesis hashes and mint authorities. The mainnet subset has since been confirmed against two independent providers (Phase 3 §3.1); the remainder is tracked as an open action in Phase 3 §12.5. **Every such constant must be re-derived from an issuer's own documentation or a second independent provider, never from the same notes or tool that produced the originals.**

### 7.2 HIGH — the fee default is below the rent-exempt minimum

`internal/fees/fees.go:18` sets `DefaultLamports = 500_000` (0.0005 SOL). `getMinimumBalanceForRentExemption(0)` returns **650,240** lamports on mainnet-beta and on devnet, both measured on 2026-09-27 — so 500,000 is below it on every real cluster.

That figure is not a constant, and the evidence is not merely that rent parameters change. A local test validator returns **890,880** for the same query, 37% higher. Both numbers are correct for their own cluster. Any constant is therefore wrong somewhere: derive it from the local validator and it is wrong in production; derive it from mainnet and it is wrong under `make test-solana`. This is the concrete reason Phase 3 §3.4 queries the node instead of hardcoding a figure.

**Mechanism — settled empirically, and two earlier revisions of this document got it wrong.** This paragraph has now been written three times. The first said the fee would be "absorbed as rent and quietly destroyed". The second, after reading `programs/system/src/system_processor.rs`, said the transfer therefore *succeeds* and strands the fee as unusable dust. **Both were wrong**, and the second was wrong in a way that reading only the System Program invites.

The resolution: the System Program genuinely does not rent-check the recipient — `transfer_verified` only validates that the sender has sufficient lamports — and a non-existent account genuinely does load with `rent_epoch = u64::MAX`. Both of those are true and neither settles the question, because the check that matters is not in the instruction at all. After the instructions run, the **runtime** validates that every account left standing by the transaction is rent-exempt, and fails the whole transaction with `TransactionError::InsufficientFundsForRent` if one is not.

Measured on a local test validator (Agave 3.1.14) on 2026-09-27, transferring to an address with no account. Amounts are given in SOL because that is what `solana transfer` takes; the lamport equivalent is in brackets:

| Amount | Lamports | Outcome |
|---|---|---|
| 0.00001 SOL | 10,000 | rejected — RPC `-32002`, `Transaction results in an account (1) with insufficient funds for rent` |
| 0.00089079 SOL | 890,879 | rejected — identical error, one lamport under the minimum |
| 0.00089088 SOL | 890,880 | accepted — finalized, `err: null`, account created at `space: 0, rentEpoch: u64::MAX` |
| 0.00100000 SOL | 1,000,000 | accepted — control, `err: null`, account created at `lamports: 1000000` |

The minimum on this cluster is **0.00089088 SOL**. The last row is *not* the minimum — it is a round control value comfortably above it, useful precisely because it is unambiguous to read. The two rows that bracket the threshold to a single lamport are the ones that carry the argument; the control only shows that the pass/fail split is not a fluke of one value.

The most misleading detail is that the program log reads `Program 11111111111111111111111111111111 invoke [1]` followed by `success` even on the rejected attempts. The instruction succeeded; the transaction did not.

**Impact.** The rejection is atomic: after the failed 1,000-lamport attempt the destination still returned `value: null` and the sender's balance was bit-for-bit unchanged. No transfer, and no transaction fee either. So a 500,000-lamport fee to an unfunded fee wallet means the settlement **cannot complete** — not that it completes and loses the fee, and not that it burns the buyer network fees in a rebuild loop. Nothing is stranded, because nothing lands.

The operational hazard is therefore a stall rather than a loss, and it is worse for being quiet: the rejection surfaces to whoever submits the transaction, which is the buyer, not the operator. From the service's point of view settlements simply stop confirming and requests age out through the reconciler.

**A correction is in the git history, and it is not being rewritten.** The commit message for `12e99eb` states the disproven version — that the transfer succeeds and strands the fee as dust. It was believed correct when written and was wrong. Rather than force-push over it, the correction is recorded here, and the commit that makes this correction says so in its own message. Reading the log without reading this section will mislead you; that is a known cost of amending by adding.

Remediation is unchanged: the fee wallet must be funded to at least the queried rent-exempt minimum before mainnet use (Phase 3 §3.4, and the query must be per-cluster — see §7.2's note below).

### 7.3 HIGH — no cluster awareness

`tokens.Default()` returns one fixed mint pair regardless of the connected chain, and the USDC constant is mainnet's. A devnet deployment advertises and prices in a mainnet token. Nothing identifies which chain the process is talking to, and nothing pins the endpoint's identity.

### 7.4 MEDIUM — receipts carry no cluster

`internal/ledger/ledger.go` `Claims` carries `tradeId`, `offerId`, `buyer`, `seller`, `description`, `amount`, `mint`, `mode`, `state`, `recordedAt`. A receipt does not record which chain settled it, so a devnet receipt is cryptographically indistinguishable from a mainnet one. Same class as §7.1.

**Addressed by Phase 3.** The Phase 3 design (revision 2) adds `cluster` as a **signed claim** for receipts issued from Phase 3 onward, with a receipt lacking the claim treated as pre-Phase-3 rather than invalid — so existing receipts stay verifiable and no signed payload is rewritten. See Phase 3 §5.5, §8 and decision D9. This item is closed by design, not by this record.

### 7.5 MEDIUM — the store has no schema versioning

`internal/store/store.go` applies a single embedded `schema.sql` via one `ExecContext` on every `Open`. There is no migration table and no version tracking, so any future schema change is an ad-hoc `ALTER` at best. This blocks the Postgres work the parent spec §11 promises for production.

**Addressed by Phase 3.** Migration 1 introduces a `schema_migrations` table with an ordered, transactional migration list. Its backfill uses a `pre-phase-3` sentinel rather than labelling legacy rows `mainnet-beta`, precisely because this record establishes that Phase 2 never ran against mainnet and was not safe to — so a `mainnet-beta` label would be false. See Phase 3 §8.

### 7.6 LOW — the reconciler has no cluster-mismatch concept

`ReconcileStats` (Examined, Settled, Disputed, Expired, Pending) has no field for a request built against a different cluster, because no such concept exists. A configuration change between building and confirming a request has no defined behaviour. **Addressed by Phase 3** decision D10.

### 7.7 LOW — an unparseable `VTESSERA_BLOCKHASH_TTL` is silently ignored

An unparseable value falls back to the default rather than failing startup. Defensible when documented, but a silent fallback on a safety-relevant setting contradicts the fail-closed posture Phase 3 adopts everywhere else. **Phase 3 makes this a startup error** for consistency; an operator who mistypes a settlement setting should find out at boot, not discover the fallback later.

### 7.8 MEDIUM — the fee is a deterrent, not a mechanism, and the docs claimed otherwise

`canonical.go` appends the fee as the third instruction debited from the buyer, and `verify.go:67` requires the buyer to be the fee payer and a signer. A buyer who omits the fee submits the two remaining instructions, and the chain accepts and finalizes them: the trade amount moves to the seller, the memo still lands, and the fee never moves. Verification only runs afterwards, so the service returns `409 SETTLEMENT_MISMATCH`, the trade is `disputed`, `GET /v1/tesseras/{id}` is `404`, and the ledger is not appended. The seller's transfer is not unwound and there is no refund path, because there is no custody to refund from.

So enforcement works exactly as specified and is worth having. What was overstated was the claim around it — "cannot be removed without invalidating the settlement", repeated in `internal/fees/fees.go`, `README.md`, and spec §6.1. Read literally, it says removal is impossible; what is true is that removal makes the settlement unrecognisable to this service, which is a weaker and more dangerous guarantee to advertise than it appears. Enforcement deters, it does not prevent, and the party carrying the risk is the buyer.

**Not fixed here, and not fixable without changing the custody model.** Escrow or a split transfer is the only mechanism-level answer, and §1 lists escrow as out of scope. What was done instead is to correct the wording in all three places and state the exposure plainly, so nobody builds a client that assumes a stripped fee costs them nothing. Phase 3 changed the fee default from `500000` to `1000` lamports, which lowers the amount at risk but not who bears it. Nothing about the fee being a deterrent rather than a mechanism changed, and the fee is still read off the node rather than hardcoded.

## 8. Explicitly not done

| Gap | Status |
|---|---|
| Mainnet readiness | No cluster concept, no endpoint pinning, no mint verification against the chain. §7.1–7.3. **Owned by Phase 3 — landed 2026-09-30**, see the Phase 3 design and `README.md`. The cluster is now named in configuration and verified at boot, per request and per reconciler tick; §7.1 (the lookalike mint) and §7.2 (the fee default) are fixed. |
| A2A `tasks/*` lifecycle and conformance | **Unassigned — at risk of drifting indefinitely.** Deferred from Phase 1 to Phase 2, not picked up, deferred again by Phase 3. It has now slipped twice and needs a named owner and target phase before it is deferred a third time. Recommendation: a standalone Phase 4 spec, since it is independent of the Solana work and of the Postgres work. |
| Postgres store | SQLite only, via `modernc.org/sqlite`. Parent spec §11 promises Postgres for production. Unassigned. |
| Token-registry governance | No admin surface, no audit trail. Adding a stablecoin is a code edit. Named in Phase 3 §12.6 as the phase that will carry the per-entry program field; the governance design itself is unwritten. |
| CI | `make test-solana` exists and is repeatable, but nothing runs it automatically. Unassigned; depends on the Phase 3 §12.5 constant re-derivation being done deliberately rather than by CI. |

## 9. Repository state

**Phase 2 is committed as `56ac399`**, on top of `1ca720e Initial commit`, which predates the settlement work and contains only `README.md`. The history is therefore two commits: an empty scaffold, then the entire settlement service, with no intermediate states. `git show --stat 56ac399` covers 56 files — `cmd/`, `internal/`, `go.mod`, `go.sum`, `Makefile`, `scripts/`, `.gitignore`, `README.md`, `logo.png`, `logo.svg`.

The Phase 3 spec and this record were committed after it, separately, so the two phases diff cleanly and §7's fixes can be attributed to the Phase 3 change rather than to the commit that introduced the defects. One correction is folded into `56ac399` itself: the `make validator-off` target. The original stop instruction in this record was `pkill -x`, which cannot work, because Linux truncates process names to 15 characters and `-x` demands an exact match against the full name.

## 10. Addendum

Added after this record was written, and deliberately kept out of §3 so the
Phase 2 delivery table still describes only what Phase 2 built.

**Public `GET /v1/metrics`.** A read-only aggregate over `trades` joined to
`receipts`, served without credentials like `/v1/ledger`. It exists so a public
directory can show which services are actually being consumed, and it is
specified in `docs/specs/2026-09-27-agent-ai-tool-usage-metrics-design.md`.

It adds no new table, write path, dependency, or migration, and it measures
nothing that Phase 2's settlement path does not already record. It is
deliberately not a Phase 3 dependency: it reads only local SQLite, so it is
unaffected by the rent, mint, and cluster defects in §7.1–7.3.
