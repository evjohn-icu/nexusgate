# NexusSlate v0.15 Local Agent API Contract

Base URL: the user’s local NexusSlate server, normally
`https://127.0.0.1:8787` when using the default Hub TLS mode. Use
`http://127.0.0.1:8787` only when the operator explicitly configured
`hub_tls.mode=off` for local development. All requests and responses are JSON
unless noted. This Skill is limited to the following requests.

## Authentication

NexusSlate has two independent bearer credentials, generated once on first Hub
start and stored under the Hub's data directory as raw token text in exact-0600
files (never in SQLite, never returned by any API response):

- **Hub administrator token** (`admin-token`) — full control: provider
  channels, roots, worker pairing, plan approval, pipeline runs. This Skill is
  never given this token.
- **Hub agent token** (`agent-token`) — the only credential this Skill holds.
  Send it as `Authorization: Bearer <agent-token>` on:
  - `POST /api/v1/repurpose/plans`
  - `POST /api/v1/repurpose/plans/{id}/revisions`

The raw agent token is read from the local `agent-token` file. The file is a
regular, non-symlink file with exact mode 0600; the Hub reuses its value after a
restart. Never print, log, persist elsewhere, or expect the token in an API
response.

The browser UI has a separate short-lived administrator session. It is not an
Agent credential and is never accepted by Worker routes. `POST
/api/v1/auth/admin/session` accepts the administrator token only over HTTPS and
with a same-origin `Origin` header, then sets an opaque `HttpOnly` session
cookie plus a separate readable CSRF cookie. The token is not returned or
stored in browser storage. Browser mutation requests authenticated by that
cookie must send the matching `X-CSRF-Token` and same-origin `Origin`; `DELETE
/api/v1/auth/admin/session` revokes the session. Sessions are memory-only and
expire on Hub restart. CLI, Agent, and Worker clients continue using their
existing Bearer headers.

The Hub keeps at most 64 browser Sessions in memory and applies a short login-failure
cooldown per peer address. Session state is lost on Hub restart; it is not persisted
in SQLite or shared across Hub processes. The default Hub URL is HTTPS, and forwarded
headers are not used to manufacture browser trust.

Every other write route — most importantly
  `POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve` and
  `POST /api/v1/pipeline/run` — is wrapped in `requireHubAdmin` rather than
  `requireAgentOrAdmin` (`internal/api/server.go`). This is enforced by the
  Hub's route wiring, not by this document.

  **The agent token is refused everywhere. What varies is whether a credential
  is demanded at all.** `requireHubAdmin` waives only a request that carries
  **no `Authorization` header** (`internal/api/network_guard.go:101-103`); any
  presented credential is checked against the administrator token and rejected
  if it does not match, from any network:

  | `admin_auth` | Trusted peer, **no** header | Trusted peer, agent token | Remote peer |
  |---|---|---|---|
  | `required` | `401` | `401` | `401` |
  | `trusted_network` (**default**) | **succeeds** | `401` | `401` |
  | `off` | succeeds | `401` | succeeds |

  So `401 Unauthorized` for the agent token holds under every mode — this Skill
  cannot reach an administrator route with the token it has. What does **not**
  hold is "an administrator route always demands a credential": under the
  default `trusted_network`, an unauthenticated request from the Hub's own
  machine or LAN succeeds. `TestAdminAuthModeRootWriteGuard` pins both halves —
  `trusted_network waives a trusted peer` expects `201`, and
  `trusted_network never waives on a presented credential` expects `401` for
  `Bearer wrong-token` from the same trusted address.

  The practical consequence for an operator: "agents draft, humans approve" is
  enforced against *this Skill* by the token in every mode, but a different
  process on the same LAN that sends no header at all is not stopped under the
  default. `hub_security.admin_auth: required` closes that.

The read-only routes below (`/api/v1/hardware`, `/api/v1/jobs`, search, plan
inspection) need no credential when the request comes from a trusted network —
by default loopback, the RFC1918 LAN ranges, and the CGNAT range overlays such
as Tailscale use. From anywhere else they return `403 Forbidden` unless a
credential is presented, and the agent token counts. So a Skill running on the
user's machine or LAN needs no header on reads, and one running elsewhere
should send the agent token on every request, reads included. `/api/v1/health`
is always reachable.

## Library scan behavior

`POST /api/v1/roots/{id}/scan` performs discovery and queue insertion
synchronously, then starts the existing single-run Pipeline in the Hub
background. The response keeps the scan counters and adds
`pipeline_started`, `pipeline_busy`, and `pipeline_status`, where the status is
`started` or `already_running`. These fields describe the trigger, not Pipeline
completion; jobs and Progress remain the source of execution status. The CLI
command `nexusslate root scan <root-id>` uses the same scan path but runs the
Pipeline synchronously before exiting. This does not enable the unattended
`library_supervisor`, whose default remains disabled.

