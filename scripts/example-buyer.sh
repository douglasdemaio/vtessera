#!/usr/bin/env bash
#
# Runs examples/buyer against a throwaway sandbox it stocks with the seeder.
#
# The buyer example is a consumer and nothing else: it discovers an offer, opens
# a trade, and negotiates. It cannot finish the trade, because accepting is the
# seller's move. So the check here is that it gets that far against a real
# marketplace — handshake, card, discovery, trade creation, negotiation — and not
# that it produces a receipt, which would need a seller.
#
# It moves no value: the marketplace is a sandbox with no chain.
set -euo pipefail

BINARY="${1:-bin/vtessera}"
SEED_BIN="${2:-bin/vtessera-seed}"
PORT="${VTESSERA_BUYER_TEST_PORT:-18094}"
BASE="http://127.0.0.1:${PORT}"
WORKDIR="$(mktemp -d)"
SECRET="0123456789abcdef0123456789abcdef"

SERVER_PID=""

cleanup() {
  [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

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

echo "== stocking the board with the seeder"
"${SEED_BIN}" \
  --base-url "${BASE}" \
  --keyring "${WORKDIR}/seed-keyring.json" \
  --once \
  >"${WORKDIR}/seed.log" 2>&1 || { cat "${WORKDIR}/seed.log"; fail "the seeder could not stock the board"; }

echo "== running the buyer example"
VTESSERA_BASE_URL="${BASE}" go run ./examples/buyer >"${WORKDIR}/buyer.log" 2>&1 || {
  cat "${WORKDIR}/buyer.log"
  fail "the buyer example failed"
}
cat "${WORKDIR}/buyer.log"

grep -q "Waiting on the seller to accept" "${WORKDIR}/buyer.log" || fail "the buyer example did not open a trade against the seeded board"
grep -q "now negotiating" "${WORKDIR}/buyer.log" || fail "the buyer example did not reach the negotiating state"

echo
echo "BUYER OK"
