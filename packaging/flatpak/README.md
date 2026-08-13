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
- `--share=network` is required so `vtessera-node` can serve agents on the
  configured port (the advertised endpoint must be reachable from the
  internet for agents to connect).
- v0 settlement is off-chain; the Solana escrow (module 4) and HNT swap
  layers are under development. See the repo `ROADMAP.md`.

## Before a FlatHub submission

1. Push `screenshots/vtessera-settings.png` to the repo so the metainfo
   screenshot URL
   (`https://raw.githubusercontent.com/douglasdemaio/vtessera/main/packaging/flatpak/screenshots/vtessera-settings.png`)
   resolves. Until then, `flatpak-builder-lint repo` reports
   `appstream-missing-screenshots` /
   `appstream-screenshots-not-mirrored-in-ostree`, and `appstreamcli
   validate` reports `screenshot-image-not-found`. These all clear
   automatically once the URL is reachable — nothing else needs changing.
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
Cargo vendoring all validate cleanly.
