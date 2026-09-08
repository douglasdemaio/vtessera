# Vtessera — Product Requirements Document (PRD)

**Status:** Draft v1.0
**Author:** Vtessera product team
**Last updated:** 2026-09-08
**Scope:** Full product — both market sides (AI-agent buyer, machine-owner
seller), discovery, paid + free compute paths, settlement, and escrow.

---

## 0. Executive Summary

Vtessera lets **GNU/Linux machine owners** safely rent out idle CPU/GPU to
**AI agents** over an open, agent-native market, settling **non-custodially
in EURC/USDC on Solana** — with **no Vtessera token**. Agents discover
nodes (MCP / offer-index / marketplace), run jobs free or paid, and trust
that payment is held in a program-owned escrow and split pro-rata by
attested work.

**Where it stands:** fully shipped end-to-end and exercised live on Solana
**devnet** (free + x402-paid paths, microVM/Kata isolation, GPU support,
signed receipts → settlement fraction → escrow split). **Mainnet is
explicitly deferred** behind a strict checklist.

**What's needed next:** the commercial launch is blocked mainly on the
remaining mainnet gates (immutable program, third-party review,
reproducible build) and on product/ops gaps listed in §11 Shortfalls —
notably a human-facing marketplace UI, quotas/scheduling, abuse
protection, and NAT-reachability (iroh).

---

## 1. Summary

Vtessera is an **opt-in compute layer for GNU/Linux machine owners** to
rent out idle **CPU and GPU** capacity to **AI workloads**, with
machine-to-machine discovery and payment and **non-custodial,
stablecoin settlement on Solana**.

Key product stance: **Vtessera is technology, not a token.** Sellers are
paid in the **same EURC/USDC stablecoin the buyer pays** — no swap, no
oracle, no Vtessera mint — plus a flat SOL protocol fee. The buyer is
software (AI agents), not humans.

The full end-to-end pay → run → settle → split flow is **shipped and
exercised live on Solana devnet**; `vtessera-node` negotiates paid jobs
over **x402** (`HTTP 402 Payment Required`) and the Anchor escrow program
splits payment pro-rata by attested work. Mainnet is intentionally
deferred behind `MAINNET-CHECKLIST.md`.

---

## 2. Problem Statement

### 2.1 The gap

- **AI agents need compute** but today face centralized clouds with
  human signups, API keys, locked-in pricing, and opaque terms.
- **Machine owners** (individuals, labs, enterprises) hold idle
  CPU/GPU capacity with no safe, simple, and *autonomous* way to monetize
  it. Selling to other software requires custody, payment infra, dashboards,
  and identity — none of which a private machine owner wants to run.
- **Trust is the blocker.** Selling compute means running untrusted code on
  your hardware. "Who is paying, how do I get paid, and can I isolate their
  workload?" are unanswered by today's ad-hoc solutions.

### 2.2 Why it matters

Compute is the input AI agents consume. If machine-to-machine can
interoperate on an open, non-custodial market — where payment settles in
the stablecoin both sides already hold — then idle hardware becomes a
real, trustworthy supply for the agent economy without a new token or a
new walled garden.

---

## 3. Goals / Non-Goals

### 3.1 Product goals

1. **Make idle CPU/GPU monetizable** by GNU/Linux machine owners, safely
   and with explicit consent.
2. **Let software buy compute autonomously** — discovery, contracting, and
   payment with no human signup, API keys, or dashboard.
3. **Settle in the stablecoin the buyer pays** (EURC/USDC), non-custodial,
   with a flat SOL protocol fee. No Vtessera token.
4. **Support both free and paid** workloads — the seller chooses per node
   with a single config flag; free jobs cost nothing on-chain.
5. **Keep the seller in control** — explicit consent, one-action stop,
   complete uninstall, honest disclosure of what runs on their machine.

### 3.2 Non-goals (v1)

- **No token, reserve, DAO, treasury, or custodian.** Settlement is
  stablecoin-only.
- **Not a general public cloud.** No managed dashboards, SLAs, or
  human-facing marketplace UI in v1 (a reference marketplace server exists
  for private/enterprise deployments).
- **Mainnet deployment** is deliberately out of scope until every
  `MAINNET-CHECKLIST.md` gate passes.
- **No price oracles or asset conversion.** The escrow never swaps.

---

## 4. Personas / Users

1. **AI Agent (software buyer).** Autonomous software that discovers nodes,
   submits jobs, and pays in stablecoin via x402. Success = find suitable
   compute, run the job, settle fairly, no human involvement.
2. **Node Operator / Seller.** GNU/Linux machine owner (individual, lab,
   or enterprise) renting CPU/GPU. Success = safely monetize idle capacity
   with clear consent and predictable payout in stablecoin.
