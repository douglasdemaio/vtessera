---
name: flatpak-build
description: Build, refresh, and smoke-test the Vtessera Flatpak (packaging/flatpak). Use whenever the user mentions the Flatpak, vtessera-gui packaging, cargo-sources.json, the agent smoke test, or Flathub — and ALWAYS after any dependency change to Cargo.lock, because the vendored cargo-sources.json must be regenerated or the offline Flatpak build silently uses stale crates.
---

# Vtessera Flatpak build

Everything lives in `packaging/flatpak/`. App ID:
`io.github.douglasdemaio.Vtessera`. The manifest builds and installs four
binaries to `/app/bin`: `vtessera-gui`, `vtesserad`, `vtessera-node`,
`vtessera-mcp`.

## After any Cargo.lock change — regenerate vendored sources first

The Flatpak build is fully offline (`cargo --offline --locked`); it consumes
`cargo-sources.json`, not crates.io. A stale file means the build either
fails or ships old code. Regenerate from `packaging/flatpak/`:

```bash
flatpak-cargo-generator ../../Cargo.lock -o cargo-sources.json
```

Commit the regenerated file together with the Cargo.lock change (pattern:
commit 7471660).

## Build + bundle

From `packaging/flatpak/`:

```bash
flatpak-builder --user --install --force-clean \
    --repo=repo build io.github.douglasdemaio.Vtessera.json

flatpak build-bundle repo vtessera.flatpak io.github.douglasdemaio.Vtessera
```

- Always export with `--default-branch=stable` when using
  `org.flatpak.Builder` — Flathub consumes the `stable` branch.
- The manifest removes the host `rust-toolchain.toml` before building so the
  rust-stable SDK extension's toolchain is used (the host pin targets musl).
- Build artifacts (`build/`, `builddir/`, `repo/`, `.flatpak-builder/`,
  `vtessera.flatpak`) are gitignored — never commit them.

## Smoke test

With the app running (GUI with "Accept workloads" on, mode "free", node on
127.0.0.1:8402):

```bash
./scripts/agent-smoke-test.sh          # 7 checks: healthz, offer, free job, receipt...
```

Override the target with `VTESSERA_NODE_URL` if the node runs elsewhere.

## Icons

Regenerated from the repo's `logo.png` with ImageMagick into
`icons/<s>x<s>/` — see `packaging/flatpak/README.md` for the loop. Only
needed when the logo changes.
