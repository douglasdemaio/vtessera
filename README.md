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

## Build

Prerequisites: `rustup` and a musl cross-compiler (`musl-tools` on
Debian/Ubuntu; equivalent on RPM-based distros). The Rust toolchain
version and the `x86_64-unknown-linux-musl` target are pinned by
`rust-toolchain.toml` and installed automatically on first `cargo`
invocation.

```bash
cargo build --release --locked --target x86_64-unknown-linux-musl
```

The static binary is at `target/x86_64-unknown-linux-musl/release/vtesserad`.

### All checks

```bash
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --locked
cargo audit
cargo deny check
```

## Quickstart

1. Copy the example config:

```bash
mkdir -p /etc/vtessera
cd packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
# Edit payout_id in the config
```

2. Run once to verify:

```bash
./vtesserad --config /etc/vtessera/vtessera.toml --once
```

3. Install and build:

```bash
sudo apt-get (or zypper) install rustup
rustup target add x86_64-unknown-linux-musl
cargo build --release --locked --target x86_64-unknown-linux-musl
```

4. Run as a daemon:

```bash
cp packaging/vtesserad.service /etc/systemd/system/
systemctl daemon-reload
systemctl start vtesserad
systemctl status vtesserad
```

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

See `packaging/vtessera.toml.example` for all options.

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
