# BUILD.md — Agent Build Specification (Vtessera, v0)

> Audience: an AI coding agent building this repository on GitHub from scratch.
> Goal: the **smallest possible** auditable Rust daemon, packaged as an RPM.
> The name `vtessera`/`vtesserad` is a placeholder — if changed, replace it
> everywhere. Anything not in "v0 scope" is OUT of scope; do not build it.

---

## 0. What you are building (v0 scope)

A single Rust binary, `vtesserad`, that:

1. Loads a TOML config and validates it.
2. Periodically samples local resource usage by reading `/proc` (read-only).
3. Emits **signed usage receipts** (Ed25519) into a state directory.

That is all. It has **no inbound network listener**, runs **unprivileged**, and
does **not** execute third-party workloads. Packaged as an RPM with a hardened
systemd unit and an AppArmor profile.

### Explicitly OUT of scope for v0 (do NOT build)
- Workload/job execution or isolation (Kata/Firecracker) — later module.
- The Solana token / on-chain program — minted later with standard tooling.
- The settlement enclave (TEE) — later module.
- Any HTTP server, RPC, web UI, or dashboard.
- Any payment, swap, or oracle logic.

---

## 1. Hard rules (non-negotiable)

1. **Language:** Rust only. No npm, Node, Python, Go, or shell logic beyond
   trivial packaging glue. Build toolchain = cargo + rpmbuild only.
2. **No unsafe:** the crate root must contain `#![forbid(unsafe_code)]`.
3. **Dependency budget:** the default (no-network) build uses exactly these
   direct crates and nothing else (transitive deps don't count toward the
   budget — `ed25519-dalek` legitimately pulls in `curve25519-dalek`,
   `zeroize`, etc.):

   | Crate | Why |
   | --- | --- |
   | `serde` (derive) | Config deserialization. |
   | `toml` | Config format. |
   | `ed25519-dalek` | Signing. |
   | `sha2` | `samples_digest` and `node_id` derivation. |
   | `rand` | Ed25519 keygen RNG (`OsRng`). |
   | `hex` | Receipt JSON encodes pubkey/sig/digest in hex. |

   Any addition must be justified in the PR and pass rule 4. Do **not** add
   `tokio`, `reqwest`, `clap`, `chrono`, `sysinfo`, or `libc` to the default
   build.
4. **Supply chain:** commit `Cargo.lock`; `cargo deny check` and `cargo audit`
   must pass in CI (config in `deny.toml`). Deny known vulnerabilities,
   duplicate versions, and non-allowlisted licenses.
5. **No inbound network in v0.** Outbound submission is a non-default Cargo
   feature `submit` (off by default); when off, the binary opens no sockets.
6. **Least privilege:** runs as an unprivileged (DynamicUser) service,
   read-only system access, no setuid, empty capability set.
7. **Static binary:** target `x86_64-unknown-linux-musl`; one self-contained
   artifact, no runtime library deps.
8. **Reproducible:** pin the Rust toolchain (`rust-toolchain.toml`), build with
   `--locked`. Bit-for-bit reproducibility additionally requires
   `SOURCE_DATE_EPOCH` set, `RUSTFLAGS="--remap-path-prefix=..."` to strip
   absolute build paths from debuginfo, and a pinned musl sysroot — see §6.
   "Reproducible" without those preconditions means deterministic given the
   same toolchain image, lockfile, and source tree.
9. **Privacy:** receipts contain billing-necessary metering only. No other
   telemetry. No phoning home in the default build.
10. **Small, reviewable modules:** one responsibility per file; document each
    public item.

---

## 2. Toolchain & system packages

- Rust toolchain pinned via `rust-toolchain.toml` to a **specific minor**
  (currently `1.96.0`), with the `x86_64-unknown-linux-musl` target added.
  `channel = "stable"` is not acceptable — it floats and breaks rule 1.8.
  Bumping the pin is an explicit PR.
- `cargo-deny` and `cargo-audit` (installed in CI via `cargo install --locked`).
- `rpmbuild` (package `rpm-build`) for local packaging; OBS for signed releases.
- `musl-tools` / musl target support for the static build.
- Nothing else. No JS/Python ecosystems.

---

## 3. Repository structure (create exactly this)

