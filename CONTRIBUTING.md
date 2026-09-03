# Contributing to Re:Footage (`nexusslate`)

Thanks for looking. This project has a small number of rules that are not style
preferences — they are the reason several packages exist. This document is
mostly those. Everything here is enforced either by CI or by review.

Repository: <https://github.com/evjohn-icu/nexusslate>
Module path: `github.com/evjohn-icu/nexusslate`
Licence: Apache-2.0 — see [`LICENSE`](LICENSE). Contributions are accepted under
the same licence (Apache-2.0 §5); there is no separate CLA.

Security bugs do **not** belong in a public issue or pull request. See
[`SECURITY.md`](SECURITY.md).

## Before you start

- **Small, boundary-shaped pull requests.** One change, one boundary. A PR that
  moves a security or data boundary should say which one, in the description and
  in `CHANGELOG.md`.
- **Open an issue first** for anything that adds a dependency, adds a database
  migration, changes an API route, or changes what the model layer is allowed to
  write. Those are design conversations, and finding out after the code is
  written is worse for everyone.
- Product/UI copy is **Chinese**. Code, identifiers, comments, commit messages,
  documentation and this file are **English**. Don't mix the two directions.

## The dependency rule

The binary is the Go standard library plus a deliberately small set of direct
dependencies — no web framework, no ORM, no router, no logging library, no
frontend build step, no assertion library. `go.mod` is the dependency truth:
do not document a dependency contract anywhere else, because it will drift.

**Adding a dependency is a discussion, not a PR detail.** Open an issue that
says what it buys, what it costs at build and audit time, and why the standard
library cannot do it. A PR that quietly grows `go.mod` will be asked to justify
it before anything else in the diff is reviewed. This is a local-first tool that
handles third-party API keys; the supply-chain surface is part of the product.

Test-only dependencies are held to the same rule. `net/http/httptest` and
`testing` are enough for everything the repo does today.

## The local gate

Run all of this before opening a pull request:

```bash
gofmt -l .                 # must print nothing
go build ./...
go vet ./...
go test ./...
```

`gofmt -l .` printing any filename fails CI, so fix formatting first — the
project does not use a formatter beyond plain `gofmt`.

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs the same four
steps and then three more, so mirror these locally if you touched the relevant
areas:

```bash
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go vet ./...   # the Worker tray is Win32 code CI cannot execute
go test -race ./internal/...
```

The Windows cross-compile and vet exist because the Worker install wizard hands
out a cross-compiled `.exe` and the notification-area tray is Win32 syscall code
no Linux runner can run; `vet` is what catches `unsafe.Pointer` misuse there.

CI installs `ffmpeg`, so `internal/media`'s integration tests actually execute on
the runner. Locally, `internal/media/process_integration_test.go` skips itself
unless `ffmpeg` and `ffprobe` are on `PATH`. If you change derive or probe
behaviour, install them and run it for real rather than trusting the skip.

## Tests

**No test ever calls a real provider API.** Provider adapters are covered by
`net/http/httptest` fixtures that serve recorded-shaped responses. A test that
reaches the network — for a key you happen to have, "just once", or behind a
build tag — will not be merged. The whole suite must run offline, deterministic,
and free.

When you add a provider adapter, the fixture test is part of the change, not a
follow-up. Cover at least: the success shape, an upstream 4xx (which must be
classified as permanent), an upstream 5xx (retryable), and a malformed body.

Run a single package or test the usual way:

```bash
go test ./internal/app -run TestPipelineStagesAnalysis -v
go test ./internal/repository/sqlite -run TestJobs -v
```

## Database migrations

Migrations live in `internal/repository/sqlite/migrations/NNNN_*.sql`, are
`go:embed`-ed, and are applied in filename order inside a transaction, tracked in
`schema_migrations`.

- **Add a new numbered file.** Take the next number after the highest existing
  one.
- **Never edit a migration that has already been applied**, including your own
  from an earlier merged PR. Someone's library has already run it; editing it
  silently diverges their schema from yours with no error. Fix forward with a new
  file.
- Keep SQL in `internal/repository/sqlite`. That package is the only place SQL
  is allowed to live.

## Retry classification

`isRetryableJobError` in `internal/app/pipeline.go` decides retry versus
permanent failure. Classification is by status code or sentinel, never by the
wording of the message:

- HTTP failures are classified by status alone (`errors.As` on
  `*common.StatusError`): a 4xx except 408/429 is permanent, unless
  `providerpool.ClassifyFailure` labels it `MemberSpent` (401/402/403 — the pool
  has already retired that key, so the next attempt selects a different member
  and retrying is right).
- Every other failure mode is classified by sentinel. A failure that no retry
  can fix is marked with `domain.Permanent(err)` at the line that creates it and
  checked with `errors.Is(err, domain.ErrPermanentFailure)`; an unmarked error
  stays retryable by default.
- Hard non-retries: `context.Canceled` / `context.DeadlineExceeded`, and ENOSPC
  (`isNoSpaceErr`), which the execute path defers/park before this decision.

**Known discrepancy:** `openspec/specs/error-classification/spec.md` currently
classifies a provider `context.DeadlineExceeded` as retryable, while the current
pipeline guard treats the sentinel as a hard non-retry. Do not resolve that
contract difference by adding message matching; change the specification and
implementation together after a provider adapter error-chain repro.

