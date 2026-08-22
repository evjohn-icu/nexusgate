# Timingdex MCP Server — usage

`cmd/timingdex-mcp` exposes the Timingdex library to MCP-capable agents
(Codex, Claude Code, Cursor, ...) over **stdio**. It is a thin client of the
Hub HTTP API — it never touches the NAS directly.

## Install

1. Build the binary: `go build -o /usr/local/bin/timingdex-mcp ./cmd/timingdex-mcp`
   (or install with the Hub binary).
2. Copy `mcp/.mcp.json.example` to your agent's MCP config and fill in the
   tokens and the Hub base URL. Codex reads `.mcp.json` from the project
   root or `~/.codex/`.

## Cross-machine (HTTPS)

When the Hub is on another machine, set `TIMINGDEX_BASE_URL` to
`https://<hub-host>:8787` and set `TIMINGDEX_HUB_FINGERPRINT` to the Hub's
SHA-256 certificate fingerprint printed by `timingdex serve` at startup. The
client refuses to start with an https base URL and no fingerprint rather than
silently accept any certificate; `http://` remains for local development.

## Tools

| Tool | Hub endpoint | Token |
|---|---|---|
| `inspect_library` | GET /api/v1/health, /hardware | none (trusted network) |
| `search_shots` | POST /api/v1/search/shots | none (trusted network) |
| `get_timeline` | GET /api/v1/assets/{id}/shots | none (trusted network) |
| `get_asset` | GET /api/v1/assets/{id} | none (trusted network) |
| `get_shot` | GET /api/v1/shots/{id} | none (trusted network) |
| `get_transcript` | GET /api/v1/assets/{id}/transcript | none (trusted network) |

`search_shots` uses the structured Search v2 endpoint, so each result carries
per-constraint `evidence` (confirmed/possible/contradicted/unknown) alongside
its score — treat a high score as a retrieval signal, the evidence as the
claim. `get_timeline` enumerates every shot of one asset (all shots and exact
time ranges), which `search_shots` does not return.

## Boundaries (same as the HTTP skill)

- Approval stays human (`approve` is refused to the agent token).
- The pipeline is never run from here.
- Provider keys are never read.

## Example agent prompts

- "Inspect the library, then find 20 shots of city night scenes."
- "Search for sunset shots, then get the timeline of the best-matching asset."
- "Get the transcript of asset XYZ and the full detail of shot ABC."
