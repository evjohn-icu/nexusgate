# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Re:Footage / `timingdex` — a local-first video footage intelligence layer written in Go
(stdlib + `modernc.org/sqlite` + `nhooyr.io/websocket` only; no web framework, no ORM, no
frontend build). One binary serves two roles: a **Hub** (database, HTTP/HTTPS API, browser
UI, secrets, local pipeline) and a **Worker** (paired remote node that performs FFmpeg derive
work, and — only when `worker enroll --provider-operation` declares it — calls a Provider for
`video_analysis`/`asr`, either through the Hub JSON proxy or, behind an explicitly enabled
setting, with a lease-bound credential). Product/UI copy is Chinese and so are the
version-scoped documents under `docs/`; code, comments, `README.md` and `CHANGELOG.md` are
English.

## Commands

```bash
go build -o timingdex ./cmd/timingdex
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
export TIMINGDEX_DATA_DIR="$PWD/.timingdex-dev"
./timingdex doctor
./timingdex root add /path/to/footage && ./timingdex root list
./timingdex root scan <root-id>     # enqueues idempotent jobs
./timingdex pipeline run            # leases and runs jobs until the queue is idle
./timingdex serve                   # HTTPS by default; prints the Worker pinning fingerprint
```

Hub state lives entirely under `$TIMINGDEX_DATA_DIR`: `timingdex.db`, `config.json`
(optional; `config.example.json` is the template), `cache/`, `admin-token` (0600),
`provider-secrets/` (0700). Deleting that directory is the way to reset.

Worker side: `timingdex worker enroll --hub https://... --fingerprint ... --pairing ...`,
then `timingdex worker run`; its config defaults to `~/.timingdex/worker.json`.

## Architecture

`cmd/timingdex/main.go` → `config.Load()` → `sqliterepo.Open` + `Migrate` →
`app.NewService` (wires providers, staging, hardware, admin token, secret store, pipeline) →
`api.NewTLSServer`. Everything below `app` is dependency-free of HTTP.

### Layer map

- `internal/domain` — pure types only (assets, jobs, shots, plans, capture memory, provider
  channels). No I/O.
- `internal/repository/sqlite` — the only SQL. `repository.go` (~2k lines) plus feature files
  (`asset_browse.go`, `capture_memory.go`, `collections.go`, `remote_jobs.go`).
- `internal/app` — `Service` (use cases, called by both CLI and API) and `Pipeline` (the job
  state machine). Repository interfaces are declared *here*, at the consumer, not in the
  sqlite package: `PipelineRepository` (narrow) and `Repository` (full, embeds it).
- `internal/api` — `net/http` `ServeMux` with Go 1.22 method+pattern routes; all routes are
  registered in `Handler()` in `server.go`.
- `internal/providers/*` — one package per upstream adapter; `factory.go` maps config names
  to implementations. `internal/providers/video` holds the multi-provider router.
- `internal/worker`, `internal/remote` — Worker runtime and the Hub↔Worker wire types.

### Processing pipeline

Jobs form a chain, not a scheduler graph: each `Pipeline.execute` case enqueues its successor
with a hash derived from the previous `InputHash`, so the whole chain is idempotent and
resumable.

```
probe → derive → speech_gate → transcribe → [align] → analyze → index
                     └──(no audio)──────────────────────┘
```

Two rules live in `internal/app/pipeline.go` and are easy to break:

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

### Model safety boundary (non-negotiable)

Model output never writes directly to canonical tables:

```
CreateModelRun (immutable cache, dedup by input hash)
  → provider call → FailModelRun on provider/validation error
  → normalize.ValidateAndNormalize + validateAnalysisShots
  → StageModelRun (raw + parsed)
  → CommitAnalysisWithShots (single transaction)
  → RebuildSearch (FTS5)
```

Failed, malformed or out-of-range responses stay in `model_runs`. The same shape governs the
Tag Curator (`AI raw tags → normalize → unresolved pool → staged proposals → human approve →
canonical catalog`) and Repurpose plans (immutable revisions; approval is human-only and
locks the plan). Agents may draft; humans approve.

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
- **Hub admin token** (`hubauth`) is generated at first start, compared with
  `crypto/subtle`, and enforced by `s.requireHubAdmin(...)` on every mutating/administrative
  route. New write endpoints default to wrapped, not open.
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
- **Capture coordinates** are exposed as a region label; source precision is admin-only
  (`GET /api/v1/admin/assets/{id}/capture-location`).
- Original media is read-only. Nothing is ever written next to source files; NAS mode
  (`source_staging.mode: copy`) copies into `cache/sources/` first.

### Database migrations

`internal/repository/sqlite/migrations/NNNN_*.sql`, `go:embed`-ed and applied in filename
order inside a transaction, tracked in `schema_migrations`. Add a new numbered file; never
edit an applied one.

### Browser UI

Pages are Go string constants of inline HTML/CSS/JS — no templates, no assets, no build step.
`internal/api/server.go` holds the base constants (`legacyLibraryIndexHTML`, `progressHTML`,
`repurposeHTML`, …).

**Check which shape a page is before editing it; guessing has been wrong repeatedly.** Only
`library_page_v015.go` overlays a base constant (`enhanceLibraryPage(legacyLibraryIndexHTML)`),
patching it by **exact-match `strings.Replace`**; a stale anchor is a *silent no-op* — the page
builds, serves, and the feature is simply gone, with no compile or runtime error. Editing the
legacy HTML is what breaks it. `providers_page_v015.go` and `setup_page_v015.go` are
self-contained constants with no `strings.Replace` at all. `branding.go` applies its own
replacements to every page.

Two guards exist because greps and review cannot see these failures:
`TestLibraryPagePatchesApplyInOrderAndBite` asserts every library-page anchor still matches,
`TestBrandedPageAnchorsBite` does the same for branding, and `page_scripts_test.go` runs
`node --check` over the `<script>` block of every page route (skipped when node is absent).
That last one exists because one misplaced character left `/worker-setup`'s script dead from
v0.18 until v0.21 while the page kept serving.

## Conventions

- Version-scoped documents: each release adds `docs/vX.Y-*.md` (goal / operations) and a
  `CHANGELOG.md` entry describing the boundary as well as the feature. Follow that when
  shipping a version-level change.
- Comments explain *why a boundary exists* rather than what the code does; keep that tone.
- Long single-line struct literals and dense config defaults are the existing style — match
  the surrounding file rather than reformatting.
- `skills/timingdex/` is a versioned agent-facing contract. It relies on
  `GET /api/v1/agent/capabilities` declaring `approval_mode: human_required` and an
  `allowed_actions` allowlist that excludes plan approval, pipeline runs, provider keys and
  raw media paths. Keep the endpoint and `skills/timingdex/references/api-contract.md` in
  sync when API surface changes.
- Capability claims are deliberately conservative in both code and docs (e.g. proprietary
  RAW is identified but reported as unrendered rather than silently treated as SDR; the
  "heuristic feature vector" is not called a learned embedding). Don't upgrade the wording
  past what the code does.
