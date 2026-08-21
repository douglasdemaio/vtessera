# Vtessera {VERSION}

Released {DATE}.

Vtessera is an opt-in compute node for AI agents, settled in EURC/USDC —
**technology, not a token**. This release is produced by the
`.github/workflows/release.yml` flow (MAINNET-CHECKLIST §7.6).

## Artifacts

- `vtessera.flatpak` — GUI + metering daemon + agent-facing node server + MCP server
- `SHA256SUMS`

## Verify the download

```sh
sha256sum -c SHA256SUMS
```

Bundle digest: `{SHA256}`

## Reproducible escrow build (MAINNET-CHECKLIST §5)

- `vtessera_escrow.so` — SHA-256: `{ESCROW_SHA256}`
- Built twice in the pinned `solana-verify` 0.5.1 Docker image
  (`.github/workflows/reproducible-build.yml` and the `release.yml`
  `reproducible-build` gate); the job fails if the two builds differ.

## Release gates (MAINNET-CHECKLIST §7)

Before announcing this release, complete each gate and link the evidence:

- [ ] **VirusTotal pre-submission (§7.6)** — upload `vtessera.flatpak` at
      https://www.virustotal.com; link the scan result here.
- [ ] **Claims re-read (§7.7)** — re-read `docs/CONSENT.md` §3 (the claims
      table). Confirm the README framing paragraph, the GUI copy, and these
      notes overstate nothing (no "secure"/"decentralized"/"sandboxed"
      framings, exact fee numbers).
- [ ] **Program verify (§5.5, post-mainnet-deploy)** — run
      `solana-verify verify-from-repo https://github.com/douglasdemaio/vtessera \
      --program-id <MAINNET_PROGRAM_ID> --url mainnet-beta \
      --commit-hash <DEPLOYED_COMMIT> --library-name vtessera_escrow \
      --mount-path programs --bpf`
      and paste the output here.
