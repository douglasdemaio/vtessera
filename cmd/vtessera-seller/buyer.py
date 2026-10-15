#!/usr/bin/env python3
"""Buy one code:explain from the vtessera standing seller, end to end.

No dependencies beyond the Python standard library: Ed25519 and Base58 are
implemented in this file, so `python3 buyer.py` runs on a machine with nothing
installed. That is deliberate. The whole point of the receipt is that a
counterparty can produce and check it without trusting a vendor's SDK.

What it does:

  1. mints a fresh Ed25519 identity -- your agent ID *is* the public key,
  2. authenticates, then publishes a card so the marketplace knows you,
  3. finds the seller's open offer,
  4. creates and negotiates a trade, then asks the seller to accept it,
  5. accepts, records, prints the signed receipt,
  6. verifies the receipt against the marketplace's own public key.

The settlement is off-chain, so no funds move and no chain is touched: the
artifact is the Ed25519-signed receipt, anchored in the marketplace's ledger.

    python3 buyer.py

Environment overrides: VTESSERA_BASE_URL, VTESSERA_SELLER_ID,
VTESSERA_SELLER_TASK, VTESSERA_CAPABILITY.
"""

import base64
import hashlib
import json
import os
import secrets
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("VTESSERA_BASE_URL", "https://vtessera.fly.dev").rstrip("/")
SELLER_ID = os.environ.get("VTESSERA_SELLER_ID", "3XCNhdcbYom1Vgii9J6Yc5gcAVfzsbhn6XqPLKYf2tAo")
SELLER_TASK = os.environ.get("VTESSERA_SELLER_TASK", "https://vtessera-seller.fly.dev/task")
USDC = os.environ.get("VTESSERA_USDC", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
CAP = os.environ.get("VTESSERA_CAPABILITY", "code:explain")

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


def b58encode(raw):
    n = int.from_bytes(raw, "big")
    out = ""
    while n > 0:
        n, r = divmod(n, 58)
        out = B58[r] + out
    for b in raw:
        if b != 0:
            break
        out = "1" + out
    return out


def b58decode(text):
    n = 0
    for ch in text:
        n = n * 58 + B58.index(ch)
    body = n.to_bytes((n.bit_length() + 7) // 8, "big")
    return b"\x00" * (len(text) - len(text.lstrip("1"))) + body


# --- Ed25519 (the reference implementation) ---------------------------------

_p = 2 ** 255 - 19
_d = -121665 * pow(121666, _p - 2, _p) % _p
_q = 2 ** 252 + 27742317777372353535851937790883648493
_gx = 15112221349535400772501151409588531511454012693041857206046113283949847762202
_gy = 46316835694926478169428394003475163141307993866256225615783033603165251855960
_g = (_gx, _gy, 1, _gx * _gy % _p)


def _add(P, Q):
    A = (P[1] - P[0]) * (Q[1] - Q[0]) % _p
    B = (P[1] + P[0]) * (Q[1] + Q[0]) % _p
    C = 2 * P[3] * Q[3] * _d % _p
    D = 2 * P[2] * Q[2] % _p
    E, F, G, H = B - A, D - C, D + C, B + A
    return (E * F, G * H, F * G, E * H)


def _mul(s, P):
    Q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            Q = _add(Q, P)
        P = _add(P, P)
        s >>= 1
    return Q


def _compress(P):
    zinv = pow(P[2], _p - 2, _p)
    x = P[0] * zinv % _p
    y = P[1] * zinv % _p
    return int.to_bytes(y | ((x & 1) << 255), 32, "little")


def _recover_x(y, sign):
    if y >= _p:
        return None
    x2 = (y * y - 1) * pow(_d * y * y + 1, _p - 2, _p)
    if x2 == 0:
        return 0 if sign == 0 else None
    x = pow(x2, (_p + 3) // 8, _p)
    if (x * x - x2) % _p != 0:
        x = x * pow(2, (_p - 1) // 4, _p) % _p
    if (x * x - x2) % _p != 0:
        return None
    if (x & 1) != sign:
        x = _p - x
    return x


def _decompress(s):
    if len(s) != 32:
        return None
    y = int.from_bytes(s, "little")
    sign = y >> 255
    y &= (1 << 255) - 1
    x = _recover_x(y, sign)
    if x is None:
        return None
    return (x, y, 1, x * y % _p)


def _equal(P, Q):
    if (P[0] * Q[2] - Q[0] * P[2]) % _p != 0:
        return False
    return (P[1] * Q[2] - Q[1] * P[2]) % _p == 0


def _expand(secret):
    h = hashlib.sha512(secret).digest()
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    a |= 1 << 254
    return a, h[32:]


def ed_public(secret):
    a, _ = _expand(secret)
    return _compress(_mul(a, _g))


def ed_sign(secret, msg):
    a, prefix = _expand(secret)
    A = _compress(_mul(a, _g))
    r = int.from_bytes(hashlib.sha512(prefix + msg).digest(), "little") % _q
    R = _compress(_mul(r, _g))
    h = int.from_bytes(hashlib.sha512(R + A + msg).digest(), "little") % _q
    return R + int.to_bytes((r + h * a) % _q, 32, "little")


def ed_verify(public, msg, signature):
    if len(public) != 32 or len(signature) != 64:
        return False
    A = _decompress(public)
    R = _decompress(signature[:32])
    s = int.from_bytes(signature[32:], "little")
    if A is None or R is None or s >= _q:
        return False
    h = int.from_bytes(hashlib.sha512(signature[:32] + public + msg).digest(), "little") % _q
    return _equal(_mul(s, _g), _add(R, _mul(h, A)))


# --- HTTP -------------------------------------------------------------------


def call(path, body=None, method="GET", token=""):
    url = path if path.startswith("http") else BASE + path
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        url,
        data=payload,
        method=method,
        headers={
            "accept": "application/json",
            "content-type": "application/json",
            **({"authorization": "Bearer " + token} if token else {}),
            "user-agent": "vtessera-buyer/1.0",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            detail = json.loads(raw)
        except ValueError:
            detail = raw.decode("utf-8", "replace")
        raise SystemExit("FAILED: %s %s -> %s %s" % (method, url, error.code, detail))


def step(number, title):
    print("\n%d. %s" % (number, title))


def ok(message):
    print("   ok %s" % message)


def main():
    print("vtessera buyer against %s" % BASE)

    step(1, "Mint an Ed25519 identity (your agent ID is your public key)")
    secret = secrets.token_bytes(32)
    agent_id = b58encode(ed_public(secret))
    ok("agent %s" % agent_id)

    step(2, "Authenticate with a challenge-response signature")
    challenge = call("/v1/auth/challenge", {"agentId": agent_id}, "POST")
    message = (
        "vtessera/auth/v1\nchallenge:%s\nagent:%s\nnonce:%s"
        % (challenge["challengeId"], agent_id, challenge["nonce"])
    ).encode()
    session = call(
        "/v1/auth/verify",
        {
            "challengeId": challenge["challengeId"],
            "signature": base64.b64encode(ed_sign(secret, message)).decode(),
        },
        "POST",
    )
    token = session["token"]
    ok("session token acquired")

    step(3, "Publish a card so the marketplace knows you")
    card = {
        "name": "buyer-%s" % agent_id[:6],
        "description": "A buyer that purchased code:explain from the standing seller.",
        "version": "0.1.0",
        "url": "https://example.invalid/vtessera-buyer",
        "publicKey": agent_id,
        "capabilities": [CAP],
        "currencies": [USDC],
        "settlementModes": ["offchain"],
        "probeTarget": "",
    }
    call("/v1/agents/%s/card" % agent_id, {"card": card}, "PUT", token)
    ok("card stored")

    step(4, "Find the seller's open offer")
    found = call("/v1/offers?capability=%s&status=open&mint=%s" % (CAP, USDC))
    offers = [o for o in found.get("offers", []) if o.get("agentId") == SELLER_ID]
    if not offers:
        raise SystemExit(
            "FAILED: no open %s offer from %s. Board: %s/v1/offers"
            % (CAP, SELLER_ID, BASE)
        )
    offer = offers[0]
    ok("offer %s at %s USDC" % (offer["id"], offer["priceAmount"]))

    step(5, "Open the trade and negotiate")
    trade = call(
        "/v1/trades",
        {"offerId": offer["id"], "settlementMode": "offchain"},
        "POST",
        token,
    )
    trade_id = trade["id"]
    call("/v1/trades/%s/negotiate" % trade_id, {}, "POST", token)
    ok("trade %s" % trade_id)

    step(6, "Ask the seller to accept by handing it the trade id")
    accepted = call(SELLER_TASK, {"tradeId": trade_id}, "POST")
    ok("seller accepted=%s result=%s" % (accepted.get("accepted"), accepted.get("result")))

    step(7, "Accept, record, and fetch the receipt")
    call("/v1/trades/%s/accept" % trade_id, {}, "POST", token)
    call("/v1/trades/%s/record" % trade_id, {}, "POST", token)
    ok("recorded")
    receipt = call("/v1/tesseras/%s" % trade_id, token=token)
    jws = receipt["jws"]
    verification_key = receipt["verificationKey"]

    step(8, "Verify the receipt against the marketplace's public key")
    header_b64, payload_b64, signature_b64 = jws.split(".")
    signing_input = ("%s.%s" % (header_b64, payload_b64)).encode()
    header = json.loads(base64.urlsafe_b64decode(header_b64 + "=="))
    if header.get("alg") != "EdDSA":
        raise SystemExit("FAILED: receipt alg is %r, not EdDSA" % header.get("alg"))
    signature = base64.urlsafe_b64decode(signature_b64 + "==")
    if not ed_verify(b58decode(verification_key), signing_input, signature):
        raise SystemExit("FAILED: receipt did not verify under %s" % verification_key)
    claims = json.loads(base64.urlsafe_b64decode(payload_b64 + "=="))
    trade_claims = claims.get("trade", {})
    settlement = claims.get("settlement", {})
    ok("signature valid under %s" % verification_key[:12])
    ok("trade %s, amount %s, mode %s"
       % (trade_claims.get("id", trade_id), trade_claims.get("amount"), trade_claims.get("mode")))
    ok("anchored in the ledger at seq %s" % settlement.get("ledgerSequence"))

    print(
        "\nDone. You bought %s for %s USDC off-chain and hold an Ed25519 receipt"
        "\nyou verified yourself. The seller never held funds, and no chain was used."
        % (CAP, offer["priceAmount"])
    )
    print("\nRECEIPT %s" % jws)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
