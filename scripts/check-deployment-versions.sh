#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
version=$(tr -d '\r\n' < "$root/VERSION")
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-alpha$ ]]; then
  printf 'invalid VERSION: %s\n' "$version" >&2
  exit 1
fi
tags=$(grep -rhoE 'nexusgate:v[0-9]+\.[0-9]+\.[0-9]+(-[[:alnum:].-]+)?' \
  "$root/docker-compose.yml" "$root"/deploy/unraid/*.xml | sort -u)
count=$(printf '%s\n' "$tags" | sed '/^$/d' | wc -l)
if [ "$count" -ne 1 ]; then
  printf 'deployment image tags differ or are missing:\n%s\n' "$tags" >&2
  exit 1
fi
if [ "$tags" != "nexusgate:$version" ]; then
  printf 'deployment image tag %s does not match VERSION %s\n' "$tags" "$version" >&2
  exit 1
fi
for symbol in internal/buildinfo.Version internal/domain.Version; do
  if ! grep -Fq "$symbol" "$root/Dockerfile"; then
    printf 'Dockerfile does not inject %s\n' "$symbol" >&2
    exit 1
  fi
done
release_series=${version%-alpha}
release_series=${release_series%.*}
for document in README.md CHANGELOG.md "docs/$release_series-release-notes.md" deploy/unraid/README.md; do
  if ! grep -Fq "$version" "$root/$document"; then
    printf '%s does not declare %s\n' "$document" "$version" >&2
    exit 1
  fi
done
printf 'deployment image tag: %s\n' "$tags"
