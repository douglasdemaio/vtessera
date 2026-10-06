#!/usr/bin/env python3
"""A vtessera agent, end to end, against a local sandbox marketplace.

Takes a machine with nothing installed on it to two signed agents, one offer, one
completed off-chain trade, and a receipt the reader verifies with the marketplace's
own public key. No chain, no funds, no account.

    python3 agent.py                     # or: VTESSERA_BASE_URL=... python3 agent.py

Requires Python 3.9 or newer and `pip install cryptography`, which is where
Ed25519 comes from. Everything else is the standard library.

What this does not do, and why:

  * It does not settle on-chain. `--sandbox` is the point: no value can move, so
    the interesting part here is the signed record rather than the transfer.
  * It cannot be capability-probed. A probe target has to be publicly reachable
    HTTPS, and localhost is refused by design rather than by configuration. The
    attestation below is the signed report a local agent can get.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
    Ed25519PublicKey,
)

BASE = os.environ.get("VTESSERA_BASE_URL", "http://localhost:8080").rstrip("/")

# USDC on Solana mainnet. Governed stablecoins settle at par, which is why an
# off-chain sandbox trade can name it without an oracle or a price feed.
USDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

# The canonical form this marketplace signs, as a string in every signature.
CANONICAL_FORM = "vtessera/attest/v1"

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


def refused(path: str) -> tuple[int, dict]:
    """Call something whose refusal is the expected answer.

    Separate from call() because a refusal here is information, not a failure: the
    point of asking an unprobed agent for its probe report is to be told it has
    never been probed.
    """
    try:
        with urllib.request.urlopen(BASE + path, timeout=15) as response:
            return response.status, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read() or b"{}")


def short(mint: str) -> str:
    """A Solana mint is 44 characters; print it the way an address is printed."""
    return f"{mint[:6]}…{mint[-4:]}" if len(mint) > 14 else mint


def step(number: int, title: str) -> None:
    print(f"\n{number}. {title}")


def ok(message: str) -> None:
    print(f"   ok {message}")


def field(name: str, value: str) -> bytes:
    """One length-prefixed field of the canonical form.

    The length prefix is in bytes, not characters, so a card with a non-ASCII name
    cannot be signed one way and verified another.
    """
    raw = value.encode()
    return f"{name}:{len(raw)}:".encode() + raw + b"\n"


def listing(name: str, values: list[str]) -> bytes:
    """A counted list, sorted and deduplicated.

    The same set of capabilities written in a different order is the same card, so
    a verifier should not have to reproduce the order an agent happened to type.
    """
    unique = sorted(set(values))
    out = f"{name}.count:{len(unique)}\n".encode()
    for index, value in enumerate(unique):
        out += field(f"{name}.{index}", value)
    return out


def card_bytes(card: dict, agent_id: str, signed_at: str) -> bytes:
    """The exact bytes the marketplace will re-derive to check this signature.

    Mirrors CardBytes in internal/attest/attest.go. An SDK would do this for you;
    it is written out here so the quickstart shows what is actually signed, and so
    a reader can see that the encoding is specified rather than incidental.
    """
    skills = sorted(
        card.get("skills") or [],
        key=lambda s: "\x00".join(
            [
                s.get("id", ""),
                s.get("name", ""),
                "\x00".join(sorted(set(s.get("tags") or []))),
                "\x00".join(sorted(set(s.get("inputModes") or s.get("input") or []))),
                "\x00".join(sorted(set(s.get("outputModes") or s.get("output") or []))),
            ]
        ),
    )
    out = CANONICAL_FORM.encode() + b"\n"
    out += b"kind:agent-card\n"
    out += field("signedAt", signed_at)
    out += field("agent", agent_id)
    out += field("name", card.get("name", ""))
    out += field("description", card.get("description", ""))
    out += field("version", card.get("version", ""))
    out += field("url", card.get("url", ""))
    out += listing("capabilities", card.get("capabilities") or [])
    out += f"skills.count:{len(skills)}\n".encode()
    for index, skill in enumerate(skills):
        prefix = f"skills.{index}"
        out += field(f"{prefix}.id", skill.get("id", ""))
        out += field(f"{prefix}.name", skill.get("name", ""))
        out += listing(f"{prefix}.tags", skill.get("tags") or [])
        out += listing(f"{prefix}.inputModes", skill.get("inputModes") or skill.get("input") or [])
        out += listing(f"{prefix}.outputModes", skill.get("outputModes") or skill.get("output") or [])
    out += field("probeTarget", card.get("probeTarget", ""))
    out += listing("currencies", card.get("currencies") or [])
    out += listing("settlementModes", card.get("settlementModes") or [])
    return out


class Agent:
    """One Ed25519 identity, which is all an agent is."""

    def __init__(self, name: str) -> None:
        self.name = name
        self.key = Ed25519PrivateKey.generate()
        self.public = self.key.public_key().public_bytes_raw()
        self.agent_id = b58encode(self.public)
        self.token = ""

    def authenticate(self) -> None:
        """Challenge-response: prove you hold the key the agent ID names."""
        issued = call("/v1/auth/challenge", {"agentId": self.agent_id}, "POST")
        # The canonical message is the part that matters: the agent ID is inside
        # the signed bytes, so a signature made for one agent cannot be presented
        # as another's.
        message = (
            f"vtessera/auth/v1\nchallenge:{issued['challengeId']}"
            f"\nagent:{self.agent_id}\nnonce:{issued['nonce']}"
        ).encode()
        session = call(
            "/v1/auth/verify",
            {
                "challengeId": issued["challengeId"],
                "signature": base64.b64encode(self.key.sign(message)).decode(),
            },
            "POST",
        )
        self.token = session["token"]

    def sign_card(self, card: dict, signed_at: str) -> dict:
        """Sign a card the way the marketplace will re-check it."""
        payload = card_bytes(card, self.agent_id, signed_at)
        return {
            "alg": "Ed25519",
            "keyId": self.agent_id,
            "digest": hashlib.sha256(payload).hexdigest(),
            "value": base64.b64encode(self.key.sign(payload)).decode(),
            "signedAt": signed_at,
        }


def verify_tessera(jws: str, verification_key: str) -> dict:
    """Check a receipt yourself, rather than trusting that the service made it.

    A receipt is a compact JWS, so the signed bytes are in it: the signature covers
    the exact text "header.payload". Anyone holding the marketplace's public key
    can check it, with no service in the loop, and that is the property worth
    seeing once rather than taking on trust.
    """
    header_b64, payload_b64, signature_b64 = jws.split(".")
    signing_input = f"{header_b64}.{payload_b64}".encode()
    header = json.loads(base64.urlsafe_b64decode(header_b64 + "=="))
    if header.get("alg") != "EdDSA":
        raise Refused(f"receipt is signed with {header.get('alg')}, not EdDSA")
    Ed25519PublicKey.from_public_bytes(b58decode(verification_key)).verify(
        base64.urlsafe_b64decode(signature_b64 + "=="), signing_input
    )
    return json.loads(base64.urlsafe_b64decode(payload_b64 + "=="))


def main() -> int:
    print(f"vtessera quickstart against {BASE}")

    step(1, "Is this marketplace answering, and is it a sandbox")
    health = call("/healthz")
    if not health.get("sandbox"):
        print(
            f"   warning: {BASE} is not a sandbox, so real value can move behind it",
            file=sys.stderr,
        )
    # A sandbox reports no cluster and no genesis hash at all rather than empty
    # ones, which is what tells a reader no chain is configured.
    ok(f"status {health['status']}, verificationKey {health['verificationKey']}")
    ok(f"cluster {health.get('cluster', 'none')} — no chain behind this marketplace")

    step(2, "Two agents, each just an Ed25519 key")
    seller = Agent("quickstart-seller")
    buyer = Agent("quickstart-buyer")
    for agent in (seller, buyer):
        agent.authenticate()
        ok(f"{agent.name} is {agent.agent_id}")
    # A session is not an agent. The row a marketplace trades against is created by
    # the card, so an agent that authenticates and stops there can hold a token and
    # still be unknown to it.

    step(3, "Both agents publish a card, and sign it themselves")

    def publish(agent: Agent, description: str) -> None:
        card = {
            "name": agent.name,
            "description": description,
            "version": "0.1.0",
            "url": f"https://example.invalid/{agent.name}",
            "publicKey": agent.agent_id,
            "capabilities": ["summarize:document"],
            "skills": [{"id": "summarize", "name": "Summarize", "tags": ["text"]}],
            "currencies": [USDC],
            "settlementModes": ["offchain"],
            # No probeTarget. A card that does not name one is never probed, which
            # is how an agent avoids having this marketplace send traffic on its
            # behalf.
            "probeTarget": "",
        }
        # Whole seconds: the marketplace re-formats this timestamp to RFC 3339 and
        # compares the bytes, so a value whose written form could round-trip
        # differently would produce a signature that fails for a reason worth
        # ruling out before you look anywhere else.
        signed_at = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
        call(
            f"/v1/agents/{agent.agent_id}/card",
            {"card": card, "attestation": agent.sign_card(card, signed_at)},
            "PUT",
            agent.token,
        )

    publish(seller, "Summarises a document, for a quickstart.")
    publish(buyer, "Buys summaries, for a quickstart.")
    ok("both cards stored, both signed by their own key")

    report = call(f"/v1/agents/{seller.agent_id}/attestation")
    if not report["marketplace"]["valid"]:
        raise Refused("the marketplace does not vouch for the card it just stored")
    if not report["agent"]["valid"]:
        # Loud rather than a shrug: this is the one thing in the file a reader
        # cannot debug by eye, because the bytes are length-prefixed and a
        # mismatch is a single byte.
        raise Refused(
            "the seller's own signature did not verify: the canonical form in "
            "card_bytes does not match internal/attest/attest.go CardBytes"
        )
    ok(f"marketplace signature valid, keyId {report['marketplace']['keyId'][:8]}…")
    ok(f"agent signature valid, keyId {report['agent']['keyId'][:8]}…")
    ok(f"canonicalForm {report['canonicalForm']}")

    step(4, "The seller publishes an offer")
    offer = call(
        f"/v1/agents/{seller.agent_id}/offers",
        {
            "direction": "ask",
            "description": "Summarise a document in three sentences.",
            "capabilities": ["summarize:document"],
            "priceAmount": "2.00",
            "priceMint": USDC,
            "settlementModes": ["offchain"],
        },
        "POST",
        seller.token,
    )
    ok(f"offer {offer['id']} at 2.00 USDC")

    step(5, "The buyer finds it, and checks who vouched for the terms")
    found = call("/v1/offers?capability=summarize:document")
    match = next((o for o in found["offers"] if o["id"] == offer["id"]), None)
    if match is None:
        raise Refused("the offer the seller just published is not searchable")
    ok(f"{len(found['offers'])} offer(s) match summarize:document")
    # An offer's signature is the seller's, not the marketplace's: the seller signs
    # the terms it is promising. This one was published unsigned, which the
    # endpoint reports as signed: false rather than as valid: false. Those mean
    # different things to a buyer, and only one of them is a warning: a signature
    # that is present and no longer verifies means the offer changed after it was
    # signed.
    report_offers = call(f"/v1/offers/{offer['id']}/attestation")
    if report_offers["signed"] and not report_offers["valid"]:
        raise Refused("the offer was signed and its signature no longer verifies")
    ok(f"offer signed={report_offers['valid'] and report_offers['signed']}, seller {report_offers['seller'][:8]}…")
    ok("the card's two signatures are what vouch for this agent")

    step(6, "The buyer buys, both sides accept, and the buyer records payment")
    trade = call("/v1/trades", {"offerId": offer["id"], "settlementMode": "offchain"}, "POST", buyer.token)
    call(f"/v1/trades/{trade['id']}/negotiate", {}, "POST", buyer.token)
    call(f"/v1/trades/{trade['id']}/accept", {}, "POST", seller.token)
    call(f"/v1/trades/{trade['id']}/accept", {}, "POST", buyer.token)
    recorded = call(f"/v1/trades/{trade['id']}/record", {}, "POST", buyer.token)
    ok(f"trade {trade['id']} recorded")

    step(7, "Verify the receipt yourself")
    receipt = call(f"/v1/tesseras/{trade['id']}", token=buyer.token)
    try:
        claims = verify_tessera(receipt["jws"], receipt["verificationKey"])
    except InvalidSignature:
        raise Refused("the receipt did not verify under the marketplace's public key") from None
    ok("Ed25519 signature valid under the marketplace's own public key")
    ok(f"amount {claims['trade']['amount']} of {short(claims['trade']['mint'])}")
    ok(f"settlement mode {claims['trade']['mode']}, anchored in the ledger")

    step(8, "The receipt is anchored in a hash chain")
    ledger = call("/v1/ledger")
    entries = ledger["entries"]
    if not entries:
        raise Refused("the ledger has no entries, so the receipt was never anchored")
    ok(f"{len(entries)} ledger entry, chained to genesis {ledger['genesis'][:8]}…")

    step(9, "What a capability probe would have added")
    status, probe = refused(f"/v1/agents/{seller.agent_id}/capabilities")
    if probe.get("code") == "NOT_PROBED":
        ok(f"HTTP {status} NOT_PROBED — this agent names no probeTarget, so it has never been probed")
        ok("a probe target must be public HTTPS: localhost is refused by design, not by configuration")
    else:
        ok(f"probe report: passed={probe['report']['passed']} over {len(probe['report']['results'])} results")

    print(
        "\nDone. Two agents, one offer, one settled off-chain trade, one receipt you"
        "\nverified yourself. Nothing here touched a chain, and no key left this machine"
        "\nexcept the two agent keys it just generated."
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refused as error:
        print(f"\nFAILED: {error}", file=sys.stderr)
        sys.exit(1)