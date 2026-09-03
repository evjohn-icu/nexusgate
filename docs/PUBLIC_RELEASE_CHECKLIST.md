# Public Release Checklist

Pre-public gate for `evjohn-icu/nexusslate`. Run through before flipping the
repository public or announcing the alpha. Keep this file short; each item is
a yes/no with the evidence where it lives.

## Repository hygiene

- [ ] `LICENSE` present (Apache-2.0)
- [ ] `SECURITY.md` present, alpha-era wording (no invented SLA)
- [ ] `CONTRIBUTING.md` present; dependency rule matches `go.mod`
- [ ] `THIRD-PARTY-LICENSES` present and current (`go mod graph` + the direct
      dependency list in `go.mod`)
- [ ] `config.example.json` contains only env-var names, never key values
- [ ] No `.env.example` with values; no `.env`, `config.json`, `worker.json`,
      `.mcp.json` (non-example) tracked
- [ ] No `admin-token` / `agent-token` / `provider-secrets/` / `store.key`
      anywhere in the tree or history
- [ ] No real API credentials in history (automated by the CI secret scan,
      which runs with `--redact`; masked report only)
- [ ] No private endpoints, personal footage, local databases or eval corpora
      with private media tracked (fixtures are lavfi-synthetic)
- [ ] No stray build artifacts tracked (`nexusslate-mcp` removed; source
      tarballs gitignored)
- [ ] Git history secret scan clean (automated: the `secret scan (full
      history)` step in `.github/workflows/ci.yml` runs gitleaks over the
      whole history; no manual scan needed)

## Gates (must be run, not assumed)

- [ ] `VERSION`, Docker/Compose, Unraid templates and release notes declare the
      same prerelease; `bash scripts/check-deployment-versions.sh` passes
- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `go test -race -count=1 ./...`
- [ ] `GOOS=windows GOARCH=amd64 go build ./...` and `go vet`
- [ ] `bash scripts/check-doc-refs.sh`
- [ ] CI green (`.github/workflows/ci.yml` incl. the 65 % coverage gate)
- [ ] Retrieval golden set green: `go test ./internal/repository/sqlite -run
      TestRetrievalGolden -v`

## v0.31 Release Preparation

- [ ] Root `VERSION` is the intended prerelease (`v0.31.0-alpha` for this
      release), and the deployment check passes:
      `bash scripts/check-deployment-versions.sh`
- [ ] Native binaries are built with the same version metadata:
      `bash scripts/build-release.sh ./dist`
- [ ] [`docs/v0.31-release-notes.md`](v0.31-release-notes.md) records the
      actual CI, benchmark, relevance-eval, and full test results; no item is
      still marked pending.
- [ ] After every gate is green, create the exact version tag and mark the
      GitHub release as an alpha/prerelease. This repository-preparation round
      does not create or push tags, releases, or container images.

## Product claims

- [ ] README quickstart works from a clean data dir
- [ ] README states alpha / technical-preview status and the "do not expose
      the Hub to the public Internet" boundary
- [ ] README architecture section does not claim visual embeddings / reranker /
      cascade routing (not implemented). Text embeddings are implemented
      (`shot_text_embeddings`, cosine scan in Go, no vector database) and must
      be described as retrieval signals, never evidence
- [ ] Local VLM quickstart matches `config.example.json` fields
- [ ] `/providers` vs `providers.*` boundary documented (openai_multiframe
      is config-only)
- [ ] Capability wording stays conservative everywhere (heuristic feature
      vector, not learned embedding)
