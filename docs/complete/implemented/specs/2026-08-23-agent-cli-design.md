# vtessera-agent — CLI entry point for AI agents

**Date:** 2026-08-23
**Status:** Approved
**Scope:** Free jobs only (no Solana deps)

## Problem

Agents must manually craft `curl` requests with hand-crafted JSON to
interact with a vtessera node. The ROADMAP (§2d) tracks this as an open
gap: "No agent CLI entry point."

## Design

A new crate `crates/agent-cli/` with binary `vtessera-agent`. Four
subcommands, no Solana dependencies.

### Subcommands

| Command | Description |
|---------|-------------|
| `discover` | Query offer-index, list available nodes |
| `offer` | Fetch a node's signed offer |
| `submit` | Submit a free job, print result |
| `health` | Check if a node is up |

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--node <url>` | `http://127.0.0.1:8402` | Node HTTP endpoint |
| `--index <url>` | `http://127.0.0.1:8403` | Offer-index URL |
| `--agent-id <id>` | random UUID | Agent identity for claim gate |
| `--json` | off | Output raw JSON instead of pretty text |

### Behavior

**`discover`** — `GET <index>/offers?available=1&mode=free`
prints a table of available nodes (node_id, device, price, endpoint).

**`offer`** — `GET <node>/offer`
prints the signed offer JSON (or pretty-printed by default).

**`submit`** — reads a `JobSpec` JSON file, POSTs to `<node>/jobs`
with `X-Agent-Id` header. Prints the response envelope (job_id,
metering, receipt). Exits non-zero on error.

**`health`** — `GET <node>/healthz`
prints `ok` or error.

### Dependencies

- `ureq` (HTTP client, already a workspace dep)
- `serde_json` (already a workspace dep)
- `clap` (arg parsing)
- `uuid` (random agent-id generation, optional — can use random bytes)

### Flatpak

Add `-p vtessera-agent` to the build step and `vtessera-agent` to the
install line in `packaging/flatpak/io.github.douglasdemaio.Vtessera.json`.

### Example usage

```bash
# Discover available free nodes
vtessera-agent discover

# Check a specific node
vtessera-agent health --node http://192.168.1.50:8402

# View a node's offer
vtessera-agent offer --node http://192.168.1.50:8402

# Submit a free job
vtessera-agent submit --node http://192.168.1.50:8402 --job job.json

# Raw JSON output
vtessera-agent discover --json
```
