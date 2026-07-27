# Changelog

## v0.16.0 — Worker Provider Modes + Library Operations

- Added an explicit, default-deny Worker-direct Provider credential path for
  trusted paired nodes. Credentials are lease-owned and memory-only; audit
  records contain no Provider key, and the documented delivery TTL is not
  misrepresented as key revocation.
- Added a Hub-side, JSON-only Worker Provider proxy. It verifies lease
  ownership, rejects media/multipart payloads and bounded responses, and
  redacts Provider keys before returning a response.
- Added Provider-channel update, test, enable, disable and safe removal APIs;
  browser responses never include API keys or secret references.
- Added Worker stage/progress events, retry/failure visibility and manual
  preferred/required derive-node routing, surfaced on `/workers`.
- Added saved, non-secret asset Collections and processing-state summary/filter
  support to the library-first browse experience.

## v0.15.0 — Capture Memory + Provider Channel Management

- Added a vendor-neutral capture-memory model with field provenance/conflict
  preservation, safe automatic shoot-session aggregation and region-level
  browsing. Camera/colour recognition covers Blackmagic, DJI, Sony, Canon,
  Apple, GoPro, Insta360, Panasonic, Nikon, Fujifilm, RED and ARRI families.
- Added RAW/HDR/Log routing metadata. Unsupported proprietary RAW remains
  visibly unrendered rather than being silently treated as ordinary SDR media.
- Added protected `/providers` management for capability-bound Provider
  channels, multiple same-Provider keys, health-aware balancing, retry,
  cooldown and ordered fallback. Provider secrets are encrypted Hub-only
  state, outside SQLite/browser/Worker configuration.
- Kept the Worker security boundary closed: Worker credential endpoints never
  deliver an upstream Provider API key. A credential TTL cannot revoke a
  long-lived key after it has been disclosed, so remote direct-provider
  execution requires a future Hub proxy or genuinely revocable upstream token.
- Tightened Hub secret storage so shared NAS data directories remain usable;
  only the dedicated `provider-secrets/` child is required to be mode `0700`.

## v0.14.2 — CJK Bigram Retrieval

- Replaced the CJK `instr(...)` full-table fallback with shared overlapping
  Bigram tokenization over existing FTS5 `unicode61` tables, including
  phrase-preserving Chinese and Chinese/English mixed MATCH queries.
- Added an idempotent migration/backfill state so existing libraries rebuild
  both asset and shot FTS indexes before the Hub reports the upgrade ready.
- Kept CJK FTS tokens out of the legacy heuristic vector's free-form hash
  space, preserving similar-shot ranking while retaining multilingual aliases.

## v0.14.1 — Hub Security Baseline

- Added a generated mode-0600 Hub administrator token and constant-time
  protection for Worker pairing, Hub management, scans, pipeline execution,
  tag governance, library-summary generation and Repurpose writes.
- Closed Compose's default LAN port exposure; Hub is internal to the Compose
  network until an operator adds an explicit HTTPS reverse proxy.
- Disabled Worker Provider-key delivery by default. When explicitly enabled,
  documentation now calls it a short-delivery lease: its TTL cannot revoke an
  upstream long-lived API key already received by a trusted Worker.

## v0.14.0 — NAS Hub + Worker Execution

- Added a Hub/Worker trust boundary: one-time Worker pairing, revocable node
  tokens, Worker heartbeats, persisted capabilities and a Hub-only SQLite DB.
- Added cross-platform Worker enrollment/config commands for Windows and Linux
  x64/ARM64, plus Docker Compose and generic systemd delivery assets.
- Added Hub self-signed HTTPS identity with certificate fingerprint pinning for
  Workers; Provider keys remain Hub-managed and never enter Worker config.
- Added NAS-direct probe behavior: `probe` reads source metadata in place while
  later media work may use the existing disposable local staging cache.
- Added source-color classification and metadata for SDR, HLG, PQ, Apple Log,
  unknown Log and RAW; RAW previews remain explicitly unavailable until a
  compatible renderer exists.
- Added Worker-local source staging and capability-matched derive execution;
  thumbnail, proxy and audio artifacts upload to Hub-owned `derived/` storage
  with lease ownership and idempotency checks.
- Added opt-in Worker Provider credential delivery. The Hub audits only Worker,
  job, provider, operation and expiry; it does not persist Provider API keys.
- Added a processing-node/workflow page at `/workers`, HDR→SDR Rec.709 preview
  rendering, Apple Log LUT-required handling, and explicit RAW refusal.

## v0.13.2 — Network Source Staging

