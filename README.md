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

Prerequisites: Rust ≥ 1.80 with the `x86_64-unknown-linux-musl` target.

```bash
rustup target add x86_64-unknown-linux-musl
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
cp packaging/vtessera.toml.example /etc/vtessera/vtessera.toml
# Edit payout_id in the config
```

2. Run once to verify:

```bash
./vtesserad --config /etc/vtessera/vtessera.toml --once
```

3. Run as a daemon:

```bash
cp packaging/vtesserad.service /etc/systemd/system/
systemctl daemon-reload
systemctl start vtesserad
systemctl status vtesserad
```

## Verify a receipt

Signed receipts are written to the state directory (default `/var/lib/vtessera/`).
Each is a JSON file containing the receipt, public key, and Ed25519 signature.

To verify a receipt:

```bash
cargo test --locked
```

Or use the `verify` function from the `sign` module programmatically.

## Config

See `packaging/vtessera.toml.example` for all options.

## Repository layout

```
vtessera/
├── src/
│   ├── main.rs          # Entry point, args, run loop
│   ├── config.rs        # TOML config loading and validation
│   ├── metrics.rs       # /proc resource sampling
│   ├── receipt.rs       # Receipt struct + canonical serialization
│   ├── sign.rs          # Ed25519 key loading, signing, verification
│   ├── spool.rs         # Atomic receipt writes to state dir
│   └── submit.rs        # Optional outbound POST (feature=submit)
├── packaging/
│   ├── vtessera.spec    # RPM spec
│   ├── vtesserad.service # Hardened systemd unit
│   ├── vtessera.apparmor # AppArmor profile
│   └── vtessera.toml.example
├── docs/                # Design documents
├── Cargo.toml
├── rust-toolchain.toml
└── deny.toml
```

## Design

See `docs/` for architecture, payments, security, and token design documents.
The authoritative build specification is `BUILD.md`.

## License

Apache-2.0