```
vtessera/
├── README.md                  # what it is + build/run quickstart
├── LICENSE                    # Apache-2.0 (recommended) or MIT
├── rust-toolchain.toml        # pin stable + musl target
├── Cargo.toml                 # crate metadata; [features] submit = [...]
├── Cargo.lock                 # COMMITTED
├── deny.toml                  # cargo-deny policy (licenses, advisories, bans)
├── .github/
│   └── workflows/
│       └── ci.yml             # fmt, clippy, test, audit, deny, build, rpm
├── src/
│   ├── main.rs                # #![forbid(unsafe_code)]; args, config, run loop
│   ├── config.rs              # load + validate TOML; typed Config struct
│   ├── metrics.rs             # read /proc, /sys; ResourceSample struct
│   ├── receipt.rs             # Receipt struct + canonical serialization
│   ├── sign.rs                # Ed25519 keygen/load + sign(receipt) -> sig
│   ├── spool.rs               # write signed receipt JSON to state dir
│   └── submit.rs              # feature="submit" ONLY: outbound POST (ureq+rustls)
├── packaging/
│   ├── vtessera.spec           # RPM spec (BuildRequires: rust, cargo)
│   ├── vtesserad.service       # hardened systemd unit (see §5)
│   ├── vtessera.apparmor       # AppArmor profile
│   └── vtessera.toml.example    # documented example config
└── docs/
    └── DESIGN.md              # link back to TOKEN-DESIGN.md / SECURITY.md
```

---

## 4. Module contracts

- **config.rs** — `Config { sample_interval_secs: u64, state_dir: PathBuf,
  key_path: PathBuf, resource_caps: Caps, payout_id: String }`. Reject unknown
  fields (`#[serde(deny_unknown_fields)]`), validate ranges, return typed errors.

  `payout_id` is validated as a **Solana base58 Ed25519 address: 32–44 chars
  from the Bitcoin/Solana base58 alphabet** (charset + length check). Full
  decode and on-curve verification are the settlement enclave's job.

- **metrics.rs** — read `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`,
  filesystem stats. Produce `ResourceSample { ts_unix, cpu_pct, mem_used_kb,
  disk_free_kb }`. No external crate; parse text directly. Never write.

  **Clock source:** `ts_unix` is wall-clock (`SystemTime::now()` =
  `CLOCK_REALTIME`). It is subject to NTP corrections including backward
  jumps. The window-finalize check uses `saturating_sub` so a backward NTP
  step never closes a window with `window_end < window_start`; the window
  simply doesn't close until forward progress resumes. `ProtectClock=yes`
  in the unit prevents the daemon from setting the clock.

- **receipt.rs** — `Receipt { schema_ver, node_id, payout_id, window_start,
  window_end, samples_digest, totals }`. `samples_digest` = SHA-256 over the
  window's samples (each sample serialized as `ts_unix(u64) || cpu_pct(f64) ||
  mem_used_kb(u64) || disk_free_kb(u64)`, all little-endian).

  **`node_id`** is **self-attesting**: hex of `SHA-256(pubkey)[..16]` (32
  lowercase hex chars). A verifier with the signed receipt's pubkey can
  recompute it. It is *not* the payout address — keep them separate so an
  operator can rotate `payout_id` without breaking historical identity.

  **Canonical bytes** for signing (all little-endian):

  ```
  schema_ver           u16
  node_id_len          u16
  node_id              utf-8 bytes
  payout_id_len        u16
  payout_id            utf-8 bytes
  window_start         u64
  window_end           u64
  samples_digest       [u8; 32]
  totals.cpu_pct_avg   f64
  totals.mem_used_kb_avg   u64
  totals.disk_free_kb_avg  u64
  totals.sample_count  u32
  ```

  The length prefixes are mandatory — they disambiguate the boundary between
  `node_id` and `payout_id`. Any change to this layout bumps `schema_ver`.
  Reviewer note: `cpu_pct_avg` is currently `f64` and IEEE 754 corner cases
  (NaN, ±0.0) would produce different signatures; in practice the value is
  bounded `[0.0, 100.0]` so this is not exploitable, but if you ever feed
  user-supplied floats here, switch to a fixed-point u32.

- **sign.rs** — load or generate an Ed25519 keypair at `key_path` (mode 0600).
  On load, the daemon **refuses to start** if the key file's mode allows any
  group or world access (`mode & 0o077 != 0`).
  `sign(&Receipt) -> SignedReceipt { receipt, pubkey, sig }`. Key never leaves
  the process; never logged.

- **spool.rs** — write `SignedReceipt` as JSON to `state_dir` with an atomic
  write (temp + rename). Filenames sortable by time. No deletion logic in v0
  — operators are responsible for archiving/rotating the spool; see §9 for
  the eventual rotation module.

- **submit.rs** — compiled only under `--features submit`. Single outbound
  HTTPS POST via `ureq` with `rustls` (no OpenSSL). Endpoint from config. Must
  be absent from the default build's dependency graph.

- **main.rs** — parse args by hand (no clap):

  | Arg | Behavior |
  | --- | --- |
  | `--config <path>` | Required. Path to TOML config. |
  | `--once` | Sample once and exit (does not finalize a window). |
  | `--version` | Print `vtesserad <version>` to stdout, exit 0. |
  | `-h`, `--help` | Print usage to stdout, exit 0. |
  | (unknown) | Print error to stderr, exit 2. |

  Exit codes: `0` = success / `--help` / `--version`; `1` = runtime error
  (config invalid, key error, IO); `2` = argument parsing error.

  Load config, init signer, derive `node_id` from the signing key's pubkey,
  loop: sample → on window boundary, build + sign + spool a receipt → sleep.
  Handle SIGTERM cleanly.

---

## 5. systemd unit (`vtesserad.service`) — required hardening

```ini
[Unit]
Description=Vtessera metering daemon
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/vtesserad --config /etc/vtessera/vtessera.toml
DynamicUser=yes
StateDirectory=vtessera
StateDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged ~@resources
CapabilityBoundingSet=
AmbientCapabilities=
# Default build opens no sockets:
RestrictAddressFamilies=AF_UNIX
IPAddressDeny=any
# If built with --features submit, set instead:
#   RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
#   IPAddressDeny=any
#   IPAddressAllow=<your submission endpoint CIDR>
#
# Note: DO NOT set ProcSubset=pid — the daemon reads /proc/meminfo and
# /proc/stat, which that option hides.
#
# Note: DynamicUser=yes randomizes the runtime username. AppArmor profiles
# pathed by username will not match; use peer/object rules.
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Target: `systemd-analyze security vtesserad.service` should report a low
exposure score (aim ≤ 2.0, "OK"/"GOOD").

