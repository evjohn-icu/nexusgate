#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tags=$(grep -rhoE 'timingdex:v[0-9]+\.[0-9]+\.[0-9]+(-[[:alnum:].-]+)?' \
  "$root/docker-compose.yml" "$root"/deploy/unraid/*.xml | sort -u)
count=$(printf '%s\n' "$tags" | sed '/^$/d' | wc -l)
if [ "$count" -ne 1 ]; then
  printf 'deployment image tags differ or are missing:\n%s\n' "$tags" >&2
  exit 1
fi
printf 'deployment image tag: %s\n' "$tags"
