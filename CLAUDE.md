# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Re:Footage / `nexusgate` — a local-first video footage intelligence layer written in Go
(stdlib + nine direct dependencies only — `modernc.org/sqlite`,
`github.com/coder/websocket`, `github.com/mark3labs/mcp-go`,
`github.com/grandcat/zeroconf`, `github.com/hirochachacha/go-smb2` and
`golang.org/x/{crypto,net,sys,text}`; no web framework, no ORM, no frontend build.
`go.mod` is the source of truth — check it before repeating this list). The
`nexusgate` binary serves two roles: a **Hub** (database, HTTP/HTTPS API, browser
UI, secrets, local pipeline) and a **Worker** (paired remote node that performs FFmpeg derive
work, and — only when `worker enroll --provider-operation` declares it — calls a Provider for
`video_analysis`/`asr`, either through the Hub JSON proxy or, behind an explicitly enabled
setting, with a lease-bound credential). Product/UI copy is Chinese and so are the
version-scoped documents under `docs/`; code, comments, `README.md` and `CHANGELOG.md` are
English.

## Commands

```bash
go build -o nexusgate ./cmd/nexusgate
go test ./...
go vet ./...

# single package / single test
go test ./internal/app -run TestPipelineStagesAnalysis -v
go test ./internal/repository/sqlite -run TestJobs -v
```

**A green `internal/app` test proves nothing about what can be persisted.** Those tests run
against in-memory fakes (`fakeChannelRepo`, `fakeUpsertOnlyRepo`) that enforce no schema
constraint, and four tests have now been found asserting states real SQLite rejects — one had
two channel members sharing a `SecretRef` that `UNIQUE(secret_ref)` forbids, and was green for
months. If a claim depends on what the database actually does, the test belongs in
`internal/repository/sqlite`, which uses real `sqlite.Open` + `Migrate`.

`internal/media/process_integration_test.go` skips itself unless `ffmpeg`/`ffprobe` are on
PATH. Everything else runs offline: provider adapters are covered by `httptest` fixtures, so
**never add a test that calls a real provider API**.

Run locally with an isolated data dir:

```bash
export NEXUSGATE_DATA_DIR="$PWD/.nexusgate-dev"
./nexusgate doctor
./nexusgate root add /path/to/footage && ./nexusgate root list
./nexusgate root scan <root-id>     # scans, enqueues, and synchronously drains the Pipeline
./nexusgate pipeline run            # leases and runs jobs until the queue is idle
./nexusgate search rebuild          # rebuilds the asset search index
./nexusgate search rebuild-embeddings
./nexusgate cache inspect|gc|verify|repair-derived
./nexusgate serve                   # HTTPS by default; prints the Worker pinning fingerprint
```

Hub state lives entirely under `$NEXUSGATE_DATA_DIR`: `nexusgate.db`, `config.json`
(optional; `config.example.json` is the template), `cache/`, `admin-token` (0600),
`provider-secrets/` (0700). Deleting that directory is the way to reset.

Worker side: `nexusgate worker enroll --hub https://... --fingerprint ... --pairing ...`,
then `nexusgate worker run`; its config defaults to `~/.nexusgate/worker.json`.

## Architecture

`cmd/nexusgate/main.go` → `config.Load()` → `sqliterepo.Open` + `Migrate` →
`app.NewService` (wires providers, staging, hardware, admin token, secret store, pipeline) →
`api.NewTLSServer`. Everything below `app` is dependency-free of HTTP.

`cmd/` holds five commands, not one. `nexusgate` (~1.8k lines) is the product; the other four
are satellites that must not be mistaken for dead directories: `nexusgate-mcp` (~300 lines)
exposes the library to MCP agents over stdio and is a **thin HTTP client of the Hub** — it
holds no database handle and never touches the NAS, so its tools follow the same
agent-token/trusted-read contract as `skills/nexusgate/`; `nexusgate-eval` and
`nexusgate-corpusgen` are the offline retrieval benchmark and its corpus generator;
`nexusgate-playwright-fixture` exists only so CI can compile-check the browser fixture.

### Layer map

- `internal/domain` — pure types only (assets, jobs, shots, plans, capture memory, provider
  channels). No I/O.
- `internal/repository/sqlite` — the only SQL. `repository.go` (~2k lines) plus feature files
  (`asset_browse.go`, `capture_memory.go`, `collections.go`, `remote_jobs.go`).
