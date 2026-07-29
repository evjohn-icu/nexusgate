# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Re:Footage / `timingdex` — a local-first video footage intelligence layer written in Go
(stdlib + `modernc.org/sqlite` + `nhooyr.io/websocket` only; no web framework, no ORM, no
frontend build). One binary serves two roles: a **Hub** (database, HTTP/HTTPS API, browser
UI, secrets, local pipeline) and a **Worker** (paired remote node that only performs FFmpeg
derive work). Product/UI copy is Chinese; code, comments and docs are English.

## Commands

```bash
go build -o timingdex ./cmd/timingdex
go test ./...
go vet ./...

# single package / single test
go test ./internal/app -run TestPipelineStagesAnalysis -v
go test ./internal/repository/sqlite -run TestJobs -v
```

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

- `isRetryableJobError` decides retry vs. permanent failure. Provider HTTP failures are
  classified by type — `errors.As` on `*common.StatusError`, where 4xx (except 408/429) is
  permanent — but every other failure mode still falls back to **substring matching on the
  error text** ("not configured", "validation_error", "metadata missing", …). If you add a
  new permanent failure mode that isn't an HTTP status, add its phrase there or it will burn
  provider quota through the 1s→2s→4s (max 30s) backoff.
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

- **Provider keys** live only in `secretstore` (encrypted, Hub-only, `provider-secrets/`
  0700, files 0600). Never in SQLite, API responses, browser storage, Worker config, logs,
  error strings or `String()`/`MarshalJSON` output. The data-encryption key is its own
  `provider-secrets/store.key` file, deliberately *not* derived from the admin token —
  deriving it made token rotation brick every CLI command. `Open` still takes the admin
  token solely to migrate a pre-`store.key` store on first run (backing the original up to
  `.pre-key-migration`); don't reintroduce it as key material.
- **Hub admin token** (`hubauth`) is generated at first start, compared with
  `crypto/subtle`, and enforced by `s.requireHubAdmin(...)` on every mutating/administrative
  route. New write endpoints default to wrapped, not open.
- **Worker trust**: one-time pairing token, revocable node token, TLS certificate
  fingerprint pinning. A Worker gets a *lease-bound, memory-only* provider credential only
  when `hub_security.allow_worker_provider_credentials` is explicitly enabled (default
  deny); otherwise it uses the Hub JSON proxy, which rejects media/multipart and bodies over
  2 MiB. Audit records (`LeaseAudit`) must stay key-free.
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
`internal/api/server.go` holds the base constants (`legacyLibraryIndexHTML`,
`progressHTML`, `repurposeHTML`, …) and the `*_v015.go` files build newer pages by
**exact-match `strings.Replace` over those constants** (see `library_page_v015.go`). Editing
the legacy HTML silently breaks those overlays — the replacement just no-ops. When changing a
v0.15+ page, check whether the string it patches still exists.

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