- Added optional `source_staging.mode: copy` for mounted NAS/network libraries.
  Timingdex copies a source to a local, versioned cache before processing and
  reuses that completed cache on later jobs.
- Kept NAS originals read-only: FFmpeg, ASR, proxy generation and model work
  execute on the local machine; no sidecars or derived files are written back
  to the mounted share.
- Added a NAS/local-source choice to the startup-command page via
  `TIMINGDEX_SOURCE_STAGING_MODE=copy`.

## v0.13.0 — Library-First Footage Browser

- Reframed `/` as the primary Timingdex experience: an editor now sees each
  source as a left thumbnail, central semantic summary, and right-side
  horizontal shot timeline.
- Timeline blocks are populated from persisted `asset_shots` time ranges and
  descriptions; they expose usable footage rather than an invented visual
  summary.
- Kept `/repurpose` as a distinct downstream planning workspace so browsing
  material precedes asking the system to assemble a proposal.

## v0.12.0 — Bounded Agent Skill

- Added a project-bundled `timingdex` Agent Skill for local API-only readiness
  checks, shot retrieval, draft-plan creation and user-directed draft
  revisions.
- Added `GET /api/v1/agent/capabilities`, declaring the allowed Agent actions
  and the human-only approval boundary. Provider keys, original-media paths and
  pipeline execution are explicitly denied.
- Documented the full revision snapshot and lock/unlock contract for agents;
  the Skill never calls the approval endpoint.

## v0.11.0 — Repurpose Selection Workspace

- Turned the local Repurpose result into an editable editorial workspace:
  choose a candidate, lock/unlock the choice, exclude unsuitable alternatives,
  find additional hybrid-search candidates, add a revision note, and save a
  new immutable draft before approval.
- Added selection invariants: a selected or excluded shot must be a section
  candidate; a selected shot cannot be excluded; a locked section cannot be
  changed without an explicit, non-persistent `unlock` action.
- Approval now requires an explicit selection for every required section that
  already has candidate material, while genuine missing-material gaps stay
  visible.

## v0.10.3 — Local Review Workspace

- Added a small local-first workspace: startup configuration guidance, pipeline
  progress with recent job state/errors, and a Repurpose Plan result/review
  page that calls the existing plan/revision/approval APIs.
- Kept provider keys out of browser/server persistence: setup generates a
  copyable terminal command only; configuration remains process-startup based.
- Normalized an empty jobs response to `[]` so a fresh library renders a useful
  zero-state rather than a client error, and suppressed harmless favicon noise
  for the local pages.

## v0.10.2 — Reliability Patch

- Added bounded retry/backoff for transient pipeline failures; configuration,
  validation and missing-input failures stay final and visible.
- Preserved required Repurpose sections when their only valid shot was already
  selected elsewhere, with an explicit `reused` marker on that candidate.
- Added an invalidating in-memory cache for decoded local feature vectors,
  reducing repeated JSON decode work without changing hybrid/similar/rare
  retrieval semantics or claiming a learned embedding.

## v0.10.1 — Correctness Patch

- Removed silent mock video analysis: provider initialization errors now fail
  service startup, and an intentionally unconfigured video provider leaves
  analysis jobs with a visible configuration error.
- Corrected provider routing: only providers that actually prepare remote
  media are prepared; fallbacks receive the portable local proxy rather than a
  foreign provider URI.
- Made validated analysis, AI tag links, shots, FTS rows and local feature
  vectors one SQLite transaction, preserving previously trusted data when a
  new response is invalid.
- Replaced silence-event counting with duration-ratio classification and added
  regression fixtures for long, short, and open-ended silent regions.
- Added Chinese literal-match retrieval for short queries (for example `雨夜`)
  while retaining FTS5 for non-CJK requests. Hybrid's local feature score is
  documented as heuristic rather than a learned embedding.

## v0.10.0 — Pre-v1 Intelligence Closure

- Completed multi-model video understanding routes: Gemini plus configurable
  Qwen, Volcengine Ark and local OpenAI-compatible VLM video endpoints, all
  constrained by the shared `VideoAnalysisResult` contract and Router fallback.
- Added SQLite-first hybrid shot search using explainable local semantic
  features generated from persisted visual analysis, alongside similar-shot and
  library-relative rare-shot discovery APIs.
- Updated Repurpose Plans with append-only revisions, editor notes, explicit
  candidate ordering, and latest-draft approval. Approved plans are immutable.
- Added v0.10 SQLite migrations, API behavior tests, provider contract
  fixtures, and a pre-v1 goal/verification document.

## v0.9.0 — Repurpose Plan MVP

