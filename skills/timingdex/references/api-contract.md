# Timingdex v0.12 Local Agent API Contract

Base URL: the user’s local Timingdex server, normally `http://127.0.0.1:8787`.
All requests and responses are JSON unless noted. This Skill is limited to the
following requests.

## Capability handshake

```text
GET /api/v1/agent/capabilities
```

Require `approval_mode` to be `human_required`. `allowed_actions` currently
contains `inspect_readiness`, `search_shots`, `create_draft_plan`,
`inspect_plan`, and `revise_draft_plan`. `denied_actions` must include
`approve_plan`, `run_pipeline`, `read_provider_keys`, and
`access_original_media_paths`.

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
`score`, plus description/tags/reasons when available. Never infer a source
file path from these IDs.

## Plan lifecycle

Create a draft:

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

Revise it with a complete ordered snapshot:

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
reserved for a human in the Timingdex workspace.
