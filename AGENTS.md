# Repository Guidelines

## Project Structure

Go monorepo for Re:Footage / `nexusslate`.

- `cmd/` contains the Hub CLI, MCP client, evaluation tools, and fixtures.
- `internal/domain` holds pure domain types; `internal/app` contains use cases and the pipeline.
- `internal/api` exposes the HTTP API and inline HTML/CSS/JS pages.
- `internal/repository/sqlite` is the only SQL implementation; migrations are in `migrations/`.
- `internal/providers`, `internal/media`, `internal/ingest`, `internal/worker`, and `internal/search` contain provider adapters, media processing, scanning, workers, and Search v2.
- Tests sit beside code; assets are under `deploy/`, `docs/`, and `scripts/`.

## Build and Development

```bash
gofmt -l .                 # must print nothing
go build ./...
go vet ./...
go test ./...
go test -race ./internal/...
```

Run focused tests with `go test ./internal/app -run TestName -v`. Media integration tests require `ffmpeg` and `ffprobe`; provider tests use offline `net/http/httptest` fixtures. Set `NEXUSSLATE_DATA_DIR` for local `doctor`, `root`, and `serve` commands.

## Coding Style and Naming

Use standard Go formatting (`gofmt`) and idiomatic exported (`PascalCase`) and unexported (`camelCase`) names. Keep repository interfaces in the consuming package (`internal/app`), SQL in `internal/repository/sqlite`, and HTTP concerns out of lower layers. Comments should explain why a boundary exists. Product/UI copy is Chinese; code, identifiers, comments, and documentation are English.

## Testing and Data Boundaries

Add tests beside changed code with descriptive `Test...` names. Persistence claims belong in `internal/repository/sqlite` tests using real migrations, not only in-memory fakes. Never call real provider APIs. Add a numbered migration for schema changes; never edit an applied migration. Model output must pass validation and staging before canonical writes, and source media remains read-only.

## Commits and Pull Requests

Use concise Conventional Commit-style subjects such as `fix(search): ...`, `feat: ...`, `test: ...`, or `docs: ...`. Keep PRs small and boundary-focused. Explain behavior and test commands; link the design issue when adding dependencies, migrations, API routes, or model-write behavior. Update `CHANGELOG.md` for version-level boundary changes. Include screenshots or reproduction steps for UI changes. Report security issues privately via `SECURITY.md`.

## Security and Configuration

Never commit `config.json`, tokens, generated worker scripts, or `provider-secrets/`. Provider keys belong in the Hub-only encrypted secret store and must not appear in SQLite, API responses, logs, browser storage, or worker configuration. New mutating API routes require Hub-admin gating. Do not expose the Hub directly to the public Internet.

## Agent Workflow

Delegate independent routine work to Luna Max sub-agents when practical, with at most 5 concurrent agents including the coordinator. Coordinator owns integration, review, and final verification. Run memory-heavy local test and benchmark processes serially.

## State Tracking

Maintain `state.md` at the repository root as the concise handoff record. Read it before starting substantial work, and update it after meaningful changes or verification. Record current status, decisions, tests run, blockers, and next steps; keep entries factual and do not store credentials, tokens, or other secrets.