- Added a Brief → Material Needs → Shot Candidates → Repurpose Plan workflow.
- Added optional OpenAI-compatible Repurpose Planner support for a small local
  LM; planner output is constrained to searchable needs and never invents
  asset IDs or timestamps.
- Added deterministic heuristic fallback so plan generation remains available
  without an LLM or when the configured planner fails.
- Added SQLite persistence and APIs for creating and retrieving plans:
  `POST /api/v1/repurpose/plans` and `GET /api/v1/repurpose/plans/{id}`.
- Plans reference exact shot ranges and explain missing required material. This
  release does not auto-edit media or export an NLE timeline.

## v0.8.0 — Shot-level Clip Search substrate

- Persisted provider-generated time ranges as `asset_shots` with descriptions,
  tags, mood, confidence and model-run provenance.
- Added SQLite FTS5 search over shot descriptions and semantic fields.
- Added `GET /api/v1/assets/{id}/shots` and
  `GET /api/v1/search/shots?q=...`.
- Kept original media immutable; search results reference exact source time
  ranges for the upcoming Repurpose Plan workflow.

## v0.7.0 — Multi Video Understanding Provider Layer

- Added a provider-independent `VideoAnalysisResult` contract for summaries,
  scenes, shots, objects, actions, mood, raw tags and confidence.
- Added `VideoUnderstandingProvider`, capability declarations, registry and
  primary/fallback router.
- Adapted Gemini through the unified contract while preserving the old flat
  `StructuredAnalysis` response format during migration.
- Routed the hardware-aware v0.6.1 pipeline through the new provider layer;
  existing Volcengine ASR, Plan providers, embedding, Tag Intelligence and
  SQLite migrations remain intact.
- Added provider-layer tests and configuration support for `vision_fallback`.

## v0.6.1 — Volcengine Plan providers

- Added first-class, quota-isolated configuration for Volcengine **Agent Plan**
  (`/api/plan/v3`) and **Coding Plan** (`/api/coding/v3`) LLM providers.
  Either plan can power the bounded Tag Curator and library-summary Agent.
- Added separate Agent Plan / Coding Plan embedding provider blocks, so the
  semantic tag-cluster pipeline can use a plan's embedding entitlement without
  sharing or changing the corresponding chat configuration.
- Added `volcengine_asr`: native Doubao Seed ASR 2.0 offline WebSocket support
  for imported footage. It emits 16 kHz PCM in compressed binary frames,
  preserves the plan resource ID, and remains compatible with the existing ASR
  fallback chain.
- Added provider/protocol fixtures for Volcengine ASR framing, compressed server
  responses, and error envelopes. No credentials or live paid calls are used by
  tests.
- Documented the concrete provider names, API-key boundaries and safe selection
  recipe. Plan model names stay user-configurable because availability changes.

## v0.6 — Hardware-accelerated derived media

- Added FFmpeg capability detection and an inspectable `GET /api/v1/hardware`
  endpoint plus `timingdex doctor` output.
- Added `auto`, `software`, `cuda` (NVDEC/NVENC), `qsv`, `vaapi`, and
  `videotoolbox` media profiles for thumbnail/proxy work.
- Hardware is used only for decoding frames and encoding the local 720p H.264
  proxy. Originals remain read-only; ASR, Gemini, embeddings and Tag Curator
  are deliberately unchanged.
- Added safe per-operation software fallback and separate cache/profile keys so
  a hardware setting change cannot reuse or relabel an incompatible artifact.

## v0.5.2 — Tag Intelligence

- Added a provider-backed embedding pipeline for normalized unresolved tags.
  The pipeline supports OpenAI-compatible embeddings and Gemini native
  `embedContent`; vectors are stored in SQLite by tag and model.
- Added transparent cosine-similarity candidate clusters. Clusters are a
  discovery aid only: each candidate goes through Tag Curator and becomes a
  normal pending human-review proposal.
- Formalized the Tag Curator as a constrained semantic provider for both tag
  grouping and library summaries. It supports OpenAI-compatible chat endpoints
  (including Qwen/Ollama-style gateways) and Gemini native `generateContent`.
- Added library-level natural-language summaries generated from a bounded
  statistics snapshot: asset count, analysis coverage and approved canonical
  tag frequency. Provider failure safely falls back to a labelled deterministic
  summary when heuristic fallback is enabled.
- Added SQLite migrations, API routes and provider/repository fixture tests.

### Deliberately unchanged

- No vector search over footage.
- No automatic tag merge or database mutation from model output.
- No hierarchy editor or bulk-review UI; both remain v0.6 governance work.
