# KVM (Cloud Hypervisor) Backend — Status & How We Verify It

> Living note. Updated: 2026-09-19.
> **Bottom line: the KVM backend runs real microVMs and passes every automated
> test we have, but its *field* behavior is only partially proven.** Treat this
> page as the agreed way to check, next time, whether it is actually working.

## Why this note exists

`--backend cloud-hypervisor` boots each job in a disposable microVM. Getting it
working has been a lot of moving parts (host kernel, initramfs, virtiofsd,
PID/eviction, metering). Automated tests pass, but that is not the same as
"works properly under load, on other machines, on the next kernel." When
someone asks *"is KVM working?"* this is what we run and what counts as a pass.

## What is verified today (this machine, kernel 7.2.4)

- `/dev/kvm` present, `cloud-hypervisor` + `virtiofsd` installed.
- Guest initramfs builds deterministically (`scripts/build-initramfs.sh`,
  sha256 recorded in `scripts/initramfs.sha256`).
- **`crates/executor/tests/ch_cpu_integration.rs` — 6/6 pass**, each test boots
  a real microVM:
  - `ch_true_returns_completed` — job runs and completes
  - `ch_exit_3_returns_failed` — non-zero exit maps to `failed`
  - `ch_sleep_60_with_short_cap_times_out` — duration cap stops the VM
  - `ch_metering_sane_on_true` — metering file produced
  - `ch_env_visible_inside_guest` — env tuple actually lands in the guest
  - `ch_workdir_cleaned_after_run` — workdir cleaned after a run
- `scripts/kvm-node-demo.sh` runs the full node loop end-to-end:
  `setup` (once) → `run` (start node, submit echo / exit 3 / timeout jobs,
  jobs come back **accepted** with **signed receipts**; the three outcomes are
  `completed`, `failed`, `timed_out` as expected) → `stop`.
- Node pre-flight (`$ vtessera-x402-client --check` / `agent health`) passes
  against a running KVM node.

## What is NOT yet proven (open questions)

- **Metering accuracy.** Receipts for cloud-hypervisor jobs show
  `cpu_seconds: 0.0` and `peak_mem_kb: 0` (see
  `crates/executor/src/cloud_hypervisor.rs`, metering is guest self-reported
  from `out/metering.json`). `elapsed_secs` looks right; CPU/memory do not.
  **Until `cpu_seconds`/`peak_mem_kb` are non-zero and sane, treat metering as
  unverified and DO NOT rely on it for billing.**
- **Host-crash / kill -9 behavior**: does the node clean up orphan VMs (qemu/
  cloud-hypervisor still holding `/dev/kvm`, virtiofsd, workdir) after an
  unexpected node death? Not tested. (Normal `stop` is tested.)
- **Different machines/kernels**: initramfs was built for kernel 7.2.4 on this
  box. It must be rebuilt + the suite re-run on any other kernel (modules,
  virtiofs availability).
- **Concurrency > 1**: `--max-concurrent-jobs 1` is the only tested config;
  multiple VMs at once (memory, KVM slots, virtiofsd instances) is untested.
- **Non-busybox images**: only `busybox` commands have been exercised; image
  pull inside the guest is untested.
- **GPU passthrough path**: separate plan/spec exist; not exercised here.

## How to (re)verify it later — the recipe

```bash
cd ~/vtessera

# 1. Host prerequisites
ls -l /dev/kvm                          # must exist
which cloud-hypervisor virtiofsd busybox-static
uname -r                                # record the kernel; initramfs is per-kernel

# 2. Build/refresh the guest initramfs (needs root once per kernel)
sudo ./scripts/kvm-node-demo.sh setup

# 3. Automated suite — the ground truth. Every test boots a real microVM.
VTESSERA_CH_INTEGRATION=1 \
CH_INITRAMFS=/var/lib/vtessera/initramfs.cpio.gz \
cargo test -p vtessera-executor --features cloud-hypervisor \
  --test ch_cpu_integration -- --nocapture --test-threads=1
# PASS = "test result: ok. 6 passed"

# 4. Full node loop (echo / exit 3 / timeout, signed receipts)
./scripts/kvm-node-demo.sh run
# PASS = all three jobs: status "accepted", receipt "signed",
#        metering exit_status completed/failed/timed_out as expected

# 5. Report the receipts + the metering blob from one job.
cat ~/.local/share/vtessera/kvm-demo/job-receipts/*.json | python3 -m json.tool
# If cpu_seconds / peak_mem_kb are still 0 -> metering still unverified.

# 6. Cleanup
./scripts/kvm-node-demo.sh stop
```

## Decision rule

| Evidence | Verdict |
|---|---|
| Suite + demo pass, receipts signed | Backend *works* (isolation + execution) |
| Same as above **and** sane non-zero metering | Backend works *and* is billing-ready |
| Any test fails, or VMs leak after kill -9 | Do **not** ship/use; file an issue with the failing step + `uname -r` |

If a future change touches `crates/executor/src/cloud_hypervisor.rs`,
`scripts/build-initramfs.sh`, the initramfs contents, or host kernel modules,
re-run steps 3 and 4 before considering the backend healthy.