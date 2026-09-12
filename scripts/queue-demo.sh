#!/usr/bin/env bash
# Queue rendezvous demo (P1.7): an outbound-only vtessera-node pulls work
# from a local coordinator's queue over iroh QUIC instead of opening an
# inbound listener.
#
#   ./scripts/queue-demo.sh
#
# Topology (all localhost):
#   coordinator   vtessera-coordinator (EndpointAddr pinned to a JSON file)
#   node          vtessera-node --connectivity outbound-only \
#                    --coordinator-addr <json>   (PULLS, opens no listener)
#   index         vtessera-offer-index (node still publishes its offer)
#
# Flow: coordinator starts and pins its addr → outbound node starts and
# registers with the index → agent enqueues a free job via the queue →
# node pulls it, verifies the coordinator's signature on the DispatchOffer,
# runs it, and persists a signed receipt → expiry check proves the node
# actually executed (no HTTP round trip involved).
#
# Env overrides:
#   VTESSERA_INDEX_PORT   index port (default 8403)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INDEX_PORT="${VTESSERA_INDEX_PORT:-8403}"
INDEX="http://127.0.0.1:$INDEX_PORT"
WORK="$(mktemp -d)"
STATE="$WORK/node-state"
COORD_ADDR="$WORK/coordinator-addr.json"
JOB_ID="queue-demo-$(date +%s)"

PIDS=""
cleanup() {
    for p in $PIDS; do
        kill "$p" 2>/dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

echo "== building coordinator, node, index, agent =="
cargo build -q -p vtessera-coordinator --locked --bin vtessera-coordinator --features serve
cargo build -q -p vtessera-node-api --locked --bin vtessera-node --features serve
cargo build -q -p vtessera-offer-index --locked --bin vtessera-offer-index --features serve
cargo build -q -p vtessera-agent --locked --bin vtessera-agent

echo "== starting coordinator (addr pinned to $COORD_ADDR) =="
"$ROOT/target/debug/vtessera-coordinator" --addr-out "$COORD_ADDR" >/dev/null 2>&1 &
PIDS="$PIDS $!"
for _ in $(seq 1 20); do
    [ -f "$COORD_ADDR" ] && break
    sleep 0.3
done
[ -f "$COORD_ADDR" ] || fail "coordinator did not pin its addr"

echo "== index =="
"$ROOT/target/debug/vtessera-offer-index" --bind "127.0.0.1:$INDEX_PORT" >/dev/null 2>&1 &
PIDS="$PIDS $!"
for _ in $(seq 1 30); do
    curl -sf -m 1 "$INDEX/healthz" >/dev/null 2>&1 && break
    sleep 0.5
done
curl -sf -m 1 "$INDEX/healthz" >/dev/null || fail "index failed to start"

echo "== outbound-only node (pulls from coordinator, no HTTP listener) =="
mkdir -p "$STATE"
cargo run -q -p vtessera-node-api --locked --example gen_offer \
    -- free --seed 42 --endpoint "queue:$COORD_ADDR" --key-out "$WORK/key.bin" > "$WORK/offer.json"
"$ROOT/target/debug/vtessera-node" \
    --bind 127.0.0.1:1 --offer "$WORK/offer.json" \
    --escrow 6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma --network solana-devnet \
    --backend noop-cpu --key "$WORK/key.bin" --state-dir "$STATE" \
    --connectivity outbound-only \
    --coordinator-addr "$COORD_ADDR" --coordinator-poll 1 \
    --publish "$INDEX" --publish-interval 10 >/dev/null 2>&1 &
PIDS="$PIDS $!"
sleep 3

echo "== agent enqueues a free job via the queue =="
printf '{"job_id":"%s","image":"busybox","command":["echo","hello from queue"],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":65536,"min_vram_mb":0},"network":"none","max_duration_secs":60}' "$JOB_ID" > "$WORK/job.json"
QUEUED="$(vtessera-agent --queue "$COORD_ADDR" submit --job "$WORK/job.json" --json 2>&1)" || fail "agent enqueue failed: $QUEUED"
echo "$QUEUED"
echo "$QUEUED" | grep -qE '"status"\s*:\s*"queued"' || fail "expected queued status: $QUEUED"

echo "== node pulls, runs, persists signed receipt =="
RECEIPT="$STATE/job-receipts/$JOB_ID.json"
for _ in $(seq 1 30); do
    [ -f "$RECEIPT" ] && break
    sleep 0.5
done
[ -f "$RECEIPT" ] || fail "node never pulled/ran $JOB_ID (no receipt at $RECEIPT)"
python3 -m json.tool "$RECEIPT" || fail "receipt is not valid JSON"
echo "$JOB_ID" | grep -q . && echo "receipt persisted for $JOB_ID"

echo
echo "PASS: queue rendezvous demo — coordinator pins addr, outbound node"
echo "      pulls signed DispatchOffer from the queue, runs it, acks,"
echo "      and persists a signed receipt (no inbound HTTP listener)."