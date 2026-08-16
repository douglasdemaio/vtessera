# Vtessera — Consent, Disclosure & Anti-Misclassification

> Vtessera turns a GNU/Linux machine into a compute node that AI agents can
> hire. Selling compute means other people execute code on your machine, so
> the product needs **consent gates**, **legible activity**, and **honest
> claims**. This document is the spec for that. It binds the GUI, the
> daemons, the packaging, and every word we publish.
>
> Status: **live.** The behavioural invariants (§1) hold for v0, the GUI
> consent flow (§2) and this doc land together, and the anti-misclassification
> checklist (§4) tracks in `MAINNET-CHECKLIST.md`. Acceptance criteria are
> §5; the README framing paragraph is §6.

---

## 1. Non-negotiable behavioural invariants

These hold for every build, on every platform, at every version. A PR that
weakens one is rejected.

1. **No autostart.** Installing the app never starts it. Neither the RPM
   spec (`%service_add_pre/post`) nor the Flatpak enables or launches the
   daemon on install. The user starts it.
2. **Two consent gates.** *Metering* consent and *accept workloads* consent
   are separate, explicit decisions. Metering consent is asked on first run
   (§2.1); accepting workloads is a second switch that defaults **off** and
   stays off until flipped (§2.2).
3. **One-action stop.** A single visible control ("Stop") stops everything
   the app started: metering and, if running, job acceptance. There is no
   state reachable in which the app is running but cannot be stopped with
   one click.
4. **No silent resume.** Nothing the user stopped restarts itself: no
   cron, no timer, no self-restart, no "resume after reboot". If the app
   runs again it is because the user started it.
5. **Legible activity.** Everything the machine does on the user's behalf
   is visible in the app while it happens and recorded afterwards — receipts
   and a per-job list (§2.3). No background activity that the UI cannot show.
6. **Complete uninstall.** Uninstalling stops and removes everything the
   app installed: the service (if enabled), the config, the identity key,
   the state directory. Documented in `README.md` ("Uninstall").
7. **No obfuscation.** Processes are named honestly (`vtesserad`,
   `vtessera-node`, `vtessera-gui`). No hidden daemons, no renamed binaries,
   no payload that only runs when the UI is closed.
8. **Declared network surface.** What opens sockets and when is documented
   and tested. v0's `vtesserad` (metering) opens **no sockets** — pinned by
   `tests/no_socket.rs`. The agent-facing node binds loopback by default and
   must not be advertised on a routable interface without explicit action.

---

## 2. GUI consent flow

### 2.1 First run — metering consent

On first launch (no recorded consent), a modal gate appears before the main
window. It states plainly:

- **What Vtessera does:** samples CPU/memory/disk usage and writes signed
  receipts to a local state folder; can run compute jobs for other agents
  **only after** the separate "Accept workloads" consent; settles paid jobs
  on Solana (devnet in v0).
- **What it never does:** starts itself, restarts after Stop, runs programs
  without permission, opens network sockets in v0 (metering alone).
- **How to exit:** one Stop button at any time; uninstall leaves nothing
  running.

Buttons: **"Not now"** (quits — nothing runs) and **"Enable metering"**
(records `metering_consent` + `consent_version` in `settings.toml`, then
shows the main window; nothing starts until Start is pressed).

Copy bumps: when `CURRENT_CONSENT_VERSION` in `crates/vtessera-gui/src/
settings.rs` is raised, stored consents at an older version re-show the gate.

### 2.2 Second gate — "Accept workloads from others"

A switch in Settings, **off by default and off until flipped**. The label
next to it says, honestly:

> Jobs run through the selected backend: 'noop-cpu' simulates; 'local-cpu'
> executes the job's commands on this machine with your user's privileges
> and **NO sandbox**. Only enable this if you trust the workloads you will
> receive.

Rationale for the honesty: the `LocalCpuExecutor`
(`crates/vtessera-executor`) is a skeleton, not a sandbox. The GUI's default
backend is `noop-cpu`. Both facts are stated in the UI rather than hidden.

Effects of the switch (persisted immediately, applied on Start):
- **Off:** Start runs only `vtesserad` — status "Metering only". No offer is
  written, no `vtessera-node` spawned, none reused.
- **On:** Start also writes the signed offer and spawns `vtessera-node` —
  status "Accepting jobs".

### 2.3 Persistent status surface

The Status page always shows one of three states:

| State | Meaning |
| --- | --- |
| **Off** | Nothing running. |
| **Metering only** | `vtesserad` sampling; no jobs accepted. |
| **Accepting jobs** | `vtesserad` + `vtessera-node` serving. |

Plus: node ID, mode, receipt count, **recent jobs** (per-job receipts under
`<state-dir>/job-receipts/`), and a static row stating who picks the
completion fraction `f` and the limit of that power (settlement authority —
see §3.1). This is the legible-activity record (§1.5).

### 2.4 Copy rules

- Plain language. No "earn", "passive income", "free money" framings in the
  UI.
- Defaults are **off** or **"Not now"**. Consent buttons say exactly what
  they do ("Enable metering").
- Never overstate isolation. If it isn't sandboxed, say so.
- Numbers are exact (fee = 100,000 lamports = 0.0001 SOL, §3.2).

---

## 3. Precision in claims

