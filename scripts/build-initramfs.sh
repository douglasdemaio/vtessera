#!/usr/bin/env bash
# Build the Cloud Hypervisor CPU executor's guest initramfs (ROADMAP.md §1,
# spec docs/superpowers/specs/2026-08-16-cloud-hypervisor-cpu-executor-design.md).
#
# The initramfs is the guest's root filesystem: busybox (static) + the job-runner
# agent (/init) + the virtio-fs kernel modules the guest needs to mount the job
# share. The guest boots from the host's kernel image but runs entirely in RAM.
#
# Deterministic: same inputs -> same bytes. mtimes are zeroed, the cpio listing
# is sorted, and gzip -n drops the archive's filename timestamp. The result's
# SHA-256 is recorded to scripts/initramfs.sha256 so a change is a reviewed
# artifact.
#
# Env overrides:
#   VTESSERA_OUT      output .cpio.gz path (default /var/lib/vtessera/initramfs.cpio.gz)
#   VTESSERA_BUSYBOX  path to a static busybox (default /usr/bin/busybox-static)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${VTESSERA_OUT:-/var/lib/vtessera/initramfs.cpio.gz}"
BUSYBOX="${VTESSERA_BUSYBOX:-/usr/bin/busybox-static}"
KVER="$(uname -r)"
MODULES_DIR="/usr/lib/modules/${KVER}/kernel/fs/fuse"
VIRTIO_NET_DIR="/usr/lib/modules/${KVER}/kernel/drivers/net"

if [ ! -x "$BUSYBOX" ]; then
    echo "error: static busybox not found at $BUSYBOX (install busybox-static)" >&2
    exit 1
fi
if [ ! -f "$MODULES_DIR/virtiofs.ko.zst" ] || [ ! -f "$MODULES_DIR/fuse.ko.zst" ]; then
    echo "error: virtio-fs kernel modules not found for $KVER in $MODULES_DIR" >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for d in bin dev etc lib lib/modules mnt proc sys tmp usr; do
    mkdir -p "$WORK/$d"
done

cp "$BUSYBOX" "$WORK/bin/busybox"

# Decompress the modules so the guest can `insmod` them directly (busybox
# insmod does not understand .zst). virtio-fs depends on fuse; the runner
# loads fuse first.
zstd -q -d -o "$WORK/lib/modules/fuse.ko" "$MODULES_DIR/fuse.ko.zst"
zstd -q -d -o "$WORK/lib/modules/virtiofs.ko" "$MODULES_DIR/virtiofs.ko.zst"

# virtio_net is optional — included if available for network policy support.
if [ -f "$VIRTIO_NET_DIR/virtio_net.ko.zst" ]; then
    zstd -q -d -o "$WORK/lib/modules/virtio_net.ko" "$VIRTIO_NET_DIR/virtio_net.ko.zst"
fi

# Busybox dispatches by argv[0]: symlink the applets the runner + jobs need.
for applet in sh mount umount insmod poweroff awk grep sed sleep kill date cat ls mkdir touch cut wc true false echo printf sync iptables ip udhcpc; do
    ln -sf busybox "$WORK/bin/$applet"
done

# /init: the job-runner agent, PID 1 in the guest.
#   1. mount proc/sysfs/devtmpfs, load fuse + virtio-fs
#   2. mount the host's job share (tag vtessera-job) at /mnt
#   3. parse manifest.json into arg + env files via awk (one item per line)
#   4. run the command with stdin closed, stdout/stderr appended to a log
#   5. meter it from /proc, write out/result.json + out/metering.json
#   6. sync + poweroff -f
#
# The host's wall-clock timer is authoritative for max_duration_secs; the
# in-guest `sleep $max_duration &` is only a best-effort backstop.
#
# Quoting strategy: awk writes raw unescaped lines (one arg per line,
# KEY=VALUE per line for env). The shell reads them with `while read` and
# builds a proper argv array — no awk→shell quoting required.
cat > "$WORK/init" <<'INIT'
#!/bin/busybox sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

insmod /lib/modules/fuse.ko
insmod /lib/modules/virtiofs.ko

mkdir -p /mnt
mount -t virtiofs vtessera-job /mnt
cd /mnt

mkdir -p /run

