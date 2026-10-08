#!/usr/bin/env bash
# Runs the reference clients in examples/ against a throwaway sandbox marketplace.
#
# The quickstart guides are the tour; the examples are what someone copies into
# their own agent, and they are worthless if they quietly stop working. This is
# the check: a real process, real signatures, a real receipt, verified by each
# client rather than trusted.
#
#   ./scripts/examples.sh [binary] [python|typescript|go|all]
#
# Each client gets its own sandbox with its own database and its own signing
# key, so none can pass on state another left behind and each verifies against
# a marketplace identity nothing else has touched.
#
# It needs the runtimes the hermetic suite deliberately does not: python3 with
# `cryptography`, and node 22.18+ for the TypeScript client, which runs
# TypeScript directly. A missing runtime is reported by name rather than
# skipped, because a client that quietly does not run is a client nobody has
# run.
set -euo pipefail

BINARY="${1:-bin/vtessera}"
CLIENT="${2:-all}"
PORT="${PORT:-18091}"
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

case "${CLIENT}" in
  python|typescript|go|all) ;;
  *) fail "unknown client '${CLIENT}': expected python, typescript, go, or all" ;;
esac

check_python() {
  command -v python3 >/dev/null || fail "python3 is not installed (examples/python)"
  python3 -c "import cryptography" 2>/dev/null \
    || fail "the python client needs \`pip install -r examples/python/requirements.txt\`"
}

check_typescript() {
  command -v node >/dev/null || fail "node is not installed (examples/typescript)"
  # 22.18 is the release that runs TypeScript without a build step, which is
  # what the client claims. An older node would fail much later and much less
  # legibly, on a syntax error in a file that says it needs no build.
  node -e 'const [a, b] = process.versions.node.split(".").map(Number); process.exit(a > 22 || (a === 22 && b >= 18) ? 0 : 1)' \
    || fail "node $(node --version) cannot run TypeScript directly; the client needs 22.18 or later"
}

check_go() {
  command -v go >/dev/null || fail "go is not installed (examples/go)"
}

# One client, one marketplace. The client generates its own identities and
# completes its own trade, so a fresh database per client is what keeps the
# three runs independent rather than sequential.
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
  curl -fsS "${BASE}/healthz" | grep -q '"sandbox":true' \
    || {
      cat "${WORKDIR}/${name}-server.log"
      fail "${name}: server never came up in sandbox mode"
    }
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

run_client() {
  local label="$1" interpreter="$2" script="$3"
  local output="${WORKDIR}/${label}.out"

  echo
  echo "== ${label}"
  start_sandbox "${label}"
  VTESSERA_BASE_URL="${BASE}" ${interpreter} "${script}" >"${output}" 2>&1 || {
    cat "${output}"
    cat "${WORKDIR}/${label}-server.log"
    fail "the ${label} client failed"
  }
  cat "${output}"
  # The client ends on a completion marker, so a run that exited 0 without
  # finishing is still a failure here.
  grep -q "^Done\." "${output}" || { cat "${output}"; fail "the ${label} client did not run to the end"; }
  stop_sandbox
  ok "${label}: handshake, one off-chain trade, a receipt it verified itself"
}

run_one() {
  case "$1" in
    python)
      check_python
      run_client python "python3 -u" examples/python/client.py
      ;;
    typescript)
      check_typescript
      run_client typescript node examples/typescript/client.ts
      ;;
    go)
      check_go
      run_client go "go run" ./examples/go
      ;;
    *)
      fail "unknown client '$1'"
      ;;
  esac
}

[[ -x "${BINARY}" ]] || { echo "building ${BINARY}" >&2; go build -trimpath -o "${BINARY}" ./cmd/vtessera; }

if [[ "${CLIENT}" == "all" ]]; then
  for one in python typescript go; do run_one "${one}"; done
else
  run_one "${CLIENT}"
fi

echo
echo "EXAMPLES OK"
