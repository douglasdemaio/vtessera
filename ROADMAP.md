# Vtessera → AI Compute for the HNT Ecosystem — Roadmap

Vtessera is **technology, not a token**: an opt-in layer that lets machine
owners rent out CPU and GPU capacity to AI workloads. It plugs into the
existing Helium / HNT ecosystem rather than launching a token of its own.

**Who the buyer is.** Primarily **AI agents** — software spinning up
agents that need compute and transact for it autonomously,
machine-to-machine, with no human in the loop. Discovery and payment are
agent-native (no signups, no API keys), and sellers advertise to other
AIs that their machine is available.

**Status today.** `vtesserad` v0 is a read-only meter — it samples
`/proc`, writes signed Ed25519 receipts to a state dir, opens no
sockets, and runs nothing for anyone else. Everything below is net-new
and (per `BUILD.md`) lives in **separate workspace crates** so none of
it expands the v0 daemon's attack surface.

**Where the focus is.** Module 1 (compute execution + accelerator
access) is the immediate priority. The money layer (Module 4) is smaller
and comes after compute works.

> **Fee constants in this document are DRAFT** — both the wallet address
> and the per-transaction amount. Nothing is finalised until the escrow
> program is deployed and the full flow has been exercised end-to-end on
> devnet. Where the prior planning prose names a specific address or
> lamport count, treat it as a placeholder that must be confirmed before
> any mainnet activity.

---

## Execution order at a glance

| # | Module | What it is | Priority |
| - | ------ | ---------- | -------- |
| 1 | **Compute execution + accelerators (CPU first, then GPU)** | Makes the box usable for AI | **Now — the focus** |
| 2 | Coordination / dispatch (MCP offer + x402 endpoint) | How jobs find a box, agree a contract, get scheduled | After 1 |
| 3 | Settlement + work attestation | Signed receipts → trustworthy "fraction of work done" | After 1 |
| 0 | Stablecoin ↔ HNT swap + flat protocol fee | Settlement plumbing | Resolves before 4 |
| 4 | Payment + non-custodial escrow (Anchor program) | Quote EURC/USDC → escrow → pro-rata release in HNT / refund | After 1–3 |
| 5 | Hardening, ops, spool rotation | Safe to run unattended at scale | Before launch |

Dependency chain: **1 → 3 → 4**. There is **no minted token, no reserve,
no custodian, no DAO, and no treasury** anywhere in this design — just a
compute layer, a decision by the machine owner, and one on-chain
payment/escrow program.

---

## 0. Stablecoin → HNT swap and the flat protocol fee

Runs in parallel with Module 1; resolves before Module 4.

- **The swap.** The escrow program converts the seller's earned
  stablecoin slice to HNT atomically on release via a Jupiter CPI,
  guarded by Pyth (HNT/USD plus EUR/USD when the buyer paid in EURC).
  Revert on stale feed or excessive deviation, blocking sandwich/MEV.
- **The flat per-transaction fee.** Charged once per *job/session*
  (never per micro-payment — see Module 4e). Refunds do not add a second
  fee.

> **DRAFT.** The current planning values are a flat **0.0001 SOL** sent
> to wallet address **`9iBQEn9yMbKVhJKEpMpPByS6pjydPmQDGaznMaCvGkzD`**.
> These are not final. Both will be confirmed when the escrow program is
> deployed and the full flow is verified on devnet. Any code that
> references these values must surface them as configuration and tag
> them as draft, never as production constants.

The **forkit** escrow contract at
<https://github.com/douglasdemaio/forkit> is the recommended starting
point — fork it and tailor it for Vtessera's pro-rata release path
(Module 4b).

---

## 1. Compute execution + accelerator access (CPU / GPU)  *(the focus)*

The leap from "watching my CPU" to "an AI job runs on my box." Largest,
most security-sensitive, most valuable piece. Lives in its **own
privileged crate**, separate from the v0 meter, with its own threat
model and audit.

