#!/usr/bin/env bash
# Runs the offer seeder against a real sandbox marketplace.
#
# The seeder's unit tests run against a fake API. That fake can be made to agree
# with the seeder by editing both, so this is the check that the real endpoints
# accept what the seeder sends: a real handshake, real cards, real offers, and a
# refresh that replaces a listing rather than duplicating it.
#
#   ./scripts/seed.sh [vtessera-binary] [vtessera-seed-binary]
#
# It needs python3 only for reading JSON out of the API, the same as the other
# process-level scripts. It moves no value: the marketplace is a throwaway
# sandbox with no chain.
set -euo pipefail

BINARY="${1:-bin/vtessera}"
SEED_BIN="${2:-bin/vtessera-seed}"
PORT="${VTESSERA_SEED_TEST_PORT:-18093}"
BASE="http://127.0.0.1:${PORT}"
WORKDIR="$(mktemp -d)"
SECRET="0123456789abcdef0123456789abcdef"

SERVER_PID=""
SEED_PID=""

cleanup() {
  [[ -n "${SEED_PID:-}" ]] && kill "${SEED_PID}" 2>/dev/null || true
  [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

count_offers() {
  curl -fsS "${BASE}/v1/offers" | python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("offers") or []))'
}

count_agents() {
  curl -fsS "${BASE}/v1/agents" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d if isinstance(d,list) else d.get("agents") or []))'
}

wait_for() {
  local want="$1" label="$2" got=""
  for _ in $(seq 1 100); do
    got="$(count_offers)"
    [[ "${got}" == "${want}" ]] && return 0
    sleep 0.2
  done
  fail "expected ${want} ${label}, saw ${got}"
}

[[ -x "${BINARY}" ]] || { echo "building ${BINARY}" >&2; go build -trimpath -o "${BINARY}" ./cmd/vtessera; }
[[ -x "${SEED_BIN}" ]] || { echo "building ${SEED_BIN}" >&2; go build -trimpath -o "${SEED_BIN}" ./cmd/vtessera-seed; }

echo "== starting a throwaway sandbox"
"${BINARY}" \
  --sandbox \
  --addr ":${PORT}" \
  --db "${WORKDIR}/sandbox.db" \
  --signer-key "${WORKDIR}/sandbox.key" \
  --session-secret "${SECRET}" \
  --rate-limit-agent-rps 200 --rate-limit-agent-burst 400 \
  --rate-limit-ip-rps 200 --rate-limit-ip-burst 400 \
  >"${WORKDIR}/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${BASE}/healthz" | grep -q '"sandbox":true' || { cat "${WORKDIR}/server.log"; fail "the sandbox never came up"; }

LISTINGS="$(python3 -c 'print(8)')"
echo "== seeding ${LISTINGS} listings"
"${SEED_BIN}" \
  --base-url "${BASE}" \
  --keyring "${WORKDIR}/seed-keyring.json" \
  --refresh 1s \
  --close-on-exit \
  >"${WORKDIR}/seed.log" 2>&1 &
SEED_PID=$!

wait_for "${LISTINGS}" "open offers after the first pass"
AGENTS_AFTER_FIRST="$(count_agents)"

# A second or two of refreshes must replace, not duplicate: a refresh that piled
# a new offer beside the old one would leave the board growing without bound.
sleep 2.5
if [[ "$(count_offers)" != "${LISTINGS}" ]]; then
  cat "${WORKDIR}/seed.log"; cat "${WORKDIR}/server.log"
  fail "a refresh duplicated offers instead of replacing them"
fi
if [[ "$(count_agents)" != "${AGENTS_AFTER_FIRST}" ]]; then
  fail "a refresh registered new agents instead of reusing the keyring"
fi
echo "ok  refresh replaces the listing and reuses the agent identity"

echo "== stopping the seeder; it was started with --close-on-exit"
kill -TERM "${SEED_PID}"
wait "${SEED_PID}" 2>/dev/null || true
SEED_PID=""
wait_for "0" "open offers after a close-on-exit shutdown"
echo "ok  the seeder withdrew the board it opened"

echo
echo "SEED OK"
