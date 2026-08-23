---
name: fleet-management
description: Design and implement fleet/configuration management at scale for vtessera nodes, using Uyuni (Salt-based) as the background management plane for the private/enterprise track. Use when the user mentions Uyuni, Salt, fleet, "manage nodes at scale", provisioning many machines, RPM/systemd packaging for fleets, config channels, or unattended node rollout. Covers the consent constraints that limit what may run in the background on consumer installs.
---

# Vtessera fleet management (Uyuni / Salt)

Goal: let an operator run tens–thousands of vtessera nodes with centrally
managed config, updates, and health, without violating the project's consent
posture.

## Hard constraints (read before designing anything)

- **CONSENT.md is binding for the public product**: no autostart, two consent
  gates, one-action stop, declared network surface. A salt-minion silently
  managing a consumer's machine violates all of that. Therefore Uyuni is an
  **opt-in enterprise/private-network feature**, layered on the existing
  private/enterprise hooks: `crates/vtessera-config` (wizard),
  `crates/marketplace-server` (private receipt store), and
  `docs/superpowers/specs/2026-08-21-private-network-design.md` ("pools, edge
  fleets").
- Fleet nodes are headless systemd deployments, NOT the Flatpak GUI. The GUI
  path (GUI spawns `vtesserad` + `vtessera-node` + `vtessera-offer-index`)
  stays untouched.
- One Ed25519 key = one node (`crates/offer/src/lib.rs::derive_node_id`).
  Fleet provisioning must generate a keypair per node, never share keys.

## What Uyuni gives us

Uyuni (uyuni-project.org) is SUSE's Salt-based systems-management server:
RPM patching, Salt states for config, remote execution, recurring actions,
and a JSON-over-HTTP + XML-RPC API for automation
(https://www.uyuni-project.org/uyuni-docs-api/uyuni/index.html). Current
release line: 2026.08. Auth: user/password against the API, session token
thereafter. Everything the WebUI does is scriptable via that API.

## Implementation task list (for the executing model)

1. **Native packaging first** (prerequisite for everything): RPM spec (and
   optionally deb) for `vtessera-node`, `vtesserad`, `vtessera-offer-index`
   with systemd units. Units must carry the hardening already promised in
   ROADMAP §5 (`systemd-analyze security` targets). New `packaging/rpm/`
   directory; wire an OBS or copr build if asked.
2. **Salt formula** `packaging/salt/vtessera-formula/`: states to install the
   RPMs, template `/etc/vtessera/node.toml` from pillar data (offer-index
   URL, publish interval, price quote, device inventory), generate the
   per-node Ed25519 keypair on first highstate (`unless:` guard — never
   regenerate), enable/start units. Pillar example + `form.yml` so Uyuni's
   formulas-with-forms UI can drive it.
3. **Uyuni onboarding doc** `docs/FLEET.md`: bootstrap minions, assign the
   formula via system group "vtessera-nodes", recurring action for
   patch+restart, config channel for node.toml overrides. State explicitly
   that this path is for machines the operator owns (consent gate satisfied
   by the operator's ownership, per CONSENT.md's enterprise carve-out — if no
   such carve-out exists yet, ADD one to CONSENT.md and get it reviewed).
4. **Fleet health back-report**: a Salt beacon or cron state that checks
   `GET /healthz` on the local node and raises a Salt event on failure, so
   Uyuni recurring actions can auto-remediate (restart unit).
5. **API automation script** `tools/uyuni-fleet.py` (or rust bin): use the
   Uyuni JSON API to bulk-create activation keys, system groups, and apply
   the formula — so a provider can go from 0→N nodes without touching the
   WebUI.
6. Do NOT add any Salt/Uyuni dependency to the Rust workspace. This entire
   feature is packaging + states + docs; the daemons stay dependency-lean
   (BUILD.md §1.3 dep budget).

## Definition of done

`docs/FLEET.md` walks a fresh Uyuni 2026.x server from zero to 3 managed
VMs each running a registered, heartbeating vtessera node visible in the
offer-index, with one recurring action proving central update works.
