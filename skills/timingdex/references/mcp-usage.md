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
| `inspect_library` | GET /api/v1/health, /hardware | trusted read (token optional) |
| `search_shots` | POST /api/v1/search/shots | trusted read (token optional) |
| `get_timeline` | GET /api/v1/assets/{id}/shots | trusted read (token optional) |
| `get_asset` | GET /api/v1/assets/{id} | trusted read (token optional) |
| `get_shot` | GET /api/v1/shots/{id} | trusted read (token optional) |
| `get_transcript` | GET /api/v1/assets/{id}/transcript | trusted read (token optional) |

Every tool is a trusted read: on the Hub's own machine or a trusted LAN/Tailnet
the request succeeds with no token at all; from any other (remote) network the
Hub answers 403 unless `TIMINGDEX_AGENT_TOKEN` is configured, so set it unless
the Hub is on a trusted network. An https base URL always requires
`TIMINGDEX_HUB_FINGERPRINT` (see Cross-machine above) or the client refuses to
start. `inspect_library` returns the health and hardware report only — a
readiness hint, not a full jobs/index readiness probe; for pipeline and job
state use the Skill's HTTP readiness flow (`/api/v1/jobs`, `/api/v1/issues`).

`search_shots` uses the structured Search v2 endpoint, so each result carries
per-constraint `evidence` (confirmed/possible/contradicted/unknown) alongside
its score — treat a high score as a retrieval signal, the evidence as the
claim. `get_timeline` enumerates every shot of one asset (all shots and exact
time ranges), which `search_shots` does not return.

## Boundaries

The MCP server is **read-only**: its six tools read library state only, and
none creates or revises repurpose plans. Drafting and revising plans is a
separate HTTP agent-token workflow (`POST /api/v1/repurpose/plans` and
`POST /api/v1/repurpose/plans/{id}/revisions`) exposed through the Skill
(`skills/timingdex/SKILL.md`), not through MCP.

Approval, pipeline runs, provider keys and raw media paths stay with the
human/admin boundary: the agent token is refused on approve and pipeline
routes, and the Hub administrator token is never given to the MCP server.
The capability handshake (`GET /api/v1/agent/capabilities`) belongs to that
HTTP Skill workflow — the MCP server does not call it; each tool is wired
directly to its read endpoint.

## Example agent prompts

- "Inspect the library, then find 20 shots of city night scenes."
- "Search for sunset shots, then get the timeline of the best-matching asset."
- "Get the transcript of asset XYZ and the full detail of shot ABC."