**Workspace home:** `crates/executor`.

### 1a. Pick a VMM that can pass through accelerators

- **Kata Containers on a Cloud Hypervisor backend** — *recommended.*
  Accepts standard **OCI images** (what AI users ship), gives VM-grade
  isolation, and supports **VFIO GPU passthrough**. The combination
  most modern sandbox platforms run in production.
- **Cloud Hypervisor directly** — if you want to manage microVMs without
  the OCI layer.
- **QEMU + VFIO** — heaviest, most complete device support; fallback for
  exotic hardware.

### 1b. CPU

The easy tier: cgroups v2 caps (reuse the v0 config's `resource_caps`),
optional vCPU pinning and NUMA-awareness. Ships first — no passthrough
needed.

### 1c. GPU (the AI money-maker)

- **Whole-GPU passthrough** via VFIO: bind to `vfio-pci`, hand to the
  guest. One tenant per GPU.
- **Sharing one GPU**, strongest to weakest isolation: **MIG**
  (hardware-partitioned instances on A100/H100+) → **vGPU / mediated
  devices** (licensed) → **time-slicing** (only for a single trusted
  tenant).
- **Guest drivers/runtime:** vendor driver + **CUDA** (NVIDIA) or
  **ROCm** (AMD), plus the NVIDIA Container Toolkit for OCI images.
  Ship a small set of pinned driver/CUDA images.
- **Security caveat:** VFIO gives the guest **DMA**, weakening the VM
  boundary vs a CPU-only guest. **Confidential GPU computing** (H100 CC
  + SEV-SNP/TDX) is the mitigation and ties to Module 3 attestation.
  Bake the attestation hooks in early even if CC ships later.

### 1d. Per-device metering (extends v0's receipts)

The economics depend on measuring the **guest's accelerator** use.
Extend `metrics.rs` (or a sidecar) to record, per job, into the signed
receipt: GPU-seconds, **VRAM-GB-hours** (via NVIDIA DCGM/vendor
telemetry), MIG profile, plus CPU/mem. These fields are what Module 3
prices and what escrow releases against. Keep the receipt node-signed as
in v0.

### 1e. Scheduling, admission, network

- **Capability-aware admission:** match on device class, GPU model,
  VRAM, MIG profile, driver/CUDA version.
- **No host network by default:** deny guest egress unless a job
  requests/pays for it (model downloads are the common explicit
  exception).
- **Caps enforced; minimal surface:** hold the executor to v0's
  `systemd-analyze security` bar.

---

## 2. Discovery + agent-facing marketplace

The layer where an AI agent **finds** a machine, learns its terms, and
either pays or uses it free. Because the buyer is software, this must be
machine-native: no human signup, no API keys, no dashboards required.
v0 ships no server, so all of this is new.

**Workspace homes:** `crates/offer` (signed offer types + signing) and
`crates/node-api` (HTTP surface, feature-gated).

### 2a. Advertising a machine to other AIs

Each seller node publishes a **signed, machine-readable offer**
describing what it sells: device class and specs (CPU/GPU model, VRAM,
MIG profile), availability, endpoint, price (in EURC/USDC) **or
`free`**, and — if paid — the seller's wallet. Sign offers with the v0
Ed25519 **node identity** so they can't be spoofed.

Expose these offers through standards agents already speak, rather than
a bespoke API:

- **MCP (Model Context Protocol)** — list the machine as a discoverable
  compute *resource/tool* an agent can enumerate and call. MCP is the
  common agent **tool-discovery** layer.
- Optionally **A2A agent cards** for agent-to-agent ecosystems that use
  them.
- A simple **central index** of current offers to start (easy to
  moderate and rate-limit); decentralize discovery later only if demand
  warrants.

### 2b. Paying (or not) — x402

For **paid** compute, use **x402**, the open HTTP-native standard for
agent payments: the node returns **HTTP 402 Payment Required** with
terms; the AI buyer signs a stablecoin payment and retries; the node
serves on confirmation. x402 is built for machine-to-machine, settles
in stablecoins, runs on Solana, and needs no accounts or keys — a clean
fit for "AI buyer pays in EURC/USDC." x402 also composes with MCP
(discover via MCP, pay via 402).

For **free** compute, the seller's offer is marked `free` and the
endpoint simply **serves the job directly (HTTP 200), never returning
402** — so **no transaction, escrow, swap, or fee ever occurs**. The
free/paid choice is one flag in the seller's config; nothing else
changes.

> x402 typically settles buyer→server in **stablecoin**. That's the
> *buyer* leg only — the protocol still swaps the seller's earned
> stablecoin to **HNT** (Module 4). x402 handles "agent pays
> stablecoin," and Module 4 handles "seller earns HNT."

### 2c. The job contract + lifecycle

A **job contract** records the agreed work and price (or `free`), what
"done" means, and any milestones for partial release. The node API is
the box's first inbound surface, so it gets the locked-down treatment:
explicit Cargo feature, restricted address families, mTLS. Lifecycle:
**discovered → agreed → (paid via x402 / free) → running → finalized →
settled**.

---

## 3. Settlement + work attestation

Turns signed receipts into two trustworthy outputs: **amounts** and,
crucially for escrow, **how much of the contracted work was actually
completed**.

**Workspace home:** `crates/settlement`.

- **TEE options:** **AMD SEV-SNP** / **Intel TDX** confidential VMs let
  a third party verify, via remote attestation, that settlement ran the
  expected code on unmodified inputs — the same chain extends to
  **confidential GPU** (1c) so a renter can verify their job ran on a
  genuine, isolated accelerator.
- Verify each receipt's Ed25519 signature against the node's `node_id`
  (`SHA-256(pubkey)[..16]`), aggregate **per-device usage**, and compute
  the **completion fraction f ∈ [0, 1]** against the job contract
  (Module 2). `f` is what drives escrow release.
- Keep all pricing/oracle logic here, out of the v0 meter.

**Recommendation:** ship a **non-TEE settlement service first**
(signed-receipt verification + a database) to prove the model, then move
into SEV-SNP/TDX before handling real value at scale.

---

## 4. Payment + non-custodial escrow  *(paid jobs only)*

This module applies **only when the seller charges.** If the seller's
offer is `free` (Module 2b), none of this runs — no escrow, no swap, no
burn, no fee; the job just executes.

**Workspace home:** `programs/vtessera-escrow` (one Anchor program;
excluded from the host workspace so the BPF toolchain isn't required
for a plain `cargo build`).

For paid jobs, the buyer's stablecoin enters a **program-owned escrow
PDA** and leaves only by on-chain rules. **No person — not the seller,
not the operator, not you — can withdraw it.** Conversion to HNT
happens at release, only on the portion the seller earns; the rest is
refunded to the buyer.

Two payment shapes, pick per job:

- **Escrow + pro-rata (committed jobs):** deposit the whole price up
  front; release the earned fraction, refund the rest (4a–4b). Best for
  long or large jobs where the buyer wants a firm commitment.
- **Pay-as-you-go via x402 (short/metered jobs):** the agent pays per
  work-unit/milestone as it goes (Module 2b). Pro-rata falls out for
  free — if the job stops at 50%, the agent simply stopped paying at
  50%, so there's nothing to refund and no whole-job escrow. Best for
  typical agent inference calls. Trust is bounded by keeping increments
  small (either side can stop at a boundary, losing at most one
  increment).

### 4a. Payment in

A single `pay_for_compute` instruction, atomically:

1. Buyer deposits the contract price in **EURC (default) or USDC** into
   the **escrow PDA**.
2. **Flat fee:** transfer `fee_lamports` to the protocol fee address via
   `SystemProgram`. The payer already holds SOL for gas, so no new
   asset to source.

Both the fee wallet and the fee amount are **DRAFT** (see §0). They
land in code as configuration values, not hard-coded constants.

That's it at payment time — the principal is now in escrow, held by
program logic alone.

### 4b. Release + refund (pro-rata by work done)

When the job finalizes, settlement (Module 3) supplies the completion
fraction **f**. The program splits the escrowed stablecoin **strictly
by f**:

- **Seller's share = f × price.** Swapped to HNT (Jupiter,
  Pyth-guarded). `burn_bps` is **burned** (the homage to HNT), and the
  remainder is paid to the **seller in HNT**.
