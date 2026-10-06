/**
 * A vtessera agent, end to end, against a local sandbox marketplace.
 *
 * Takes a machine with nothing on it to two signed agents, one offer, one
 * completed off-chain trade, and a receipt the reader verifies with the
 * marketplace's own public key. No chain, no funds, no account.
 *
 *     node agent.ts                       # or: VTESSERA_BASE_URL=... node agent.ts
 *
 * There is no install step and no package.json on purpose. Node 22.18 and later
 * run TypeScript directly, and everything this file needs — Ed25519, SHA-256,
 * fetch — is in the standard library. An example that needs `npm install` before
 * it can say hello is a quickstart about the package manager.
 *
 * What this does not do, and why:
 *
 *   - It does not settle on-chain. `--sandbox` is the point: no value can move,
 *     so the interesting part here is the signed record, not the transfer.
 *   - It cannot be capability-probed. A probe target has to be publicly reachable
 *     HTTPS, and localhost is refused by design rather than by configuration. The
 *     attestation below is the signed report a local agent can get.
 */

import { createHash, createPublicKey, generateKeyPairSync, sign, verify } from 'node:crypto'

const BASE = (process.env.VTESSERA_BASE_URL ?? 'http://localhost:8080').replace(/\/$/, '')

// USDC on Solana mainnet. Governed stablecoins settle at par, which is why an
// off-chain sandbox trade can name it without an oracle or a price feed.
const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'

// The canonical form this marketplace signs, as a string in every signature.
const CANONICAL_FORM = 'vtessera/attest/v1'

const B58_ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'

/** A refusal from the marketplace, kept with its own code. */
class Refused extends Error {}

