#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
version=$(tr -d '\r\n' < "$root/VERSION")
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-alpha$ ]]; then
  printf 'invalid VERSION: %s\n' "$version" >&2
  exit 1
fi
out=${1:-"$root/dist"}
mkdir -p "$out"

ldflags="-s -w -X github.com/evjohn-icu/nexusgate/internal/buildinfo.Version=$version -X github.com/evjohn-icu/nexusgate/internal/domain.Version=$version"
suffix=
if [ "${GOOS:-$(go env GOOS)}" = windows ]; then
  suffix=.exe
fi

go build -trimpath -ldflags="$ldflags" -o "$out/nexusgate$suffix" "$root/cmd/nexusgate"
go build -trimpath -ldflags="$ldflags" -o "$out/nexusgate-mcp$suffix" "$root/cmd/nexusgate-mcp"

# Worker binaries are the same nexusgate binary, cross-compiled for the three
# worker platforms (see internal/api/worker_setup_page.go workerPlatforms).
# The Hub serves these from <DataDir>/worker-binaries/ so an operator does not
# need a Go toolchain to enrol a node.
for target in "linux amd64 nexusgate-linux-amd64" \
              "linux arm64 nexusgate-linux-arm64" \
              "windows amd64 nexusgate-windows-amd64.exe"; do
  set -- $target
  GOOS=$1 GOARCH=$2 go build -trimpath -ldflags="$ldflags" -o "$out/$3" "$root/cmd/nexusgate"
done

printf 'built nexusgate %s in %s\n' "$version" "$out"
