# Local Network Test Notes — Issue #69 Follow-up

**Date:** 2026-08-22  
**Node:** `18fa157a9e975b4441cb9a4c2773d120` @ `192.168.178.82:8402`  
**Index:** `192.168.178.82:8403`  
**Agent:** `5vWdYSmcNJnoj2PM8LfLRvnviujnQCT1Eu8Csw6Jzfhs`  

## Tests Run

| # | Duration | Price (micros) | Status | Notes |
|---|----------|----------------|--------|-------|
| 1 | 10s | 4,030 | ✅ accepted | First test, baseline |
| 2 | 60s | 24,180 | ✅ accepted | Longer job |
| 3 | 120s | 48,360 | ✅ accepted | Double duration |
| 4 | MCP discover | — | ✅ | Verified MCP interface works |

## Issues Found

### Critical

1. **Offer endpoint is `127.0.0.1` — breaks remote agents**  
   The offer's `endpoint` field is `http://127.0.0.1:8402`. This is the loopback address  
   configured at node startup. Remote agents cannot use this. The candidates list has  
   the STUN reflexive address (`194.15.87.26:<random>`), but the `endpoint` in the  
   offer body is what agents use to submit jobs.  
   - **Fix:** The node should set `endpoint` to its LAN IP or STUN reflexive address,  
     or the x402 client should be smart enough to use a candidate address instead of  
     the offer endpoint.

2. **STUN reflexive port is random — not routable**  
   The STUN probe discovers the reflexive address, but the port is whatever STUN  
   assigns. The node listens on 8402, but the reflexive port (e.g. 50715) doesn't  
   map back to 8402 without port forwarding.  
   - **Fix:** For internet mode, the node needs either: (a) port forwarding configured,  
     (b) TURN relay, or (c) UDP hole punching. Currently none are implemented.

3. **No job listing endpoint**  
   `GET /jobs` returns nothing — no way for agents to see their past jobs or receipts  
   from the HTTP API. Only the MCP `discover` tool shows index data.  
   - **Fix:** Add a `GET /jobs` endpoint that returns signed receipts from the state dir.

### Medium

4. **`finalize_pro_rata` fails — agent can't finalize**  
   The x402 client tries to finalize (drain escrow to seller) but fails because the  
   agent isn't the settlement authority. This is correct behavior but the error message  
   is a raw Solana RPC error — not user-friendly.  
   - **Fix:** Either: (a) don't attempt finalize from the agent (it's the node's job),  
     or (b) return a clear error before submitting the tx.

5. **Each test mints a new test stablecoin**  
   The x402 client creates a fresh mint per run, so test tokens accumulate on devnet.  
   Not a bug, but messy for repeated testing.  
   - **Fix:** Add a `--reuse-mint <addr>` option to reuse an existing test mint.

6. **Node job_id differs from agent's job_id**  
   The agent sends `job_id` in the job spec, but the node generates its own. The  
   receipt's `job_id` doesn't match what the agent sent. This is by design (agent  
   generates job_id for escrow PDA derivation), but confusing in logs.  
   - **Fix:** Document this clearly, or have the node echo back the agent's job_id  
     in the response.

### Low

7. **Index `GET /` returns "not found" — no root handler**  
   Hitting the index root returns a bare "not found" string. Should return a  
   JSON status page or at least a proper 404 with content-type.  
   - **Fix:** Add a `GET /` handler that returns `{"status":"ok","offers":N}`.

8. **MCP discover returns escaped JSON inside JSON**  
   The MCP response wraps the index JSON as a string inside `content[0].text`.  
   Double-encoding makes it hard to parse from agents.  
   - **Fix:** Consider returning the index JSON directly, or at least set  
     `content[0].type = "application/json"` so clients know to parse it.

## Node Identification

For the marketplace to work, agents need to reliably identify nodes:

| Method | Current State | What's Needed |
|--------|--------------|---------------|
| `node_id` (derived from pubkey) | ✅ Stable, deterministic | — |
| `pubkey_hex` in offer | ✅ Present | — |
| `endpoint` in offer body | ❌ Set to `127.0.0.1` | Must be routable address |
| Candidates (STUN reflexive) | ✅ Present | Need port mapping or TURN |
| Index registration | ✅ Working | Heartbeat TTL keeps it alive |
| Claims (FCFS) | ✅ Working | — |
| MCP discover | ✅ Working | Fix double-encoding |

## Recommendations for GitHub Issues

### Must-fix before internet mode

1. **Node endpoint should be auto-detected** — use LAN IP or STUN reflexive, not `127.0.0.1`
2. **Port forwarding / TURN for internet nodes** — STUN alone doesn't help behind NAT
3. **Agent should use candidate address, not offer endpoint** — or fix the endpoint

### Should-fix for marketplace UX

4. **Add `GET /jobs` endpoint** — agents need to see their job history
5. **Better finalize error handling** — don't submit doomed transactions
6. **Index root handler** — proper health check endpoint
7. **Document job_id flow** — agent vs node job_id behavior

### Nice-to-have

9. **Mint reuse in x402-client** — cleaner for testing
10. **MCP discover content-type** — proper JSON response