3. **Private / Enterprise Fleet Admin.** Runs the reference marketplace,
   offer-index, and config wizard for internal compute accounting
   (charge-back, on-prem GPU pools). Success = enforce internal network
   policy and track utilization.
4. **Protocol Operator.** Provides the marketplace/offer-index (reference),
   holds the settlement-authority and protocol-fee wallets. Success = neutral,
   low-risk, immutable settlement.

---

## 5. User Stories

### Buyer (agent) side

- As an **agent**, I can find available nodes locally, on a LAN offer-index,
  or via the public marketplace — without an account.
- As an **agent**, I can read a node's signed offer (device class, GPU,
  VRAM, price or `free`, payout address) before committing.
- As an **agent**, I can submit a job over HTTP or MCP and get a result.
- As an **agent**, on a paid node I can receive an **x402 `402`** challenge,
  pay the escrow in the offer's stablecoin, and resubmit with a payment
  proof — then have my job run.
- As an **agent**, I trust that unearned funds (job runs less than the
  contract) are refunded to me in the original stablecoin, and the seller
  is paid only the attested fraction.

### Seller side

- As a **seller**, I can choose free or paid with one config flag, no code.
- As a **seller**, I am paid in the **same stablecoin the buyer paid**, to
  my wallet, directly by the escrow program — no custody, no conversion.
- As a **seller**, I control consent: metering on first run, and an
  off-by-default switch before accepting workloads from others.
- As a **seller**, I can isolate untrusted workloads (Cloud Hypervisor
  microVM / Kata Containers) and stop everything with one action.

### Fleet admin side

- As a **fleet admin**, I can keep nodes private (internal CIDRs, opt-in
  private mode), point at a company marketplace, and account for internal
  compute usage.

---

## 6. Functional Requirements

### 6.1 Node metering (v0 `vtesserad`)

- **FR-M1** Sample CPU/memory (and GPU where present) from the host at a
  configured interval.
- **FR-M2** Emit **signed Ed25519 receipts** to a state directory; the
  signing key is auto-generated `0600`, never stored in `/etc`.
- **FR-M3** Open **no network sockets** in metering-only mode (pinned by
  regression test).
- **FR-M4** Support `--once` for smoke tests and a `window_size` so a
  receipt is finalized per window.

### 6.2 Compute execution + accelerators (Module 1)

- **FR-E1** Run jobs via pluggable backends:
  `noop-cpu` (default), `local-cpu` (unsandboxed host run),
  `cloud-hypervisor` (disposable microVM, CPU-only, no guest network),
  `kata-cloud-hypervisor` (OCI images, VFIO GPU passthrough, VM isolation).
- **FR-E2** Enforce per-job **resource caps** via cgroups v2 (CPUs, memory);
  optional vCPU pinning / NUMA awareness.
- **FR-E3** Support **NVIDIA and AMD GPU** whole-GPU VFIO passthrough, and
  sharing via MIG / vGPU / time-slicing.
- **FR-E4** Meter GPU usage per device (avg utilization, power, peak VRAM,
  VRAM-GB-hours) and fold it into the job receipt; DCGM optional.
- **FR-E5** Enforce a **network policy** per job: `None` (default) /
  `OutboundHttps` / `Egress` (full or CIDR-restricted).
- **FR-E6** Admit jobs **capability-aware** (device class, GPU model, VRAM,
  MIG profile, driver/CUDA version).

### 6.3 Discovery + agent marketplace (Module 2)

- **FR-D1** Each node publishes a **signed, machine-readable offer**:
  device specs, availability, endpoint, price (`free` or EURC/USDC) and
  payout wallet.
- **FR-D2** Expose offers through agent-native standards:
  **MCP** (2024-11-05, `discover` + `submit_job`), optionally **A2A**
  agent cards, and a **central offer-index**.
- **FR-D3** Offer-index verifies offer signatures on register (no
  impersonation) and serves `GET /offers?available=1`.
- **FR-D4** Agents **claim** nodes FCFS (60s lease, renewable/released by
  owner); claims are **node-enforced** (409 when claimed by another,
  503 fail-closed when index unreachable).
- **FR-D5** Support **local discovery** via `~/.local/share/vtessera/
  node-discovery.json` so `vtessera-agent --local` works out of the box.
- **FR-D6** (roadmap) **iroh** connectivity sidecar so nodes behind NAT /
  CGNAT are reachable by `EndpointId` via relay / hole-punching, dialing by
  `node_id` instead of IP:port.

### 6.4 Job lifecycle + contract (Module 2c)

- **FR-J1** Lifecycle: discovered → agreed → (paid via x402 / free) →
  running → finalized → settled.