## Capability handshake

```text
GET /api/v1/agent/capabilities
```

Require `approval_mode` to be `human_required`. `allowed_actions` currently
contains `inspect_readiness`, `search_shots`, `read_transcript`,
`create_draft_plan`, `inspect_plan`, and `revise_draft_plan`.
`allowed_write_routes` lists the exact
routes that accept the agent token (the two `POST /api/v1/repurpose/plans...`
routes above). `denied_actions` must include `approve_plan`, `run_pipeline`,
`read_provider_keys`, `access_original_media_paths`, and `export_timeline`.
The last one covers `GET /api/v1/repurpose/plans/{id}/export.edl` and
`.fcpxml`: both require the Hub administrator token and reject the agent
token, because an FCPXML names the absolute path of every original file and
and an EDL is the artifact an editor cuts with. Do not attempt either; hand the
plan id to the operator instead. The response also
carries an `auth` object describing the header format above; treat it as
documentation, not as something to branch on. The response additionally
carries a `providers` object (`asr`/`vision`/`repurpose`/`tag_curator`/
`embedding`/`alignment`), each listing the selected `primary` and `fallbacks`
names plus the `configured` blocks that are actually enabled (`name`,
`protocol`, `model` — never credentials). It is informational: read it to
learn which capabilities this Hub is wired for, and do not branch on its exact
shape.

## Readiness and retrieval

```text
GET /api/v1/health
GET /api/v1/hardware
GET /api/v1/jobs?limit=100
GET /api/v1/search/shots/hybrid?q=<query>&limit=20
POST /api/v1/search/shots
GET /api/v1/shots/{shot-id}/similar?limit=10&date_from=&status=
GET /api/v1/discover/rare-shots?limit=20
GET /api/v1/assets/{asset-id}/transcript
GET /api/v1/cost/summary
GET /api/v1/pipeline/throttle
```

`GET /api/v1/shots/{shot-id}/similar` accepts the same filters the browse
listing does, so "more like this" can be scoped to the material you are
already working within: the facet params (`asset_type`, `asset_shot_size`,
`asset_camera_motion`, `asset_audio_type`, `asset_quality`, `asset_usable_as`,
`min_duration_ms`, `max_duration_ms`) and the asset-context params
(`date_from`, `date_to` — both `YYYY-MM-DD` — plus `region`, `camera`,
`session`, `status`). An unparseable date or an unknown status is a 400
`invalid_request`, never a silently empty result. Omitting them all is the
unfiltered behaviour this endpoint has always had.

`GET /api/v1/assets/{asset-id}/transcript` returns the asset's transcript with
the strongest timing evidence available. It is a trusted read like the rest of
the retrieval surface:

```json
{
  "asset_id": "asset_8",
  "source": "aligned",
  "language": "zh",
  "text": "完整文本",
  "segments": [{"start_ms": 0, "end_ms": 1200, "text": "句子"}],
  "words": [{"start_ms": 0, "end_ms": 260, "text": "词", "confidence": 0.94}]
}
```

`source` is required and tells you where the timestamps come from:

- `aligned` — word-level forced alignment; `words` is present with precise
  per-word `start_ms`/`end_ms` (and optional `confidence`). This is the
  strongest timing evidence the pipeline has.
- `asr` — the ASR transcript's sentence segments only; `words` is omitted.

The word stream is contiguous across shot boundaries — do not re-split it by
shot. `404 not_found` means the asset has no transcript at all, which is
different from an empty one: do not treat a 404 as silent footage. The request
needs no credential from a trusted network and answers `403` elsewhere unless
the agent token is sent, like every other read route.

`GET /api/v1/search/shots/hybrid` is the legacy retrieval endpoint and keeps
its exact response shape. `POST /api/v1/search/shots` is the structured
Search v2 endpoint (same read scoping: trusted network or a credential; it
never mutates state):

```json
POST /api/v1/search/shots
{
  "query": "夜晚下雨，有人撑伞走过街道",
  "mode": "auto",
  "limit": 20,
  "offset": 0,
  "diversity": 0.2,
  "include_evidence": true,
  "include_context": false,
  "facets": { "shot_sizes": ["wide"] },
  "asset_filter": { "captured_from": "2026-01-01T00:00:00Z", "session_id": "sess_12" }
}
```

`mode` is `auto` (the default; the Hub routes the intent itself) or one of
`fact`/`speech`/`semantic`/`similar`/`creative`.

