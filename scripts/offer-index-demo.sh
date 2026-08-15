#!/usr/bin/env bash
# End-to-end offer-index demo: two vtessera-nodes publish signed offers to a
# central offer index, an agent claims one first-come-first-served, and the
# node enforces the claim — plus the MCP `discover` tool.
#
#   ./scripts/offer-index-demo.sh
#
# Topology (all localhost):
#   index   127.0.0.1:8403   vtessera-offer-index (--features serve)
#   node A  127.0.0.1:8402   free offer,  --publish http://127.0.0.1:8403
#   node B  127.0.0.1:8405   paid offer,  --publish http://127.0.0.1:8403
#
# Flow: both nodes register → agent-demo claims node A (201) while
# agent-other is refused (409, FCFS) → agent-demo's job runs (200) but
# agent-other's is refused (409, node enforcement) → MCP discover lists both
# offers with claim status → claim released → a job with no agent id is
# refused (409, identity required) and agent-other's job now runs (200).
#
# Env overrides:
#   VTESSERA_INDEX_PORT   index port (default 8403)
#   VTESSERA_A_PORT       node A port (default 8402)
#   VTESSERA_B_PORT       node B port (default 8405)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INDEX_PORT="${VTESSERA_INDEX_PORT:-8403}"
A_PORT="${VTESSERA_A_PORT:-8402}"
B_PORT="${VTESSERA_B_PORT:-8405}"
INDEX="http://127.0.0.1:$INDEX_PORT"
A_URL="http://127.0.0.1:$A_PORT"
B_URL="http://127.0.0.1:$B_PORT"
ESCROW="6jK6oEaLtGm5tCKNB3aCpp3Wq5K7gbVBdEfqqLMQ7uma"
WORK="$(mktemp -d)"

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

# http_status <method> <url> [body-file] [headers...] -> echoes HTTP code.
http_status() {
    local method="$1" url="$2" body="${3:-}" out="$WORK/curl-body.json"
    shift 2 || true
    local args=(-s -m 10 -o "$out" -w '%{http_code}' -X "$method")
    [ -n "$body" ] && args+=(-d @"$body")
    for h in "$@"; do
        args+=(-H "$h")
    done
    curl "${args[@]}" "$url"
    cp "$out" "$WORK/last-body.json"
}

json_field() {
    python3 -c "import json,sys;print(json.load(sys.stdin)$1)" < "$WORK/last-body.json"
}

echo "== building node, index, gen_offer =="
cargo build -q -p vtessera-node-api --locked --bin vtessera-node --features serve
cargo build -q -p vtessera-offer-index --locked --bin vtessera-offer-index --features serve

echo "== starting index on 127.0.0.1:$INDEX_PORT =="
"$ROOT/target/debug/vtessera-offer-index" --bind "127.0.0.1:$INDEX_PORT" >/dev/null 2>&1 &
PIDS="$PIDS $!"
for _ in $(seq 1 30); do
    curl -sf -m 1 "$INDEX/healthz" >/dev/null 2>&1 && break
    sleep 0.5
done
curl -sf -m 1 "$INDEX/healthz" >/dev/null || fail "index failed to start"

echo "== signing offers and starting nodes A (free) and B (paid) =="
cargo run -q -p vtessera-node-api --locked --example gen_offer \
    -- free --seed 1 --endpoint "$A_URL" --key-out "$WORK/key-a.bin" > "$WORK/offer-a.json"
cargo run -q -p vtessera-node-api --locked --example gen_offer \
    -- paid --seed 2 --endpoint "$B_URL" --key-out "$WORK/key-b.bin" > "$WORK/offer-b.json"

"$ROOT/target/debug/vtessera-node" \
    --bind "127.0.0.1:$A_PORT" --offer "$WORK/offer-a.json" \
    --escrow "$ESCROW" --network solana-devnet --backend noop-cpu \
    --key "$WORK/key-a.bin" --state-dir "$WORK/a" \
    --publish "$INDEX" --publish-interval 10 >/dev/null 2>&1 &
PIDS="$PIDS $!"
"$ROOT/target/debug/vtessera-node" \
    --bind "127.0.0.1:$B_PORT" --offer "$WORK/offer-b.json" \
    --escrow "$ESCROW" --network solana-devnet --backend noop-cpu \
    --key "$WORK/key-b.bin" --state-dir "$WORK/b" \
    --publish "$INDEX" --publish-interval 10 >/dev/null 2>&1 &
PIDS="$PIDS $!"

for _ in $(seq 1 30); do
    [ "$(curl -sf -m 1 "$INDEX/offers" | python3 -c 'import json,sys;print(json.load(sys.stdin)["count"])' 2>/dev/null || echo 0)" = "2" ] && break
    sleep 0.5
done

