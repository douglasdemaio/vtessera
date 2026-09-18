# Session Lessons — Paid-Flow Deep-Dive (2026-09-13)

Follow-up items from the x402/paid-flow, multi-laptop, Solana-devnet session.
These are the rough spots to smooth out over the next week. Each item has:
what happened, why it burned time, and the concrete fix/work to explore.

---

## 1. Shared devnet config PDA blocks `finalize_pro_rata` for every buyer

**Symptom:** `x402-client` pay → job → receipt (steps 1–4) all succeed, then
step 5 `finalize_pro_rata (f = 1.0)` fails with
`AnchorError NotSettlementAuthority` on the `config` account.

**Root cause:** the config PDA is a **singleton**, derived as
`find_program_address([b"vtessera_config_v2"], program_id)` (see
`programs/vtessera-escrow/src/lib.rs`, `CONFIG_SEED`). Whoever calls
`init_config` *first* on the shared devnet program owns settlement authority
and can rotate it (`update_config`) — but only by signing with the old
authority. A fresh buyer (this session's `34Wxj…`) can pay into the escrow but
can never finalize, because someone else's wallet initialized the config first.

**Burn:** No way for the paid loop to complete on devnet. Re-runs, fee-wallet
churn, and wallet swapping all hit the same wall.

**RESOLVED this session (2026-09-13):** the user had already deployed a fresh
escrow (then `D4iX…`, now superseded by the `8UJy6…` rotation) whose `Config`
account (`3CHz4ruzxTJaK1Vkt4rRgxjabdqGRzFxe4gRE7MvmDQx`) is initialized with
**settlement authority = `34Wxj37y8yCynoxsqkvZ5o2Wj3xH36XFkQ1AVUpawCZB`** (the
buyer), fee wallet = `J59EPy…`, fee = 100,000 lamports. Running the paid flow
against *that* deployment with `x402-client --program D4iX…` completes
`finalize_pro_rata` end to end. Verified: `pay_for_compute` → job accepted →
`finalize (f=1.0)` drained escrow to the seller ATA, 0 micros left.

> **Lineage:** `D4iX…` (and its config PDA `3CHz4…`) was retired on
> 2026-09-17 by the audited-build rotation to `8UJy6…` (fresh keypair: the
> in-place upgrade couldn't grow `ProgramData`). The new program's config PDA
> is `init_config`'d fresh at deploy.

**Fix to explore (still valid for the general design):**
- Make `finalize` not require the *global singleton config* for settlement
  authority; e.g. derive per-contract or per-buyer authority instead.
- Or have the client read the config's current `settlement_authority` on-chain
  and either (a) `update_config` if the caller has the old key, or (b) print a
  clear "config owned by <key> — no path to finalize" diagnostic instead of a
  cryptic Anchor error.
- Long term: key the config PDA per chain/per program deployment (a
  deployment-owned registry) so a test/demo deployment can self-manage it.
- Alternative for demos: deploy a **fresh program instance** per demo run
  (fresh program ID ⇒ fresh config PDA ⇒ buyer is first `init_config`, owns
  authority). Trade-off: cost/SOL gas, and every node/clip must point at the
  new program ID.
- **Done (this session):** `x402-client` gained a `--program <addr>` flag that
  overrides the hardcoded `PROGRAM_ID_STR`, so it can target a fresh
  deployment. `PROGRAM_ID_STR` stays as the default.

## 2. On-chain state lookups contradicted the transaction itself

**Symptom:** the pay transaction at slot ~497853116 *succeeded* and the
program read `config.fee_wallet` (so the config PDA existed at that slot), but
immediately after, every `getAccountInfo` / `solana account` against
`45JFFH3PQKxRAZqu432h3gdkF48ZSLfZAxtSpjKzdx9V` returned `NOT FOUND`, and
`getSignaturesForAddress` returned zero history for it across three RPC
endpoints (`api.devnet.solana.com` via raw RPC + CLI, `ankr`).

**RESOLVED (this session):** the config PDA query was chasing the *old* shared
deployment (`6jK6…`). The fresh deployment is `D4iX…` with a *different*
config PDA (`3CHz4…`), which is initialized and readable. `45JFFH3…` belongs
to the `6jK6` deployment you no longer need to use. This session also
confirmed the anchor `Config` account discriminator (`9b0caae0 1efacc82` =
`sha256("account:Config")[:8]`) lets you vet a config PDA's layout from raw
RPC bytes without the SDK.

**Fixes to explore (still valid):**
- Cross-check RPC nodes; treat the explorer
  (`https://explorer.solana.com/?cluster=devnet`) + the confirmed transaction
  as ground truth, not a single `getAccountInfo` call.
- Detect and surface RPC inconsistency in tooling (compare two independent
  nodes; warn on mismatch).
- Add a `--explorer <sig>` helper or "paste tx to explorer" instruction in
  `x402-client` output so operators validate against canonical state.
- Consider pinning the demo to a specific devnet RPC endpoint rather than the
  default LB.

## 3. Payer/payout wallet sourcing is ad hoc

**Symptom:** to run the intended flow this laptop must *pay from*
`34Wxj37y8yCynoxsqkvZ5o2Wj3xH36XFkQ1AVUpawCZB` (the other laptop is the seller,
payout `5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs`). The `34Wxj…` keypair
existed **nowhere** on this machine — no `id.json`, no base58 any where, no
config reference. We only obtained it when you pasted the base58 privkey, and
it had to be fed to `solana-keygen pubkey` in JSON-array format.

**Burn:** Wallet identity vs wallet *key* confusion; hunting for keys across
`~/.config/solana`, Flatpak config, node `identity.key` files, env vars.

**Fixes to explore:**
- A documented key story: devnet test payment wallet stored at a fixed path
  (now: `~/.config/solana/payer.json`), funded automatically, referenced via
  the existing `VTESSERA_PAYER` env.
- Add a `--payer <path>` explicit CLI flag to the demo (today it's env-only).
- Have `gen_offer`/`vtessera-node` emit *where the payout key lives* next to
  the payout_id it writes into offers, so offers aren't signed with payout ids
  that don't exist locally.

## 4. The flatpak/GUI-node payout config vs. the agent's payer is two wallets

**Symptom:** settings.toml / vtessera.toml of the Flatpak GUI node set
`payout_id = "34Wxj…"` and `mode = "free"` with currency usdc. Meanwhile the
*buyer* agent is expected to pay *from* `34Wxj…` — i.e. the same address is
being used as "the node's earnings address" *and* "the buyer's spending
address." On a shared devnet that's harmless, but conceptually muddled and a
double-use of one keypair.

**Burn:** Confusion about which side a wallet belongs to; risk of using a
node's payout key as a hot spender.

**Fix to explore:** define a wallet matrix per machine
(buyer/payer, seller/payout, node identity), document it in the devnet-demo
skill, and make the demo scripts assert the correct role for each key.

## 5. Demo offer payouts must be verified at runtime

**Symptom:** we launched a paid node on :8502 with a freshly signed offer whose
`payout_id` was `5fML…` — but the node's live `/offer` endpoint served a
*stale* offer with the default `payout_id 9WzDX…` because a leftover node from
a previous session was already bound to :8502 (only visible once we
`pkill`ed and restarted clean).

**Burn:** We nearly ran a paid flow against a node that would pay out to the
wrong wallet; caught it only by diffing the JSON.

**Fixes to explore:**
- Demo scripts: verify the live `/offer` price *and* payout match the signed
  file before paying; fail loudly otherwise.
- Node binary: refuse to start if the bind port is already in use (or make the
  demo choose a free port and log it).
- `vtessera-node` should log offer payout_id at startup so drift is visible in
  the flatpak/systemd logs.

## 6. `x402-client` is a working demo but a poor diagnostics tool

**Symptom:** steps were hand-parsed, hardcoded constants
(`PROGRAM_ID_STR`, `DEVNET_RPC`, `FEE_WALLET_STR`), single-shot run-to-success,
and the finalize error surfaced as a raw RpcResponseError.

**Burn:** Every failure required reading raw JSON; no `--verbose`, no
pre-flight checks (is the config PDA initialized? by whom? is the payer funded?
does the seller have an ATA?).

**Fixes to explore:**
- Add `--verbose`/`--check` modes that dump: config PDA state + current
  settlement authority, payer SOL/token balances, seller ATA state, offer
  payout_id — all before paying.
- Map Anchor errors (`NotSettlementAuthority`, `WrongFeeWallet`, etc.) to
  human-readable remediation hints.
- Make `--mint <addr>` the default for the paid demo (pay real Circle devnet
  USDC) rather than a throwaway test mint, since a junk mint's "payment" is
  invisible to the human eye on the seller side.

## 7. Seller visibility: a paid job must show up on the *other* laptop

**Symptom:** you watched devnet for the payment (good) but saw no job in the
other laptop's job log. The paid run(s) executed against a local throwaway node
on :8502, not the real node on `192.168.178.82:8402` (which currently offers
**free** mode).

**Burn:** "paid" was tested in isolation, not against the actual seller node —
so the end-to-end story (pay → seller node executes → receipts → settle) has
not actually been exercised across laptops yet.

**PROGRESS (this session):** the full paid loop now completes *locally end to
end* against a node running the `D4iX…` escrow deployment with payout
`5fML…` (the other laptop's wallet) — accepted, executed, paid out 100 micros
to the seller ATA. The remaining gap is only the **network hop**: driving the
same client against the node at `192.168.178.82:8402` after flipping that node
to paid mode (offer payout `5fML…`).

**DONE — network hop exercised (2026-09-16):** after the other laptop pulled
upstream and restarted its node in paid mode, this laptop ran the real paid
flow against `http://192.168.178.82:8402`:
- `GET /offer` → **paid** mode, program `D4iX…`, payout `5fML…`, price
  3903 micros/device-second → agreed 60 device-seconds = 234 kmicros.
- `POST /jobs` → x402 challenge (escrow `D4iX…` matches default program).
- On-chain payment via client-minted test stablecoin, escrow filled, proof
  back to `POST /jobs` → `200 accepted`, job executed on the far laptop
  (`local-cpu`, `cpu_seconds=1`, `exit_status=completed`).
- `finalize_pro_rata (f=1.0)` → escrow drained to seller ATA; seller balance
  change observed; `agent SOL` returned to ~4.97 (only micros moved).
- **Far-side proof:** `GET http://192.168.178.82:8402/jobs/<id>/status` →
  `status=completed` with full signed metering (`cea74ba7aa6b8…`). The paid
  job lands in the *other* laptop's job log. Item 7 fully closed.

**Fixes to explore:**
- Flip the remote node to paid mode (offer payout `5fML…`) and drive
  `x402-client` against `http://192.168.178.82:8402` with
  `VTESSERA_PAYER=~/.config/solana/payer.json`, `--seller 5fML…`, and
  `--program D4iX…` (the deployment whose config settles to the buyer).
- Cross-check the seller node's job log / state dir shows the paid job.
- Add a "paid end-to-end across machines" entry to the devnet-demo skill with
  the exact env vars + commands.

## 8. Tooling nits worth fixing while we're in here

- The user-supplied tx signature this session came out too short (86 chars;
  Solana sigs are base58 of 64 bytes, typically 87–88) — tools rejected it
  with misleading `WrongSize`. Document/validate signature length where txs are
  shared.
- Python PDA re-derivation didn't match Anchor's address (DSA/off-curve check
  is subtle) — use the SDK's `findProgramAddress`/`find_program_address`, not a
  hand-rolled script, whenever a PDA must be validated.
- `token-address` subcommand doesn't exist in this `spl-token`/Solana CLI
  version — use `getTokenAccountsByOwner` (jsonParsed) instead.
- Free-mode detection relies on the client parsing `price.mode`/micros; keep
  that path tested — it's the "nothing to pay" shortcut.

---

## Quick wins (do first)
1. Flip remote node (`192.168.178.82:8402`) to paid; verify live offer payout
   = `5fML…` and it serves the `D4iX…` escrow.
2. Run `x402-client` against it with `--program D4iX…`,
   `VTESSERA_PAYER=~/…/payer.json`; confirm the paid job lands in the
   *seller's* log.
3. Add `--verbose`/pre-flight checks + Anchor-error remap to `x402-client`.
4. Write a wallet-matrix doc + a "paid across laptops" devnet-demo section.

## Deeper fixes
5. Rework escrow finalize authority so a buyer can settle without owning the
   singleton config PDA (see item 1) — even though `--program D4iX…` 
   now unblocks demos, the shared-program singleton is still a footgun.
6. The `45JFFH3…` RPC-vs-transaction puzzle is explained by targeting the *old*
   `6jK6…` deployment; not a genuine RPC inconsistency (item 2). Archive, but
   still add the `--explorer` nicety.
7. `vtessera-node` should detect port collisions at startup (item 5).
## Next up (post-#122, all-merged state)

Everything on the 7-item demo + doc list is now merged and green
(#120 GUI resize fix, #121 paid-flow deep-dive docs, #122 item-7 network-hop
verification), plus the full check matrix passes from a fresh
`cargo update` (fmt, clippy -D warnings, test --locked, audit, deny).

Two genuinely-forward items remain, both lower-priority than the queue work:

1. **Seller payout-id mismatch on a PAID hop is the one footgun not yet
   surfaced by the client.** The offer carries
   `payout_id = 5fMLGtXrcTXyxXt7RGz7qLgnbxH2nnvkTcXmBRxAARfs` but a fresh
   `--seller` runs with a keypair the seller doesn't own, so the escrow
   finalizes to *that* ATA and the seller never sees the funds. Every paid
   demo used `--seller` from the offer; the CLI should warn loudly (or
   require `--seller`) whenever `offer.payout_id != ATA(owner(seller))`.
   Design option: `--check` already surfaces it; promote to a hard error.

2. **GUI resize on Flatpak was fixed by wrapping pages in scrollers
   (#120), but the notebook tab bar itself is still a fixed-layout strip.**
   Shrinking below the tab-bar minimum still clips the labels. A follow-up
   could let tabs wrap/ellipsize, but that's cosmetic and lower value than
   the escrow-authority work.

Neither blocks the marketplace; both are safe follow-ups for a future
session. Standing next task per the queue is the escrow
settlement-authority rotation work (programs/vtessera-escrow), which wraps
the paid demo into a real multi-buyer loop.

---

## 8. `x402-client` dials paid nodes over iroh (no router config) — 2026-09-18

**Scenario:** agent CLI from a remote network (train, NAT/CPE) needed to run
the full `x402` paid flow against a home-laptop node that is marketplace-listed
but reachable only over iroh. `vtessera-agent --node <lan-url>` cannot work
remotely; the pre-existing `agent --payment` path also can't build the full
proof (only `{tx, amount_micros}` → node rejects 402 with `missing job_id`),
and the AGENTS.md `spl-token transfer → escrow ATA` guide is **stale**: the
node verifies the deposit landed in the *per-job contract PDA's* ATA
(`crates/node-api/src/bin/vtessera_node.rs`, `ProofPayload` ~line 756,
`escrow_ata` ~line 1019), so plain transfers to the escrow account never
verify.

**Done this session:** `crates/x402-client` (standalone crate, excluded from
host workspace) gained an iroh dial mode that mirrors agent-cli's resolver + 
QUIC round-trip:

- New flags: `--node-id <endpoint_id>` (dial the endpoint over iroh),
  `--index <offer-index>` (default `http://127.0.0.1:8403`),
  `--marketplace <nodes.json url>` (candidate source when the index is stale
  or unreachable). `--node http://...` remains for LAN.
- `Dial::Tcp | Dial::Iroh` enum with `describe()`; `http_request` dispatches
  on it. Iroh path resolves candidates via index→marketplace fallback
  (`resolve_node_candidates`, mirroring `agent-cli::resolve_node_candidates`),
  builds a tokio current-thread runtime, `Endpoint::bind` (presets::N0),
  `connect(EndpointAddr, VTESSERA_ALPN)`, `open_bi`, writes HTTP/1.1
  (method/path + extra headers + content-length), reads body, parses status +
  headers + content-length into `HttpResponse`.
- Deps added: `iroh = "1"`, `tokio` (rt/rt-multi-thread/macros/time),
  `ureq` (rustls+json for index/marketplace fetch), `serde_json = "1"`,
  `vtessera-transport = { path = "../transport", features = ["serve"] }`.
  Co-exists with `solana 3.x` on ed25519-dalek 2.2 / curve25519-dalek 4.1.3.
- Kept `--check` preflight; it now shows `dial: iroh:<id>` and validates offer
  payout↔`--seller` matching, challenge escrow↔program, config PDA, payer
  funds, seller ATA.

**Verified live over devnet (2026-09-18):** preflight green and full paid
round-trip from a train network: GET /offer → 402 challenge → on-chain
`pay_for_compute` (234,180 micros USDC) → resubmit with full proof → 200
accepted/executed → `finalize_pro_rata` → escrow ATA drained to 0, seller ATA
credited. Two Rust-heavy concerns were safety-checked first: iroh 1.1.0 +
solana 3.x share ed25519-dalek 2.2, and the `vtessera-transport` serve feature
added no conflicting quinn. Run it with:

```bash
cargo run --manifest-path crates/x402-client/Cargo.toml -- \
  --node-id <endpoint_id> \
  --marketplace https://douglasdemaio.github.io/vtessera/nodes.json \
  --mint 4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU \
  --seller <offer.payout_id> \
  --seconds 60
```

**Follow-up:** update AGENTS.md paid-job section to say the modern CLI is
`vtessera-x402-client --node-id ...` (full proof, iroh), and mark the raw
`spl-token transfer` flow as legacy (works only if the node predates the
per-job contract-PDA verifier).
