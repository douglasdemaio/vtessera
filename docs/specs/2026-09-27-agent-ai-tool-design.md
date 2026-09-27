# agent-ai-tool.com — Design

Date: 2026-09-27
Status: Draft, pending review
Target repository: `douglasdemaio/agent-ai-tool` (separate from `vtessera`)

## Summary

A static, crawler-visible directory site at `agent-ai-tool.com` that helps AI
agents find agent tooling and, more importantly, discover *how to connect* to it.
Each listing carries machine-readable endpoints (A2A agent card, MCP endpoint)
so a visiting agent can act on what it reads rather than merely note that a tool
exists.

Built as a Go static-site generator with no framework and no npm dependency,
published through GitHub Pages.

## Goals

- Be findable and consumable by AI agents and answer engines.
- Let an agent fetch one file and learn how to connect to every listed tool.
- Present HTML for humans and JSON for agents; never make an agent parse markup
  to obtain an endpoint.
- Ship independently of `vtessera`, including before `vtessera` is deployed.
- Remain maintainable by one person without a build toolchain to install.

## Non-goals

- Not a marketplace. No listings are bought, sold, or ranked commercially.
- Not a substitute for a protocol registry. It is a curated directory, and
  says so.
- No client-side rendering of live data. See "Rejected: browser-patched live
  data" below.
- No analytics or user tracking of **visitors**. No page-view counting, no
  referrer capture, no IP or user-agent collection, no third-party analytics
  script. Marketplace activity figures derived from public ledger data are not
  visitor analytics and are specified separately in
  `2026-09-27-agent-ai-tool-usage-metrics-design.md`.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Content model | Curated set plus one live entry | A purely curated list rots unattended; a purely generated one inherits upstream outages. Hybrid keeps the differentiated content verifiably correct. |
| Repository | Separate from `vtessera` | Different lifecycles: content changes weekly, the service is mid-Phase-3. The only coupling is a build-time HTTP fetch, not code. |
| Build | Go generator, stdlib only | No npm, no new runtime. Matches existing fluency and cannot rot the way a neglected theme config does. Hugo is the better tool but introduces a toolchain and theming rabbit hole. |
| Crawler policy | Allow retrieval, block training | Retrieval and answer-engine bots are the actual visitors and produce citations. Training crawlers send no traffic. |
| Listing schema | Agent-connectable, rich fields optional | A link index serves humans; endpoints serve agents. `last_verified` makes staleness visible. |
| Build failure | Fall back to cached live data | Someone else's downtime must never block publishing. |

## Rejected alternatives

- **Pure hand-written HTML.** Ruled out: the live vtessera entry must be fetched
  at build time, and hand-edited HTML would be wrong within a day.
- **Client-side live data.** Ruled out: many crawlers do not execute JavaScript,
  so agent-readable content would be absent exactly when agents read it. It
  would look correct in a browser and be empty to the intended audience.
- **Hard build failure on fetch error.** Ruled out: couples site availability to a
  service that does not exist yet and cannot settle.
- **In-build link checking.** Ruled out: slow and flaky, and a flaky build is one
  people learn to ignore. Moved to a separate non-blocking CI job.
- **Hosting the site inside `vtessera`.** Ruled out: shares a repo with a service
  under active design and binds the site to the service's toolchain constraints.

## Architecture

```
agent-ai-tool/
  main.go                    # generator: load -> fetch -> merge -> render
  internal/content/          # parse + validate curated entries
  internal/live/             # fetch vtessera, fall back to cache
  internal/render/           # templates -> public/
  content/entries/*.json     # curated listings (committed)
  content/live-vtessera.json # last successful agents fetch (committed)
  content/live-metrics.json  # last successful usage fetch (committed)
  public/                    # generated output
  .github/workflows/pages.yml
```

Four units with deliberately narrow interfaces:

- `content` parses and validates curated entries. Knows nothing about the network.
- `live` fetches the vtessera agent list and manages cache fallback. Knows
  nothing about HTML.
- `render` turns entries into files. Knows nothing about either of the above.
- `main.go` is the only place the three meet.

Each unit is testable in isolation, and an internal change does not propagate to
consumers.

## Data model

One JSON file per curated listing.

```json
{
  "slug": "vtessera",
  "name": "vtessera",
  "summary": "Non-custodial A2A agent marketplace.",
  "url": "https://example.invalid",
  "agent_card_url": "https://example.invalid/.well-known/agent-card.json",
  "mcp_endpoint_url": null,
  "category": "marketplace",
  "access": "open-source",
  "source": "live",
  "last_verified": "2026-09-27T09:00:00Z"
}
```

- `agent_card_url` and `mcp_endpoint_url` are the fields that earn the site a
  crawler. Either may be null.
- `last_verified` is required on every entry and rendered visibly.
- `category` and `access` are optional.
- `source` is `curated` or `live`. The generator sets it for the vtessera entry;
  it is authored for all others, so a reader can always distinguish a
  human-maintained listing from a machine-refreshed one.
- Validation is strict. Unknown fields are rejected; a malformed entry fails the
  build rather than rendering a half-broken card.

