#!/usr/bin/env bash
# One-command x402 demo: the AI-agent client pays the Vtessera escrow on
# Solana devnet and submits a job to a vtessera-node.
#
#   ./scripts/x402-demo.sh
#
# Uses an already-running node if one answers /healthz (e.g. the one the
# Flatpak GUI/daemon starts on 127.0.0.1:8402), otherwise builds and
# starts a local vtessera-node with a freshly signed paid offer.
#
# Env overrides:
#   VTESSERA_NODE_URL    node base URL (default http://127.0.0.1:8402)
#   VTESSERA_JOB_SECONDS agreed device-seconds (default 60)
#   VTESSERA_OFFER_MODE  free|paid for the locally-started node (default paid)
#   VTESSERA_BACKEND     noop-cpu|local-cpu executor for the local node (default noop-cpu)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_URL="${VTESSERA_NODE_URL:-http://127.0.0.1:8402}"
JOB_SECONDS="${VTESSERA_JOB_SECONDS:-60}"
OFFER_MODE="${VTESSERA_OFFER_MODE:-paid}"
BACKEND="${VTESSERA_BACKEND:-noop-cpu}"
PORT="${VTESSERA_NODE_PORT:-8402}"
ESCROW="6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma"

started_node=0
cleanup() {
    if [ "$started_node" = "1" ]; then
        kill "$NODE_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

if curl -sf -m 2 "$NODE_URL/healthz" >/dev/null 2>&1; then
    echo "using existing node at $NODE_URL"
else
    echo "no node at $NODE_URL — starting a local one on 127.0.0.1:$PORT ..."
    cd "$ROOT"
    OFFER_FILE="$(mktemp)"
    cargo run -q -p vtessera-node-api --locked --example gen_offer -- "$OFFER_MODE" > "$OFFER_FILE"
    cargo build -q -p vtessera-node-api --locked --bin vtessera-node --features serve
    "$ROOT/target/debug/vtessera-node" \
        --bind "127.0.0.1:$PORT" \
        --offer "$OFFER_FILE" \
        --escrow "$ESCROW" \
        --network solana-devnet \
        --backend "$BACKEND" >/dev/null 2>&1 &
    NODE_PID=$!
    started_node=1
    for _ in $(seq 1 30); do
        curl -sf -m 1 "$NODE_URL/healthz" >/dev/null 2>&1 && break
        sleep 0.5
    done
    if ! curl -sf -m 1 "$NODE_URL/healthz" >/dev/null 2>&1; then
        echo "node failed to start" >&2
        exit 1
    fi
fi

cd "$ROOT/crates/x402-client"
exec cargo run --offline -- --node "$NODE_URL" --seconds "$JOB_SECONDS"
