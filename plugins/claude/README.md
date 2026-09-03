# NexusSlate Claude Code plugin

Adds NexusSlate's footage capability provider to Claude Code: inspect the
local-first library, search shots with evidence, and read word-level
transcripts. The plugin exposes the current six MCP tools as read-only
inspection tools; draft-plan writes belong to the separate HTTP Skill workflow.

The plugin declares `nexusslate-mcp` as a stdio MCP server. It does **not**
bundle a binary — install `nexusslate-mcp` on `PATH` first:

```bash
go build -o /usr/local/bin/nexusslate-mcp ./cmd/nexusslate-mcp
```

## Environment

The MCP server reads these variables from the environment (the plugin's
`.mcp.json` passes them through):

| Variable | Meaning |
|---|---|
| `NEXUSSLATE_BASE_URL` | Hub URL. The normal local Hub is `https://127.0.0.1:8787`; use `http://127.0.0.1:8787` only when `hub_tls.mode=off` is explicitly configured for local development. For a cross-machine Hub use `https://<hub-host>:8787`. |
| `NEXUSSLATE_AGENT_TOKEN` | The Hub agent token (from `<dataDir>/agent-token`). Needed for reads off the trusted LAN. |
| `NEXUSSLATE_HUB_FINGERPRINT` | The Hub's SHA-256 certificate fingerprint, printed by `nexusslate serve` at startup. **Required** when `NEXUSSLATE_BASE_URL` is `https://` — the server refuses to start without it rather than accept an unpinned certificate. |

## Boundaries

- The MCP server is read-only: search, inspect and retrieve. It does not create
  or revise Repurpose plans. Those draft actions belong to the separate HTTP
  Skill workflow; humans approve plans in the browser UI.
- The MCP agent never sees raw media paths, provider keys or the administrator
  token.

## Install

```bash
claude plugin marketplace add evjohn-icu/nexusslate
claude plugin install nexusslate@nexusslate
```
