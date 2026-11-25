"""Shared fixtures for the inference runtime tests: synthetic lock entries."""
from __future__ import annotations

import contextlib
import json
import os
import subprocess
import sys
from pathlib import Path
from unittest import mock

from inference import adapters

REPO_ROOT = Path(__file__).resolve().parents[2]


def lock_path(tmp, entries: list[dict]) -> Path:
    """Write a synthetic lock and point the adapter registry at it."""
    tmp = Path(tmp)
    lock = {
        "schema_version": "inference.models.lock.v1",
        "generated_at": "2026-11-25",
        "device": {},
        "environments": {},
        "notes": [],
        "models": entries,
    }
    path = tmp / "models.lock.json"
    path.write_text(json.dumps(lock), encoding="utf-8")
    return path


def entry(catalog_id: str = "synthetic-events", model_id: str = "synthetic/model",
          revision: str = "rev-1", family: str = "synthetic",
          operations: tuple[str, ...] = ("sound_events",), artifact_status: str | None = None,
          files: tuple[str, ...] | None = ("model.bin",), dimension: int | None = None,
          dtypes: tuple[str, ...] = ("float32",), backend: str = "synthetic",
          backends: tuple[str, ...] | None = None) -> dict:
    model: dict = {
        "catalog_id": catalog_id, "model_id": model_id, "revision": revision,
        "family": family, "operations": list(operations),
        "runtime": {"backend": backend, "dtypes": list(dtypes)},
        "output": {} if dimension is None else {"dimension": dimension},
    }
    if artifact_status is not None:
        model["artifact_status"] = artifact_status
    if files is not None:
        model["files"] = [{"path": name, "size": 1, "sha256": "0" * 64} for name in files]
    if backends is not None:
        model["runtime"]["backends"] = list(backends)
    return model


def make_fixture_dir(tmp, model: dict) -> Path:
    """Create the locked artifact files for one model under tmp/models."""
    tmp = Path(tmp)
    root = tmp / "models"
    model_dir = root / model["catalog_id"]
    model_dir.mkdir(parents=True, exist_ok=True)
    for spec in model.get("files", []):
        (model_dir / spec["path"]).write_bytes(b"fixture")
    return root


@contextlib.contextmanager
def patched_catalog(tmp: Path, entries: list[dict]):
    path = lock_path(tmp, entries)
    with mock.patch.object(adapters, "_LOCK_PATH", path):
        yield path


@contextlib.contextmanager
def patched_dependencies(available: bool):
    with mock.patch.object(adapters, "_dependency_available", return_value=available):
        yield


def run_dispatch(*args: str) -> subprocess.CompletedProcess:
    """Run the real dispatcher from the repository root, as the Go executor does."""
    env = dict(os.environ)
    existing = env.get("PYTHONPATH")
    env["PYTHONPATH"] = str(REPO_ROOT) + (os.pathsep + existing if existing else "")
    return subprocess.run([sys.executable, "-B", "-m", "inference.dispatch", *args],
                          capture_output=True, cwd=str(REPO_ROOT), env=env, timeout=180)


def parse_single_frame(proc: subprocess.CompletedProcess) -> dict:
    lines = [line for line in proc.stdout.decode("utf-8").splitlines() if line.strip()]
    assert len(lines) == 1, f"expected exactly one protocol frame, got {lines!r}"
    return json.loads(lines[0])