---

## 6. Build & package commands

```bash
# build (static, locked, deterministic when env vars are set)
rustup target add x86_64-unknown-linux-musl
export SOURCE_DATE_EPOCH=$(git log -1 --pretty=%ct)
export RUSTFLAGS="--remap-path-prefix=$PWD=/build --remap-path-prefix=$HOME/.cargo=/cargo"
cargo build --release --locked --target x86_64-unknown-linux-musl

# checks (must all pass)
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test --locked
cargo audit
cargo deny check

# package (local)
rpmbuild -bb packaging/vtessera.spec   # or build via an OBS instance for signed releases
```

The RPM installs: the binary to `/usr/bin/vtesserad`, the unit to
`/usr/lib/systemd/system/`, the AppArmor profile, and
`vtessera.toml.example` to `/etc/vtessera/`.

---

## 7. CI (`.github/workflows/ci.yml`)

One workflow, Rust toolchain only. Jobs/steps: checkout → install pinned
toolchain + musl target → `fmt --check` → `clippy -D warnings` → `test --locked`
→ `cargo audit` → `cargo deny check` → build static release → build RPM →
upload the RPM as an artifact. No other language runtimes in the workflow.

---

## 8. Definition of done (acceptance criteria)

- [ ] `#![forbid(unsafe_code)]` present; `cargo build` clean with no warnings.
- [ ] Default build's direct dependencies match the table in §1.3 exactly;
      `submit` deps (`ureq`) absent unless the feature is enabled.
- [ ] `cargo deny check` and `cargo audit` pass.
- [ ] Static musl binary runs, samples `/proc`, and writes signed receipts that
      verify against the embedded public key.
- [ ] No open sockets in the default build (verify with `ss -lntup`).
- [ ] `systemd-analyze security vtesserad.service` ≤ 2.0.
- [ ] RPM installs cleanly on a current RPM-based GNU/Linux distribution; service starts as DynamicUser.
- [ ] Reproducible: two clean builds **with the same `SOURCE_DATE_EPOCH`,
      `RUSTFLAGS`, toolchain image, and sysroot** produce the same binary
      SHA-256.
- [ ] README documents config, build, install, and how to verify a receipt.

---

## 9. Later modules (separate repos/crates; not now)

Job isolation (Kata/Firecracker) · settlement enclave (SEV-SNP/TDX) · Solana
token + EURC/BTC reserve plumbing per `TOKEN-DESIGN.md`. Each is added behind
its own crate and review; none expand the v0 daemon's attack surface.
