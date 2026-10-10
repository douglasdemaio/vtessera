# Gateway as a forwarding destination — design (2026-10-10)

## The current model

vtessera is an AGP **gateway**: `POST /agp/route` (JSON-RPC
`agp/route_intent`) selects the cheapest agent whose announced policy
satisfies the intent's constraints and returns a `RouteResult`. The `path` in
that result is the **agent's own squad path**, and the **caller** is the one
who then performs the A2A call to that agent. How an agent is actually
addressed is a client concern; this service only ever routes.

Result: a forwarding gateway (the `agent-ai-tool.com` directory, another
marketplace, an aggregator) that receives an intent it cannot serve has no
stated contract for handing it to us, and an agent that lands here must
synthesise the agent-facing call from `route.path` and the registry card
itself.

## What "destination" means here

We want a developer who already ships an A2A agent — or who owns a gateway
that fielded an intent it cannot fulfil — to be able to say *"take this,"*
and to have the marketplace hand back a result that is **addressed and ready
to send**, produced from records the marketplace already holds and verifies.

## Stage 1 — routing plus a rendered task envelope (implemented)

New JSON-RPC method `agp/route_task` on the existing `POST /agp/route`
route, plus a declared capability on the already-published AGP extension in
the server card.

Request:

```json
{
  "jsonrpc": "2.0",
  "method": "agp/route_task",
  "params": {
    "target_capability": "general:summarize",
    "payload": { },
    "policy_constraints": { }
  }
}
```

Response:

```json
{
  "jsonrpc": "2.0",
  "result": {
    "target_capability": "general:summarize",
    "route": { "path": "…", "agent_id": "…", "offer_id": "…", "cost": 0.01, "cost_amount": "0.01", "cost_mint": "…", "policy": { } },
    "task": {
      "jsonrpc": "2.0",
      "id": "<new uuid>",
      "method": "message",
      "params": {
        "message": {
          "role": "agent",
          "kind": "task",
          "taskId": "<new uuid>",
          "contextId": "<new uuid>",
          "targetCapability": "general:summarize",
          "payload": <the intent payload>
        }
      }
    },
    "considered": 3,
    "table_fingerprint": "…"
  }
}
```

The `task` is rendered from the selected announcement's squad path and the
registered agent card the marketplace holds and attests to — the caller gets a
single JSON-RPC `message` body it can POST directly to `route.path` (or the
agent's card URL / preferred transport) without building the envelope from
scratch. Nothing on the service side is invoked by building the envelope:
selection is read-only, the task is caller-constructed content wrapped in
routing metadata, and there are no trade, cap, or settlement side effects.

The server card's existing AGP extension entry gains a
`"forwarding": {"methods": ["agp/route_intent", "agp/route_task"]}`
parameter, so a gateway that reads the card knows the contract without code.

**Why this is Stage 1 and safe.** It adds no outbound network call from the
service: trust boundary 6 (the only caller-supplied egress today is the
capability probe) is untouched. It reuses `agp.Routing.buildTable`/`Route`
verbatim, the registered card, and RPC plumbing that already exists. The
attestation the marketplace holds for the card is what lets a caller trust the
rendered `path` more than it trusts the agent the path names.

## Stage 2 — transit relay (deferred, explicitly not now)

Making vtessera itself POST the task to the chosen agent and relay the result
and the task lifecycle. This is the genuinely "destination-in-the-act" step,
and it is deferred because it:

- adds a **second outbound path to caller-supplied URLs** (one boundary-6
  note says the capability probe is the *only* such path — a relay breaks
  that invariant and the threat model must be updated with a relay-specific
  section before the code);
- needs a pending-task store (the service has none; tasks resolve
  asynchronously in A2A);
- needs an identity decision for *who authenticates to the agent on the
  caller's behalf* — the marketplace holds no agent private key and must not
  start holding or proxying one;
- has real cost (idle listeners, timeouts, abuse) for a marketplace that is
  itself off-chain-only on the sandbox.

It is worth a separate design document only if Stage 1 shows demand. The
extension contract in Stage 1 is designed so that Stage 2 can add a `"relay"`
mode without breaking the `"route"` contract.

## Decisions

These were settled before implementation; the record is here so the next
reader does not re-open them.

1. **Stage 1 as scoped** (route_task + card capability + tests), with Stage 2
   explicitly out of scope until separately signed off. The relay (Stage 2) is
   the point of "gateway-as-destination" for some integrators, but Stage 1 is
   the protocol answer they can adopt today and it makes the relay a
   consequence rather than a leap.
2. **Envelope ownership: the marketplace renders `task` exactly as above.** It
   is metadata over caller content, and the caller owns the content. This is
   what makes the envelope *addressed and ready to send*; echoing a
   caller-supplied task would leave the addressing as the caller's job, which
   is the exact gap Stage 1 closes.
3. **Method name: `agp/route_task` on the existing route** (`POST /agp/route`).
   The mux already routes methods over that endpoint; one endpoint, two
   methods, matches the existing JSON-RPC shape with less surface.

## Delivery surface (as implemented)

- `internal/agp/route_task.go` — `MethodRouteTask`, `ForwardingMethods`
  (`["agp/route_intent", "agp/route_task"]`), and `Table.RouteTask`, which is
  `Table.Route` plus a rendered `Task *TaskEnvelope` on the result.
  `RouteResult.Task` is `omitempty`, so `agp/route_intent` responses are
  byte-for-byte unchanged.
- `internal/httpapi/server.go` — `agp/route_task` is accepted on
  `POST /agp/route` and dispatched to `RouteTask`; the AGP extension in the
  server card gains `"forwarding": {"methods": [...]}`, and the skills list
  gains `agp_route_task`.
- Tests: `internal/agp/` (envelope shape, distinct envelope ids across calls,
  `Route` attaches no envelope, failure modes shared with `Route`,
  `ForwardingMethods`) and `internal/httpapi/` (rendered envelope reached
  through the wire, `route_intent` omits `task`, unknown method refused).

The envelope mints fresh UUIDs for `id`, `taskId` and `contextId` on every
call. Retry/idempotency semantics — a repeated `route_task` for the same work
producing the same `taskId` — are deliberately not done here: that is a caller
or Stage-2 concern, not a selector's.