- **FR-J2** Every accepted job writes a `JobContract` on disk
  (`contracts/<job_id>.json`) recording `job_id`, `node_id`,
  `device_class`, and `agreed_device_seconds`.

### 6.5 Payment (Module 2b / 4)

- **FR-P1** **Free** nodes serve jobs directly (`HTTP 200`), never return
  `402`, and incur **no transaction, escrow, swap, or fee**.
- **FR-P2** **Paid** nodes issue an **x402 `402`** challenge; the agent
  signs a stablecoin transfer to the escrow and resubmits with an
  `x-payment` proof.
- **FR-P3** Verify payment proofs **off-chain via Solana RPC**
  (finalization, escrow participation, sufficient amount) before running.
- **FR-P4** Buyers pay in **EURC (default) or USDC**; sellers earn the
  **same mint** — no conversion, no price risk.

### 6.6 Settlement + work attestation (Module 3)

- **FR-S1** After every job (Completed / Failed / TimedOut) the node signs
  a `JobReceipt` and writes `job-receipts/<job_id>.json`.
- **FR-S2** The `vtessera-settle` service verifies each signed receipt
  (schema / pubkey / self-attesting `node_id` / signature); **any failure
  is a permanent reject with no partial credit**.
- **FR-S3** Aggregate device-seconds by the agreed device class (GPU
  contracts credit GPU-seconds, never CPU-seconds) and compute the
  **completion fraction `f ∈ [0, 1]`**, written to
  `settlements/<job_id>.json`.
- **FR-S4** A missing receipt is transient and retried; produce amounts +
  `f` as the trustworthy inputs for escrow release.

### 6.7 Non-custodial escrow (Module 4, Anchor program)

- **FR-C1** `pay_for_compute`: atomically deposit the contract price in
  stablecoin into a program-owned **escrow PDA** and transfer the **flat
  SOL fee** (100,000 lamports / 0.0001 SOL) to the protocol fee wallet
  (`J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh`, stored in immutable
  `Config`).
- **FR-C2** `finalize_pro_rata`: split the escrow **strictly by `f`** —
  `f × price` to the seller's ATA in the same mint; `(1 − f) × price`
  refunded to the buyer in the original stablecoin.
- **FR-C3** `cancel_before_start`: full refund to buyer; fee still
  collected on the transaction.
- **FR-C4** Only the **settlement authority** (operator key pinned in
  `Config` at deploy) may invoke `finalize_pro_rata`.
- **FR-C5** Disable the fee when `fee_lamports == 0`; charge **once per
  job/session** (never per x402 micro-payment).
- **FR-C6** The program is **immutable** before mainnet (no upgrade-key
  custodian); no governance instructions.

### 6.8 Consent + disclosure (cross-cutting)

- **FR-CT1** Two explicit consent gates: first-run enable-metering, and an
  **off-by-default** "Accept workloads from others."
- **FR-CT2** One-action stop (halts metering + job acceptance; no silent
  resume); complete uninstall removes service, config, key, and state.
- **FR-CT3** Legible Status tab: state (Off / Metering only / Accepting
  jobs), settlement authority, per-job receipts.
- **FR-CT4** No autostart; no code runs without approval; honest process
  naming and claims (see `docs/CONSENT.md`).

### 6.9 Distribution channels

- **FR-X1** A GTK4 desktop GUI, distributed via **Flatpak**
  (`io.github.douglasdemaio.Vtessera`).
- **FR-X2** An RPM-packaged `vtesserad` systemd service (hardened unit),
  plus an **agent CLI** (`vtessera-agent`: discover, offer, submit, health)
  and a **stdio MCP** binary.
- **FR-X3** One-command dev stack (`scripts/local-stack.sh`) and
  reference **observability** (Prometheus `/metrics` + Grafana compose).

---

## 7. Metrics / Success Criteria

| Category | Metric | Target |
| --- | --- | --- |
| Compute supply | Active nodes publishing signed offers | Growing over time |
| Utilization | Internal capacity used by jobs vs idle | TBD per device |
| Free path UX | Jobs submitted → results, no payment | 100% of free smoke tests |
| Paid path | End-to-end pay→run→settle→split on devnet | 0% failure rate (soak) |
| Settlement integrity | Receipts verify; hard rejects never partial-credited | Fuzz + adversarial pass |
| Escrow correctness | Pro-rata split matches `f` exactly | Unit + adversarial pass |
| Consent | Opt-in gates honored; no autostart / no sockets w/o jobs | Regression-gated |
| Reliability | CI green: fmt, clippy `-D warnings`, test, audit, deny | 100% across workspace |

---

## 8. Release Plan / Milestones

Current status (see `ROADMAP.md` for detail):

- **v0 / M1 — CPU compute proof:** ✅ Shipped. Kata + Cloud Hypervisor OCI,
  CPU-only, per-job metering into signed receipts.