- **Buyer's refund = (1 − f) × price**, returned to the buyer **in the
  original stablecoin** (no swap, so the buyer bears no HNT price risk
  on unused funds).

Worked examples on a job priced at 100 EURC:

- **f = 1.0 (complete):** 100 EURC → swapped to HNT, burn slice
  removed, rest to seller. No refund.
- **f = 0.5 (half done):** 50 EURC → HNT (burn + seller); **50 EURC
  refunded to the buyer**.
- **f = 0.0 (nothing delivered):** full 100 EURC refunded to the buyer;
  seller paid nothing.

Only the **earned** stablecoin is ever converted, and conversion
happens **at release**, never held-then-converted by a human. For long
jobs, the contract (Module 2) can define milestones so escrow streams
partial releases as each fraction completes, rather than one final
split.

### 4c. Currencies, swap, and price safety

- **Sellers are paid in HNT** — their machine should earn the ecosystem
  token, and every job is a real on-market **HNT buy + burn**, so
  compute demand becomes recurring demand for HNT.
- **Buyers deal only in stablecoin** (EURC default for ECB-anchored
  price stability; USDC optional). They never need to understand or
  hold HNT.
- **Swap:** the seller's earned slice goes stablecoin→HNT via a
  **Jupiter** CPI. The **DEX is the price**, **Pyth is the guard**
  (HNT/USD, plus EUR/USD for EURC): **revert if the executed price
  deviates beyond tolerance or a feed is stale**, blocking sandwich/MEV.
