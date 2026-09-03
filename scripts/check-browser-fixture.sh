#!/bin/sh
set -eu

# Keep both the server package and the standalone launcher visible to CI.
go test -tags playwrightfixture ./internal/api/browserfixture -run '^$' -count=1
go test -tags playwrightfixture ./cmd/nexusslate-playwright-fixture -run '^$' -count=1