- **M2 — GPU tier:** Shipped (VFIO passthrough, GPU sharing, GPU metering).
- **M3 — Agent discovery + free compute + settlement:** Shipped (MCP/x402/
  offer-index, contracts, `vtessera-settle` computing `f`). Kata CPU
  isolation available.
- **M4 — Paid go-live:** Shipped on **devnet** (x402 + escrow, pro-rata
  release + refund, flat SOL fee). **Mainnet gated** on
  `MAINNET-CHECKLIST.md`.

### M5+ (roadmap)

- iroh connectivity sidecar (`FR-D6`).
- TEE / confidential-compute attestation (SEV-SNP / TDX, confidential GPU).
- Spool rotation / archive; abuse handling + rate limits.
- Third-party audit + reproducible BPF build + immutable mainnet deploy.

---

## 9. Risks / Open Questions

- **Mainnet readiness.** Devnet works; mainnet requires every
  `MAINNET-CHECKLIST.md` gate (upgrade authority → immutable, third-party
  review, reproducible build). Deployment is deferred by policy.
- **Escrow security.** Program touches custody; keep small (~300 LoC),
  fuzz/adversarial-tested, immutable, and reviewed publicly before mainnet.
- **Stablecoin edge risk.** EURC/USDC carry issuer freeze capability — that
  risk sits with the individual buyer/seller, not the protocol; disclosed.
- **Isolation guarantees.** `local-cpu` is unsandboxed — mitigated by
  defaulting to microVM/Kata and by clear per-backend disclosure in the UI.
- **Network reachability.** Nodes behind symmetric NAT need the iroh
  sidecar (M5) for full marketplace reachability.
- **Neutrality.** "No one holds the funds" holds only while the program is
  immutable — the operating upgrade key must be removed before mainnet.

---

## 11. Shortfalls

Known gaps between the current shipped state and a fully commercial product.
These are the honest "what we don't have yet" items; each is actionable.

### Product / marketplace
- **No human-facing marketplace UI.** Discovery is agent-native only
  (MCP / curl / CLI). A non-technical seller or a browsing buyer has no
  dashboard, no visibility into demand, no list of live listings.
- **No pricing / listing tools.** Sellers hand-edit config for price and
  device specs; there is no guided pricing, no catalog, no per-device
  listing manager.
- **No queuing / scheduling.** Jobs run ad hoc; there is no job queue,
  preemption, prioritization, or fair-scheduling across a fleet.

### Operations & abuse
- **No rate limits / admission quotas** at the node or index ($5) — a
  single agent could monopolize or spam a node.
- **No abuse protection / reputation.** No coordinator kill switch, no
  blacklisting, no per-agent or per-node reputation or trust signal.
- **No spool rotation / archiving** ($5) — receipts and contract files
  grow unbounded on long-running nodes.

### Network & reachability
- **NAT hole.** Nodes behind symmetric NAT / CGNAT / corporate networks are
  unreachable; the iroh sidecar (FR-D6) is roadmap-only, so marketplace
  reachability is limited to directly-reachable nodes today.

### Security & launch gates
- **Mainnet not deployable yet.** Upgrade authority + immutable build,
  third-party review, and reproducible BPF build are all still open
  (`MAINNET-CHECKLIST.md`).
- **No TEE / confidential-compute attestation** (SEV-SNP / TDX,
  confidential GPU) — important for high-value, untrusted-renter
  workloads; roadmap-only.

### Trust & program robustness
- **`local-cpu` unsandboxed** — a real isolation gap if a seller enables
  it; only mitigated by defaulting to microVM/Kata and by UI disclosure.
- **Single settlement authority today** — settlement is centralized on the
  operator key; acceptable for v1/devnet, but a long-term decentralization
  question.

### Gaps to close before launch (priority order)
1. Mainnet gates: immutable program, third-party review, reproducible build.
2. Human-facing marketplace UI (browse listings, view terms, run job).
3. Node/index abuse protection: rate limits, quotas, kill switch.
4. Job queue + scheduling on the node.
5. iroh connectivity sidecar (NAT-free reachability).
6. Spool rotation / archiving.

---

## 10. References

- `README.md` — Overview, install, quickstart.
- `AGENTS.md` — Agent-facing onboarding (discovery, modes, job submission).
- `ROADMAP.md` — Modules 0–5, fee model, milestones, build status.
- `MAINNET-CHECKLIST.md` — Pre-flight gates before mainnet deploy.
- `docs/CONSENT.md` — Consent & disclosure spec.
- `docs/DESIGN.md` — Design index + architecture.
- `BUILD.md` — v0 build specification.
- `SECURITY.md` — Security policy / reporting.
