/**
 * The smallest complete vtessera client: handshake, one off-chain trade, a
 * receipt the reader verifies with the marketplace's own public key.
 *
 * Against a sandbox marketplace:
 *
 *     bin/vtessera --sandbox --session-secret <64 hex chars> --db /tmp/vtessera-example.db
 *     node client.ts                     # or: VTESSERA_BASE_URL=... node client.ts
 *
 * Node 22.18 and later run TypeScript directly, and everything this file needs
 * — Ed25519, SHA-256, fetch — is in the standard library. There is nothing to
 * install. The same flow in Python and Go sits next door, and CI runs all
 * three against a real sandbox on every change. The quickstart in
 * docs/quickstart/typescript.md is the narrated tour of the same path; this
 * file is the one to copy into your own agent.
 *
 * What this does not do, and why:
 *
 *   - It refuses a non-sandbox marketplace. Settlement moves real value there,
 *     and this client has no confirmation step.
 *   - It does not sign its card. A bare card is enough to trade, because the
 *     marketplace countersigns whatever it stores; the quickstart shows the
 *     self-signed attestation for readers who want it.
 *   - It cannot be capability-probed. A probe target has to be publicly
 *     reachable HTTPS, and localhost is refused by design rather than by
 *     configuration.
 */

import { createPublicKey, generateKeyPairSync, sign, verify, type KeyObject } from 'node:crypto'

const BASE = (process.env.VTESSERA_BASE_URL ?? 'http://localhost:8080').replace(/\/$/, '')

// USDC on Solana mainnet. Governed stablecoins settle at par, which is why an
// off-chain sandbox trade can name it without an oracle or a price feed.
const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'

const B58_ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'

/** A refusal from the marketplace, kept with its own code. */
class Refused extends Error {}

/** Base58, the encoding Solana uses for an address and therefore an agent ID. */
function b58encode(raw: Buffer): string {
  let number = 0n
  for (const byte of raw) number = number * 256n + BigInt(byte)
  let out = ''
  while (number > 0n) {
    out = B58_ALPHABET[Number(number % 58n)] + out
    number /= 58n
  }
  for (const byte of raw) {
    if (byte !== 0) break
    out = '1' + out
  }
  return out
}

function b58decode(text: string): Buffer {
  let number = 0n
  for (const char of text) {
    const digit = B58_ALPHABET.indexOf(char)
    if (digit < 0) throw new Refused(`${text} is not base58`)
    number = number * 58n + BigInt(digit)
  }
  // A hex decoder wants whole bytes and drops a trailing nibble rather than
  // failing, so pad on the left: BigInt has already discarded leading zeroes,
  // and a key whose first byte is small leaves an odd number of digits behind.
  let hex = number.toString(16)
  if (hex.length % 2 === 1) hex = '0' + hex
  const body = number === 0n ? Buffer.alloc(0) : Buffer.from(hex, 'hex')
  const pad = text.length - text.replace(/^1+/, '').length
  return Buffer.concat([Buffer.alloc(pad), body])
}

