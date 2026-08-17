# Vtessera → AI Compute for the Solana Stablecoin Ecosystem — Roadmap

Vtessera is **technology, not a token**: an opt-in layer that lets machine
owners rent out CPU and GPU capacity to AI workloads. It settles on Solana
in stablecoins rather than launching a token of its own.

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

> **Fee.** The canonical fee is a flat **0.0001 SOL** (100,000 lamports)
> per agent↔node transaction to wallet
> **`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`**, stored in `Config`
> at `init_config` and immutable after deploy. It is charged on
> `pay_for_compute`, `finalize_pro_rata`, and `cancel_before_start` — even
> when a contract never completes.

---

## Execution order at a glance

| # | Module | What it is | Priority |
| - | ------ | ---------- | -------- |
| 1 | **Compute execution + accelerators (CPU first, then GPU)** | Makes the box usable for AI | **Now — the focus** |
| 2 | Coordination / dispatch (MCP offer + x402 endpoint) | How jobs find a box, agree a contract, get scheduled | After 1 |
| 3 | Settlement + work attestation | Signed receipts → trustworthy "fraction of work done" | After 1 |
| 0 | Flat protocol fee (per-transaction SOL) | Settlement plumbing | Resolves before 4 |
| 4 | Payment + non-custodial escrow (Anchor program) | Quote EURC/USDC → escrow → pro-rata release in EURC/USDC / refund | After 1–3 |
| 5 | Hardening, ops, spool rotation | Safe to run unattended at scale | Before launch |

Dependency chain: **1 → 3 → 4**. There is **no minted token, no reserve,
no custodian, no DAO, and no treasury** anywhere in this design — just a
compute layer, a decision by the machine owner, and one on-chain
payment/escrow program.

---

## 0. The flat protocol fee

Runs in parallel with Module 1; resolves before Module 4.

- **The flat per-transaction fee.** Charged once per *job/session*
  (never per micro-payment — see Module 4e), and on every agent↔node
  transaction even when a contract never completes — it funds protocol
  infrastructure. It is charged on `pay_for_compute` (buyer),
  `finalize_pro_rata` (settlement authority), and `cancel_before_start`
  (buyer), and is skipped when `fee_lamports == 0`. Refunds do not add a
  second fee.
- **No swap, no oracle.** The escrow pays the seller directly in the
  same stablecoin the buyer deposited; the fee is the only thing this
  module adds on top of the escrow program.

The canonical fee is a flat **0.0001 SOL** (100,000 lamports) sent to
wallet address **`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`**,
stored in `Config` at `init_config` and immutable after deploy.

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

**Shipped: Cloud Hypervisor CPU backend.** A production CPU execution path
that boots each job in a disposable Cloud Hypervisor microVM (host kernel +
custom initramfs, no guest network, virtio-fs job share). Feature-gated
behind `cloud-hypervisor` in the executor crate; `vtessera-node --backend
cloud-hypervisor` wires it. `scripts/build-initramfs.sh` builds the
guest. GPU, networking, CPU pinning, and the Kata/OCI path remain
follow-ups below.

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

**Status (current):** all three are implemented. Offers sign/verify
(`crates/offer`); the node serves a real MCP `2024-11-05` endpoint
(`POST /mcp`, plus a stdio `vtessera-mcp` binary) and an A2A card at
`/.well-known/agent.json` (`crates/node-api`); `crates/offer-index`
verifies and serves current offers (push register + optional pull
seeding) at `GET /offers`. Executor wiring is in: the `serve`-gated
`vtessera-node`/`vtessera-mcp` binaries take `--backend` (default
`noop-cpu`; `local-cpu` for unisolated host execution) and run free
offers' jobs through the executor in `crates/executor` (§1). What's
still open is the **paid** path: a submitted payment proof is not yet
verified on-chain, so paid jobs return an honest 501 until the verifier
lands (§3/§4).

