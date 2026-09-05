# NexusGate MCP Server — usage

`cmd/nexusgate-mcp` exposes the NexusGate library to MCP-capable agents
(Codex, Claude Code, Cursor, ...) over **stdio**. It is a thin client of the
Hub HTTP API — it never touches the NAS directly.

## Install

1. Build the binary: `go build -o /usr/local/bin/nexusgate-mcp ./cmd/nexusgate-mcp`
   (or install with the Hub binary).
2. Copy `mcp/.mcp.json.example` to your agent's MCP config and fill in the
   tokens and the Hub base URL. Codex reads `.mcp.json` from the project
   root or `~/.codex/`.

## Cross-machine (HTTPS)

When the Hub is on another machine, set `NEXUSGATE_BASE_URL` to
`https://<hub-host>:8787` and set `NEXUSGATE_HUB_FINGERPRINT` to the Hub's
SHA-256 certificate fingerprint printed by `nexusgate serve` at startup. The
client refuses to start with an https base URL and no fingerprint rather than
silently accept any certificate; `http://` remains for local development.

## Tools

| Tool | Hub endpoint | Token |
|---|---|---|
| `inspect_library` | GET /api/v1/health, /hardware, /setup/status, /jobs/summary | trusted read (token optional) |
| `search_shots` | POST /api/v1/search/shots | trusted read (token optional) |
| `get_timeline` | GET /api/v1/assets/{id}/shots | trusted read (token optional) |
| `get_asset` | GET /api/v1/assets/{id} | trusted read (token optional) |
| `get_shot` | GET /api/v1/shots/{id} | trusted read (token optional) |
| `get_transcript` | GET /api/v1/assets/{id}/transcript | trusted read (token optional) |

Every tool is a trusted read: on the Hub's own machine or a trusted LAN/Tailnet
the request succeeds with no token at all; from any other (remote) network the
Hub answers 403 unless `NEXUSGATE_AGENT_TOKEN` is configured, so set it unless
the Hub is on a trusted network. An https base URL always requires
`NEXUSGATE_HUB_FINGERPRINT` (see Cross-machine above) or the client refuses to
start. `inspect_library` is a readiness probe, not just a liveness one: it
returns health, hardware, the setup status (healthy roots, runnable providers,
searchable shots, index state) and the job summary (queued/running/failed) in
one call, so an agent can tell an incomplete library from an idle one before it
plans against it. All four are fetched with the agent token except `/health`,
which is anonymous by design. It still does not enumerate individual jobs or
issues — for that, use the Skill's HTTP flow (`/api/v1/jobs`,
`/api/v1/issues`).

`search_shots` uses the structured Search v2 endpoint, so each result carries
per-constraint `evidence` (confirmed/possible/contradicted/unknown) alongside
its score — treat a high score as a retrieval signal, the evidence as the
claim. `get_timeline` enumerates every shot of one asset (all shots and exact
time ranges), which `search_shots` does not return.

`get_shot` returns one shot's speech alongside its metadata, and says where the
timing came from: `transcript_source` is `aligned` (word boundaries, in
`transcript`), `asr` (sentence segments only, in `transcript_segments`), or
absent when the asset has no transcript at all. The optional align stage never
runs on most assets, so an empty `transcript` is not evidence of silence —
read `transcript_source`.

### `search_shots` arguments

| Argument | Type | Notes |
|---|---|---|
| `query` | string, required | Natural language, a tag, or a quoted phrase |
| `limit` | number | Default 20 |
| `offset` | number | Zero-based offset into the full ranked list; default 0 |
| `filters` | object or string | A JSON object, or that object encoded as a string — both accepted |

`filters` is passed to the Hub as the request's `facets` field. Supported keys:
`asset_types`, `shot_sizes`, `camera_motions`, `audio_types`, `qualities`,
`usable_as` (each a JSON array of strings), and `min_duration_ms` /
`max_duration_ms` (integers). Example: `{"asset_types":["broll"]}`.

**A filter is never silently ignored.** An unknown facet key (`asset_type`
singular, say) is refused by name with the supported list, rather than being
forwarded to a Hub that drops unknown JSON fields — the failure mode that
matters here is not a missing result but a *wider* one, and an over-wide result
set is indistinguishable from a correct one by inspection. Malformed JSON and
wrong argument types are refused the same way. If a call returns results, the
filters you sent were applied.

Paginate with `offset`, and read the response's `has_more`, `next_offset` and
`window_exhausted` fields rather than inferring the end of the list from a
short page. `window_exhausted` means the ranked retrieval window itself ran
out — the answer is to narrow the query, not to page further.

## Boundaries

The MCP server is **read-only**: its six tools read library state only, and
none creates or revises repurpose plans. Drafting and revising plans is a
separate HTTP agent-token workflow (`POST /api/v1/repurpose/plans` and
`POST /api/v1/repurpose/plans/{id}/revisions`) exposed through the Skill
(`skills/nexusgate/SKILL.md`), not through MCP.

Approval, pipeline runs, provider keys and raw media paths stay with the
human/admin boundary, and the Hub administrator token is never given to the
MCP server.

**How that boundary is enforced depends on `hub_security.admin_auth`.** The
agent token is genuinely refused — but under the default, *presenting nothing*
is not. `requireHubAdmin` waives the credential only when the request carries
**no `Authorization` header at all** (`network_guard.go:101-103`); any presented
credential is validated as the admin token or rejected:

| `admin_auth` | Trusted peer, **no** header | Trusted peer, agent token | Remote peer |
|---|---|---|---|
| `required` | `401` | `401` | `401` |
| `trusted_network` (**default**) | **succeeds** | `401` | `401` |
| `off` | succeeds | `401` | succeeds |

So "the agent token is refused on administrator routes" holds everywhere — the
MCP server can never escalate with the token it has. What does not hold is
"an administrator route always demands a credential": under the default, a
process on the Hub's own machine or LAN that sends **no header** gets through.
`TestAdminAuthModeRootWriteGuard` pins both halves — `trusted_network waives a
trusted peer` expects `201`, and `trusted_network never waives on a presented
credential` expects `401` for `Bearer wrong-token` from the same address.

Set `hub_security.admin_auth: required` if you want the credential demanded
rather than inferred from network topology.

The capability handshake (`GET /api/v1/agent/capabilities`) belongs to that
HTTP Skill workflow — the MCP server does not call it; each tool is wired
directly to its read endpoint.

## Example agent prompts

- "Inspect the library, then find 20 shots of city night scenes."
- "Search for sunset shots, then get the timeline of the best-matching asset."
- "Get the transcript of asset XYZ and the full detail of shot ABC."