- `internal/app` — `Service` (use cases, called by both CLI and API) and `Pipeline` (the job
  state machine). Repository interfaces are declared *here*, at the consumer, not in the
  sqlite package: `PipelineRepository` (narrow) and `Repository` (full, embeds it).
  It also holds **hand-written projections** of other packages' types onto the wire —
  `MountGuideStep`, `MountGuideNote`, `ComposeVolumeSuggestion` — so `internal/mount`'s types
  never appear in a JSON response. A field added upstream and not copied into the projection
  is a dead link that compiles, passes the upstream package's tests, and reaches nothing:
  `mount.Step.Kind`/`File` were added to stop the browser rendering an `/etc/fstab` entry as a
  runnable command, and stopped at this boundary with every Go-side test green.
  `TestMountGuideStepProjectionCarriesKindAndFile` now counts upstream against projected.
- `internal/api` — `net/http` `ServeMux` with Go 1.22 method+pattern routes; all routes are
  registered in `Handler()` in `server.go`.
- `internal/providers/*` — one package per upstream adapter; `factory.go` maps config names
  to implementations. `internal/providers/video` holds the multi-provider router.
- `internal/worker`, `internal/remote` — Worker runtime and the Hub↔Worker wire types.

### Processing pipeline

Jobs form a chain, not a scheduler graph: each `Pipeline.execute` case enqueues its successor
with a hash derived from the previous `InputHash`, so the whole chain is idempotent and
resumable. The `analyze` stage enqueues `JobIndex` as its successor.

```
probe → derive → speech_gate → transcribe → [align] → analyze → index
                     └──(no audio)──────────────────────┘
```

Two rules live in `internal/app/pipeline.go` and are easy to break:

- Canonical writes are lease-bound at the write boundary: repository writes carry both the
  leased `jobID` and its `owner`, and compare-and-swap the active lease before committing.

- `isRetryableJobError` decides retry vs. permanent failure, and **nothing about that
  decision reads message text**. Provider HTTP failures are classified by status: `errors.As`
  on `*common.StatusError`, where 4xx is permanent **except** 408/429, which clear on their
  own, and 401/402/403, which describe the key rather than the request —
  `providerpool.ClassifyFailure` returns `MemberSpent` for those, the pool retires that
  member, and the next one is tried. Everything else is permanent only if it says so:
  `domain.Permanent(err)` marks it, `errors.Is(err, domain.ErrPermanentFailure)` reads it,
  and **the default is retry**, so an unforeseen failure gets attempts rather than a verdict.
  `Permanent` does not touch the message — that text reaches `jobs.last_error_message` and
  the `/progress` page — and it keeps the wrapped chain reachable, so `errors.As` still finds
  a `*common.StatusError` through it.

  The bar for marking is structural, not a preference: **the next attempt would present
  identical inputs to a deterministic decision.** Model output that failed validation
  qualifies; a timeout does not. Where a family of failures shares that argument, the mark
  belongs on a wrapper around the whole family — `analysisShotsProblem`, `normalizeAnalysis`,
  `assetShotsProblem`, the provider builders — so that a new check added inside one inherits
  the verdict without anyone remembering. That placement is the point: it was the *missing*
  case in a hand-maintained list that let two of `validateAnalysisShots`'s four refusals be
  retried, each retry another paid call for the same answer.
- **Errors carry meaning as sentinels, not as prose.** Classification is `errors.Is` against
  a sentinel — `internal/domain/errors.go` for anything two layers share, the owning package
  otherwise (`nleexport.ErrInvalidTimeline`, `providerchannels.ErrRouteExhausted`). One
  condition gets one identity: `internal/app` re-exports `domain`'s as aliases rather than
  declaring its own, so the two cannot disagree. Outside the phrase list above, no production
  code decides behaviour by reading `err.Error()`; do not reintroduce it. When wrapping, keep
  the cause in the chain — `fmt.Errorf` accepts more than one `%w`, and flattening one to
  `%v` has silently broken classification here before.
- `common.ReadError` truncates the upstream body it embeds in the error. That text reaches
  `jobs.last_error_message` in SQLite and the `/progress` page, so an unbounded copy of a
  relay's echoed request would persist a Provider key. Keep the bound if you touch it.
- Derived-artifact cache paths embed the hardware mode/profile
  (`thumbnail-<mode>.jpg`, `proxy-<mode>.mp4`, profile hash `proxy-720-<profile>`) so a
  software x264 proxy is never mislabelled as an NVENC/QSV/VideoToolbox result. Keep that
  property when touching `media.HardwarePlan` or artifact naming.