**Offer-index live-demo wiring (shipped):** nodes can register their
signed offer with an index via `--publish <index-url>`
(`--publish-interval`, default 60s) and agents claim nodes
first-come-first-served (`POST /offers/<node_id>/claim`, 60s lease TTL,
renew by owner, release by owner, `GET /offers?available=1` filters out
claimed nodes). Claims are index-authoritative and **node-enforced**:
once a node publishes, it requires an agent identity on every job — HTTP
`X-Agent-Id` header or MCP `submit_job`'s `agent_id` — refuses a node
claimed by someone else (409), and fails closed (503) if its index is
unreachable. The MCP `discover` tool lists current offers with claim
state. `scripts/offer-index-demo.sh` exercises the whole flow end to end.

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

> x402 typically settles buyer→server in **stablecoin**. That's the whole
> money path: the seller is paid in the **same stablecoin the buyer
> paid** (Module 4). x402 handles "agent pays stablecoin," and Module 4
> handles "seller earns that stablecoin."

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

**Shipped (non-TEE first):** per-job metering receipts. `vtessera-node`
signs a `JobReceipt` (`schema_ver 2`, wrapping the executor's
`JobMetering`) after every job run — Completed, Failed, and TimedOut
alike — and writes it to `<state-dir>/job-receipts/<job_id>.json`. The
`vtessera-settle` service watches a shared state dir (contracts/
+ job-receipts/ → settlements/), verifies each signed receipt
(schema, pubkey, self-attesting `node_id`, signature — any failure is a
permanent reject, no partial credit), guards against device downgrades
(a GPU contract is credited in GPU-seconds, never CPU-seconds), and
writes `settlements/<job_id>.json` with the completion fraction `f`.
`crates/settlement` holds the schema, sign/verify, key loading
(`--key` / `--state-dir` on the node), the sweep logic, and the
`vtessera-settle` binary. *The escrow split itself (§4b) is still the
escrow program's job; settlement produces the `f` it needs.* The TEE
verification layer remains follow-up.

---

## 4. Payment + non-custodial escrow  *(paid jobs only)*

This module applies **only when the seller charges.** If the seller's
offer is `free` (Module 2b), none of this runs — no escrow, no fee; the
job just executes.

**Workspace home:** `programs/vtessera-escrow` (one Anchor program;
excluded from the host workspace so the BPF toolchain isn't required
for a plain `cargo build`).

For paid jobs, the buyer's stablecoin enters a **program-owned escrow
PDA** and leaves only by on-chain rules. **No person — not the seller,
not the operator, not you — can withdraw it.** The seller's earned slice
is paid directly in the same stablecoin mint the buyer deposited; the
rest is refunded to the buyer.

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
2. **Flat fee:** transfer `config.fee_lamports` to `config.fee_wallet`
   via `SystemProgram`. The payer already holds SOL for gas, so no new
   asset to source.

The fee wallet and amount are stored in `Config` at `init_config` (see
§0) — immutable on-chain configuration, not hard-coded constants.

That's it at payment time — the principal is now in escrow, held by
program logic alone.

### 4b. Release + refund (pro-rata by work done)

When the job finalizes, settlement (Module 3) supplies the completion
fraction **f**. The program splits the escrowed stablecoin **strictly
by f**:

- **Seller's share = f × price**, paid directly to the seller's ATA in
  the contract's stablecoin mint — no swap, no oracle, no conversion.
- **Buyer's refund = (1 − f) × price**, returned to the buyer **in the
  original stablecoin** (the same mint, so the buyer bears no price risk
  on unused funds).

Worked examples on a job priced at 100 EURC:

- **f = 1.0 (complete):** 100 EURC → seller, in EURC. No refund.
- **f = 0.5 (half done):** 50 EURC → seller; **50 EURC refunded to the
  buyer**.
- **f = 0.0 (nothing delivered):** full 100 EURC refunded to the buyer;
  seller paid nothing.

The escrow never converts assets: the seller earns exactly what the
buyer paid, in the same mint, and the release happens **at finalize**,
never held-then-converted by a human. For long jobs, the contract
(Module 2) can define milestones so escrow streams partial releases as
each fraction completes, rather than one final split.

