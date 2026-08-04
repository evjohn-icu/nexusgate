# Timingdex v0.13 Local Agent API Contract

Base URL: the user’s local Timingdex server, normally `http://127.0.0.1:8787`.
All requests and responses are JSON unless noted. This Skill is limited to the
following requests.

## Authentication

Timingdex has two independent bearer credentials, generated once on first Hub
start and stored under the Hub's data directory (mode 0600, never in SQLite,
never returned by any API response):

- **Hub administrator token** (`admin-token`) — full control: provider
  channels, roots, worker pairing, plan approval, pipeline runs. This Skill is
  never given this token.
- **Hub agent token** (`agent-token`) — the only credential this Skill holds.
  Send it as `Authorization: Bearer <agent-token>` on:
  - `POST /api/v1/repurpose/plans`
  - `POST /api/v1/repurpose/plans/{id}/revisions`

  Every other write route — most importantly
  `POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve` and
  `POST /api/v1/pipeline/run` — accepts only the administrator token and
  returns `401 Unauthorized` for the agent token, or for no credential at all.
  This is enforced by the Hub's route wiring (`requireAgentOrAdmin` vs.
  `requireHubAdmin` in `internal/api/server.go`), not by this document.

The read-only routes below (`/api/v1/hardware`, `/api/v1/jobs`, search, plan
inspection) need no credential when the request comes from a trusted network —
by default loopback, the RFC1918 LAN ranges, and the CGNAT range overlays such
as Tailscale use. From anywhere else they return `403 Forbidden` unless a
credential is presented, and the agent token counts. So a Skill running on the
user's machine or LAN needs no header on reads, and one running elsewhere
should send the agent token on every request, reads included. `/api/v1/health`
is always reachable.

## Capability handshake

```text
GET /api/v1/agent/capabilities
```

Require `approval_mode` to be `human_required`. `allowed_actions` currently
contains `inspect_readiness`, `search_shots`, `create_draft_plan`,
`inspect_plan`, and `revise_draft_plan`. `allowed_write_routes` lists the exact
routes that accept the agent token (the two `POST /api/v1/repurpose/plans...`
routes above). `denied_actions` must include `approve_plan`, `run_pipeline`,
`read_provider_keys`, `access_original_media_paths`, and `export_timeline`.
The last one covers `GET /api/v1/repurpose/plans/{id}/export.edl` and
`.fcpxml`: both require the Hub administrator token and reject the agent
token, because an FCPXML names the absolute path of every original file and
an EDL is the artifact an editor cuts with. Do not attempt either; hand the
plan id to the operator instead. The response also
carries an `auth` object describing the header format above; treat it as
documentation, not as something to branch on.

## Readiness and retrieval

```text
GET /api/v1/health
GET /api/v1/hardware
GET /api/v1/jobs?limit=100
GET /api/v1/search/shots/hybrid?q=<query>&limit=20
GET /api/v1/shots/{shot-id}/similar?limit=10
GET /api/v1/discover/rare-shots?limit=20
```

Shot results contain an `id` (the shot ID), `asset_id`, `start_ms`, `end_ms`,
`score`, `description`, `tags`, and the `lexical_score`/`semantic_score` when
available. Rare-shot results additionally carry a `rarity_score` and a `reason`.
Never infer a source file path from these IDs.

## Plan lifecycle

Create a draft (send the agent token as described above):

```json
POST /api/v1/repurpose/plans
{
  "brief": "30 秒深圳城市生活宣传片",
  "duration_ms": 30000,
  "style": "城市生活",
  "audience": "品牌客户"
}
```

Inspect it:

```text
GET /api/v1/repurpose/plans/{plan-id}
GET /api/v1/repurpose/plans/{plan-id}/revisions
```

Revise it with a complete ordered snapshot (also with the agent token):

```json
POST /api/v1/repurpose/plans/{plan-id}/revisions
{
  "editor_note": "用户选定雨夜航拍作为开场",
  "sections": [
    {
      "role": "opening",
      "query": "urban night",
      "duration_ms": 5000,
      "required": true,
      "candidates": [
        {
          "shot_id": "shot-123",
          "asset_id": "asset-456",
          "start_ms": 12000,
          "end_ms": 18000,
          "score": 0.93
        }
      ],
      "selected_shot_id": "shot-123",
      "locked": true,
      "excluded_shot_ids": []
    }
  ]
}
```

When changing a previously locked selected shot, add `"unlock": true` to
that section in this one request. Do not send the approve endpoint: approval is
reserved for a human in the Timingdex workspace, and the agent token would be
refused with `401` if you tried.
