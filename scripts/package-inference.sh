#!/usr/bin/env bash
# Package the checked-in inference module (dispatch/protocol/adapters code,
# the model lock inference/models.lock.json and the pinned requirements) as
# nexusgate-inference.tar.gz for release uploads and native installs.
#
# Weights (inference/models/), virtualenvs (inference/.venv*), benchmark
# caches and bytecode are never included: they are multi-gigabyte or
# regenerable runtime artifacts, neither reviewable nor portable. Model
# weights arrive at runtime through the Hub's hash-verified install path.
#
# Callers: scripts/build-release.sh and .github/workflows/release.yml. Keep
# this command the single authority for the exclusion list.
set -euo pipefail

out=${1:?usage: scripts/package-inference.sh <output-directory>}
root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$out"

tar -czf "$out/nexusgate-inference.tar.gz" \
    --exclude='inference/.venv' \
    --exclude='inference/.venv-*' \
    --exclude='inference/.cache' \
    --exclude='inference/models' \
    --exclude='__pycache__' \
    --exclude='*.pyc' \
    -C "$root" \
    inference

printf 'packaged %s\n' "$out/nexusgate-inference.tar.gz"
