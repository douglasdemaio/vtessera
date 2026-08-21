# Security Policy

## Supported Versions

Vtessera is pre-release software. There are no tagged releases yet, and
the `v0` metering daemon should be considered experimental.

| Version            | Supported          |
| ------------------ | ------------------ |
| `main` (unreleased)| :white_check_mark: |
| Anything older     | :x:                |

Only the current `main` branch receives security fixes. Once tagged
releases exist, this table will list the supported minor versions and
the fix will land in a new patch release rather than a branch backport.

## Reporting a Vulnerability

**Please do not open a public issue for security problems.**

Report privately through GitHub's
[private vulnerability reporting](https://github.com/douglasdemaio/vtessera/security/advisories/new)
(Security tab → Report a vulnerability). If you can't use that, email douglasdemaio@gmail.com.

Please include:

- Affected commit SHA or RPM version, and target (e.g. `x86_64-unknown-linux-musl`)
- Steps to reproduce, ideally a minimal config or receipt that triggers it
- Impact: what an attacker gains (forged receipt, key disclosure, local
  privilege escalation, sandbox escape from the systemd/AppArmor confinement)


Please give us 90 days before public disclosure, or until a fix ships,
whichever comes first. We'll coordinate the timeline with you if the fix
needs longer.

### In scope

- Forgery, replay, or tampering of signed usage receipts
- Ed25519 signing key disclosure, weak key generation, or unsafe key
  file permissions
- Local privilege escalation via the `vtesserad` daemon, its systemd unit
  (`DynamicUser`), or the AppArmor profile
- Config parsing bugs reachable from `/etc/vtessera/vtessera.toml`
- Path traversal or unsafe writes in the state directory
  (`/var/lib/vtessera/`)
- RPM packaging issues: unsafe scriptlets, wrong ownership or modes
- Supply-chain issues in pinned dependencies not already flagged by
  `cargo audit` / `cargo deny`
- Escrow program vulnerabilities: unauthorized fund withdrawal,
  double-spend, settlement authority bypass, integer overflow in
  `f` calculation, ATA manipulation, or any path that could drain
  buyer or seller funds (see `programs/vtessera-escrow/SECURITY.md`)

### Out of scope

- Metering inaccuracy or resource-accounting disputes that have no
  security impact
- Attacks requiring existing root on the host
- Denial of service against your own machine by misconfiguring your own daemon
- Solana validator censorship or front-end/RPC vectors (network-level trust)
- Stablecoin issuer freeze powers (Circle can freeze USDC/EURC addresses —
  that risk sits with the individual buyer or seller)

## Security Design

See `docs/` for the architecture and security design documents.
