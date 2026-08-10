#!/bin/sh
set -eu

# Keep the optional server build visible to CI even when the smoke test is unavailable.
go test -tags playwrightfixture ./internal/api/browserfixture -run '^$' -count=1