### Search (v0.27+)

`internal/search` is the layered retrieval engine behind every search endpoint: query
compiler (offline controlled vocabulary, positional negation) → intent router
(auto/fact/speech/semantic/similar/creative, no LLM) → retrieval channels → fusion
(RRF k=60; WeightedBlend is the compat mode) → evidence gate → diversity selection.
**Recall may be fuzzy; claims may not**: a shot may rank for any signal, but only the
gate says a shot *contains* something (`confirmed`/`possible`/`contradicted`/`unknown`
— unknown is never "确认无人"). The text-embedding channel
(`shot_text_embeddings`, per-shot float32 cosine scan, model-tagged, cluster threshold
`embeddingCutoffFraction`) is a retrieval signal, never evidence; the pipeline embeds
changed shots after each analysis commit, and `nexusgate search rebuild-embeddings`
rebuilds all rows — the model-switch entry, and it never re-runs VLM analysis. The
search profile is `v2-profile-2`. Speech phrases are validated as a complete ordered phrase
within one shot: ASCII components use whole-word matching, CJK segmentation is tolerated,
spans cannot be reused, adjacent components may be at most 1500ms apart, and matches never
cross shot boundaries. The
legacy GET hybrid endpoint is served by the same engine via
`search.Service.LegacySearch`, pinned by `TestSearchV2CompatMatchesLegacy`. The
regression floor is `TestRetrievalGolden` + `TestSearchV2Benchmark` (72 queries, six
pipelines, per-intent RetrievalFP/AssertionFP) in `internal/repository/sqlite`; a
ranking change that moves these numbers needs a deliberate reason, not an accident.
`internal/eval` (with `cmd/nexusgate-eval`) is a *separate* offline harness that scores against
the legacy `HybridSearchShots` path rather than this engine — useful, but not the source of the
RetrievalFP/AssertionFP figures above; don't cite one for the other.

### Model safety boundary (non-negotiable)

Model output never writes directly to canonical tables:

```
CreateModelRun (immutable cache, dedup by input hash)
  → provider call → FailModelRun on provider/validation error
  → normalize.ValidateAndNormalize + validateAnalysisShots
  → StageModelRun (raw + parsed)
  → CommitAnalysisWithShots (single transaction, lease-bound by jobID/owner)
  → RebuildSearch (FTS5)
```

Failed, malformed or out-of-range responses stay in `model_runs`. The same shape governs the
Tag Curator (`AI raw tags → normalize → unresolved pool → staged proposals → human approve →
canonical catalog`) and Repurpose plans (immutable revisions; approval is human-only and
 locks the plan). Agents may draft; humans approve.

### Cost Guides

`PipelineThrottle.DailyCostGuide` and `MonthlyCostGuide` are advisory operator
references only. They never gate or defer `JobTranscribe` or `JobAnalyze`.
`cost_ledger` is append-only and records post-commit estimates in the channel's
relative unit; it is never a billing record. The API uses
`daily_cost_guide`/`monthly_cost_guide` and reports `today_estimate`/
`month_estimate`. Legacy `daily_budget`/`monthly_budget` request fields are
accepted for one version and normalized to the guide fields, but new responses
never emit the old names. The persisted `budget_exhausted` category remains
readable for historical jobs and is no longer produced.

### Provider layer

Two coexisting configuration paths, unified behind the ordinary `providers.*` interfaces:

1. **Legacy config** — `providers.*` blocks in `config.json`/env (`base_url`, `path`,
   `protocol`, `auth_header`, `auth_scheme`, `extra_headers`, `model`), designed so relay /
   reverse-proxy endpoints work without code changes.
2. **Provider channels (v0.15+)** — capability-scoped routes persisted in SQLite and managed
   at `/providers`. `app/provider_channel_runtime.go` resolves a capability to a member,
   pulls the key from `secretstore`, and returns something implementing the *same* provider
   interface, so `Pipeline` is unaware of channels. `internal/providerpool` owns health,
   cooldown, inflight limits and ordered fallback; `internal/providerchannels` owns the
   executor/metadata. Channels win; legacy config is the fallback.

Adding a provider means: adapter package + `providers/factory.go` entry + capability
metadata in `internal/providerchannels` + a `httptest` fixture test.

### Security boundaries

These invariants are the point of several packages — preserve them when editing:

