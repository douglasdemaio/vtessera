# Vtessera

An **opt-in compute marketplace client for GNU/Linux operating systems**, shipped as a signed RPM.

A machine owner who wants to rent out spare compute installs `vtesserad`,
sets a price, and earns when AI agents (or anyone) run workloads on their box.
The marketplace settles payments per job and meters real usage.

> **This is not a paywall on distributions.** The distro, its mirrors, and its
> packages stay free. This is an *optional* package that only people who
> choose to sell compute install.

## v0 — metering daemon

The v0 binary (`vtesserad`) samples local resource usage from `/proc`,
produces **signed usage receipts** (Ed25519), and writes them to a state
directory. No inbound network listener. Runs unprivileged (DynamicUser).

### What v0 does NOT do (later phases)

- Execute third-party workloads (Kata/Firecracker)
- Settle payments or interact with any blockchain
- Listen on any port

## Prerequisites

You need a Rust toolchain. The Rust version and (optionally) the musl
target are pinned by `rust-toolchain.toml` and installed automatically
on first `cargo` invocation.

Install `rustup` if you don't have it:

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
. "$HOME/.cargo/env"
```

For the static / RPM build path you also need musl. Package names:

| Distro | Command |
| --- | --- |
| openSUSE Tumbleweed / Leap | `sudo zypper install musl musl-devel` |
| Fedora / RHEL              | `sudo dnf install musl-gcc`           |
| Debian / Ubuntu            | `sudo apt install musl-tools`         |

You can skip musl entirely if you only want a local glibc build for
testing — see "Build (quick, glibc)" below.

## Build

### Build (quick, glibc) — for local testing

```bash
cargo build --release
```

Binary lands at `target/release/vtesserad`.

### Build (static musl) — for production / RPM

```bash
cargo build --release --locked --target x86_64-unknown-linux-musl
```

Binary lands at `target/x86_64-unknown-linux-musl/release/vtesserad`.
This is the artifact CI publishes and what the RPM ships.

### All checks

```bash
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --locked
cargo audit
cargo deny check
```

## Quickstart — smoke test (no systemd)

The fastest way to confirm the daemon works on your box:

```bash
# 1. Build
cargo build --release

# 2. Drop a config into place
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
# Edit payout_id to your own Solana base58 address (or keep the example
# placeholder for testing — but replace it before production).
sudo "${EDITOR:-vi}" /etc/vtessera/vtessera.toml

# 3. Run once. This generates /etc/vtessera/identity.key on first run
#    and writes one sample, then exits.
sudo ./target/release/vtesserad --config /etc/vtessera/vtessera.toml --once
```

On success you'll see `vtesserad started: ...` on stderr. `--once` exits
before finalizing a window, so no receipt is written yet — that's
expected. To see a receipt land, drop `--once` and let it run for at
least `window_size` seconds (default 60), then `Ctrl-C`:

```bash
sudo ./target/release/vtesserad --config /etc/vtessera/vtessera.toml
# wait ~60s, then Ctrl-C
sudo ls /var/lib/vtessera/   # JSON receipts appear here
```

## Install as a systemd service

The shipped unit is hardened (DynamicUser, ProtectSystem=strict, no
ambient capabilities). That hardening has one consequence worth
calling out: at runtime `/etc/vtessera` is **read-only**, so the daemon
cannot auto-generate the identity key from inside the service. You
need to bootstrap the key once before starting the service.

```bash
# 1. Install the binary where the unit expects it
sudo install -m 0755 target/release/vtesserad /usr/bin/vtesserad
#   (or substitute target/x86_64-unknown-linux-musl/release/vtesserad
#    if you built with musl)

# 2. Install the config (if you haven't already)
sudo mkdir -p /etc/vtessera
sudo cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
sudo "${EDITOR:-vi}" /etc/vtessera/vtessera.toml   # set payout_id

