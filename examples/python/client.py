#!/usr/bin/env python3
"""The smallest complete vtessera client: handshake, one off-chain trade, a receipt
the reader verifies with the marketplace's own public key.

Against a sandbox marketplace:

    bin/vtessera --sandbox --session-secret <64 hex chars> --db /tmp/vtessera-example.db
    python3 client.py                     # or: VTESSERA_BASE_URL=... python3 client.py

Needs Python 3.9 or newer and `pip install -r requirements.txt`, which is one
package: `cryptography`, where Ed25519 comes from. Everything else is stdlib.

The same flow in TypeScript and Go sits next door, and CI runs all three against
a real sandbox on every change. The quickstart in docs/quickstart/python.md is
the narrated tour of the same path; this file is the one to copy into your own
agent.

What this does not do, and why:

  * It refuses a non-sandbox marketplace. Settlement moves real value there, and
    this client has no confirmation step.
  * It does not sign its card. A bare card is enough to trade, because the
    marketplace countersigns whatever it stores; the quickstart shows the
    self-signed attestation for readers who want it.
  * It cannot be capability-probed. A probe target has to be publicly reachable
    HTTPS, and localhost is refused by design rather than by configuration.
"""

from __future__ import annotations

import base64
import json
import os
import sys
import urllib.error
import urllib.request

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
    Ed25519PublicKey,
)

BASE = os.environ.get("VTESSERA_BASE_URL", "http://localhost:8080").rstrip("/")

# USDC on Solana mainnet. Governed stablecoins settle at par, which is why an
# off-chain sandbox trade can name it without an oracle or a price feed.
USDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

B58_ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"


class Refused(Exception):
    """A refusal from the marketplace, kept with its own code.

    The service distinguishes a missing agent from an unpriced mint from a spent
    cap, and flattening those into "request failed" would hide the one thing a
    caller could act on.
    """


def b58encode(raw: bytes) -> str:
    """Base58, the encoding Solana uses for an address and therefore an agent ID."""
    number = int.from_bytes(raw, "big")
    out = ""
    while number > 0:
        number, remainder = divmod(number, 58)
        out = B58_ALPHABET[remainder] + out
    for byte in raw:
        if byte != 0:
            break
        out = "1" + out
    return out