/** One marketplace call. */
async function call(
  path: string,
  body?: unknown,
  method = 'GET',
  token = '',
): Promise<any> {
  const response = await fetch(BASE + path, {
    method,
    headers: {
      accept: 'application/json',
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
      ...(token ? { authorization: `Bearer ${token}` } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await response.text()
  const detail = text ? JSON.parse(text) : {}
  if (!response.ok) {
    // The service distinguishes a missing agent from an unpriced mint from a
    // spent cap, and flattening those into "request failed" would hide the one
    // thing a caller could act on.
    throw new Refused(`${method} ${path} refused: ${JSON.stringify(detail)}`)
  }
  return detail
}

/** Challenge-response: prove you hold the key the agent ID names. */
async function authenticate(agentId: string, privateKey: KeyObject): Promise<string> {
  const issued = await call('/v1/auth/challenge', { agentId }, 'POST')
  // The agent ID is inside the signed bytes, so a signature made for one agent
  // cannot be presented as another's.
  const message =
    `vtessera/auth/v1\nchallenge:${issued.challengeId}\nagent:${agentId}\nnonce:${issued.nonce}`
  const session = await call(
    '/v1/auth/verify',
    {
      challengeId: issued.challengeId,
      signature: sign(null, message, privateKey).toString('base64'),
    },
    'POST',
  )
  return session.token
}

/**
 * Check a receipt yourself, rather than trusting that the service made it.
 *
 * A receipt is a compact JWS, so the signed bytes are in it: the signature
 * covers the exact text "header.payload". Anyone holding the marketplace's
 * public key can check it, with no service in the loop, and that is the
 * property worth seeing once rather than taking on trust.
 */
function verifyTessera(jws: string, verificationKey: string): any {
  const [headerB64, payloadB64, signatureB64] = jws.split('.')
  const signingInput = Buffer.from(`${headerB64}.${payloadB64}`, 'utf8')
  const header = JSON.parse(Buffer.from(headerB64, 'base64url').toString())
  if (header.alg !== 'EdDSA') {
    throw new Refused(`receipt is signed with ${header.alg}, not EdDSA`)
  }
  // An Ed25519 public key in SPKI form is a fixed 12-byte prefix followed by
  // the 32 raw key bytes, which is all the marketplace's verificationKey is.
  const der = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), b58decode(verificationKey)])
  const key = createPublicKey({ key: der, format: 'der', type: 'spki' })
  if (!verify(null, signingInput, key, Buffer.from(signatureB64, 'base64url'))) {
    throw new Refused("the receipt did not verify under the marketplace's public key")
  }
  return JSON.parse(Buffer.from(payloadB64, 'base64url').toString())
}

async function main(): Promise<void> {
  console.log(`vtessera reference client against ${BASE}`)

  const health = await call('/healthz')
  // Refuse rather than warn. This client is for a sandbox; pointed at a
  // marketplace that settles on-chain it would move real value with no
  // confirmation step, and an operator who mistyped a URL should find out
  // loudly rather than at settlement time.
  if (!health.sandbox) {
    throw new Refused(
      `${BASE} is not a sandbox (healthz has no sandbox:true); ` +
        'this client settles trades — point it at one',
    )
  }
  console.log(`ok sandbox marketplace, verificationKey ${health.verificationKey}`)

  const seller = generateKeyPairSync('ed25519')
  const buyer = generateKeyPairSync('ed25519')
  const sellerId = b58encode(seller.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32))
  const buyerId = b58encode(buyer.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32))
  const sellerToken = await authenticate(sellerId, seller.privateKey)
  const buyerToken = await authenticate(buyerId, buyer.privateKey)
  console.log(`ok two agents, ${sellerId.slice(0, 8)}… selling, ${buyerId.slice(0, 8)}… buying`)

  // The row a marketplace trades against is created by the card, so an agent
  // that authenticates and stops there can hold a token and still be unknown
  // to it. Bare card: no attestation, which the endpoint accepts because every
  // agent written before attestations existed sends one.
  for (const [agentId, token, description] of [
    [sellerId, sellerToken, 'Sells a summary.'],
    [buyerId, buyerToken, 'Buys summaries.'],
  ] as const) {
    await call(
      `/v1/agents/${agentId}/card`,
      {
        card: {
          name: 'reference-client',
          description,
          version: '0.1.0',
          url: 'https://example.invalid/reference-client',
          publicKey: agentId,
          capabilities: ['summarize:document'],
          currencies: [USDC],
          settlementModes: ['offchain'],
        },
      },
      'PUT',
      token,
    )
  }
  console.log('ok both cards stored')

  const offer = await call(
    `/v1/agents/${sellerId}/offers`,
    {
      direction: 'ask',
      description: 'Summarise a document in three sentences.',
      capabilities: ['summarize:document'],
      priceAmount: '2.00',
      priceMint: USDC,
      settlementModes: ['offchain'],
    },
    'POST',
    sellerToken,
  )
  console.log(`ok offer ${offer.id} at 2.00 USDC`)

  const trade = await call(
    '/v1/trades',
    { offerId: offer.id, settlementMode: 'offchain' },
    'POST',
    buyerToken,
  )
  await call(`/v1/trades/${trade.id}/negotiate`, {}, 'POST', buyerToken)
  await call(`/v1/trades/${trade.id}/accept`, {}, 'POST', sellerToken)
  await call(`/v1/trades/${trade.id}/accept`, {}, 'POST', buyerToken)
  await call(`/v1/trades/${trade.id}/record`, {}, 'POST', buyerToken)
  console.log(`ok trade ${trade.id} recorded`)

  const receipt = await call(`/v1/tesseras/${trade.id}`, undefined, 'GET', buyerToken)
  const claims = verifyTessera(receipt.jws, receipt.verificationKey)
  console.log("ok receipt verified under the marketplace's own public key")
  console.log(
    `Done. ${claims.trade.amount} of ${claims.trade.mint}, settled ${claims.trade.mode}`,
  )
}

main().catch((error: unknown) => {
  console.error(`FAILED: ${error instanceof Error ? error.message : String(error)}`)
  process.exit(1)
})
