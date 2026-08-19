# Timingdex MCP Server — usage

`cmd/timingdex-mcp` exposes the Timingdex library to MCP-capable agents
(Codex, Claude Code, Cursor, ...) over **stdio**. It is a thin client of the
Hub HTTP API — it never touches the NAS directly.

## Install

1. Build the binary: `go build -o /usr/local/bin/timingdex-mcp ./cmd/timingdex-mcp`
   (or install with the Hub binary).
2. Create a Hub **agent token** and a Hub **administrator token**:
   - agent token: used for `create_edit_plan` / `revise_edit_plan`.
   - admin token: used for `request_source_media` (linking original media into
     the on-demand WebDAV space; that endpoint is administrator-only).
   Find them in `<dataDir>/agent-token` and `<dataDir>/admin-token`, or set
   the config overrides (`hub_security.agent_token` /
   `hub_security.admin_token` environment-only fields).
3. Copy `mcp/.mcp.json.example` to your agent's MCP config and fill in the
   tokens and the Hub base URL. Codex reads `.mcp.json` from the project
   root or `~/.codex/`.

## Tools

| Tool | Hub endpoint | Token |
|---|---|---|
| `inspect_library` | GET /api/v1/health, /hardware | none (trusted network) |
| `search_footage` | POST /api/v1/search/shots | none (trusted network) |
| `get_shots` | GET /api/v1/assets/{id}/shots | none (trusted network) |
| `get_transcript` | GET /api/v1/assets/{id}/transcript | none (trusted network) |
| `create_edit_plan` | POST /api/v1/repurpose/plans | agent |
| `revise_edit_plan` | POST /api/v1/repurpose/plans/{id}/revisions | agent |
| `request_source_media` | POST /api/v1/admin/webdav/spaces/{id}/links | admin |

`search_footage` uses the structured Search v2 endpoint, so each result carries
per-constraint `evidence` (confirmed/possible/contradicted/unknown) alongside
its score — treat a high score as a retrieval signal, the evidence as the
claim. `get_shots` enumerates every shot of one asset (all shots and exact
time ranges), which `search_footage` does not return.

## Boundaries (same as the HTTP skill)

- Approval stays human (`approve` is refused to the agent token).
- The pipeline is never run from here.
- Provider keys are never read.
- `request_source_media` only *links* a file into the WebDAV space — the file
  is streamed from the NAS on demand and the real path is never returned.

## Example agent prompts

- "Inspect the library, then find 20 shots of city night scenes."
- "Create an edit plan: 60s city tourism promo, then revise it with
  concrete shot selections."
- "Link the original media of shot X into delivery space space-abc so I can
  mount it in Final Cut."