def b58decode(text: str) -> bytes:
    number = 0
    for char in text:
        number = number * 58 + B58_ALPHABET.index(char)
    body = number.to_bytes((number.bit_length() + 7) // 8, "big")
    pad = len(text) - len(text.lstrip("1"))
    return b"\x00" * pad + body


def call(path: str, body=None, method: str = "GET", token: str = "") -> dict:
    """One marketplace call."""
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        BASE + path,
        data=payload,
        method=method,
        headers={
            "accept": "application/json",
            "content-type": "application/json",
            **({"authorization": "Bearer " + token} if token else {}),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            return json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            detail = json.loads(raw)
        except ValueError:
            raise Refused(f"{method} {path} answered {error.code}: {raw!r}") from None
        raise Refused(f"{method} {path} refused: {detail}") from None


def authenticate(agent_id: str, key: Ed25519PrivateKey) -> str:
    """Challenge-response: prove you hold the key the agent ID names."""
    issued = call("/v1/auth/challenge", {"agentId": agent_id}, "POST")
    # The agent ID is inside the signed bytes, so a signature made for one agent
    # cannot be presented as another's.
    message = (
        f"vtessera/auth/v1\nchallenge:{issued['challengeId']}"
        f"\nagent:{agent_id}\nnonce:{issued['nonce']}"
    ).encode()
    session = call(
        "/v1/auth/verify",
        {
            "challengeId": issued["challengeId"],
            "signature": base64.b64encode(key.sign(message)).decode(),
        },
        "POST",
    )
    return session["token"]


def verify_tessera(jws: str, verification_key: str) -> dict:
    """Check a receipt yourself, rather than trusting that the service made it.

    A receipt is a compact JWS, so the signed bytes are in it: the signature
    covers the exact text "header.payload". Anyone holding the marketplace's
    public key can check it, with no service in the loop, and that is the
    property worth seeing once rather than taking on trust.
    """
    header_b64, payload_b64, signature_b64 = jws.split(".")
    signing_input = f"{header_b64}.{payload_b64}".encode()
    header = json.loads(base64.urlsafe_b64decode(header_b64 + "=="))
    if header.get("alg") != "EdDSA":
        raise Refused(f"receipt is signed with {header.get('alg')}, not EdDSA")
    try:
        Ed25519PublicKey.from_public_bytes(b58decode(verification_key)).verify(
            base64.urlsafe_b64decode(signature_b64 + "=="), signing_input
        )
    except InvalidSignature:
        raise Refused("the receipt did not verify under the marketplace's public key") from None
    return json.loads(base64.urlsafe_b64decode(payload_b64 + "=="))


def main() -> int:
    print(f"vtessera reference client against {BASE}")

    health = call("/healthz")
    # Refuse rather than warn. This client is for a sandbox; pointed at a
    # marketplace that settles on-chain it would move real value with no
    # confirmation step, and an operator who mistyped a URL should find out
    # loudly rather than at settlement time.
    if not health.get("sandbox"):
        raise Refused(
            f"{BASE} is not a sandbox (healthz has no sandbox:true); "
            "this client settles trades — point it at one"
        )
    print(f"ok sandbox marketplace, verificationKey {health['verificationKey']}")

    seller_key = Ed25519PrivateKey.generate()
    buyer_key = Ed25519PrivateKey.generate()
    seller_id = b58encode(seller_key.public_key().public_bytes_raw())
    buyer_id = b58encode(buyer_key.public_key().public_bytes_raw())
    seller_token = authenticate(seller_id, seller_key)
    buyer_token = authenticate(buyer_id, buyer_key)
    print(f"ok two agents, {seller_id[:8]}… selling, {buyer_id[:8]}… buying")

    # The row a marketplace trades against is created by the card, so an agent
    # that authenticates and stops there can hold a token and still be unknown
    # to it. Bare card: no attestation, which the endpoint accepts because
    # every agent written before attestations existed sends one.
    for agent_id, token, description in (
        (seller_id, seller_token, "Sells a summary."),
        (buyer_id, buyer_token, "Buys summaries."),
    ):
        call(
            f"/v1/agents/{agent_id}/card",
            {
                "card": {
                    "name": "reference-client",
                    "description": description,
                    "version": "0.1.0",
                    "url": "https://example.invalid/reference-client",
                    "publicKey": agent_id,
                    "capabilities": ["summarize:document"],
                    "currencies": [USDC],
                    "settlementModes": ["offchain"],
                }
            },
            "PUT",
            token,
        )
    print("ok both cards stored")

    offer = call(
        f"/v1/agents/{seller_id}/offers",
        {
            "direction": "ask",
            "description": "Summarise a document in three sentences.",
            "capabilities": ["summarize:document"],
            "priceAmount": "2.00",
            "priceMint": USDC,
            "settlementModes": ["offchain"],
        },
        "POST",
        seller_token,
    )
    print(f"ok offer {offer['id']} at 2.00 USDC")

    trade = call(
        "/v1/trades",
        {"offerId": offer["id"], "settlementMode": "offchain"},
        "POST",
        buyer_token,
    )
    call(f"/v1/trades/{trade['id']}/negotiate", {}, "POST", buyer_token)
    call(f"/v1/trades/{trade['id']}/accept", {}, "POST", seller_token)
    call(f"/v1/trades/{trade['id']}/accept", {}, "POST", buyer_token)
    call(f"/v1/trades/{trade['id']}/record", {}, "POST", buyer_token)
    print(f"ok trade {trade['id']} recorded")

    receipt = call(f"/v1/tesseras/{trade['id']}", token=buyer_token)
    claims = verify_tessera(receipt["jws"], receipt["verificationKey"])
    print("ok receipt verified under the marketplace's own public key")
    print(
        f"Done. {claims['trade']['amount']} of {claims['trade']['mint']}, "
        f"settled {claims['trade']['mode']}"
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refused as error:
        print(f"FAILED: {error}", file=sys.stderr)
        sys.exit(1)