# --- GPU detection: load driver from workload image if present ---------------
# Scan for VGA (0x030000) or 3D controller (0x030200) in sysfs.
# The workload image is expected to contain driver/<vendor>.ko files.
for _cls in /sys/bus/pci/devices/*/class; do
    [ -r "$_cls" ] || continue
    _class=$(cat "$_cls" 2>/dev/null)
    case "$_class" in
        0x030000|0x030200)
            _devdir=$(dirname "$_cls")
            _vendor=$(cat "$_devdir/vendor" 2>/dev/null)
            case "$_vendor" in
                0x10de) # NVIDIA
                    if [ -f /mnt/driver/nvidia.ko ]; then
                        insmod /mnt/driver/nvidia.ko 2>/dev/null || true
                    fi
                    if [ -f /mnt/driver/nvidia-uvm.ko ]; then
                        insmod /mnt/driver/nvidia-uvm.ko 2>/dev/null || true
                    fi
                    ;;
                0x1002) # AMD
                    if [ -f /mnt/driver/amdgpu.ko ]; then
                        insmod /mnt/driver/amdgpu.ko 2>/dev/null || true
                    fi
                    ;;
            esac
            break
            ;;
    esac
done

# --- network policy enforcement (§1e) --------------------------------------
# Read network_policy from the manifest. If not "none", bring up the NIC
# and apply iptables rules to enforce the policy.
NETWORK_POLICY="$(sed -n 's/^.*"network_policy":"\([^"]*\)".*$/\1/p' manifest.json)"
[ -n "$NETWORK_POLICY" ] || NETWORK_POLICY="none"

if [ "$NETWORK_POLICY" != "none" ]; then
    insmod /lib/modules/virtio_net.ko 2>/dev/null || true
    ip link set eth0 up 2>/dev/null || true
    # Busybox udhcpc for DHCP; ignore failure (static IP may be configured).
    udhcpc -i eth0 -n -q 2>/dev/null || true

    case "$NETWORK_POLICY" in
        outbound_https)
            # Allow DNS (UDP+TCP/53) and HTTPS (TCP/443), drop everything else.
            iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
            iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT
            iptables -A OUTPUT -p tcp --dport 443 -j ACCEPT
            iptables -A OUTPUT -j DROP
            ;;
        egress)
            # Check for CIDR restrictions in the manifest.
            CIDRS="$(sed -n 's/^.*"allowed_cidrs":\[\(.*\)\].*$/\1/p' manifest.json)"
            if [ -n "$CIDRS" ]; then
                # Parse CIDRs from JSON array (e.g. "10.0.0.0/8","172.16.0.0/12")
                echo "$CIDRS" | tr ',' '\n' | sed 's/"//g' | while IFS= read -r _cidr; do
                    [ -n "$_cidr" ] && iptables -A OUTPUT -d "$_cidr" -j ACCEPT
                done
                iptables -A OUTPUT -j DROP
            fi
            # No CIDRs = no restrictions (all egress allowed).
            ;;
    esac
    echo "network: policy=$NETWORK_POLICY" >> out/job.log
fi

# --- parse manifest.json (JSON → raw lines) --------------------------------
# awk extracts command args and env pairs as raw strings (one per line).
# No quoting is done here — the shell handles it below.
awk '
function unesc(s,    esc, out, i, c) {
    if (length(s) >= 2 && substr(s, 1, 1) == "\"" && substr(s, length(s), 1) == "\"")
        s = substr(s, 2, length(s) - 2)
    esc = 0; out = ""
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (esc) {
            if (c == "n") out = out "\n"
            else if (c == "t") out = out "\t"
            else if (c == "r") out = out "\r"
            else if (c == "\\") out = out "\\"
            else if (c == "\"") out = out "\""
            else if (c == "/") out = out "/"
            else if (c == "u") { i += 4; out = out "?" }
            else out = out c
            esc = 0
        } else if (c == "\\") esc = 1
        else out = out c
    }
    return out
}

function splitarr(s, out,    n, i, c, inq, esc, start, bd) {
    n = 0; inq = 0; esc = 0; start = 1; bd = 0
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (esc) { esc = 0; continue }
        if (inq) {
            if (c == "\\") esc = 1
            else if (c == "\"") inq = 0
        } else {
            if (c == "\"") inq = 1
            else if (c == "[") bd++
            else if (c == "]") bd--
            else if (c == "," && bd == 0) { out[++n] = substr(s, start, i - start); start = i + 1 }
        }
    }
    out[++n] = substr(s, start)
    return n
}

function field(f, doc,    p, i, c, inq, esc, depth, body) {
    p = index(doc, "\"" f "\":[")
    if (p == 0) return ""
    body = ""; inq = 0; esc = 0; depth = 0
    for (i = p + length(f) + 3; i <= length(doc); i++) {
        c = substr(doc, i, 1)
        if (esc) { esc = 0; body = body c; continue }
        if (inq) {
            if (c == "\\") esc = 1
            else if (c == "\"") inq = 0
            body = body c
        } else {
            if (c == "\"") { inq = 1; body = body c }
            else if (c == "[") { depth++; if (depth > 1) body = body c }
            else if (c == "]") { depth--; if (depth == 0) return body; body = body c }
            else body = body c
        }
    }
    return body
}

{ doc = doc $0 }
END {
    n = splitarr(field("command", doc), a)
    if (n == 0) {
        print "127" > "/run/exit_on_error"
    } else {
        for (i = 1; i <= n; i++) print unesc(a[i]) > "/run/args.txt"
    }
    m = splitarr(field("env", doc), e)
    for (i = 1; i <= m; i++) {
        pair = e[i]
        if (length(pair) >= 2 && substr(pair, 1, 1) == "[" && substr(pair, length(pair), 1) == "]")
            pair = substr(pair, 2, length(pair) - 2)
        np = splitarr(pair, kv)
        if (np < 2) continue
        k = unesc(kv[1]); v = unesc(kv[2])
        printf "%s=%s\n", k, v > "/run/env.txt"
    }
}
' manifest.json

if [ -f /run/exit_on_error ]; then
    echo 'fatal: manifest.json missing or unparseable' >> out/job.log
    echo '{"exit_code":127}' > out/result.json
    echo '{"cpu_seconds":0,"peak_mem_kb":0,"elapsed_secs":0}' > out/metering.json
    sync; poweroff -f
fi

JOB_ID="$(sed -n 's/^.*"job_id":"\([^"]*\)".*$/\1/p' manifest.json)"
MAX_DURATION_SECS="$(sed -n 's/^.*"max_duration_secs":\([0-9]*\).*$/\1/p' manifest.json)"

# --- build argv from the raw lines written by awk --------------------------
set --
if [ -f /run/args.txt ]; then
    while IFS= read -r _arg; do
        set -- "$@" "$_arg"
    done < /run/args.txt
fi

# --- build env from the raw lines written by awk ----------------------------
if [ -f /run/env.txt ]; then
    while IFS= read -r _line; do
        export "$_line"
    done < /run/env.txt
fi

{
    echo "job=$JOB_ID"
    echo "command: $*"
} >> out/job.log

# --- run the job ------------------------------------------------------------
"$@" < /dev/null >> out/job.log 2>&1 &
JOB_PID=$!

# Best-effort in-guest cap (host timer is authoritative).
WATCHDOG=""
if [ -n "$MAX_DURATION_SECS" ] && [ "$MAX_DURATION_SECS" -gt 0 ]; then
    ( sleep "$MAX_DURATION_SECS"; kill -9 "$JOB_PID" 2>/dev/null ) &
    WATCHDOG=$!
fi

START=$(date +%s)
CPU_TICKS_MAX=0
PEAK_MEM_KB=0
while kill -0 "$JOB_PID" 2>/dev/null; do
    if [ -r "/proc/$JOB_PID/stat" ]; then
        T=$(awk '{print $14+$15}' "/proc/$JOB_PID/stat")
        if [ -n "$T" ] && [ "$T" -gt "$CPU_TICKS_MAX" ]; then CPU_TICKS_MAX=$T; fi
    fi
    if [ -r "/proc/$JOB_PID/status" ]; then
        P=$(awk '/VmPeak/{print $2}' "/proc/$JOB_PID/status")
        if [ -n "$P" ] && [ "$P" -gt "$PEAK_MEM_KB" ]; then PEAK_MEM_KB=$P; fi
    fi
    sleep 1
done
EXIT_CODE=0
wait "$JOB_PID" 2>/dev/null || EXIT_CODE=$?
if [ -n "$WATCHDOG" ]; then kill "$WATCHDOG" 2>/dev/null; fi
END=$(date +%s)
ELAPSED=$((END - START))

CPU_SECONDS=0
if [ -n "$CPU_TICKS_MAX" ] && [ "$CPU_TICKS_MAX" -gt 0 ]; then
    CPU_SECONDS=$(awk "BEGIN{printf \"%.3f\", $CPU_TICKS_MAX/100}")
fi
[ -n "$PEAK_MEM_KB" ] || PEAK_MEM_KB=0

SIGNAL=null
if [ "$EXIT_CODE" -gt 128 ]; then
    SIGNAL=$((EXIT_CODE - 128))
fi

echo "exit_code=$EXIT_CODE cpu_seconds=$CPU_SECONDS peak_mem_kb=$PEAK_MEM_KB elapsed_secs=$ELAPSED" >> out/job.log

echo "{\"exit_code\":$EXIT_CODE,\"signal\":$SIGNAL}" > out/result.json
echo "{\"cpu_seconds\":$CPU_SECONDS,\"peak_mem_kb\":$PEAK_MEM_KB,\"elapsed_secs\":$ELAPSED}" > out/metering.json

sync
poweroff -f
INIT

chmod +x "$WORK/init"

# --- deterministic cpio + gzip ---------------------------------------------
# cpio --reproducible zeros device fields; we additionally zero all mtimes
# and inodes with touch + find to ensure byte-identical output across runs.
find "$WORK" -depth -exec touch -h -d @0 {} +
CPIO_RAW="${WORK}.cpio"
mkdir -p "$(dirname "$OUT")"
(
    cd "$WORK"
    find . | LC_ALL=C sort | cpio -o -H newc --reproducible 2>/dev/null
) > "$CPIO_RAW"
gzip -n -9 "$CPIO_RAW"
cp "$CPIO_RAW.gz" "$OUT"

sha256sum "$OUT" | awk '{print $1}' > "$ROOT/scripts/initramfs.sha256"

echo "built $OUT ($(du -h "$OUT" | cut -f1))"
echo "kernel: $KVER"
echo "busybox: $(sha256sum "$BUSYBOX" | cut -c1-16)"
echo "sha256: $(cat "$ROOT/scripts/initramfs.sha256")"
