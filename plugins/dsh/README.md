# NexusGate DeepSeek Harness (DSH) plugin

Adds NexusGate's footage capability provider to DeepSeek Harness (dsh): the
agent can inspect the local-first library, search shots with evidence, and
read word-level transcripts through the same six read-only MCP tools
(`inspect_library`, `search_shots`, `get_timeline`, `get_transcript`,
`get_asset`, `get_shot`). Draft-plan writes stay in the separate HTTP Skill
workflow; approval remains human-only in the browser UI.

The plugin is a configuration bundle only — it contains no Node code. It
inserts one `@deepseek-ai/dsh-mcp-client` row into the profile composition,
which spawns your installed `nexusgate-mcp` binary over stdio. Install the
binary first:

```bash
go build -o /usr/local/bin/nexusgate-mcp ./cmd/nexusgate-mcp
```

## Install

Install the bundle into a dsh profile (dsh resolves `@deepseek-ai/dsh-mcp-client`
from its own install, so no extra npm packages are needed):

```bash
dsh plugin --profile web add link:/path/to/nexusgate/plugins/dsh
```

Restart dsh. The server's tools appear under the stable names
`mcp__nexusgate__<tool>` (for example `mcp__nexusgate__search_shots`), the
same namespace shape Claude Code and Codex use.

## Environment

The MCP server reads these variables. They are passed through explicitly by
the bundle because the DSH MCP bridge scrubs credential-shaped environment
names from spawned children — without the explicit pass-through
`NEXUSGATE_AGENT_TOKEN` would be dropped before the child starts:

| Variable | Meaning |
|---|---|
| `NEXUSGATE_BASE_URL` | Hub URL. The normal local Hub is `https://127.0.0.1:8787`; use `http://127.0.0.1:8787` only when `hub_tls.mode=off` is explicitly configured for local development. For a cross-machine Hub use `https://<hub-host>:8787`. |
| `NEXUSGATE_AGENT_TOKEN` | The Hub agent token (from `<dataDir>/agent-token`). Needed for reads off the trusted LAN. |
| `NEXUSGATE_HUB_FINGERPRINT` | The Hub's SHA-256 certificate fingerprint, printed by `nexusgate serve` at startup. **Required** when `NEXUSGATE_BASE_URL` is `https://` — the server refuses to start without it rather than accept an unpinned certificate. |

Set them the same way you would for the Claude Code plugin, e.g. in the shell
that launches dsh or in your service manager's environment.

## Boundaries

- The MCP server is read-only: search, inspect and retrieve. It does not
  create or revise Repurpose plans. Those draft actions belong to the separate
  HTTP Skill workflow; humans approve plans in the browser UI.
- The dsh agent never sees raw media paths, provider keys or the administrator
  token.
- If the Hub or the MCP child is not running when dsh starts, dsh still boots
  (the bridge tolerates a failed initial connection) and the tools appear once
  the server is reachable.