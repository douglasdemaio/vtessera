#!/usr/bin/env bash
# End-to-end smoke test against a real vtessera process.
set -euo pipefail

BINARY="${1:-bin/vtessera}"
PORT="${PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
WORKDIR="$(mktemp -d)"
SECRET="0123456789abcdef0123456789abcdef"
# SMOKE_RPC_URL is deliberately not supported. Enabling on-chain settlement needs
# a fee wallet funded above the rent-exempt minimum and at least one governed
# mint that actually exists on the chain being pointed at; a fresh local validator
# has neither, and preflight now refuses to start without both. Rather than
# provision a mint here, the on-chain path is covered by make test-solana, which
# does exactly that setup. Without an endpoint, this run proves the property that
# matters for a default deployment: on-chain trades are refused, not mishandled.
export SMOKE_RPC_URL=""
AGP_URI="https://github.com/a2aproject/a2a-samples/tree/main/extensions/agp"

cleanup() {
  [[ -n "${SERVER_PID:-}" ]] && kill "${SERVER_PID}" 2>/dev/null || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok $*"; }

"${BINARY}" \
  --addr ":${PORT}" \
  --db "${WORKDIR}/smoke.db" \
  --signer-key "${WORKDIR}/signer.key" \
  --session-secret "${SECRET}" \
  >"${WORKDIR}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 50); do
  curl -fsS "${BASE}/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${BASE}/healthz" >/dev/null || { cat "${WORKDIR}/server.log"; fail "server never became healthy"; }
ok "server is healthy"

CARD="$(curl -fsS "${BASE}/.well-known/agent-card.json")"
grep -q "$AGP_URI" <<<"${CARD}" || fail "agent card does not advertise the AGP extension"
grep -q '"agent_role":"gateway"' <<<"${CARD}" || fail "agent card does not declare the gateway role"
grep -q '"supported_agp_versions":\["1.0"\]' <<<"${CARD}" || fail "agent card does not declare AGP 1.0"
ok "agent card declares the AGP gateway extension"

python3 -u - "${BASE}" "${WORKDIR}" <<'PY'
import base64, json, os, subprocess, sys, tempfile, urllib.error, urllib.request

base, workdir = sys.argv[1], sys.argv[2]
rpc_url = os.environ.get("SMOKE_RPC_URL", "")
cluster = os.environ.get("SMOKE_CLUSTER", "")
usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def b58encode(raw):
    num = int.from_bytes(raw, "big")
    out = ""
    while num > 0:
        num, rem = divmod(num, 58)
        out = alphabet[rem] + out
    return alphabet[0] * (len(raw) - len(raw.lstrip(b"\0"))) + out


def sign(key_path, message, workdir, tag):
    """openssl pkeyutl needs a file for Ed25519 one-shot signing."""
    message_path = "%s/%s.msg" % (workdir, tag)
    with open(message_path, "wb") as handle:
        handle.write(message)
    return subprocess.run(
        ["openssl", "pkeyutl", "-sign", "-inkey", key_path, "-rawin", "-in", message_path],
        check=True, capture_output=True,
    ).stdout


def call(path, payload=None, method="POST", token=None, expect=200):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as resp:
            status, body = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        status, body = err.code, err.read()
    parsed = json.loads(body) if body else {}
    if status != expect:
        raise SystemExit("FAIL: %s %s = %d (want %d): %s" % (method, path, status, expect, body))
    return parsed


def make_agent(name, price):
    """Register an agent with a real Ed25519 key via challenge-response auth."""
    key_path = "%s/%s.pem" % (workdir, name)
    subprocess.run(
        ["openssl", "genpkey", "-algorithm", "ed25519", "-out", key_path],
        check=True, capture_output=True,
    )
    der = subprocess.run(
        ["openssl", "pkey", "-in", key_path, "-outform", "DER"],
        check=True, capture_output=True,
    ).stdout
    pub = subprocess.run(
        ["openssl", "pkey", "-in", key_path, "-pubout", "-outform", "DER"],
        check=True, capture_output=True,
    ).stdout[-32:]
    agent_id = b58encode(pub)

    issued = call("/v1/auth/challenge", {"agentId": agent_id}, expect=201)
    message = (
        "vtessera/auth/v1\nchallenge:%s\nagent:%s\nnonce:%s"
        % (issued["challengeId"], agent_id, issued["nonce"])
    ).encode()
    signature = sign(key_path, message, workdir, name)
    session = call("/v1/auth/verify", {
        "challengeId": issued["challengeId"],
        "signature": base64.b64encode(signature).decode(),
    })
    token = session["token"]

    call("/v1/agents/%s/card" % agent_id, {
        "name": name,
        "description": "smoke test agent",
        "url": "https://%s.example.com" % name,
        "version": "0.1.0",
        "publicKey": agent_id,
        "currencies": [usdc],
        "skills": [{"id": "summarize", "name": "Summarize"}],
    }, method="PUT", token=token)

    offer = call("/v1/agents/%s/offers" % agent_id, {
        "direction": "ask",
        "description": "summarize a document",
        "capabilities": ["summarize:document"],
        "priceAmount": price,
        "priceMint": usdc,
        "settlementModes": ["offchain", "onchain"],
    }, token=token, expect=201)
    return {"id": agent_id, "token": token, "offer": offer["id"], "price": price}


cheap = make_agent("cheap", "2.00")
pricey = make_agent("pricey", "9.00")
print("  ok two agents registered offers via challenge-response auth")

routed = call("/agp/route", {
    "jsonrpc": "2.0", "id": 1, "method": "agp/route_intent",
    "params": {"target_capability": "summarize:document", "payload": {"doc": "a"}},
})
assert routed["result"]["route"]["agent_id"] == cheap["id"], routed
assert routed["result"]["route"]["cost_amount"] == "2.00", routed
assert routed["result"]["considered"] == 2, routed
print("  ok AGP routed to the cheapest compliant agent")

violation = call("/agp/route", {
    "jsonrpc": "2.0", "id": 2, "method": "agp/route_intent",
    "params": {
        "target_capability": "summarize:document",
        "payload": {},
        "policy_constraints": {"currencies": ["not-a-real-mint"]},
    },
})
assert violation["error"]["code"] == -32201, violation
print("  ok AGP returned -32201 for an unsatisfiable policy")

missing = call("/agp/route", {
    "jsonrpc": "2.0", "id": 3, "method": "agp/route_intent",
    "params": {"target_capability": "does:not:exist", "payload": {}},
})
assert missing["error"]["code"] == -32200, missing
print("  ok AGP returned -32200 for an unknown capability")

trade = call("/v1/trades", {"offerId": cheap["offer"], "settlementMode": "offchain"}, token=pricey["token"], expect=201)
tid = trade["id"]
call("/v1/trades/%s/negotiate" % tid, {}, token=pricey["token"])
call("/v1/trades/%s/accept" % tid, {}, token=cheap["token"])
call("/v1/trades/%s/accept" % tid, {}, token=pricey["token"])
recorded = call("/v1/trades/%s/record" % tid, {}, token=pricey["token"])
tessera = recorded["tessera"]
assert tessera["jws"].count(".") == 2, tessera
assert tessera["claims"]["trade"]["amount"] == "2.00", tessera
assert tessera["claims"]["settlement"]["ledgerEntryHash"], tessera
print("  ok trade recorded and a signed virtual tessera issued")

ledger = call("/v1/ledger", None, method="GET")
assert len(ledger["entries"]) == 1, ledger
assert ledger["entries"][0]["prevHash"] == ledger["genesis"], ledger
print("  ok ledger anchored to the genesis hash")

metrics = call("/v1/metrics", None, method="GET")
assert metrics["totals"]["delivered"] == 1, metrics
assert metrics["totals"]["disputed"] == 0, metrics
assert metrics["totals"]["cancelled"] == 0, metrics
assert metrics["totals"]["consumers"] == 1, metrics
assert metrics["totals"]["services"] == 1, metrics
assert len(metrics["agents"]) == 1, metrics
assert metrics["agents"][0]["delivered"] == 1, metrics
assert metrics["asOf"], metrics
print("  ok public metrics count the delivery without a session")

if rpc_url:
    tokens = call("/v1/tokens", None, method="GET")
    listed = {token["address"]: token for token in tokens["tokens"]}
    assert usdc in listed, tokens
    assert listed[usdc]["decimals"] == 6, tokens
    # A mint address names an account on one chain, so the list has to say which
    # chain it is describing or it reintroduces the ambiguity the cluster work
    # exists to remove.
    assert tokens["cluster"] == cluster, tokens
    assert all(t["cluster"] == cluster for t in tokens["tokens"]), tokens
    assert tokens["settlementFee"]["lamports"] == 1000, tokens
    assert tokens["settlementFee"]["payer"] == "buyer", tokens
    print("  ok governed tokens and the exact buyer-paid fee are published")

    # The agents are Ed25519 keypairs, which are also valid Solana keypairs, so
    # the buyer can fund and sign. The flow itself is covered by make test-solana.
    chain_trade = call(
        "/v1/trades",
        {"offerId": pricey["offer"], "settlementMode": "onchain"},
        token=cheap["token"],
        expect=201,
    )
    call("/v1/trades/%s/negotiate" % chain_trade["id"], {}, token=cheap["token"])
    call("/v1/trades/%s/accept" % chain_trade["id"], {}, token=pricey["token"])
    call("/v1/trades/%s/accept" % chain_trade["id"], {}, token=cheap["token"])
    issued = call(
        "/v1/trades/%s/settlement" % chain_trade["id"], None,
        method="POST", token=cheap["token"], expect=201,
    )
    settlement = issued["settlement"]
    assert settlement["unsignedTx"], issued
    assert settlement["feeLamports"] == 500000, issued
    assert issued["trade"]["state"] == "settlement_pending", issued
    assert "signature" not in settlement, "the service must never return a signature"
    print("  ok buyer received an unsigned settlement transaction it must sign")

    # A live request blocks cancellation, because the blockhash may be broadcast.
    blocked = call(
        "/v1/trades/%s/cancel" % chain_trade["id"], {"reason": "changed my mind"},
        method="POST", token=cheap["token"], expect=409,
    )
    assert blocked["code"] == "SETTLEMENT_IN_PROGRESS", blocked
    print("  ok cancellation refused while a settlement transaction is live")

    # A signature that was never submitted is pending, never a dispute.
    unseen = "4srenKHRoQyr5x3pwjEZLRDaooLTUFKShgd1qi1sboPENZEWY6zsDwUVq9Hyuw5TjrziuhP5u4bq44pYd3GH1qVB"
    pending = call(
        "/v1/trades/%s/confirm" % chain_trade["id"], {"signature": unseen},
        method="POST", token=cheap["token"], expect=202,
    )
    assert pending["code"] == "SETTLEMENT_PENDING", pending
    still = call("/v1/trades/%s" % chain_trade["id"], None, method="GET", token=cheap["token"])
    assert still["state"] == "settlement_pending", still
    print("  ok an unseen signature is pending, not disputed")

    garbage = call(
        "/v1/trades/%s/confirm" % chain_trade["id"], {"signature": "not-a-signature"},
        method="POST", token=cheap["token"], expect=400,
    )
    assert garbage["code"] == "INVALID_SIGNATURE", garbage
    print("  ok a malformed signature is a 400, not an internal error")
else:
    rejected = call(
        "/v1/trades",
        {"offerId": pricey["offer"], "settlementMode": "onchain"},
        token=cheap["token"],
        expect=501,
    )
    assert rejected["code"] == "ONCHAIN_UNAVAILABLE", rejected
    print("  ok on-chain settlement is refused with 501 when no RPC endpoint is configured")
PY
# Replaying a consumed challenge must fail.
python3 -u - "${BASE}" "${WORKDIR}" <<'PY'
import base64, json, os, subprocess, sys, urllib.error, urllib.request

base, workdir = sys.argv[1], sys.argv[2]
alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def b58encode(raw):
    num = int.from_bytes(raw, "big")
    out = ""
    while num > 0:
        num, rem = divmod(num, 58)
        out = alphabet[rem] + out
    return alphabet[0] * (len(raw) - len(raw.lstrip(b"\0"))) + out


key_path = "%s/replay.pem" % workdir
subprocess.run(["openssl", "genpkey", "-algorithm", "ed25519", "-out", key_path], check=True, capture_output=True)
pub = subprocess.run(
    ["openssl", "pkey", "-in", key_path, "-pubout", "-outform", "DER"],
    check=True, capture_output=True,
).stdout[-32:]
agent_id = b58encode(pub)


def sign(key_path, message, workdir, tag):
    """openssl pkeyutl needs a file for Ed25519 one-shot signing."""
    message_path = "%s/%s.msg" % (workdir, tag)
    with open(message_path, "wb") as handle:
        handle.write(message)
    return subprocess.run(
        ["openssl", "pkeyutl", "-sign", "-inkey", key_path, "-rawin", "-in", message_path],
        check=True, capture_output=True,
    ).stdout


def post(path, payload, expect):
    req = urllib.request.Request(
        base + path, data=json.dumps(payload).encode(), method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as resp:
            status, body = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        status, body = err.code, err.read()
    if status != expect:
        raise SystemExit("FAIL: POST %s = %d (want %d): %s" % (path, status, expect, body))
    return json.loads(body)


issued = post("/v1/auth/challenge", {"agentId": agent_id}, 201)
message = ("vtessera/auth/v1\nchallenge:%s\nagent:%s\nnonce:%s"
           % (issued["challengeId"], agent_id, issued["nonce"])).encode()
signature = sign(key_path, message, workdir, "replay")
payload = {"challengeId": issued["challengeId"], "signature": base64.b64encode(signature).decode()}
post("/v1/auth/verify", payload, 200)
post("/v1/auth/verify", payload, 409)
print("  ok a consumed challenge cannot be replayed")
PY

echo "SMOKE OK"
