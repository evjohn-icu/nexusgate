---
name: nexusgate
description: Retrieve footage and inspect a local NexusGate video library through its read-only MCP tools. Use when asked to find reusable video material, check library readiness, search shots with evidence, or read timelines, transcripts, or shot detail; never to approve plans, manage provider keys, touch media, or run the processing pipeline.
---

# NexusGate library tools (DSH / MCP)

NexusGate exposes the library through six read-only MCP tools. They are thin
clients of the local Hub HTTP API and never read provider keys, raw media
paths, or the administrator token.

## Workflow

1. Call `mcp__nexusgate__inspect_library` first. It returns Hub health,
   hardware, setup status (healthy roots, runnable providers, searchable
   shots, index state) and job summary (queued/running/failed) in one call, so
   a healthy response tells you whether the library holds searchable shots
   versus being incomplete.
2. Call `mcp__nexusgate__search_shots` to find material. Each result carries
   `shot_id`, `asset_id`, `start_ms`, `end_ms`, `score`, and per-constraint
   `evidence` (`confirmed` / `possible` / `contradicted` / `unknown`). Treat
   the score as a retrieval signal and the evidence as the claim; do not assert
   a shot "contains" something unless its evidence says so.
   - Arguments: `query` (required), `limit` (default 20), `offset` (default 0),
     `filters` (a JSON object, or that object encoded as a string).
   - Supported `filters` keys: `asset_types`, `shot_sizes`, `camera_motions`,
     `audio_types`, `qualities`, `usable_as` (each an array of strings), and
     `min_duration_ms` / `max_duration_ms` (integers). An unknown key is refused
     with an error, never silently ignored, because an ignored filter returns a
     wider result set that looks correct.
   - Page with `offset` and read the response's `has_more`, `next_offset`, and
     `window_exhausted` fields; `window_exhausted` means narrow the query, not
     page further.
3. Call `mcp__nexusgate__get_timeline` (arg `asset_id`) to enumerate every shot
   of one asset with exact `start_ms`/`end_ms`, when full-timeline context
   around a match is needed.
4. Call `mcp__nexusgate__get_transcript` (arg `asset_id`) for the word-level
   transcript; its `source` field is `aligned` (word-level) or `asr`
   (sentence-level).
5. Call `mcp__nexusgate__get_asset` (arg `asset_id`) for asset metadata,
   analysis state, derived/proxy status, and transcript availability.
6. Call `mcp__nexusgate__get_shot` (arg `shot_id`) for one shot's full detail
   and speech. Read `transcript_source` (`aligned` / `asr` / absent) before
   quoting its timing; an absent transcript is not evidence of silence.

## Boundaries

- These tools are read-only. Draft-plan creation and revision
  (`POST /api/v1/repurpose/plans...`) and plan approval are not available
  through them, and no plan-write HTTP route is in scope; do not attempt them.
  Approval stays human-only in the NexusGate workspace.
- Never request, read, retain, or print provider keys, raw media paths, or the
  administrator token.