There is no error-text substring list. If you introduce a new permanent failure
mode that is not an HTTP status, mark it `domain.Permanent(...)` where it is
created; do not phrase-match the message. Otherwise it burns provider quota
through the 1s → 2s → 4s (max 30s) backoff until the attempt budget is gone.

The behaviour is pinned by tests in `internal/app` (`retry_test.go`,
`retry_permanence_test.go`, `pipeline_spent_key_retry_test.go`,
`redact_error_test.go`): they assert classification comes from the status or the
sentinel, not the message text, and that the marker survives wrapping. Keep
them green when you change a failure path.

## The browser UI: read this before editing a page

Pages are Go string constants of inline HTML/CSS/JS. There are no templates, no
static assets and no build step.

`internal/api/server.go` holds the base constants (`legacyLibraryIndexHTML`,
`progressHTML`, `repurposeHTML`, …). The `*_v015.go` files build the newer pages
by **exact-match `strings.Replace` over those constants** — see
`internal/api/library_page_v015.go`.

That means: **editing the legacy constant silently no-ops the overlay.** The
replacement finds no match, returns the string unchanged, and nothing errors.
The page just quietly loses the newer behaviour.

So when you change a v0.15+ page:

1. Find every `strings.Replace` whose input is the constant you are editing.
2. Check that the exact substring each one searches for still exists.
3. Update the overlay in the same commit, and load the page to confirm.

## Boundaries that reviews will not bend on

These are why the code is shaped the way it is. A PR that crosses one needs an
argument, not just a green build.

- **Model output never writes directly to canonical tables.** The path is
  `CreateModelRun` → provider call → `FailModelRun` on error →
  `ValidateAndNormalize` + shot validation → `StageModelRun` →
  `CommitAnalysisWithShots` (one transaction) → `RebuildSearch`. Failed,
  malformed or out-of-range responses stay in `model_runs`. The same shape
  governs tag curation and Repurpose plans. **Agents may draft; humans approve.**
- **Provider keys live only in `secretstore`** — never in SQLite, API responses,
  browser storage, Worker config, logs, error strings, or `String()` /
  `MarshalJSON` output.
- **New write endpoints default to gated.** Wrap them in
  `s.requireHubAdmin(...)`; do not add an open mutating route.
- **Original media is read-only.** Nothing is written next to a source file.
  Derived artifacts go under the data directory.
- **Derived-artifact cache paths embed the hardware mode/profile**
  (`thumbnail-<mode>.jpg`, `proxy-<mode>.mp4`, `proxy-720-<profile>`) so a
  software x264 proxy is never mislabelled as an NVENC/QSV/VideoToolbox result.
  Preserve that when touching `media.HardwarePlan` or artifact naming.
- **`common.ReadError`'s truncation bound stays.** The text it embeds reaches
  `jobs.last_error_message` and the `/progress` page; an unbounded copy of a
  relay's echoed request would persist a provider key.
- **Repository interfaces are declared at the consumer** (`internal/app`), not in
  the sqlite package. `internal/domain` is pure types with no I/O. Everything
  below `internal/app` is free of HTTP.
- **Capability claims stay conservative** in code and docs. Proprietary RAW is
  "identified, not decoded"; shot similarity is a "heuristic feature vector", not
  a learned embedding. Don't upgrade the wording past what the code does.

## Adding a provider

Four parts, in one PR:

1. An adapter package under `internal/providers/`.
2. An entry in `internal/providers/factory.go`.
3. Capability metadata in `internal/providerchannels`.
4. An `httptest` fixture test (see [Tests](#tests)).

Provider channels (SQLite-backed, managed at `/providers`) win over legacy
`providers.*` config; both resolve to the same `providers.*` interfaces, so
`Pipeline` stays unaware of which path supplied the client. Keep it that way.

## Documentation and changelog

- Every version-level change gets a `CHANGELOG.md` entry that describes **the
  boundary the change moved**, not just the feature.
- A release-scoped change also gets a `docs/vX.Y-*.md` (goal / operations)
  document, following the existing files.
- Comments explain *why a boundary exists*, not what the line does. Match that
  tone.
- Long single-line struct literals and dense config defaults are the existing
  style. Match the surrounding file; do not reformat unrelated code in your diff.
- `skills/nexusslate/` is a versioned agent-facing contract. If you change the API
  surface, keep `GET /api/v1/agent/capabilities` and
  `skills/nexusslate/references/api-contract.md` in sync — the skill relies on
  `approval_mode: human_required` and an `allowed_actions` allowlist that
  excludes plan approval, pipeline runs, provider keys and raw media paths.

## Running it locally

```bash
export NEXUSSLATE_DATA_DIR="$PWD/.nexusslate-dev"
go build -o nexusslate ./cmd/nexusslate

./nexusslate doctor
./nexusslate root add /path/to/footage && ./nexusslate root list
./nexusslate root scan <root-id>     # scans, enqueues, and drains the Pipeline
./nexusslate pipeline run
./nexusslate serve
```

All Hub state is under `$NEXUSSLATE_DATA_DIR` (`nexusslate.db`, optional
`config.json`, `cache/`, `admin-token`, `agent-token`, `provider-secrets/`).
Deleting that directory is how you reset. Never commit it, a `config.json` with
real endpoints, a generated Worker install script, or anything from
`provider-secrets/`.
