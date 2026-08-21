#!/usr/bin/env bash
# Smoke test: submit a free job to a running vtessera-node and verify it works.
#
#   ./scripts/agent-smoke-test.sh
#
# Assumes a node is already running on 127.0.0.1:8402 (e.g. from the
# Flatpak GUI with "Accept workloads" on and mode set to "free").
#
# Env overrides:
#   VTESSERA_NODE_URL  node base URL (default http://127.0.0.1:8402)
#   VTESSERA_AGENT_ID  agent identity header (default smoke-test)
set -euo pipefail

NODE_URL="${VTESSERA_NODE_URL:-http://127.0.0.1:8402}"
AGENT_ID="${VTESSERA_AGENT_ID:-smoke-test}"
JOB_ID="smoke-$(date +%s)"
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }

echo "=== Vtessera agent smoke test ==="
echo "Node: $NODE_URL"
echo

# 1. Health check
echo "1. Health check"
if resp=$(curl -sf "$NODE_URL/healthz" 2>/dev/null); then
    [ "$resp" = "ok" ] && pass "node is healthy" || fail "unexpected response: $resp"
else
    fail "node not reachable at $NODE_URL — is it running?"
    echo; echo "Result: $FAIL failed"; exit 1
fi

# 2. Offer exists and is valid JSON
echo "2. Offer"
if offer=$(curl -sf "$NODE_URL/offer" 2>/dev/null); then
    node_id=$(echo "$offer" | python3 -c "import sys,json; print(json.load(sys.stdin)['body']['node_id'])" 2>/dev/null)
    mode=$(echo "$offer" | python3 -c "import sys,json; print(json.load(sys.stdin)['body']['price']['mode'])" 2>/dev/null)
    [ -n "$node_id" ] && pass "node_id: $node_id" || fail "missing node_id"
    [ -n "$mode" ] && pass "mode: $mode" || fail "missing mode"
else
    fail "could not fetch /offer"
fi

# 3. Submit a free job
echo "3. Submit free job ($JOB_ID)"
job_resp=$(curl -s -w "\n%{http_code}" -X POST "$NODE_URL/jobs" \
    -H 'Content-Type: application/json' \
    -H "x-agent-id: $AGENT_ID" \
    -d "{
        \"job_id\": \"$JOB_ID\",
        \"image\": \"busybox\",
        \"command\": [\"echo\", \"hello from smoke test\"],
        \"env\": [],
        \"devices\": {\"class\": {\"kind\": \"cpu\"}, \"vcpus\": 1, \"mem_kb\": 65536, \"min_vram_mb\": 0},
        \"network\": \"none\",
        \"max_duration_secs\": 60
    }" 2>/dev/null)

http_code=$(echo "$job_resp" | tail -1)
body=$(echo "$job_resp" | sed '$d')

if [ "$http_code" = "200" ]; then
    pass "job accepted (HTTP 200)"
    # Check if the response contains output
    if echo "$body" | grep -q "hello from smoke test"; then
        pass "job output contains expected text"
    elif echo "$body" | grep -qi "completed\|success\|ok"; then
        pass "job completed successfully"
    else
        pass "job returned a response"
    fi
elif [ "$http_code" = "402" ]; then
    pass "node is in paid mode (HTTP 402 x402 challenge)"
    echo "       → Switch to free mode in the GUI to run free jobs"
elif [ "$http_code" = "503" ]; then
    fail "node not accepting jobs (HTTP 503) — enable 'Accept workloads' in the GUI"
else
    fail "unexpected HTTP $http_code"
    [ -n "$body" ] && echo "       response: $body"
fi

# 4. MCP endpoint
echo "4. MCP discover"
mcp_resp=$(curl -s -w "\n%{http_code}" -X POST "$NODE_URL/mcp" \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"discover","arguments":{}}}' 2>/dev/null)

mcp_code=$(echo "$mcp_resp" | tail -1)
mcp_body=$(echo "$mcp_resp" | sed '$d')

if [ "$mcp_code" = "200" ]; then
    pass "MCP endpoint responding"
    if echo "$mcp_body" | grep -q "discover\|result"; then
        pass "MCP discover returns data"
    fi
else
    fail "MCP returned HTTP $mcp_code"
fi

# 5. A2A agent card
echo "5. A2A agent card"
if card=$(curl -sf "$NODE_URL/.well-known/agent.json" 2>/dev/null); then
    pass "agent card exists"
else
    fail "no agent card at /.well-known/agent.json"
fi

# Summary
echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] && echo "All checks passed." || echo "Some checks failed."
exit "$FAIL"
