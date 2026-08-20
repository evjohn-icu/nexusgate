# Timingdex Claude Code plugin

Adds Timingdex's footage capability provider to Claude Code: inspect the
local-first library, search shots with evidence, read word-level transcripts,
and draft reviewable edit plans.

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

- Approval stays human: the agent can draft and revise plans, never approve
  them or run the pipeline.
- The agent never sees raw media paths, provider keys, or the administrator
  token.
- The plugin passes only the agent token, so the admin-guarded
  `request_source_media` MCP tool is not available here: original-media
  delivery via WebDAV is operator-mediated (an operator creates the space and
  hands the editing software its WebDAV credentials).
- `TIMINGDEX_ADMIN_TOKEN` must **not** be exported in the operator's
  environment (shell profile, CI): the spawned `timingdex-mcp` inherits the
  parent environment, and Claude Code merges the `.mcp.json` env with it — so
  an exported admin token would make the admin-gated `request_source_media`
  tool available to the agent, contradicting the boundary above. Set it only
  where the operator (not the agent) will use it.

## Install

```bash
claude plugin marketplace add evjohn-icu/timingdex
claude plugin install timingdex@timingdex
```
