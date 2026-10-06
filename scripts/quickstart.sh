#!/usr/bin/env bash
# Runs both quickstarts against a throwaway sandbox marketplace.
#
# The quickstarts are the API's documentation, and documentation that has drifted
# from the API is worse than no documentation. This is the check that they still
# work: a real process, real signatures, a real receipt, and the two readers
# verifying that receipt themselves rather than being told it verified.
#
# It needs two runtimes the hermetic suite deliberately does not: python3 with
# `cryptography` for the Python guide, and node 22.18+ for the TypeScript one,
# which runs TypeScript directly and so needs nothing installed. A missing runtime
# is reported by name rather than skipped, because a quickstart that quietly does
# not run is a quickstart nobody has run.
set -euo pipefail

BINARY="${1:-bin/vtessera}"
PORT="${PORT:-18090}"
BASE="http://127.0.0.1:${PORT}"
WORKDIR="$(mktemp -d)"
SECRET="0123456789abcdef0123456789abcdef"

cleanup() {
  [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok $*"; }

command -v python3 >/dev/null || fail "python3 is not installed (docs/quickstart/python.md)"
python3 -c "import cryptography" 2>/dev/null \
  || fail "the python quickstart needs \`pip install cryptography\` (docs/quickstart/python.md)"
command -v node >/dev/null || fail "node is not installed (docs/quickstart/typescript.md)"
# 22.18 is the release that runs TypeScript without a build step, which is what the
# TypeScript guide claims. An older node would fail much later and much less
# legibly, on a syntax error in a file the guide says needs no build.
node -e 'const [a, b] = process.versions.node.split(".").map(Number); process.exit(a > 22 || (a === 22 && b >= 18) ? 0 : 1)' \
  || fail "node $(node --version) cannot run TypeScript directly; the guide needs 22.18 or later"

# One guide, one marketplace. Each run gets its own database and its own signing
# key, so neither guide can pass on state the other left behind, and the two
# readers get two different marketplace identities to verify against.
start_sandbox() {
  local name="$1"
  "${BINARY}" \
    --sandbox \
    --addr ":${PORT}" \
    --db "${WORKDIR}/${name}.db" \
    --signer-key "${WORKDIR}/${name}.key" \
    --session-secret "${SECRET}" \
    >"${WORKDIR}/${name}-server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 50); do
    curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  curl -fsS "${BASE}/healthz" >/dev/null || {
    cat "${WORKDIR}/${name}-server.log"
    fail "${name}: server never became healthy"
  }
  # The first claim each guide makes is that this is a sandbox, so check it here
  # rather than trusting the flag this script just passed.
  curl -fsS "${BASE}/healthz" | grep -q '"sandbox":true' \
    || fail "${name}: the server did not come up in sandbox mode"
}

stop_sandbox() {
  [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  wait "${SERVER_PID}" 2>/dev/null || true
  SERVER_PID=""
  # The next sandbox has to take the same port, so the socket must be gone.
  for _ in $(seq 1 25); do
    curl -fsS "${BASE}/healthz" >/dev/null 2>&1 || return 0
    sleep 0.2
  done
  fail "the port is still answering after the server was stopped"
}

run_guide() {
  local label="$1" interpreter="$2" script="$3"
  local output="${WORKDIR}/${label}.out"

  echo
  echo "== ${label}"
  VTESSERA_BASE_URL="${BASE}" ${interpreter} "${script}" >"${output}" 2>&1 || {
    cat "${output}"
    cat "${WORKDIR}/${label}-server.log"
    fail "the ${label} quickstart failed"
  }
  cat "${output}"
  # Both scripts end on a completion marker, so a run that exited 0 without
  # finishing is still a failure here.
  grep -q "^Done\." "${output}" || { cat "${output}"; fail "the ${label} quickstart did not run to the end"; }
  stop_sandbox
  ok "${label}: two signed agents, a settled off-chain trade, a receipt the reader verified"
}

start_sandbox python
run_guide python "python3 -u" quickstart/python/agent.py

start_sandbox typescript
run_guide typescript node quickstart/typescript/agent.ts

echo
echo "QUICKSTART OK"