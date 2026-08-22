---
name: checks
description: Run the full CI-equivalent check suite for the vtessera workspace (fmt, clippy -D warnings, test --locked, cargo audit, cargo deny) exactly as .github/workflows/ci.yml does. Use this BEFORE every commit or PR, whenever the user says "run the checks", "does CI pass", "clean up warnings", or after any Rust change — CI enforces -D warnings, so a single unused variable fails the build.
---

# Vtessera check suite

Run these from the repo root, in this order (cheap → expensive), and fix
failures before moving on. CI (`.github/workflows/ci.yml`) runs the same
sequence and treats every warning as an error.

```bash
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --locked
cargo audit
cargo deny check
```

## Things that bite

- **`-D warnings` is enforced.** Unused fields on deserialized structs are a
  recurring clippy failure — prefix them with `_` rather than adding
  `#[allow]` (see commit 5a3183e for the established pattern).
- **The workspace excludes four trees** that pin the Solana 1.18 SDK and
  build with their own `Cargo.lock`: `programs/vtessera-escrow`,
  `crates/devnet-demo`, `crates/x402-client`, `tests/adversarial`.
  A root `cargo build`/`clippy`/`test` does NOT cover them. If you touched
  one, run the same checks inside that directory.
- **`crates/vtesserad` has a hard dependency budget** (BUILD.md §1.3):
  serde, toml, ed25519-dalek, sha2, rand, hex — nothing else in the default
  build. Never add a dependency to vtesserad without flagging it to the user.
- **The v0 no-socket invariant** is pinned by `tests/no_socket.rs`; if it
  fails, something added network capability to the default build — that is
  a design violation, not a test to update.
- Static release build, when needed:
  `cargo build --release --locked --target x86_64-unknown-linux-musl`
  (toolchain is pinned by `rust-toolchain.toml`; don't bump it casually).
