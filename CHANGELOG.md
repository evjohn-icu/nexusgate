# Changelog

## Unreleased — Disk Load Limits and Worker Onboarding

- Added `timingdex worker run --tray`: a Windows notification-area icon with a
  settings link and a quit item, built on Win32 through stdlib `syscall`. A tray
  icon is one API plus a message pump, and the alternatives each cost the property
  the install wizard depends on — one cross-compiled .exe with no DLLs beside it.
  Qt would need a C++ toolchain and ~50MB of runtime libraries; the CGO tray
  libraries would need a mingw cross-compiler. CI now cross-compiles and vets for
  Windows, since no test on a Linux runner can execute that code.
- Added a loopback settings page on the Worker for the three values the Hub
  cannot push: the hub URL, the pinned fingerprint and the node token. Those are
  the trust anchor — a Worker must already know where to look before it can be
  told anything — so everything else stays Hub-decided. The server binds
  127.0.0.1 only and every route is gated by a random token in the URL, because a
  loopback port is reachable by any process on the machine; a token mismatch
  answers 404 rather than 401, which would confirm the server exists. The node
  token can be replaced but never read back, and submitting it empty keeps the
  existing one so the hub address can be changed without re-pasting a credential
  that is never displayed. Fingerprint validation reuses the client's own
  canonicalisation so the stored form is the compared form.
- Added pipeline throttling, editable at `/settings` and effective on the next
  lease rather than on restart. The pipeline is strictly sequential, so the levers
  are not concurrency: `read_rate` caps FFmpeg's input read speed via `-readrate`
  (an input option, hence placed before `-i`, where it is not silently ignored),
  and a per-job cooldown turns a multi-hour scan from continuous disk load into
  duty-cycled load. Only whole-file reads honour the rate — thumbnail extraction
  decodes one frame, where a cap would only slow the seek. Both default to off so
  an upgrade does not silently slow an existing library.
- `-readrate` support is probed once before use. It arrived in FFmpeg 5.1, and on
  the 4.x builds many NAS and LTS installs still ship, passing it makes FFmpeg
  exit on an unrecognized option before reading a frame — an error matching none
  of `isRetryableJobError`'s permanent phrases, so enabling the throttle would
  have retried every derive through the full backoff chain instead of failing
  usefully. Where the flag is missing the rate limit is skipped with one warning.
- Added off-peak scheduling with two size thresholds: assets above
  `defer_above_bytes` run only inside the window, assets at or below
  `immediate_max_bytes` are exempt from both the deferral and the rate cap. The
  window is judged in the Hub's local time and the settings page shows the Hub's
  own clock, because "01:00" configured from another time zone otherwise means
  something the operator cannot see. Deferral is applied in the lease predicate,
  not after leasing: leasing increments `attempt_count`, so a held job that were
  leased and put back would exhaust its retries long before its window opened.
- The throttle applies to Workers too, decided by the Hub. `remote.WorkerJob`
  carries the source size and rate; a Worker must not be able to opt itself out of
  a limit that protects a disk it shares.
- Added `/worker-setup`, a wizard that generates a ready-to-run install script
  (PowerShell for Windows, POSIX sh for Linux) with hub URL, certificate
  fingerprint, one-time pairing token and library-root mounts filled in. The Hub
  distributes Worker binaries the operator places in
  `$DATA_DIR/worker-binaries/`, so no separate web server is needed on the LAN.
  The script endpoint is admin-only because the script embeds a single-use
  credential, and every request-supplied value is quoted for the target shell.
- Settings now live in a generic `settings(key, value, updated_at)` table.
  Runtime-editable configuration should not need a migration per setting, and
  v0.15 already established SQLite as its home.
- Fixed a stale test: the lease query-plan assertion held its own copy of the SQL
  and kept passing after the size ceiling was added to the real query, asserting
  a plan for a statement nothing executed. The predicate is now shared.

## Unreleased — Retrieval Performance + Agent Credential Boundary

- The unauthenticated read routes (browse, search, thumbnails, proxy, jobs,
  tags, plan inspection, hardware) are now restricted by source network. They
  carry no token so the browser UI works without one on a home LAN, which also
  meant a forwarded port served the entire library to anyone who could reach
  it. The default allowlist is loopback, the RFC1918 ranges, link-local, IPv6
  ULA and the CGNAT range that Tailscale-style overlays assign; override it with
  `hub_security.trusted_read_networks`, where an explicit list replaces the
  defaults rather than extending them and a malformed range fails startup.
  Forwarded headers are deliberately ignored — they are attacker-controlled on a
  directly exposed listener — so a reverse-proxied deployment must filter for
  itself. A valid admin or agent token is admitted from any network: this closes
  anonymous reads without breaking remote use. The HTML pages stay open; they
  hold no library data and are where the token is entered.
- Added a way back for failed jobs: `timingdex pipeline retry-failed`, and a
  "重试失败作业" button on `/progress` backed by
  `POST /api/v1/pipeline/retry-failed` (admin only). Both ways a job stops being
  retried — exhausted attempts and permanent classification — were one-way, and
  `EnqueueJob` is `INSERT OR IGNORE`, so rescanning the library did not revive
  them. Work that failed only because a provider had not been configured yet was
  stranded permanently once the provider was added. Succeeded and running jobs
  are left untouched.
