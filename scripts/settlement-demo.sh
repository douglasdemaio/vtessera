#!/usr/bin/env bash
# End-to-end settlement demo: run a job on a vtessera-node, then turn its
# signed job receipt into a completion-fraction settlement record.
#
#   ./scripts/settlement-demo.sh
#
# Builds (if needed), starts a local vtessera-node with a freshly signed
# free offer, submits one job, writes the matching job contract, and runs
# `vtessera-settle --once` to produce settlements/<job_id>.json.
#
# Env overrides:
#   VTESSERA_PORT   node port (default 8490)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${VTESSERA_PORT:-8490}"
ESCROW="6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma"
JOB_ID="settle-demo-$(date +%s)"
WORK="$(mktemp -d)"

NODE_PID=""
cleanup() {
    [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "== building node + gen_offer =="
cargo build -q -p vtessera-node-api --locked --bin vtessera-node --features serve
cargo run -q -p vtessera-node-api --locked --example gen_offer \
    -- free --key-out "$WORK/key.bin" > "$WORK/offer.json"

echo "== starting node on 127.0.0.1:$PORT =="
"$ROOT/target/debug/vtessera-node" \
    --bind "127.0.0.1:$PORT" \
    --offer "$WORK/offer.json" \
    --escrow "$ESCROW" \
    --network solana-devnet \
    --backend noop-cpu \
    --key "$WORK/key.bin" \
    --state-dir "$WORK" >/dev/null 2>&1 &
NODE_PID=$!

for _ in $(seq 1 30); do
    curl -sf -m 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
    sleep 0.5
done
curl -sf -m 1 "http://127.0.0.1:$PORT/healthz" >/dev/null || {
    echo "node failed to start" >&2
    exit 1
}

echo "== submitting job $JOB_ID =="
curl -sf -m 10 -X POST "http://127.0.0.1:$PORT/jobs" \
    -d "{\"job_id\":\"$JOB_ID\",\"image\":\"busybox\",\"command\":[\"echo\",\"hi\"],\"env\":[],\"devices\":{\"class\":{\"kind\":\"cpu\"},\"vcpus\":1,\"mem_kb\":65536,\"min_vram_mb\":0,\"driver_hint\":null},\"network\":\"none\",\"max_duration_secs\":60}" \
    >/dev/null
echo "signed receipt: $WORK/job-receipts/$JOB_ID.json"

echo "== writing job contract (agreed 60 device-seconds) =="
NODE_ID="$(python3 -c "import json;print(json.load(open('$WORK/job-receipts/$JOB_ID.json'))['receipt']['node_id'])")"
mkdir -p "$WORK/contracts"
cat > "$WORK/contracts/$JOB_ID.json" <<EOF
{"job_id":"$JOB_ID","node_id":"$NODE_ID","device_class":{"kind":"cpu"},"agreed_device_seconds":60,"milestones":[]}
EOF

echo "== settling =="
cargo build -q -p vtessera-settlement --locked --bin vtessera-settle
"$ROOT/target/debug/vtessera-settle" --state-dir "$WORK" --once

echo "== settlement record =="
cat "$WORK/settlements/$JOB_ID.json"
echo
python3 -c "import json;r=json.load(open('$WORK/settlements/$JOB_ID.json'));print('f =', r['completion_fraction'])"