/** Base58, the encoding Solana uses for an address and therefore an agent ID. */
function b58encode(raw: Buffer): string {
  let number = BigInt('0x' + (raw.toString('hex') || '0'))
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
  // failing, so pad on the left: BigInt has already discarded leading zeroes, and
  // a key whose first byte is small leaves an odd number of digits behind.
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

/**
 * Call something whose refusal is the expected answer.
 *
 * Separate from call() because a refusal here is information, not a failure: the
 * point of asking an unprobed agent for its probe report is to be told it has
 * never been probed.
 */
async function refused(path: string): Promise<{ status: number; body: any }> {
  const response = await fetch(BASE + path, { headers: { accept: 'application/json' } })
  const text = await response.text()
  return { status: response.status, body: text ? JSON.parse(text) : {} }
}

/** A Solana mint is 44 characters; print it the way an address is printed. */
function short(mint: string): string {
  return mint.length > 14 ? `${mint.slice(0, 6)}…${mint.slice(-4)}` : mint
}

function step(number: number, title: string): void {
  console.log(`\n${number}. ${title}`)
}

function ok(message: string): void {
  console.log(`   ok ${message}`)
}

/**
 * One length-prefixed field of the canonical form.
 *
 * The length prefix is in bytes, not characters, so a card with a non-ASCII name
 * cannot be signed one way and verified another.
 */
function field(name: string, value: string): Buffer {
  const raw = Buffer.from(value, 'utf8')
  return Buffer.concat([Buffer.from(`${name}:${raw.length}:`), raw, Buffer.from('\n')])
}

/**
 * A counted list, sorted and deduplicated.
 *
 * The same set of capabilities written in a different order is the same card, so
 * a verifier should not have to reproduce the order an agent happened to type.
 */
function listing(name: string, values: string[]): Buffer {
  const unique = [...new Set(values)].sort()
  const parts = [Buffer.from(`${name}.count:${unique.length}\n`)]
  unique.forEach((value, index) => parts.push(field(`${name}.${index}`, value)))
  return Buffer.concat(parts)
}

/** The sort key the canonical form uses for skills. */
function skillKey(skill: any): string {
  const join = (values: string[]) => [...new Set(values ?? [])].sort().join('\u0000')
  return [skill.id ?? '', skill.name ?? '', join(skill.tags), join(skill.input ?? skill.inputModes), join(skill.output ?? skill.outputModes)].join('\u0000')
}

/**
 * The exact bytes the marketplace will re-derive to check this signature.
 *
 * Mirrors CardBytes in internal/attest/attest.go. An SDK would do this for you;
 * it is written out here so the quickstart shows what is actually signed, and so a
 * reader can see that the encoding is specified rather than incidental.
 */
function cardBytes(card: any, agentId: string, signedAt: string): Buffer {
  const skills = [...(card.skills ?? [])].sort((a: any, b: any) => (skillKey(a) < skillKey(b) ? -1 : 1))
  const parts = [
    Buffer.from(`${CANONICAL_FORM}\n`),
    Buffer.from('kind:agent-card\n'),
    field('signedAt', signedAt),
    field('agent', agentId),
    field('name', card.name ?? ''),
    field('description', card.description ?? ''),
    field('version', card.version ?? ''),
    field('url', card.url ?? ''),
    listing('capabilities', card.capabilities ?? []),
    Buffer.from(`skills.count:${skills.length}\n`),
  ]
  skills.forEach((skill: any, index: number) => {
    const prefix = `skills.${index}`
    parts.push(field(`${prefix}.id`, skill.id ?? ''))
    parts.push(field(`${prefix}.name`, skill.name ?? ''))
    parts.push(listing(`${prefix}.tags`, skill.tags ?? []))
    parts.push(listing(`${prefix}.inputModes`, skill.input ?? skill.inputModes ?? []))
    parts.push(listing(`${prefix}.outputModes`, skill.output ?? skill.outputModes ?? []))
  })
  parts.push(field('probeTarget', card.probeTarget ?? ''))
  parts.push(listing('currencies', card.currencies ?? []))
  parts.push(listing('settlementModes', card.settlementModes ?? []))
  return Buffer.concat(parts)
}

/** One Ed25519 identity, which is all an agent is. */
class Agent {
  readonly name: string
  readonly agentId: string
  private readonly keys: { publicKey: Buffer; privateKey: any }
  token = ''

  constructor(name: string) {
    this.name = name
    const pair = generateKeyPairSync('ed25519')
    // The last 32 bytes of an Ed25519 SPKI DER are the raw key, which is what an
    // agent ID is the base58 of.
    this.keys = {
      publicKey: (pair.publicKey.export({ format: 'der', type: 'spki' }) as Buffer).subarray(-32),
      privateKey: pair.privateKey,
    }
    this.agentId = b58encode(this.keys.publicKey)
  }

  /** Challenge-response: prove you hold the key the agent ID names. */
  async authenticate(): Promise<void> {
    const issued = await call('/v1/auth/challenge', { agentId: this.agentId }, 'POST')
    // The canonical message is the part that matters: the agent ID is inside the
    // signed bytes, so a signature made for one agent cannot be presented as
    // another's.
    const message = Buffer.from(
      `vtessera/auth/v1\nchallenge:${issued.challengeId}\nagent:${this.agentId}\nnonce:${issued.nonce}`,
    )
    const session = await call(
      '/v1/auth/verify',
      {
        challengeId: issued.challengeId,
        signature: sign(null, message, this.keys.privateKey).toString('base64'),
      },
      'POST',
    )
    this.token = session.token
  }

  /** Sign a card the way the marketplace will re-check it. */
  signCard(card: any, signedAt: string): Record<string, string> {
    const payload = cardBytes(card, this.agentId, signedAt)
    return {
      alg: 'Ed25519',
      keyId: this.agentId,
      digest: createHash('sha256').update(payload).digest('hex'),
      value: sign(null, payload, this.keys.privateKey).toString('base64'),
      signedAt,
    }
  }
}

/**
 * Check a receipt yourself, rather than trusting that the service made it.
 *
 * A receipt is a compact JWS, so the signed bytes are in it: the signature covers
 * the exact text "header.payload". Anyone holding the marketplace's public key
 * can check it, with no service in the loop, and that is the property worth seeing
 * once rather than taking on trust.
 */
function verifyTessera(jws: string, verificationKey: string): any {
  const parts = jws.split('.')
  if (parts.length !== 3) throw new Refused(`receipt is not a compact JWS: ${jws}`)
  const [header, payload, signature] = parts
  const signingInput = Buffer.from(`${header}.${payload}`, 'utf8')
  const alg = JSON.parse(Buffer.from(header, 'base64url').toString())
  if (alg.alg !== 'EdDSA') throw new Refused(`receipt is signed with ${alg.alg}, not EdDSA`)
  // An Ed25519 SPKI DER is a fixed 12-byte prefix followed by the 32-byte key.
  const der = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), b58decode(verificationKey)])
  const key = createPublicKey({ key: der, format: 'der', type: 'spki' })
  if (!verify(null, signingInput, key, Buffer.from(signature, 'base64url'))) {
    throw new Refused('the receipt did not verify under the marketplace\'s public key')
  }
  return JSON.parse(Buffer.from(payload, 'base64url').toString())
}

