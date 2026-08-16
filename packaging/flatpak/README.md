# Vtessera — Flatpak packaging

This directory contains everything needed to build `Vtessera` as a Flatpak:
a GTK4 desktop app that turns a GNU/Linux machine into a Vtessera compute
node (metering daemon + agent-facing node server) for AI agents.

## Layout

```
packaging/flatpak/
├── io.github.douglasdemaio.Vtessera.json   # flatpak-builder manifest
├── io.github.douglasdemaio.Vtessera.metainfo.xml
├── io.github.douglasdemaio.Vtessera.desktop
├── cargo-sources.json                      # generated: crates.io archives for offline builds
├── icons/<size>x<size>/                    # generated: hicolor PNG icons (from logo.png)
├── README.md
└── .gitignore
```

The manifest builds three binaries and installs them to `/app/bin`:

- `vtessera-gui` — the GTK4 front-end (the app users launch)
- `vtesserad` — the v0 metering daemon (spawned by the GUI)
- `vtessera-node` — the agent-facing HTTP server (spawned by the GUI)

## Build

Install `flatpak-builder` (or the `org.flatpak.Builder` Flatpak), plus the
`org.freedesktop.Sdk.Extension.rust-stable` SDK extension for the current
GNOME runtime. Then:

```sh
flatpak-builder --user --install \
    --repo=repo \
    build \
    io.github.douglasdemaio.Vtessera.json

flatpak build-bundle repo vtessera.flatpak io.github.douglasdemaio.Vtessera
```

With `org.flatpak.Builder`:

```sh
flatpak run org.flatpak.Builder --user --install --force-clean \
    --default-branch=stable \
    --repo=repo \
    build \
    io.github.douglasdemaio.Vtessera.json
```

The build is fully offline for crates: `cargo-sources.json` vendors every
dependency and the manifest builds with `cargo --offline --locked`. The
host repo's `rust-toolchain.toml` (which pins a musl target) is removed
before building so the rust-stable SDK toolchain is used. Always export to
the `stable` branch (`--default-branch=stable`) — that is the branch
FlatHub consumes.

## Regenerating cargo-sources.json

After adding or bumping a dependency, regenerate from the workspace lock:

```sh
flatpak-cargo-generator ../../Cargo.lock -o cargo-sources.json
```

## Regenerating the icons

Icons are resized from the repo's `logo.png` into hicolor sizes:

```sh
for s in 16 22 24 32 36 48 64 72 96 128 192 256 512; do
    mkdir -p icons/${s}x${s}
    magick ../../logo.png -resize ${s}x${s} \
        icons/${s}x${s}/io.github.douglasdemaio.Vtessera.png
done
```

## Notes

- The app only ever writes inside the Flatpak's own dirs
  (`~/.var/app/io.github.douglasdemaio.Vtessera/`); no `--filesystem`
  overrides are needed.
- v0 settlement is off-chain; the Solana escrow (module 4) settles
  sellers directly in the same stablecoin the buyer paid (EURC/USDC).
  See the repo `ROADMAP.md`.

## Permission rationale (docs/CONSENT.md §4)

JSON manifests cannot hold comments, so each `finish-arg` is justified here.
The set is the minimum for a GTK4 GUI that can also run the node server:

| finish-arg | Why it's there | What it does NOT grant |
| --- | --- | --- |
| `--share=network` | `vtessera-node` must accept connections from agents on the configured port (the advertised endpoint has to be reachable). Also covers `vtesserad`'s optional `submit` feature (off by default). | No host-filesystem access; the network grants are sandbox-scoped. |
| `--socket=wayland` | The GUI's display connection (native). | Nothing else — no portals beyond the defaults. |
| `--socket=fallback-x11` | Display fallback for X11-only sessions. | |
| `--share=ipc` | Required for X11 shared memory (XShm) when running on the fallback X11 socket. | |
| `--device=dri` | Hardware-accelerated rendering of the GUI itself (OpenGL compositing). | It does not grant host GPU *compute* to jobs — the executor runs outside the Flatpak's GPU scope in v0. |

Deliberately **not** granted: `--filesystem=home` or any `--filesystem`
override (state stays inside the app dir), `--socket=session-bus` /
`--system-talk-name` (no system service access), and no `--talk-name`
for arbitrary D-Bus targets.

## Signed releases (MAINNET-CHECKLIST §7.6)

Every release is produced by `.github/workflows/release.yml` (manual
dispatch or a `v*` tag push). It builds this Flatpak, computes the
SHA-256 digest, and drafts a GitHub release with:

- the `vtessera.flatpak` bundle and a `SHA256SUMS` file
- a release-notes template that carries the digest, a VirusTotal
  pre-submission checklist (upload the bundle at https://www.virustotal.com
  before announcing; link the result), and the `docs/CONSENT.md` §3
  claims re-read (MAINNET-CHECKLIST §7.7)

The digest in the release notes lets a user verify the artifact they
downloaded:

```sh
sha256sum -c SHA256SUMS   # compare against the digest in the release notes
```

## Before a FlatHub submission

1. `screenshots/vtessera-settings.png` is committed, so the metainfo
   screenshot URL
   (`https://raw.githubusercontent.com/douglasdemaio/vtessera/main/packaging/flatpak/screenshots/vtessera-settings.png`)
   resolves. (Before it was pushed, `flatpak-builder-lint repo` reported
   `appstream-missing-screenshots` /
   `appstream-screenshots-not-mirrored-in-ostree`, and `appstreamcli
   validate` reported `screenshot-image-not-found`. These clear
   automatically once the URL is reachable.)
2. Re-run the lint checks below and fix anything new.
3. Optionally add more screenshots (e.g. the Status view) to
   `screenshots/` and reference them in the metainfo.

Validation commands (run inside the `org.flatpak.Builder` / `org.gnome.Sdk`
sandboxes):

```sh
flatpak-builder-lint manifest io.github.douglasdemaio.Vtessera.json
flatpak-builder-lint repo repo
appstreamcli validate io.github.douglasdemaio.Vtessera.metainfo.xml
```

Current status: `finish-args` (ipc/x11), branch naming, metainfo schema and
Cargo vendoring all validate cleanly; the screenshot URL resolves; releases
carry SHA-256 digests via `.github/workflows/release.yml`.