`facets` narrows by the **shot's** own controlled vocabulary (`asset_types`,
`shot_sizes`, `camera_motions`, `audio_types`, `qualities`, `usable_as`,
`min_duration_ms`, `max_duration_ms`). `asset_filter` narrows by the **owning
asset's** capture context and is a separate object: `captured_from`,
`captured_to` (RFC3339), `region_label`, `camera_model`, `session_id`,
`status`. A nil or all-empty `asset_filter` matches everything. Use it to scope
a search to one shoot ("only the material from session X") without spelling the
shoot out in the query text.

`offset` pages the final ranked list **after** selection and diversity — it
never trims the recall pool, so paging does not change what ranked. The
response echoes the resolved intent, adds `search_id`/`query_hash` (for
correlating feedback with a query), reports its own paging, and each result
carries per-constraint `evidence`:

```json
{
  "query": { "raw": "夜晚下雨，有人撑伞走过街道", "intent": "fact" },
  "search_id": "9f2c…", "query_hash": "a1b2c3d4e5f60718",
  "offset": 0, "limit": 20, "has_more": true, "next_offset": 20,
  "window_exhausted": false,
  "results": [{
    "shot_id": "shot_123", "asset_id": "asset_8", "filename": "A0038.MOV",
    "start_ms": 50120, "end_ms": 56480, "score": 0.91,
    "scores": { "lexical": 0.78, "heuristic_semantic": 0.82, "rrf": 0.91 },
    "rank": 1,
    "evidence": [
      { "constraint_type": "object", "constraint": "person", "state": "confirmed", "sources": ["objects"] },
      { "constraint_type": "object", "constraint": "umbrella", "state": "confirmed", "sources": ["objects", "description"] },
      { "constraint_type": "weather", "constraint": "rain", "state": "possible", "sources": ["description"] }
    ]
  }]
}
```

Paging — read these fields rather than inferring the end of the list from a
short page:

- `has_more` — another page exists inside the search window. `next_offset` is
  present exactly when `has_more` is true and names the offset to request next.
- `window_exhausted` — this page ends at the hard `MaxSearchWindow` boundary.
  More matches may exist beyond it, but no `offset` will reach them: the window
  caps how far one query may traverse. The answer is a narrower query or a
  tighter `facets`/`asset_filter`, not more paging. `has_more` is false when
  `window_exhausted` is true, so a client that only reads `has_more` will
  believe it saw everything.

Evidence semantics — the boundary this Skill must respect:

- `confirmed` — the canonical term appears in a structured observation field
  (objects/actions/tags/mood). This is a model observation. Structured and
  positive description evidence is reported together, in deterministic source
  order.
- `possible` — the term appears only in the description (narrative), or a
  complete aligned transcript speech phrase matches. A speech phrase must
  match all components in order, with ASCII whole-word matching, CJK sequence
  matching across ASR segmentation, and no more than 1500 ms between adjacent
  matched spans. Partial, reordered, or over-gap phrases are `unknown`.
- `contradicted` — the shot's own words explicitly negate the term
  ("no people", "没有人"). Explicit description negation wins over structured
  positive evidence; both structured and description sources are returned.
- `unknown` — no evidence found. `unknown` is NOT "确认无人": absence is never
  asserted from silence. A `negated: true` evidence entry means the query
  asked for the absence (e.g. "没有人的海边空镜"): `confirmed` there means the
  forbidden thing was observed in that shot, `unknown` means it was not
  observed — never read `unknown` on a negated entry as a confirmed absence.

For a negated query, structured positive evidence remains `confirmed` and is
the exclusion signal even when the description says the term is absent.
Description-only positive evidence is `possible` and is also excluded;
description-only absence remains `unknown` and keeps the shot.

Do not treat a high `score` as proof that the shot contains what the query
named: scores are retrieval signals, evidence is the claim. The `scores` map
may include `text_embedding` when the Hub has an embedding provider
configured (`providers.embedding` or a provider channel); like every other
signal it is retrieval, never evidence.

Shot results contain an `id` (the shot ID), `asset_id`, `start_ms`, `end_ms`,
`score`, `description`, `tags`, and the `lexical_score`/`semantic_score` when
available. Rare-shot results additionally carry a `rarity_score` and a `reason`.
Never infer a source file path from these IDs.

## Cost Guide Reads

`GET /api/v1/cost/summary` is a trusted read and returns the accumulated ledger
guides for the current UTC periods:

```json
{ "today_estimate": 0.0, "month_estimate": 0.0 }
```

`GET /api/v1/pipeline/throttle` returns the configured advisory settings under
`throttle.daily_cost_guide` and `throttle.monthly_cost_guide`. These values are
operator references only; they never gate or defer transcription or analysis.

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
reserved for a human in the NexusSlate workspace, and the agent token is refused
with `401` there under every `admin_auth` mode (see the table above).
