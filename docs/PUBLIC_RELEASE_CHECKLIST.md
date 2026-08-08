# Public Release Checklist

Pre-public gate for `evjohn-icu/timingdex`. Run through before flipping the
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
- [ ] No real API credentials in history (see secret scan; masked report only)
- [ ] No private endpoints, personal footage, local databases or eval corpora
      with private media tracked (fixtures are lavfi-synthetic)
- [ ] No stray build artifacts tracked (`timingdex-mcp` removed; source
      tarballs gitignored)
- [ ] Git history secret scan clean (gitleaks/trufflehog or the manual
      pattern scan in the release prep round)

## Gates (must be run, not assumed)

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `go test -race -count=1 ./...`
- [ ] `GOOS=windows GOARCH=amd64 go build ./...` and `go vet`
- [ ] `bash scripts/check-doc-refs.sh`
- [ ] CI green (`.github/workflows/ci.yml` incl. the 65 % coverage gate)
- [ ] Retrieval golden set green: `go test ./internal/repository/sqlite -run
      TestRetrievalGolden -v`

## Product claims

- [ ] README quickstart works from a clean data dir
- [ ] README states alpha / technical-preview status and the "do not expose
      the Hub to the public Internet" boundary
- [ ] README architecture section does not claim embeddings / reranker /
      cascade routing (not implemented)
- [ ] Local VLM quickstart matches `config.example.json` fields
- [ ] `/providers` vs `providers.*` boundary documented (openai_multiframe
      is config-only)
- [ ] Capability wording stays conservative everywhere (heuristic feature
      vector, not learned embedding)