- **Provider keys** live in `secretstore` (encrypted, Hub-only, `provider-secrets/`
  0700, files 0600) when configured through provider channels. **Legacy `providers.*`
  blocks in `config.json` hold keys in plaintext** on disk and should be migrated to
  provider channels. Keys must never appear in SQLite, API responses, browser storage,
  Worker config, logs, error strings or `String()`/`MarshalJSON` output.
  The data-encryption key is its own
  `provider-secrets/store.key` file, deliberately *not* derived from the admin token —
  deriving it made token rotation brick every CLI command. `Open` still takes the admin
  token solely to migrate a pre-`store.key` store on first run (backing the original up to
  `.pre-key-migration`); don't reintroduce it as key material.
  `Store.Rekey()` rotates the data-encryption key in-place: new key, re-encrypt all
  secrets with fresh nonces, atomically persist both files, backup old key to
  `store.key.pre-rekey`. It holds the write lock for its entire duration and rolls
  back the ciphertext on key-write failure.
- **Hub admin token** (`hubauth`) is generated at first start and compared with
  `crypto/subtle`. Whether it is demanded is controlled by
  `hub_security.admin_auth`: `required` always demands it; `trusted_network`
  (the default) waives it for peers inside `admin_auth_networks` (LAN peers
  skip the password, internet peers still need it); `off` never demands it. The
  waiver is decided from `RemoteAddr` alone, exactly like the trusted-read
  guard, and inherits that guard's blind spot: behind a reverse proxy or a
  published Docker port every peer looks RFC1918, so a containerised Hub with
  `trusted_network` and no explicit `admin_auth_networks` refuses to start
  (see `ValidateContainerAdminAuth` in `cmd/nexusgate`). New write endpoints
  default to wrapped, not open. It is not the only mutation guard: agent-facing
  plan writes use `requireAgentOrAdmin`, and the nine `/api/v1/worker/*` routes
  authenticate in-handler through `s.authenticatedWorker(...)` against the node
  token (`enroll` is the exception — a Worker has no token yet, so it presents
  the one-time pairing token). Three guards, one table:
  `TestAPIRouteInventoryGuardMatrix` is only worth its name while every route in
  `Handler()` also has a row there, so add both or neither.
- **Worker trust**: one-time pairing token, revocable node token, TLS certificate
  fingerprint pinning. A Worker gets a *lease-bound, memory-only* provider credential only
  when `hub_security.allow_worker_provider_credentials` is explicitly enabled (default
  deny); otherwise it uses the Hub JSON proxy, which rejects media/multipart and bodies over
  2 MiB. Audit records (`LeaseAudit`) must stay key-free. **Both paths read `providers.*`
  config only and deliberately never resolve a provider channel** — pushing channel-scoped
  keys, member pools and health state out to a remote node is the opposite of why the
  credential path is default-deny. A Hub configured entirely through `/providers` therefore
  cannot serve a Worker, and says so: `ErrWorkerProviderConfiguredAsChannelOnly` (403) is a
  distinct answer from `ErrWorkerProviderNotConfigured` (503), because telling an operator
  their provider is "not configured" when they configured it in the browser blames them for
  something they did not do.
  `provider_operations` is enrollment-time trust; heartbeats cannot grant provider access.
- **Capture coordinates**: source precision is admin-only
  (`GET /api/v1/admin/assets/{id}/capture-location`). The `RegionLabel` surfaced elsewhere is
  free text set by whoever wrote the row — nothing in the tree reverse-geocodes latitude and
  longitude into it. It is therefore *not* a coarsened view of the coordinates, and calling it
  one would promise a privacy transform that does not exist.
- Original media is read-only. Nothing is ever written next to source files; NAS mode
  (`source_staging.mode: copy`) copies into `cache/sources/` first.

### Database migrations

`internal/repository/sqlite/migrations/NNNN_*.sql`, `go:embed`-ed and applied in filename
order inside a transaction, tracked in `schema_migrations`. `Migrate` does more than apply
files, and the extra steps are the reason a half-applied schema has not happened: it refuses to
start on an `integrity_check` failure or without free disk for `2×dbsize + 256MiB`, snapshots
the database with `VACUUM INTO`, applies every pending file inside one `BEGIN IMMEDIATE` with
`foreign_keys` off (SQLite ignores the pragma mid-transaction), and runs
`pragma_foreign_key_check` *after* the commit. The current migration ceiling is
`0035_pipeline_executors.sql`, and the file count is 35 — the numbering does not track it in
either direction: `0025` was abandoned before it landed and is skipped, while `0034` is used
**twice**. Ordering is by full filename, and `schema_migrations` is keyed by full filename
(`repository.go:306`/`:325`), so the duplicate prefix applies and records cleanly —
`0034_asr_segments.sql` sorts before `0034_asset_shot_ordinal_index.sql` and both run. It is a
naming-discipline problem, not a correctness one; do not "fix" it by renaming an applied file.
The newest migrations are:

