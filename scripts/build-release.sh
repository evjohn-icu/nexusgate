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

ldflags="-s -w -X github.com/evjohn-icu/timingdex/internal/buildinfo.Version=$version -X github.com/evjohn-icu/timingdex/internal/domain.Version=$version"
suffix=
if [ "${GOOS:-$(go env GOOS)}" = windows ]; then
  suffix=.exe
fi

go build -trimpath -ldflags="$ldflags" -o "$out/timingdex$suffix" "$root/cmd/timingdex"
go build -trimpath -ldflags="$ldflags" -o "$out/timingdex-mcp$suffix" "$root/cmd/timingdex-mcp"
printf 'built timingdex %s in %s\n' "$version" "$out"