What we can say, and what we must say instead. Applies to the README, the
GUI, the ROADMAP, release notes, and any marketing.

| Do not say | Say instead |
| --- | --- |
| "Secure", "uncrackable" | Specific controls: `DynamicUser`, `ProtectSystem=strict`, AppArmor profile, `#![forbid(unsafe_code)]` — each with what it does and does not do |
| "Decentralized", "non-custodial" unqualified | "Funds are held by a program-owned escrow PDA on Solana and released only by the on-chain rules below" — the escrow program is the custodian |
| "No fees" | "No Vtessera token and no percentage fee — a flat SOL protocol fee of 100,000 lamports per settlement transaction" |
| "The operator can't touch your money" | "The settlement authority can set `f` and can **never finalize**; it cannot redirect escrowed funds to itself, but it can choose not to finalize" (centralisation documented) |
| "Runs AI jobs in a sandbox" | "'local-cpu' executes jobs on this machine with the user's privileges and no sandbox; 'noop-cpu' simulates" |
| "Earn with your idle machine" | "Vtessera lets other agents rent CPU/GPU time on your machine, settled in EURC/USDC" |

### 3.1 Settlement authority

Centralisation is disclosed, not hidden. The settlement authority — a single
key pinned in the escrow `Config` at `init_config` — chooses the completion
fraction `f`. That is a **functional gate**, not governance: it stops an
arbitrary caller from finalizing an escrow with a fabricated `f`. The
authority **cannot** redirect escrowed funds to itself; it **can** decline
to finalize. `Config` is immutable; there are no governance instructions.
Documented in `programs/vtessera-escrow/SECURITY.md` and
`MAINNET-CHECKLIST.md` §3.

### 3.2 The flat fee

The fee is a flat **100,000 lamports = 0.0001 SOL** per transaction, set in
`Config` at `init_config`, immutable after deploy. It is charged on
`pay_for_compute`, `finalize_pro_rata`, and `cancel_before_start` — including
`cancel_before_start` on a contract that never completed. Skipped when
`fee_lamports == 0` (that escape hatch is for local testing). Documented in
`README.md` ("Currencies") and `ROADMAP.md` §0.

---

## 4. Anti-misclassification checklist

We ship a *compute node*, not an *AI product*. These keep the two apart:

- **Reproducible builds** — `solana-verify` SHA pinned at mainnet
  (`MAINNET-CHECKLIST.md` §5).
- **Signed releases** — the RPM/Flatpak artifacts are published with SHA-256
  digests, and the binary SHA appears in the release notes.
- **VirusTotal pre-submission** — every release binary is scanned before
  announcement; the result links in the release notes.
- **Honest process naming** — `vtesserad`, `vtessera-node`, `vtessera-gui`.
  Nothing masquerades under another name (§1.7).
- **Minimal Flatpak permissions** — see `packaging/flatpak/README.md` for
  the per-permission rationale (share=network is required to serve offers;
  the GUI itself needs only display/GPU sockets).
- **Hardening visible** — `programs/vtessera-escrow/SECURITY.md` links the
  host-side controls (systemd unit, AppArmor, no-socket v0) alongside the
  program trust model.
- **Security contact** — `douglasdemaio@gmail.com`, published in the
  program's on-chain `security` metadata and in SECURITY.md.
- **Third-party review before mainnet** — community review first, paid audit
  gated on revenue/TVL (`MAINNET-CHECKLIST.md` §4).

---

## 5. Acceptance criteria

The release gates this PR's work satisfies. Each is checkable; the
checked ones land with the consent-disclosure PR.

- [x] First launch with no `settings.toml` shows the metering gate; the main
      window is not shown until "Enable metering"; "Not now" quits.
- [x] After consent, `settings.toml` contains `metering_consent = true` and
      the current `consent_version`; relaunch skips the gate.
- [x] Bumping `CURRENT_CONSENT_VERSION` re-shows the gate for stored
      settings at an older version.
- [x] "Accept workloads from others" is **off** on fresh install and off in
      `Settings::default()`.
- [x] With it off, Start reaches state "Metering only": `vtesserad` runs, no
      offer file is written, no node is spawned or reused.
- [x] With it on, Start reaches "Accepting jobs": offer written, node spawned.
- [x] The isolation warning text ("NO sandbox", "with your user's privileges")
      is present verbatim next to the switch.
- [x] Stop from any running state returns to "Off" with one click.
- [x] Status page shows the settlement-authority row and a recent-jobs list
      (populated from `<state-dir>/job-receipts/`).
- [x] v0 metering opens no sockets: `cargo test -p vtesserad` runs
      `no_socket` and passes.
- [x] README carries the framing paragraph (§6); `docs/CONSENT.md` is linked
      from `README.md` and `ROADMAP.md`.

---

## 6. README framing paragraph

> Vtessera is an opt-in compute node for AI agents, settled in EURC/USDC.
> It asks your permission before it does anything: metering consent on first
> run, and a separate, off-by-default switch before it accepts workloads from
> other agents. Jobs run through your chosen backend — simulated by default,
> or executed on this machine with no sandbox if you say so. You can stop
> everything with one button and uninstall completely at any time. Vtessera
> never starts itself, never restarts itself, and never runs code you didn't
> approve. There is no token, and settlement is a flat SOL protocol fee on
> top of the stablecoin the buyer pays.
