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
2. Request `GET /api/v1/agent/capabilities` first.
3. Stop if the response does not declare `approval_mode: human_required`, or
   if the requested action is absent from `allowed_actions`.
4. Never request, read, retain or print API keys, environment values, raw media
   paths, or provider configuration.

## Workflow

### Inspect readiness

1. Read `/api/v1/health`, `/api/v1/hardware` and `/api/v1/jobs?limit=100`.
2. Summarize pending/running/failed work plainly. A failed or unfinished job is
   evidence that the library may be incomplete, not a reason to invent results.
3. Do not call the pipeline-run endpoint. It is intentionally outside this
   Skill’s allowlist because it may trigger cost and long-running work.

### Find material

1. Translate the user’s brief into a short retrieval query.
2. Call `/api/v1/search/shots/hybrid?q=<url-encoded-query>&limit=20`.
3. If useful, call `/api/v1/shots/{shot-id}/similar?limit=10` or
   `/api/v1/discover/rare-shots?limit=20`.
4. Report each proposed clip as `asset_id`, `shot_id`, `start_ms`, `end_ms`,
   score and API-provided reasons. State “no match” when the result is empty.

### Create a draft plan

1. Send the user’s brief to `POST /api/v1/repurpose/plans`.
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
   `sections` array and an `editor_note` that records the user’s decision.
6. Report the new revision number and the exact selection changes.

## Human Approval Boundary

Never call `POST /api/v1/repurpose/plans/{id}/revisions/{n}/approve`.

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