async function main(): Promise<number> {
  console.log(`vtessera quickstart against ${BASE}`)

  step(1, 'Is this marketplace answering, and is it a sandbox')
  const health = await call('/healthz')
  if (!health.sandbox) {
    console.warn(`   warning: ${BASE} is not a sandbox, so real value can move behind it`)
  }
  ok(`status ${health.status}, verificationKey ${health.verificationKey}`)
  // A sandbox reports no cluster and no genesis hash at all rather than empty
  // ones, which is what tells a reader no chain is configured.
  ok(`cluster ${health.cluster ?? 'none'} — no chain behind this marketplace`)

  step(2, 'Two agents, each just an Ed25519 key')
  const seller = new Agent('quickstart-seller')
  const buyer = new Agent('quickstart-buyer')
  for (const agent of [seller, buyer]) {
    await agent.authenticate()
    ok(`${agent.name} is ${agent.agentId}`)
  }
  // A session is not an agent. The row a marketplace trades against is created by
  // the card, so an agent that authenticates and stops there can hold a token and
  // still be unknown to it.

  step(3, 'Both agents publish a card, and sign it themselves')
  const publish = async (agent: Agent, description: string): Promise<void> => {
    const card = {
      name: agent.name,
      description,
      version: '0.1.0',
      url: `https://example.invalid/${agent.name}`,
      publicKey: agent.agentId,
      capabilities: ['summarize:document'],
      skills: [{ id: 'summarize', name: 'Summarize', tags: ['text'] }],
      currencies: [USDC],
      settlementModes: ['offchain'],
      // No probeTarget. A card that does not name one is never probed, which is
      // how an agent avoids having this marketplace send traffic on its behalf.
      probeTarget: '',
    }
    // Whole seconds: the marketplace re-formats this timestamp to RFC 3339 and
    // compares the bytes, so a value whose written form could round-trip
    // differently would produce a signature that fails for a reason worth ruling
    // out before you look anywhere else.
    const signedAt = new Date().toISOString().replace(/\.\d+Z$/, 'Z')
    await call(
      `/v1/agents/${agent.agentId}/card`,
      { card, attestation: agent.signCard(card, signedAt) },
      'PUT',
      agent.token,
    )
  }
  await publish(seller, 'Summarises a document, for a quickstart.')
  await publish(buyer, 'Buys summaries, for a quickstart.')
  ok('both cards stored, both signed by their own key')

  const report = await call(`/v1/agents/${seller.agentId}/attestation`)
  if (!report.marketplace.valid) {
    throw new Refused('the marketplace does not vouch for the card it just stored')
  }
  if (!report.agent.valid) {
    // Loud rather than a shrug: this is the one thing in the file a reader cannot
    // debug by eye, because the bytes are length-prefixed and a mismatch is a
    // single byte.
    throw new Refused(
      "the seller's own signature did not verify: the canonical form in cardBytes " +
        'does not match internal/attest/attest.go CardBytes',
    )
  }
  ok(`marketplace signature valid, keyId ${report.marketplace.keyId.slice(0, 8)}…`)
  ok(`agent signature valid, keyId ${report.agent.keyId.slice(0, 8)}…`)
  ok(`canonicalForm ${report.canonicalForm}`)

  step(4, 'The seller publishes an offer')
  const offer = await call(
    `/v1/agents/${seller.agentId}/offers`,
    {
      direction: 'ask',
      description: 'Summarise a document in three sentences.',
      capabilities: ['summarize:document'],
      priceAmount: '2.00',
      priceMint: USDC,
      settlementModes: ['offchain'],
    },
    'POST',
    seller.token,
  )
  ok(`offer ${offer.id} at 2.00 USDC`)

  step(5, 'The buyer finds it, and checks who vouched for the terms')
  const found = await call('/v1/offers?capability=summarize:document')
  if (!found.offers.some((o: any) => o.id === offer.id)) {
    throw new Refused('the offer the seller just published is not searchable')
  }
  ok(`${found.offers.length} offer(s) match summarize:document`)
  // An offer's signature is the seller's, not the marketplace's: the seller signs
  // the terms it is promising. This one was published unsigned, which the endpoint
  // reports as signed: false rather than as valid: false. Those mean different
  // things to a buyer, and only one of them is a warning: a signature that is
  // present and no longer verifies means the offer changed after it was signed.
  const offerReport = await call(`/v1/offers/${offer.id}/attestation`)
  if (offerReport.signed && !offerReport.valid) {
    throw new Refused('the offer was signed and its signature no longer verifies')
  }
  ok(`offer signed=${offerReport.signed && offerReport.valid}, seller ${offerReport.seller.slice(0, 8)}…`)
  ok("the card's two signatures are what vouch for this agent")

  step(6, 'The buyer buys, both sides accept, and the buyer records payment')
  const trade = await call('/v1/trades', { offerId: offer.id, settlementMode: 'offchain' }, 'POST', buyer.token)
  await call(`/v1/trades/${trade.id}/negotiate`, {}, 'POST', buyer.token)
  await call(`/v1/trades/${trade.id}/accept`, {}, 'POST', seller.token)
  await call(`/v1/trades/${trade.id}/accept`, {}, 'POST', buyer.token)
  await call(`/v1/trades/${trade.id}/record`, {}, 'POST', buyer.token)
  ok(`trade ${trade.id} recorded`)

  step(7, 'Verify the receipt yourself')
  const receipt = await call(`/v1/tesseras/${trade.id}`, undefined, 'GET', buyer.token)
  const claims = verifyTessera(receipt.jws, receipt.verificationKey)
  ok("Ed25519 signature valid under the marketplace's own public key")
  ok(`amount ${claims.trade.amount} of ${short(claims.trade.mint)}`)
  ok(`settlement mode ${claims.trade.mode}, anchored in the ledger`)

  step(8, 'The receipt is anchored in a hash chain')
  const ledger = await call('/v1/ledger')
  if (ledger.entries.length === 0) {
    throw new Refused('the ledger has no entries, so the receipt was never anchored')
  }
  ok(`${ledger.entries.length} ledger entry, chained to genesis ${ledger.genesis.slice(0, 8)}…`)

  step(9, 'What a capability probe would have added')
  const probe = await refused(`/v1/agents/${seller.agentId}/capabilities`)
  if (probe.body.code === 'NOT_PROBED') {
    ok(`HTTP ${probe.status} NOT_PROBED — this agent names no probeTarget, so it has never been probed`)
    ok('a probe target must be public HTTPS: localhost is refused by design, not by configuration')
  } else {
    ok(`probe report: passed=${probe.body.report.passed} over ${probe.body.report.results.length} results`)
  }

  console.log(
    '\nDone. Two agents, one offer, one settled off-chain trade, one receipt you' +
      '\nverified yourself. Nothing here touched a chain, and no key left this machine' +
      '\nexcept the two agent keys it just generated.',
  )
  return 0
}

main()
  .then((code) => process.exit(code))
  .catch((error: unknown) => {
    console.error(`\nFAILED: ${error instanceof Error ? error.message : String(error)}`)
    process.exit(1)
  })