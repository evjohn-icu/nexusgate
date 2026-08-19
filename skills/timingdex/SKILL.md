---
name: timingdex
description: Safely inspect a local Timingdex video library, retrieve shot-level footage, and create or revise a reviewable repurpose plan through its local HTTP API. Use when a user asks to find reusable video material, plan a new edit from existing footage, inspect library readiness, or revise a Timingdex draft; never use it to approve plans, manage provider keys, modify media, or run the processing pipeline.
---

# Timingdex Asset Planner

Use Timingdex as an editorial research and draft-planning system. Return
concrete shot IDs, asset IDs and time ranges; do not claim that a clip is
available unless the API returned it.

## Preconditions

1. Ask for the local Timingdex base URL when it is not supplied. Default to
   `http://127.0.0.1:8787` only when that is appropriate for the user’s local
   machine.
2. Ask for the Timingdex **agent token** (a credential distinct from the Hub
   administrator token) if one is not already supplied. Send it as
   `Authorization: Bearer <agent-token>` on every write request below. Reading
   endpoints (`/api/v1/hardware`, `/api/v1/jobs`, search, plan inspection) need
   no credential when the request comes from the Hub's own machine or LAN; from
   any other network they answer `403 Forbidden` unless the agent token is sent,
   so send it on reads too whenever the base URL is not local.
3. Request `GET /api/v1/agent/capabilities` first.
4. Stop if the response does not declare `approval_mode: human_required`, or
   if the requested action is absent from `allowed_actions`.
5. Never request, read, retain or print API keys, environment values, raw media
   paths, or provider configuration. The agent token itself is not a provider
   key: still don't print it back to the user or log it, since Timingdex never
   accepts it back in a response body and there is no reason for it to appear
   in transcript output.

The capability contract is version `v0.15`. Its current action and route
allowlist is:

- `allowed_actions`: `inspect_readiness`, `search_shots`, `read_transcript`,
  `create_draft_plan`, `inspect_plan`, `revise_draft_plan`
- Agent-token write routes: `POST /api/v1/repurpose/plans` and
  `POST /api/v1/repurpose/plans/{id}/revisions`
- `denied_actions`: `approve_plan`, `run_pipeline`, `read_provider_keys`,
  `access_original_media_paths`, `export_timeline`, `manage_tags`,
  `manage_collections`, `manage_webdav_accounts`, `manage_webdav_spaces`,
  `manage_roots`, `manage_workers`, `manage_provider_channels`,
  `manage_pipeline_throttle`, `manage_library_summary`

These values must match `GET /api/v1/agent/capabilities`; do not infer an
additional permission from a route being readable. Read-only route details and
field semantics are in [the API contract](references/api-contract.md).

## Workflow

### Via MCP (recommended when the agent supports MCP)

If this agent runs with MCP access to `timingdex-mcp` (see
`mcp/.mcp.json.example` and `references/mcp-usage.md`), prefer the MCP tools over
hand-built HTTP calls:

1. `inspect_library` first — confirm the library is healthy before planning.
2. `search_footage(q, limit)` — find shots (returns shot id, asset id, time
   ranges, score, description).
3. `get_transcript(asset_id)` — read the word-level timeline transcript; its
   `source` field says whether timestamps are word-aligned (`aligned`) or
   sentence-level only (`asr`).
4. `create_edit_plan(brief)` — draft the edit plan.
5. `revise_edit_plan(plan_id, sections_json)` — fill sections with concrete
   shot selections.
6. `request_source_media(space_id, asset_id)` — when the user wants the
   footage delivered for editing, link the original media into the operator's
   on-demand WebDAV space and return the mount path. This tool calls an
   administrator-guarded route: it only works when a Hub administrator token is
   configured on the MCP server, so with an agent-token-only configuration the
   delivery step is operator-mediated (an operator creates the space and hands
   the editing software its WebDAV credentials).

The same boundaries apply: approval stays human, the pipeline is never run,
and provider keys are never read.

### Via HTTP (when MCP is unavailable)

### Inspect readiness

1. Read `/api/v1/health`, `/api/v1/hardware` and `/api/v1/jobs?limit=100`.
2. Read `/api/v1/cost/summary` and `/api/v1/pipeline/throttle` when reporting
   cost context: use `today_estimate`/`month_estimate` for accumulated estimates
   and `throttle.daily_cost_guide`/`throttle.monthly_cost_guide` for advisory
   settings.
3. Summarize pending/running/failed work plainly. A failed or unfinished job is
   evidence that the library may be incomplete, not a reason to invent results.
4. Do not call the pipeline-run endpoint. It is intentionally outside this
   Skill’s allowlist because it may trigger cost and long-running work.

### Find material

1. Translate the user’s brief into a short retrieval query.
2. Call `/api/v1/search/shots/hybrid?q=<url-encoded-query>&limit=20`.
3. For structured search, use `POST /api/v1/search/shots`; if useful, call
   `/api/v1/shots/{shot-id}/similar?limit=10` or
   `/api/v1/discover/rare-shots?limit=20`.
4. Report each proposed clip as `asset_id`, `shot_id`, `start_ms`, `end_ms`,
   score and API-provided reasons. State “no match” when the result is empty.

### Create a draft plan

1. Send the user’s brief to `POST /api/v1/repurpose/plans` with the agent
   token in the `Authorization` header.
2. Retrieve the resulting plan and explain each section, its selected candidate
   (if any), missing needs and any `reused` candidate.
3. Treat the response as a draft. Never represent it as an approved edit.

### Revise a draft plan

1. Read the latest revision with
   `GET /api/v1/repurpose/plans/{plan-id}/revisions`.
2. Preserve the full ordered section snapshot. Change only the editorial choice
   requested by the user.
3. Ensure a `selected_shot_id` belongs to that section’s `candidates` and is
   not in `excluded_shot_ids`.
4. Preserve `locked: true` selections. To change one, send `unlock: true` in
   that one revision request; Timingdex consumes this action and does not store
   it in the resulting snapshot.
5. Submit `POST /api/v1/repurpose/plans/{plan-id}/revisions` with the complete
   `sections` array, an `editor_note` that records the user’s decision, and the
   agent token in the `Authorization` header.
6. Report the new revision number and the exact selection changes.

## Human Approval Boundary

Never call `POST /api/v1/repurpose/plans/{id}/revisions/{n}/approve`. This is
not only a convention this Skill follows: the agent token is refused on that
route by the Hub's own access control (it returns `401`), and on
`POST /api/v1/pipeline/run` for the same reason. There is no token this Skill
can hold that would make either call succeed — the Hub administrator token
that does is deliberately never given to this Skill.

When a draft is ready, show the user the selected shots, remaining gaps,
exclusions and locked sections, then ask them to approve it in the Timingdex
workspace. If an API response says a required section needs selection, explain
which section needs a human choice rather than bypassing the validation.

## Failure Handling

- `404` plan or shot: report that the library state changed or the ID is not
  available; do not substitute a guessed ID.
- Empty search: say that no indexed match was found and offer a broader query
  or a clearly labelled missing-material need.
- `409` immutable plan: show the approved plan and explain that it cannot be
  revised.
- `400` revision validation: preserve the prior revision and explain the
  violated selection/lock/exclusion rule.

Read [the API contract](references/api-contract.md) before constructing a
request body or interpreting a response field.
