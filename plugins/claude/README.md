# Timingdex Claude Code plugin

Adds Timingdex's footage capability provider to Claude Code: inspect the
local-first library, search shots with evidence, and read word-level
transcripts.

The plugin declares `timingdex-mcp` as a stdio MCP server. It does **not**
bundle a binary — install `timingdex-mcp` on `PATH` first:

```bash
go build -o /usr/local/bin/timingdex-mcp ./cmd/timingdex-mcp
```

## Environment

The MCP server reads these variables from the environment (the plugin's
`.mcp.json` passes them through):

| Variable | Meaning |
|---|---|
| `TIMINGDEX_BASE_URL` | Hub URL. Default `http://127.0.0.1:8787` for a local Hub. For a cross-machine Hub use `https://<hub-host>:8787`. |
| `TIMINGDEX_AGENT_TOKEN` | The Hub agent token (from `<dataDir>/agent-token`). Needed for reads off the trusted LAN. |
| `TIMINGDEX_HUB_FINGERPRINT` | The Hub's SHA-256 certificate fingerprint, printed by `timingdex serve` at startup. **Required** when `TIMINGDEX_BASE_URL` is `https://` — the server refuses to start without it rather than accept an unpinned certificate. |

## Boundaries

- The MCP server is read-only: search, inspect, retrieve. Agents draft plans;
  humans approve them in the browser UI.
- The agent never sees raw media paths, provider keys, or the administrator
  token.

## Install

```bash
claude plugin marketplace add evjohn-icu/timingdex
claude plugin install timingdex@timingdex
```