- Permanently failed jobs now report honestly. Marking them terminal by
  exhausting `attempt_count` kept them out of the lease predicate but showed a
  job that ran once as `3/3` on the progress page. A `jobs.terminal` column
  records the fact directly, so the attempt counter is again a count of actual
  runs and the page labels the job as permanently failed. The Worker derive
  lease predicate honours the same flag; it previously would have re-dispatched
  a job the Hub had already given up on.
- Job leasing no longer sorts on every call. `LeaseNextJob` orders by
  `priority DESC, created_at`, an order no `state`-leading index can supply once
  the predicate spans two states, so each lease built a temp b-tree over every
  leasable row — 170ms per lease measured on a 40k-row queue. A partial index
  storing exactly the leasable rows in that order removes the sort; the query
  names it with `INDEXED BY` because SQLite's cost model does not choose it.
- Added a scoped agent credential (`$DATA_DIR/agent-token`, 0600, same atomic
  write and constant-time comparison as the admin token). It is accepted on
  exactly two routes — repurpose plan creation and revision — and refused
  everywhere else. Plan approval and pipeline runs stay admin-only, so the
  `approval_mode: human_required` boundary is now enforced by access control
  rather than by Skill prompt text. Previously the documented agent workflow
  either failed with 401 or required handing the agent the admin token, which
  also unlocked the two actions the capability contract declares denied.
- FTS5 deletes no longer scan the whole shadow table. `asset_search` and
  `asset_shot_search` declare `asset_id UNINDEXED`, so the per-asset delete
  that runs at the end of every analyze was a full index scan, making a
  from-scratch reprocess quadratic in library size. Deletes now resolve the
  FTS rowid through a mapping table first; existing indexes are backfilled by
  the migration.
- Tag search now uses an index. Adding `idx_asset_tag_links_normalized` alone
  was not sufficient — a three-way `OR` spanning `LEFT JOIN`ed tables prevents
  SQLite from pushing any single disjunct down as a seek — so the query is now
  a `UNION` of single-predicate selects, which is set-equivalent and lets every
  branch seek its own index.
- `RebuildSearch` is transactional; its delete and insert could previously be
  interrupted between statements, leaving an asset missing from the search
  index until it was re-analysed.
- Removed two N+1 query patterns: asset browse issued two artifact lookups per
  row (up to 1000 extra queries per page) and shoot-session listing issued one
  lookup per session.
- Deriving an asset now runs `ffprobe` once instead of three times on both the
  Hub and Worker paths, reusing the classification the probe stage already
  stored. Note this also makes derive agree with the metadata the library
  reports: Log footage identified only through capture metadata is now
  consistently reported as needing a LUT instead of being silently rendered as
  ordinary SDR.
- Added CI (build, vet, gofmt, tests, race) and removed dead code:
  `repurposeHTML`, `legacyProvidersHTML`, `legacySetupHTML`, the unregistered
  `listAssets` handler, a redundant `derived_artifacts` index and the unused
  top-level `migrations/` directory. The Tag Curator page now carries the brand
  element its shared page wrapper had been silently failing to substitute.

## Unreleased — Regression Repair

- Restored the browser write surface. v0.14.1 put the Hub admin token in front
  of Tag governance, pipeline runs and Repurpose writes but only taught
  `/workers` to send it, so every write button on `/tags`, `/repurpose` and
  `/progress` had been returning 401 since that release — including plan
  creation, revision and approval, the product's primary workflow. Each page now
  carries a token field and attaches the header at its single fetch choke point;
  the token stays in page memory and is never written to browser storage.
- Opened `GET /api/v1/jobs` to unauthenticated readers so the progress panel
  polls without a token, but `last_error` is now disclosed only to admin
  callers. That field can embed a truncated upstream Provider response body,
  which is where a relay's echoed key would surface; public callers get a
  `has_error` boolean instead.
- Made derived-artifact writes atomic. FFmpeg wrote thumbnails, proxies and
  extracted audio directly to their final paths, so a run killed midway left a
  truncated file that the pipeline's `os.Stat` idempotency check then accepted
  as a finished artifact, silently poisoning every downstream stage until the
  cache was cleared by hand. Output is now published by rename.
- Permanent job failures are now terminal. The pipeline classified errors as
  permanent but still called `CompleteJob`, and the lease predicate accepts
  failed jobs with attempts remaining — so a permanently failed job was re-run
  up to `max_attempts` anyway, immediately and without even the backoff a
  retryable error receives. Provider-channel misconfiguration and unsupported
  capabilities are also classified permanent now instead of burning the full
  retry chain.
- `Weight` and `MaxInflight` now affect Provider-channel selection. Both round
  tripped through SQLite, the admin API and the UI while never reaching the
  pool, so a member configured with a concurrency cap of one still accepted
  unlimited concurrent work.
- Bounded model output before it reaches canonical tables. Analysis summaries,
  tag lists and shot counts had no size limits, so a model stuck in a repetition
  loop could write unbounded text into SQLite and the FTS index. Oversized
  values are truncated on rune boundaries rather than rejected, keeping the
  existing lenient normalisation contract.

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