### 4c. Currencies and price safety

- **Sellers are paid in the same stablecoin the buyer paid** — EURC
  (default for ECB-anchored price stability) or USDC, whichever the
  node's signed offer puts on it. No Vtessera token, no swap.
- **Buyers deal only in stablecoin.** They never need to understand or
  hold anything else.
- **No oracle, no slippage.** Because the escrow pays out in the same
  mint it holds, there is no conversion step and no price risk for
  either side — nothing to guard against sandwich/MEV.
- **Liquidity:** none needed. The seller's payout is the buyer's
  deposit, mint-for-mint.

### 4d. Design principle — neutral settlement

The protocol settles in the **same stablecoin the buyer paid**
(EURC/USDC). The reasoning:

- **No mintable protocol token.** There is no Vtessera token to price,
  buy, or burn, and no external-market dependency — the payout model
  isn't hostage to any third-party asset.
- Stablecoins at the edges (EURC/USDC) carry their **issuer's freeze**
  capability — Circle can freeze a USDC/EURC address. That risk sits
  with the individual buyer or seller and is **their** responsibility,
  not the protocol's. The protocol takes no view on it.
- The protocol is **single-operator and neutral**: it adds an escrow
  program and a flat SOL fee, nothing else. Accountability for misuse
  should attach to the **actors** who misuse a service, not to neutral
  settlement code.

**Condition for this to hold:** the protection tracks **immutability**.
Autonomous code no one controls sits on the neutral side; a live
upgrade key or an operator exercising discretion makes that operator
the reachable actor. Make the settlement program **immutable** before
mainnet, and keep the settlement authority — the operator's key, pinned
in `Config` at deploy — the only signer that can trigger
`finalize_pro_rata`.

Honesty about limits: censorship resistance is never absolute.
Network-level (validator/relayer) and front-end/RPC vectors remain, and
the stablecoin edges keep their issuers' freeze powers. The claim is
narrow and accurate — **the protocol holds no funds and never converts
assets** — not that the system is unfreezable end to end.

### 4e. The fee

A **flat per-transaction** protocol fee, set once in `Config` at
`init_config` (see §0): **100,000 lamports (0.0001 SOL)** to wallet
**`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`**. Properties of the
fee model:

- Flat ⇒ scales with transaction **count** (egalitarian across job
  sizes, reads like network gas).
- Charged on `pay_for_compute` (buyer), `finalize_pro_rata`
  (settlement authority), and `cancel_before_start` (buyer) — every
  agent↔node transaction funds protocol infrastructure, even when a
  contract never completes. Refunds don't add a second fee.
- `fee_lamports == 0` disables the fee (no-op transfer skipped).
- **Micropayment caveat:** a flat SOL fee is fine per job, but for
  **x402 pay-as-you-go** where each increment may be sub-cent, a flat
  per-payment fee can exceed the payment itself. For that path, keep
  the fee per job/session (not per micro-payment). Free jobs incur
  **no fee** (no transaction).

### 4f. How it wires together

```
buyer ──EURC/USDC──▶ escrow PDA  (program-owned; no human can withdraw)
      ──flat fee ───▶ protocol fee wallet (100,000 lamports SOL)

           job runs ─▶ signed receipts ─▶ settlement (Module 3) ─▶ completion fraction f

on finalize, escrow splits by f:
   f × price       ─▶ SELLER (same stablecoin mint — no swap, no burn)
   (1 − f) × price ─▶ refund ─▶ BUYER (original stablecoin)
```

Net: **buyer pays stablecoin into escrow → work is attested → earned
part is paid to the seller in the same stablecoin, unearned part
refunded to the buyer — all by program logic, no custodian.**

**One program** (escrow + fee transfer). No swap, no oracle, no token
mint, no governance, no registry.

> **Trust caveat:** "no one holds the funds" only holds if the program
> rules can't be quietly changed. For real trustlessness, make the
> program **immutable** before mainnet — otherwise the upgrade key is an
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