### Live snapshot

`content/live-vtessera.json` holds the last successful response from
`vtessera /v1/agents`. Its absence is a normal first-class state, not an error.
This is what allows the site to ship before `vtessera` is deployed.

The live entry overrides any curated vtessera stub during merge.

## Pipeline

- `go run ./...` writes `public/` locally.
- `.github/workflows/pages.yml` builds and deploys via
  `actions/upload-pages-artifact` and `actions/deploy-pages`. No `gh-pages`
  branch. **This job holds `contents: read` and writes no commits.**
- A **separate** scheduled refresh job holds `contents: write`. It fetches the
  live feeds and commits the snapshots, which is what makes "(committed)" in the
  architecture tree true and what lets the cache-age display mean anything.
- Every push trigger is path-filtered to exclude `content/live-*.json`, so the
  refresh commit cannot re-trigger a build. This loop-closure rule is what the
  earlier draft of this spec lacked, and its absence is why the cache
  lifecycle here was previously self-contradictory.
- Triggers: push to `main` (content edits), weekly cron (refresh the live
  feeds), manual dispatch.
- The apex domain is emitted by the generator from a single config constant
  rather than a committed `public/CNAME`, so there is one source of truth.
- `gofmt` and `go vet` run in CI, matching the `vtessera` baseline.

### DNS

`agent-ai-tool.com` is an apex domain and cannot use `CNAME`. Four `A` records
pointing at GitHub's Pages addresses are required, configured at the registrar.
This is a manual step and blocks the first deploy.

## Discovery surface

- `robots.txt` — retrieval and answer-engine crawlers allowed, training crawlers
  disallowed. Bot token names must be verified against each vendor's current
  documentation at implementation time rather than written from memory; vendors
  rename these frequently.
- `sitemap.xml` — every page, generated from entries.
- `llms.txt` — plain-markdown index written for model consumption, not a
  rendering of the HTML.
- `/agents.json` — every entry with its endpoints in one machine-readable file.
  Highest-value artifact on the site: an agent fetches one file and learns how to
  connect to everything listed.
- `/.well-known/agent-card.json` — the directory describes itself as an
  agent-consumable resource, consistent with what `vtessera` serves.
- Per-entry JSON-LD typed as `SoftwareApplication`, plus `title`, `description`,
  and canonical URL on every page.

### Stated limitation

`robots.txt` is a request, not an enforcement mechanism. It signals intent and
is trivially ignored by a determined party. There is no technical guarantee that
content is excluded from training corpora. A real guarantee would be a separate
and considerably heavier licensing decision, deliberately out of scope.

## Error handling

Three tiers, distinguished by consequence:

1. **Live fetch fails** (timeout, non-200, malformed or empty body) — fall back
   to the committed cache, mark the vtessera section stale with the cache's age,
   emit a loud warning, and **succeed**. Publishing never depends on an external
   service being up.
2. **No cache and no service** — render "not yet available" and succeed. This is
   the expected first-deploy state.
3. **Content validation fails** — hard fail naming file, field, and reason. A
   malformed curated entry is a defect in this repository and should stop the
   build rather than silently drop a card.

### Security

All interpolated content is HTML-escaped. Curated entries are trusted but
`live-vtessera.json` is untrusted network input: a compromised or misbehaving
`vtessera` could otherwise inject markup into pages served to crawlers. A test
specifically attempts script injection through a live-fetched field.

### Link rot

Entry URLs are not checked during the build. A separate non-blocking CI job
probes them and files an issue listing anything unreachable. The build stays
deterministic while rot still surfaces.

`last_verified` on curated entries and the two live snapshots are the only
things that vary between builds, so rebuilds are otherwise diff-clean. The
snapshots now genuinely advance, because the refresh job commits them.

## Testing

- `content` — valid entry parses; each required field missing fails; unknown
  fields rejected; duplicate slug rejected.
- `live` — `httptest` covering 200, timeout, 500, malformed body, empty body,
  and cache fallback when the service dies.
- `render` — script injection escaped; `last_verified` present in output;
  `robots.txt` carries the retrieval/training split; `sitemap.xml` lists every
  entry; `agents.json` is valid JSON containing all endpoints.
- One end-to-end golden-file test over a fixture set, so accidental template
  changes appear as a diff.

## Dependency on vtessera

This site has no build-time code dependency on `vtessera`; it reads the deployed
service over HTTP at build time. Until `vtessera` is deployed the live section
is simply absent, which is a supported state.

The `code-improvement` agentic workflow in the `vtessera` repository is
unrelated to this project and is not expected to maintain this site.

## Open items

- Verify current AI crawler token names against vendor documentation.
- Confirm the Pages `A` records for the apex domain at the registrar.
- Decide the initial curated entry set and who maintains it.
- Confirm the full list of `vtessera` fields worth surfacing once deployed.
- Usage metrics are specified in
  `2026-09-27-agent-ai-tool-usage-metrics-design.md`. Their dependency,
  `GET /v1/metrics`, now exists and is public and unauthenticated; the site
  work that consumes it has not started.