- `0032_collection_shot_positions.sql`
- `0033_asset_probe_identity.sql`
- `0034_asr_segments.sql`
- `0034_asset_shot_ordinal_index.sql`
- `0035_pipeline_executors.sql`

Add a new numbered file; never edit an applied one. Take the next free number by reading the
directory, not by incrementing what this paragraph says.

### Browser UI

Pages are Go string constants of inline HTML/CSS/JS — no templates, no assets, no build step.
`internal/api/server.go` holds the base constants (`legacyLibraryIndexHTML`, `progressHTML`,
`repurposeHTML`, …).

**Check which shape a page is before editing it; guessing has been wrong repeatedly.** Three
`strings.Replace` layers stack, and a stale anchor in any of them is a *silent no-op* — the page
builds, serves, and the feature is simply gone, with no compile or runtime error:

- `shelledPage` (`app_shell.go`) — three anchors, every page, injects the shared nav/shell.
- `enhanceLibraryPage` (`library_page_v015.go`) — **19 ordered anchors**
  (`libraryPagePatches`, `library_page_v015.go:121-237`), library page only, applied once at
  package init to build `libraryIndexHTML` from `legacyLibraryIndexHTML`. Count the slice
  rather than trusting this number; adding a patch and not updating it here is how it drifted.
- `brandedPage` (`branding.go`) — two anchors, every page, applied **per request**.

Only the library page carries an overlay; `providers_page_v015.go` and `setup_page_v015.go` are
self-contained constants whose own text is never patched. Editing the legacy HTML underneath an
anchor is what breaks it.

Two guards exist because greps and review cannot see these failures:
`TestLibraryPagePatchesApplyInOrderAndBite` asserts every library-page anchor still matches
*exactly once* and replays the whole chain bit-for-bit. `TestBrandedPageAnchorsBite` is weaker
by construction — it only requires each branding anchor to appear somewhere across the eleven
pages, so a branding anchor that goes stale on one page alone still passes. `page_scripts_test.go` runs
`node --check` over the `<script>` block of every page route (skipped when node is absent).
That last one exists because one misplaced character left `/worker-setup`'s script dead from
v0.18 until v0.21 while the page kept serving.

**UI copy lives in two places, and only one of them ships.** `i18n.go` embeds
`//go:embed locales/*.json`, which does **not** match `locales/fragments/` — the runtime
catalog is the top-level `locales/<lang>.json`, and `locales/fragments/<page>.json` is a
separate, hand-synced authoring copy — one file per *page*, each holding all
five languages, rather than one file per language. Two different tests watch the two files, so editing one
of them looks half-green: `TestLibraryRootsPageTranslatesEveryGuidanceKey` reads the fragment
and passes, while `TestEveryPageMarkerAndRuntimeKeyResolves` reads the catalog and fails. A new
key belongs in both, in all five languages, or the page renders the raw key name at runtime.

## Conventions

- Version-scoped documents: each release adds `docs/vX.Y-*.md` (goal / operations) and a
  `CHANGELOG.md` entry describing the boundary as well as the feature. Follow that when
  shipping a version-level change.
- Comments explain *why a boundary exists* rather than what the code does; keep that tone.
- Long single-line struct literals and dense config defaults are the existing style — match
  the surrounding file rather than reformatting.
- `skills/nexusgate/` is a versioned agent-facing contract. It relies on
  `GET /api/v1/agent/capabilities` declaring `approval_mode: human_required` and an
  `allowed_actions` allowlist that excludes plan approval, pipeline runs, provider keys and
  raw media paths. Keep the endpoint and `skills/nexusgate/references/api-contract.md` in
  sync when API surface changes.
- Capability claims are deliberately conservative in both code and docs (e.g. proprietary
  RAW is identified but reported as unrendered rather than silently treated as SDR; the
  "heuristic feature vector" is not called a learned embedding). Don't upgrade the wording
  past what the code does.