## Consent & disclosure (cross-cutting)

Vtessera sells compute — other people execute code on your machine — so the
product carries an explicit consent contract. The full spec lives in
[`docs/CONSENT.md`](docs/CONSENT.md); the roadmap items are:

- **GUI consent flow (done in the consent-disclosure PR):** first-run
  metering gate (§2.1), the off-by-default "Accept workloads from others"
  switch with honest no-sandbox copy (§2.2), and the three-state status
  surface (Off / Metering only / Accepting jobs) with a recent-jobs list and
  the settlement-authority row (§2.3).
- **Behavioural invariants (§1):** no autostart, two consent gates,
  one-action stop, no silent resume, legible activity, complete uninstall,
  honest process naming, declared + tested network surface (v0 metering
  opens no sockets — `tests/no_socket.rs`).
- **Precision in claims (§3):** the do-not-say / say-instead table governs
  the README and UI copy; settlement-authority centralisation and the flat
  fee are disclosed, not hidden.
- **Anti-misclassification (§4):** reproducible builds, signed releases with
  digests, VirusTotal pre-submission, minimal Flatpak permissions with
  rationale, hardening visible in SECURITY.md, third-party review before
  mainnet. Tracked in `MAINNET-CHECKLIST.md`.
- **Future:** accept-workloads consent should gate the *executor choice* per
  job once sandboxed backends (Kata, Module 1e) exist — the no-sandbox copy
  then becomes a per-backend disclosure rather than the default truth.

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

Nothing — the devnet program ships the **production** path. The earned
slice is paid to the seller in the same stablecoin the buyer deposited;
there is no swap. The old devnet stub is deleted, and the devnet
redeploy of this build is pending.

## Mainnet criteria (DEFERRED — do not deploy until met)

Devnet works. Mainnet doesn't follow automatically. Each gating item
below is expanded into concrete numbered steps in
[`MAINNET-CHECKLIST.md`](MAINNET-CHECKLIST.md) — that file is the
authoritative tracker (this section is the summary).

Before the program is deployed to mainnet-beta, **all** of the
following must hold:

- [ ] **Direct stablecoin settlement implemented and tested.** The
  program pays the seller in the same stablecoin mint the buyer
  deposited — no swap, no oracle, no burn. The production path is
  exercised in unit and adversarial tests.
- [ ] **Settlement authority pinned at deploy.** The operator's key, set
  in `Config` by `init_config`, signs `finalize_pro_rata` so no
  arbitrary caller can finalize an escrow with a fabricated `f`.
  `Config` is immutable after `init_config` — no governance
  instructions.
- [ ] **Fee config confirmed.** `fee_wallet` =
  `J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh` and `fee_lamports` =
  100,000, set at `init_config` and reviewed publicly.
- [ ] **Upgrade authority handled.** A single dev keypair as upgrade
  authority is an implicit custodian (ROADMAP §4d). Either set the
  upgrade authority to a multisig or make the program immutable
  (`solana program set-upgrade-authority --final`).
- [ ] **Third-party audit** of the escrow program. The program is
  small (~300 LoC) but touches custody — reviewable in an afternoon,
  but ship the review.
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
   yet. **Partial:** the free path runs today — a node with `--backend`
   executes free-offer jobs through `crates/executor` (noop-cpu or
   local-cpu) and returns metering over HTTP and MCP, and the offer-index
   live-demo wiring (node `--publish`, FCFS claims with node
   enforcement, MCP `discover`; `scripts/offer-index-demo.sh`) is
   shipped. What M3 still wants is the Kata/Cloud Hypervisor CPU
   isolation from §1.
4. **M4 — Paid go-live:** Module 0 cleared; x402 payment + escrow
   program live — agent pays EURC/USDC (escrow for committed jobs,
   pay-as-you-go for short ones) + flat SOL fee, pro-rata release
   (earned → seller in the same stablecoin) and refund (unearned →
   buyer in stablecoin), program immutable. Real, non-custodial,
   agent-native marketplace.
