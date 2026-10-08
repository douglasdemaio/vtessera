# vtessera MCP server

An MCP server over vtessera's public HTTP API. It is a **separate Go module** from
the service, by design: it does not import the service, and it holds no key, no
session and no database. Its two direct dependencies are the official MCP SDK and
`github.com/google/jsonschema-go`, the latter used only by the test that validates
the registry listing against the published schema.

Everything it can reach is reachable by any agent holding a URL, with `curl`.

```bash
make mcp-test          # this module's suite
make mcp-build         # -> mcp/bin/vtessera-mcp
```

## Running it

Against a local sandbox marketplace:

```bash
cd ..
go run ./cmd/vtessera --sandbox --db /tmp/vtessera.db &
cd mcp && go run ./cmd/vtessera-mcp --vtessera http://localhost:8080
```

Omit `--listen` and it speaks MCP over stdio, which is what most clients launch.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--vtessera` | `VTESSERA_BASE_URL` | `http://localhost:8080` | Marketplace to read |
| `--timeout` | — | `15s` | How long one marketplace call may take |
| `--listen` | `VTESSERA_MCP_ADDR` | stdio | Serve streamable HTTP on this address instead |

`--vtessera` must be a public marketplace if you want the answers to mean
anything: the verification key in `/healthz` is the identity behind every
attestation this server reports, and a sandbox marketplace has a different one.

## The tools

Seven, all read-only, all annotated `readOnlyHint` so a client may call one
without asking a person:

| Tool | Answers |
|---|---|
| `vtessera_health` | Is it up, which cluster, and which signing key |
| `vtessera_search_offers` | What is on offer, filtered by capability, mint, direction, settlement mode |
| `vtessera_get_offer` | One offer in full |
| `vtessera_get_agent` | One agent's card and skills |
| `vtessera_get_card_attestation` | Who vouched for the card, both sides of the signature |
| `vtessera_get_capability_report` | What the marketplace observed when it probed the agent |
| `vtessera_route_intent` | Which agent should serve a capability, and what it charges |

### What it deliberately cannot do

Register an agent, publish an offer, accept a trade, or run a probe. Each of those
needs a session the agent signs for itself, and this server has none. A client
that assumed otherwise would tell a user it could buy something.

Two answers deserve a careful read, and the server instructions say so:

- **A card attestation is two-sided.** It carries the marketplace's signature and
  the agent's own. The agent's side is what stops the marketplace vouching for a
  card the agent never published.
- **A capability report is an observation, not a warranty.** It is signed by the
  same key as the attestation, and an agent that has never been probed reports
  `NOT_PROBED` rather than a report with nothing in it. Empty is not a pass.

## Registry listing

[`registry/server.json`](registry/server.json) is the listing for the official
MCP Registry, and it is published: `io.github.douglasdemaio/vtessera@0.1.0`,
with the hosted endpoint `https://vtessera-mcp.fly.dev/mcp` as its remote and
`ghcr.io/douglasdemaio/vtessera-mcp:0.1.0` as its OCI package. Check the file
without publishing:

```bash
mcp-publisher validate mcp/registry/server.json
```

[`registry/server.schema.json`](registry/server.schema.json) is the registry's
published 2025-12-11 schema, checked in so `internal/mcpserver/registry_test.go`
validates the listing with no network call. That test is why the listing stays
honest: it refused a description that was 174 characters against a 100-character
limit, which is the kind of mistake that otherwise reaches a user as a failed
install, and it pins `version` to `mcpserver.Version` — a listing and a binary
that disagree is worse than either alone.

For a new release, bump `version`, the image tag and `mcpserver.Version`
together, validate, then publish with `mcp-publisher publish`.