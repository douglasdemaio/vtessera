# Localhost discovery — well-known file

**Date:** 2026-08-23
**Status:** Approved
**Scope:** Same-machine discovery via a JSON file

## Problem

Agents on the same machine as a vtessera node must know the port (8402)
to connect. The ROADMAP (§2d) tracks this as the remaining UX gap.

## Design

The node writes a small JSON file to a known location on start and
removes it on stop. The agent CLI reads it to discover the running node.

### File location

`$XDG_DATA_HOME/vtessera/node-discovery.json`
Fallback: `~/.local/share/vtessera/node-discovery.json`

### File contents

```json
{
  "endpoint": "http://127.0.0.1:8402",
  "node_id": "abc123...",
  "index": "http://127.0.0.1:8403",
  "pid": 12345
}
```

### Who writes it

The GUI's `daemon.rs` writes the file when it starts the node, removes
it when the node stops. If the node crashes, the file persists (stale)
— the agent CLI checks the PID field to detect this.

### Who reads it

`vtessera-agent` with `--local` flag reads the file and uses the
`endpoint` field. Falls back to `http://127.0.0.1:8402` if the file
doesn't exist or is stale.

### Staleness check

If `pid` is set, the agent checks if the process is alive via
`kill(pid, 0)`. If dead, treats the file as stale and falls back.

### Changes

1. **daemon.rs** — write file on start, remove on stop
2. **vtessera-agent** — add `--local` flag, read discovery file
3. **README** — document the discovery file location