echo "== index listing =="
curl -sf -m 2 "$INDEX/offers" | python3 -m json.tool
NODE_A="$(curl -sf -m 2 "$INDEX/offers" | python3 -c 'import json,sys;print(json.load(sys.stdin)["offers"][0]["offer"]["body"]["node_id"])')"
[ -n "$NODE_A" ] || fail "could not read node A id from index"

JOB_A1="offer-demo-a1-$(date +%s)"
JOB_A2="offer-demo-a2-$(date +%s)"
JOB_A3="offer-demo-a3-$(date +%s)"
printf '{"job_id":"%s","image":"busybox","command":["echo","hi"],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":65536,"min_vram_mb":0,"driver_hint":null},"network":"none","max_duration_secs":60}' "$JOB_A1" > "$WORK/job1.json"
printf '{"job_id":"%s","image":"busybox","command":["echo","hi"],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":65536,"min_vram_mb":0,"driver_hint":null},"network":"none","max_duration_secs":60}' "$JOB_A2" > "$WORK/job2.json"
printf '{"job_id":"%s","image":"busybox","command":["echo","hi"],"env":[],"devices":{"class":{"kind":"cpu"},"vcpus":1,"mem_kb":65536,"min_vram_mb":0,"driver_hint":null},"network":"none","max_duration_secs":60}' "$JOB_A3" > "$WORK/job3.json"

echo "== FCFS claim: agent-demo claims node A, agent-other refused =="
printf '{"agent_id":"agent-demo"}' > "$WORK/claim.json"
code="$(http_status POST "$INDEX/offers/$NODE_A/claim" "$WORK/claim.json")"
[ "$code" = "201" ] || fail "expected 201 claiming node A, got $code ($(cat "$WORK/last-body.json"))"
echo "agent-demo claimed node A: 201"

printf '{"agent_id":"agent-other"}' > "$WORK/claim2.json"
code="$(http_status POST "$INDEX/offers/$NODE_A/claim" "$WORK/claim2.json")"
[ "$code" = "409" ] || fail "expected 409 for agent-other, got $code ($(cat "$WORK/last-body.json"))"
echo "agent-other claim refused: 409"

echo "== node enforcement: agent-demo's job runs, agent-other's is refused =="
code="$(http_status POST "$A_URL/jobs" "$WORK/job1.json" -H "x-agent-id: agent-demo")"
[ "$code" = "200" ] || fail "expected 200 for agent-demo, got $code ($(cat "$WORK/last-body.json"))"
echo "agent-demo job: 200 ($(json_field '["status"]'))"

code="$(http_status POST "$A_URL/jobs" "$WORK/job2.json" -H "x-agent-id: agent-other")"
[ "$code" = "409" ] || fail "expected 409 for agent-other, got $code ($(cat "$WORK/last-body.json"))"
grep -q 'claimed by' "$WORK/last-body.json" || fail "409 body should name the claimant: $(cat "$WORK/last-body.json")"
echo "agent-other job refused: 409 (node claimed by agent-demo)"

echo "== MCP discover on node A =="
cat > "$WORK/mcp.json" <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"discover","arguments":{}}}
EOF
code="$(http_status POST "$A_URL/mcp" "$WORK/mcp.json")"
[ "$code" = "200" ] || fail "expected 200 for MCP discover, got $code"
DISCOVER_TEXT="$(json_field '["result"]["content"][0]["text"]')"
DISCOVER_COUNT="$(printf '%s' "$DISCOVER_TEXT" | python3 -c 'import json,sys;print(json.load(sys.stdin)["count"])')"
[ "$DISCOVER_COUNT" = "2" ] || fail "expected 2 offers from discover, got $DISCOVER_COUNT"
echo "discover returned $DISCOVER_COUNT offers:"
printf '%s' "$DISCOVER_TEXT" | python3 -m json.tool
grep -q '"claimed_by"' <<< "$DISCOVER_TEXT" || fail "discover output should include claim state"

echo "== release: agent-demo releases, then identity-gate checks =="
code="$(http_status DELETE "$INDEX/offers/$NODE_A/claim" "$WORK/claim.json")"
[ "$code" = "200" ] || fail "expected 200 releasing claim, got $code ($(cat "$WORK/last-body.json"))"
echo "claim released: 200"

code="$(http_status POST "$A_URL/jobs" "$WORK/job2.json")"
[ "$code" = "409" ] || fail "expected 409 without agent id, got $code ($(cat "$WORK/last-body.json"))"
grep -q 'agent identity required' "$WORK/last-body.json" || fail "expected 'agent identity required': $(cat "$WORK/last-body.json")"
echo "job without agent id refused: 409 (agent identity required)"

code="$(http_status POST "$A_URL/jobs" "$WORK/job2.json" -H "x-agent-id: agent-other")"
[ "$code" = "200" ] || fail "expected 200 for agent-other after release, got $code ($(cat "$WORK/last-body.json"))"
echo "agent-other job after release: 200"

echo
echo "PASS: offer-index demo — publish, FCFS claim, node enforcement, MCP discover, release"