# 3. Bootstrap the identity key — run the daemon once as root so it
#    can write /etc/vtessera/identity.key while /etc is still writable.
sudo /usr/bin/vtesserad --config /etc/vtessera/vtessera.toml --once
sudo ls -l /etc/vtessera/identity.key   # should exist, mode 0600

# 4. Install and start the service
sudo cp packaging/vtesserad.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start vtesserad
sudo systemctl status vtesserad
```

Receipts will land under `/var/lib/vtessera/` (systemd's DynamicUser
symlinks this to `/var/lib/private/vtessera/` — both paths work).

Watch live logs:

```bash
sudo journalctl -u vtesserad -f
```

## Troubleshooting

**`status=203/EXEC` / `Unable to locate executable '/usr/bin/vtesserad'`**
The binary isn't installed yet. Run step 1 above (`install -m 0755 ... /usr/bin/vtesserad`).

**`error: failed to load/generate key: Read-only file system (os error 30)`**
The identity key doesn't exist yet and the hardened unit can't create
it because `/etc` is read-only inside the service sandbox. Run step 3
above to bootstrap the key once outside systemd.

**`System call ~@resources is not known, ignoring.`**
Harmless warning on older systemd. The seccomp filter just drops that
group; the daemon still starts.

**Service stuck in restart loop**
`sudo systemctl reset-failed vtesserad` clears the rate-limit, then
check `journalctl -u vtesserad -e --no-pager -o cat` for the real
error.

## Receipt format

Signed receipts are written to the state directory (default
`/var/lib/vtessera/`). Each is a JSON file containing the receipt, the
operator's Ed25519 public key, and the signature over the canonical
receipt bytes defined in `BUILD.md` §4.

There is no CLI verify subcommand in v0 — verification is library-only.
Downstream tools and the future settlement enclave verify receipts by
calling `sign::verify` against the canonical-byte layout. A standalone
verifier tool is a later module (see `BUILD.md` §9).

## Config

See `packaging/vtessera.toml.example` for all options. Required
fields: `sample_interval_secs`, `state_dir`, `key_path`, `payout_id`.

`payout_id` must be a Solana base58 Ed25519 address (32–44 chars from
the base58 alphabet). The daemon refuses to start with an empty or
malformed value.

## Repository layout

```
vtessera/
├── README.md                    # this file
├── BUILD.md                     # authoritative v0 build specification
├── TOKEN-DESIGN.md              # VTESS token model (voted multi-asset reserve)
├── LICENSE                      # Apache-2.0
├── Cargo.toml / Cargo.lock
├── rust-toolchain.toml          # pinned Rust toolchain + musl target
├── deny.toml                    # cargo-deny policy (schema v2)
├── src/
│   ├── main.rs                  # entry point, args, run loop
│   ├── config.rs                # TOML config loading and validation
│   ├── metrics.rs               # /proc resource sampling
│   ├── receipt.rs               # Receipt struct + canonical serialization
│   ├── sign.rs                  # Ed25519 key loading, signing, verification
│   ├── spool.rs                 # atomic receipt writes to state dir
│   └── submit.rs                # optional outbound POST (feature=submit)
├── packaging/
│   ├── vtessera.spec            # RPM spec (consumed by OBS / local rpmbuild)
│   ├── vtesserad.service        # hardened systemd unit
│   ├── vtessera.apparmor        # AppArmor profile
│   └── vtessera.toml.example    # documented example config
├── docs/
│   └── DESIGN.md                # index pointing at BUILD.md / TOKEN-DESIGN.md
└── .github/workflows/ci.yml
```

## Design

- **`BUILD.md`** — authoritative v0 build specification (scope, hard
  rules, module contracts, systemd hardening, CI, definition of done).
- **`TOKEN-DESIGN.md`** — VTESS token model (fixed supply, voted
  multi-asset reserve over {SOL, BTC, EURC, USDC}, EURC stability floor,
  biannual holder vote). Token plumbing is a later phase than the v0
  daemon; the daemon does not depend on it.

The settlement enclave, job isolation, and on-chain token deployment are
later modules (see `BUILD.md` §9).

## License

Apache-2.0
