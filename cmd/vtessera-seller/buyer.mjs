#!/usr/bin/env node
// Buy one code:explain from the vtessera standing seller, end to end.
//
// No dependencies beyond the Node standard library: Ed25519 comes from
// WebCrypto and Base58 is implemented below, so this runs on a machine with
// nothing installed. That is deliberate. The whole point of the receipt is that
// a counterparty can produce and check it without trusting a vendor's SDK.
//
//   curl -s https://vtessera-seller.fly.dev/buyer.mjs | node --input-type=module
//
// Environment overrides: VTESSERA_BASE_URL, VTESSERA_SELLER_ID,
// VTESSERA_SELLER_TASK, VTESSERA_CAPABILITY.

const BASE = (process.env.VTESSERA_BASE_URL || "https://vtessera.fly.dev").replace(/\/+$/, "");
const SELLER_ID = process.env.VTESSERA_SELLER_ID || "3XCNhdcbYom1Vgii9J6Yc5gcAVfzsbhn6XqPLKYf2tAo";
const SELLER_TASK = process.env.VTESSERA_SELLER_TASK || "https://vtessera-seller.fly.dev/task";
const CAP = process.env.VTESSERA_CAPABILITY || "code:explain";

const ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const subtle = globalThis.crypto.subtle;
const encoder = new TextEncoder();

function b58encode(bytes) {
  let n = 0n;
  for (const b of bytes) n = n * 256n + BigInt(b);
  let out = "";
  while (n > 0n) {
    out = ALPHABET[Number(n % 58n)] + out;
    n /= 58n;
  }
  for (const b of bytes) {
    if (b !== 0) break;
    out = "1" + out;
  }
  return out;
}

function b58decode(text) {
  let n = 0n;
  for (const ch of text) {
    const v = ALPHABET.indexOf(ch);
    if (v < 0) throw new Error("bad base58 character " + JSON.stringify(ch));
    n = n * 58n + BigInt(v);
  }
  const body = [];
  let m = n;
  while (m > 0n) {
    body.unshift(Number(m % 256n));
    m /= 256n;
  }
  const pad = text.length - text.replace(/^1+/, "").length;
  return Uint8Array.from([...new Array(pad).fill(0), ...body]);
}

async function call(path, { body = undefined, method = "GET", token = "" } = {}) {
  const url = path.startsWith("http") ? path : BASE + path;
  const headers = { accept: "application/json", "user-agent": "vtessera-buyer/1.1" };
  if (body !== undefined) headers["content-type"] = "application/json";
  if (token) headers["authorization"] = "Bearer " + token;
  const response = await fetch(url, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let parsed;
  try {
    parsed = text ? JSON.parse(text) : {};
  } catch {
    parsed = text;
  }
  if (!response.ok) {
    throw new Error(`FAILED: ${method} ${url} -> ${response.status} ${JSON.stringify(parsed)}`);
  }
  return parsed;
}

let stepNumber = 0;
function step(title) {
  console.log("\n" + ++stepNumber + ". " + title);
}
function ok(message) {
  console.log("   ok " + message);
}

async function main() {
  console.log("vtessera buyer against " + BASE);

  step("Mint an Ed25519 identity (your agent ID is your public key)");
  const keys = await subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  const publicBytes = new Uint8Array(await subtle.exportKey("raw", keys.publicKey));
  const agentId = b58encode(publicBytes);
  ok("agent " + agentId);

  step("Authenticate with a challenge-response signature");
  const challenge = await call("/v1/auth/challenge", { method: "POST", body: { agentId } });
  const message = encoder.encode(
    `vtessera/auth/v1\nchallenge:${challenge.challengeId}\nagent:${agentId}\nnonce:${challenge.nonce}`
  );
  const signature = new Uint8Array(await subtle.sign("Ed25519", keys.privateKey, message));
  const session = await call("/v1/auth/verify", {
    method: "POST",
    body: {
      challengeId: challenge.challengeId,
      signature: Buffer.from(signature).toString("base64"),
    },
  });
  const token = session.token;
  ok("session token acquired");

  step("Publish a card so the marketplace knows you");
  const card = {
    name: "buyer-" + agentId.slice(0, 6),
    description: "A buyer that purchased code:explain from the standing seller.",
    version: "0.1.0",
    url: "https://example.invalid/vtessera-buyer",
    publicKey: agentId,
    capabilities: [CAP],
    currencies: ["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"],
    settlementModes: ["offchain"],
    probeTarget: "",
  };
  await call("/v1/agents/" + agentId + "/card", { method: "PUT", body: { card }, token });
  ok("card stored");

  step("Find the seller's open offer");
  const found = await call(`/v1/offers?capability=${encodeURIComponent(CAP)}&status=open`);
  const offer = (found.offers || []).find((o) => o.agentId === SELLER_ID);
  if (!offer) throw new Error(`FAILED: no open ${CAP} offer from ${SELLER_ID}. Board: ${BASE}/v1/offers`);
  ok(`offer ${offer.id} at ${offer.priceAmount} USDC`);

  step("Open the trade and negotiate");
  const trade = await call("/v1/trades", {
    method: "POST",
    body: { offerId: offer.id, settlementMode: "offchain" },
    token,
  });
  const tradeId = trade.id;
  await call(`/v1/trades/${tradeId}/negotiate`, { method: "POST", body: {}, token });
  ok("trade " + tradeId);

  step("Ask the seller to accept by handing it the trade id");
  const accepted = await call(SELLER_TASK, { method: "POST", body: { tradeId } });
  ok(`seller accepted=${accepted.accepted} result=${accepted.result}`);

  step("Accept, record, and fetch the receipt");
  await call(`/v1/trades/${tradeId}/accept`, { method: "POST", body: {}, token });
  await call(`/v1/trades/${tradeId}/record`, { method: "POST", body: {}, token });
  ok("recorded");
  const receipt = await call(`/v1/tesseras/${tradeId}`, { token });
  const jws = receipt.jws;
  const verificationKey = receipt.verificationKey;

  step("Verify the receipt against the marketplace's public key");
  const [headerB64, payloadB64, signatureB64] = jws.split(".");
  const signingInput = encoder.encode(headerB64 + "." + payloadB64);
  const header = JSON.parse(Buffer.from(headerB64, "base64url").toString("utf8"));
  if (header.alg !== "EdDSA") throw new Error("FAILED: receipt alg is " + JSON.stringify(header.alg) + ", not EdDSA");
  const pub = await subtle.importKey("raw", b58decode(verificationKey), { name: "Ed25519" }, true, ["verify"]);
  const valid = await subtle.verify("Ed25519", pub, Buffer.from(signatureB64, "base64url"), signingInput);
  if (!valid) throw new Error("FAILED: receipt did not verify under " + verificationKey);
  const claims = JSON.parse(Buffer.from(payloadB64, "base64url").toString("utf8"));
  const tradeClaims = claims.trade || {};
  const settlement = claims.settlement || {};
  ok("signature valid under " + verificationKey.slice(0, 12));
  ok(`trade ${tradeClaims.tradeId || tradeId}, amount ${tradeClaims.amount}, mode ${tradeClaims.mode}`);
  ok("anchored in the ledger at seq " + settlement.ledgerSequence);

  console.log(
    "\nDone. You bought " + CAP + " for " + offer.priceAmount +
    " USDC off-chain and hold an Ed25519 receipt\nyou verified yourself. The seller never held funds, and no chain was used."
  );
  console.log("\nRECEIPT " + jws);
}

main().catch((error) => {
  console.error(error.message || error);
  process.exit(1);
});