- **Liquidity:** large/bursty volume moves the HNT pool; consider
  batching/TWAP or an OTC market-maker, and always cap slippage with a
  revert. The **seller bears HNT price/slippage risk** on their share
  (consistent with wanting HNT exposure) — state this in the contract.

### 4d. Design principle — neutral settlement

The protocol settles in **HNT**, and the conversion to HNT is the
neutral balance point of the system. The reasoning:

- Stablecoins at the edges (EURC/USDC) carry their **issuer's freeze**
  capability — Circle can freeze a USDC/EURC address. That risk sits
  with the individual buyer or seller and is **their** responsibility,
  not the protocol's. The protocol takes no view on it.
- The protocol's own reward rail is HNT, a decentralized asset that —
  **with its mint freeze authority revoked** — no single issuer can
  freeze at will. (Verify this on-chain; if HNT's freeze authority is
  not null, this property doesn't hold.)
- The principle is **credible neutrality**: accountability for misuse
  should attach to the **actors** who misuse a service, not to neutral
  settlement code.

**Condition for this to hold:** the protection tracks **immutability**.
Autonomous code no one controls sits on the neutral side; a live
upgrade key or an operator exercising discretion makes that operator
the reachable actor. Make the settlement program **immutable** (or its
upgrade authority a public multisig/timelock) — the same step that
makes "no one holds the funds" true also keeps the code on the
neutral-infrastructure side of the line.

Honesty about limits: censorship resistance is never absolute.
Network-level (validator/relayer) and front-end/RPC vectors remain, and
the stablecoin edges keep their issuers' freeze powers. The claim is
narrow and accurate — **the settlement asset isn't subject to a single
issuer's freeze** — not that the system is unfreezable end to end.

### 4e. The fee (DRAFT)

A **flat per-transaction** protocol fee. See §0 for the draft values.
Properties the fee model should preserve:

- Flat ⇒ scales with transaction **count** (egalitarian across job
  sizes, reads like network gas). Charged once at payment; refunds
  don't add a second fee.
- **Denomination choice:** SOL is simplest but its fiat value floats.
  For a *fixed* sub-cent value, set the fee in stablecoin. To scale
  with job *value* instead of count, use a small `fee_bps`. The trade-
  off lands when the escrow program is finalised.
- Voluntary **donations** can go to the same address on top of the fee;
  keep any UI tip clearly optional so it stays distinct from the
  mandatory fee.
- **Micropayment caveat:** a flat SOL fee is fine per job, but for
  **x402 pay-as-you-go** where each increment may be sub-cent, a flat
  per-payment fee can exceed the payment itself. For that path, charge
  the fee **once per job/session** (not per micro-payment) or switch to
  a small `fee_bps`. Free jobs incur **no fee** (no transaction).

### 4f. How it wires together

```
buyer ──EURC/USDC──▶ escrow PDA  (program-owned; no human can withdraw)
      ──flat fee ───▶ protocol fee wallet (DRAFT)

           job runs ─▶ signed receipts ─▶ settlement (Module 3) ─▶ completion fraction f

on finalize, escrow splits by f:
   f × price       ─▶ swap (Jupiter, Pyth-guarded) ─▶ burn_bps ─▶ SPL burn
                                                  └─ remainder ─▶ SELLER (HNT)
   (1 − f) × price ─▶ refund ─▶ BUYER (original stablecoin)
```

Net: **buyer pays stablecoin into escrow → work is attested → earned
part is swapped to HNT (minus burn) for the seller, unearned part
refunded to the buyer — all by program logic, no custodian.**

**One program** (escrow + swap CPI + fee transfer). Everything else —
HNT mint, Pyth feeds, Jupiter, SPL burn — already exists on-chain. No
governance, no registry, no token mint.

> **Trust caveat:** "no one holds the funds" only holds if the program
> rules can't be quietly changed. For real trustlessness, make the
> program **immutable** (or its upgrade authority a public
> multisig/timelock) before mainnet — otherwise the upgrade key is an
> implicit custodian.

---

## 5. Hardening, ops, spool rotation

- **Spool rotation:** v0 has no deletion logic — receipts grow forever.
  Add archiving/rotation before long-running deployments.
- Re-run `systemd-analyze security` on every new privileged component
  (executor, dispatch API).
- Abuse handling: rate limits, job-admission policy, a coordinator kill
  switch.
- Keep `cargo deny` / `cargo audit` green across all crates.

---

## Build status

CI is green for the v0 daemon under the workspace layout. Every new
module crate that lands here ships with its own CI stanza so the green
status reflects the whole project as modules come online.

### Devnet status

The escrow program is **live on Solana devnet** at
**`6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma`** (program ID;
ProgramData `Gvu3Vb4ZhxnHV33KCZHcgmWWFyVUXjQ7ocW1KjjiRuuh`).

The full pay → run → settle → split flow has been exercised end-to-end
against devnet — see `crates/devnet-demo` for the runnable
demonstration. Sample transactions on devnet:

- `pay_for_compute` — buyer pays 2.000000 micros into escrow PDA, flat
  SOL fee transferred:
  `4bMRoN57g1qYnybFHiuiJbQf9uCvpa5ZfrhmbvJDEoXAyND29x9uXy1LCuwNS6AT3yrbvsr6nPyQcf97RyktjC4h`
- `finalize_pro_rata` — settlement asserts `f = 0.5`; on-chain split:
  seller 1.000000, buyer refund 1.000000:
  `2ygddeAFUYRuuxwXk3MkSQarp9ffH5sCYE5xDXLujYcyurrdUC7Xdkk1yTo2Yx3fuo3zcqmvgbWN7nhUkcQ5xmQn`

### What's stubbed vs production

The devnet program ships with the **STUB** payout path (ROADMAP §0):
the earned slice is paid to the seller in the same stablecoin the buyer
deposited. The production design swaps to HNT via Jupiter (Pyth-guarded)
and burns `DRAFT_BURN_BPS`. The IX signature, the buyer-side semantics,
the pro-rata math, and the refund path are identical between stub and
production; only the seller's leg changes when the swap goes in.

## Mainnet criteria (DEFERRED — do not deploy until met)

Devnet works. Mainnet doesn't follow automatically. Each gating item
below is expanded into concrete numbered steps in
[`MAINNET-CHECKLIST.md`](MAINNET-CHECKLIST.md) — that file is the
authoritative tracker (this section is the summary).

Before any of the DRAFT fee values harden and the program is deployed
to mainnet-beta, **all** of the following must hold:

- [ ] **Jupiter swap + Pyth guard wired and tested.** The current stub
  pays the seller in stablecoin; production pays in HNT. Going to
  mainnet with the stub silently breaks the "seller earns HNT" promise.
- [ ] **Pyth feed addresses pinned** for HNT/USD and EUR/USD. Stale-
  feed and deviation guards exercised in adversarial tests.
- [ ] **Burn slice exercised** end-to-end on devnet with a real HNT
  mint (or its devnet stand-in) so the SPL burn CPI is known to
  succeed against the same account graph.
- [ ] **Settlement authority is not a single keypair.** The
  `settlement_authority` signer in `finalize_pro_rata` must be a
  Squads / Realms multisig or a timelocked PDA before mainnet.
- [ ] **Upgrade authority moved to a public multisig/timelock.** A
  single dev keypair as upgrade authority is an implicit custodian
  (ROADMAP §4d). Either set the upgrade authority to a multisig or
  make the program immutable (`solana program set-upgrade-authority
  --final`).
- [ ] **Fee constants confirmed.** `DRAFT_FEE_LAMPORTS`,
  `DRAFT_FEE_WALLET_TODO`, `DRAFT_MAX_SLIPPAGE_BPS`, `DRAFT_BURN_BPS`
  replaced with finalised values that have been reviewed publicly.
- [ ] **Third-party audit** of the escrow program. The program is
  small (~300 LoC) but touches custody and an external swap CPI —
  reviewable in an afternoon, but ship the review.
- [ ] **Reproducible BPF build** with documented `cargo build-sbf`
  inputs and `sha256` of the .so committed.

Until every box is ticked, the devnet program is the only deployment.
No mainnet test of any size — even "just 2 USDC to see it work" — runs
before then. The cost of being wrong on mainnet (lost user funds,
broken neutrality claim, recoverable only by upgrade-key custodial
intervention) is asymmetric versus the benefit of an earlier demo.

---

## Suggested milestones

1. **M1 — CPU compute proof:** Kata + Cloud Hypervisor running OCI
   workloads, CPU-only, with per-job metering into signed receipts. No
   money.
2. **M2 — GPU tier:** VFIO passthrough (whole-GPU, then MIG), CUDA/ROCm
   images, GPU-second + VRAM metering. The AI demand.
3. **M3 — Agent discovery + free compute:** signed machine-readable
   offers exposed via MCP; central offer index; **free path working
   end-to-end** (agent finds a node, runs a job, no payment). Plus
   settlement computing the completion fraction `f`. No on-chain money
   yet.
4. **M4 — Paid go-live:** Module 0 cleared; x402 payment + escrow
   program live — agent pays EURC/USDC (escrow for committed jobs,
   pay-as-you-go for short ones) + fee, pro-rata release (earned → HNT
   via Jupiter, minus burn) and refund (unearned → buyer in stablecoin),
   program immutable or multisig-upgrade. Real, non-custodial,
   agent-native marketplace.
