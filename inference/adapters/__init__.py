"""Locked model catalog and lazy adapter registry."""
from __future__ import annotations

import importlib
import importlib.util
import json
from pathlib import Path
from typing import Any, Mapping

from .base import AdapterContext, ModelUnavailable, UnsupportedInput, UnsupportedOperation

_LOCK_PATH = Path(__file__).resolve().parents[1] / "models.lock.json"
_FAMILY_MODULES = {
    "siglip2": ("inference.adapters.siglip2", "Siglip2Adapter"),
    "wemm": ("inference.adapters.wemm", "WemmAdapter"),
    "clap": ("inference.adapters.clap", "ClapAdapter"),
    "yamnet": ("inference.adapters.yamnet", "YamnetAdapter"),
    "campplus": ("inference.adapters.speaker", "SpeakerAdapter"),
    "eres2netv2": ("inference.adapters.speaker", "SpeakerAdapter"),
    "pyannote": ("inference.adapters.diarization", "DiarizationAdapter"),
}


def load_catalog() -> dict[str, Any]:
    try:
        with _LOCK_PATH.open("r", encoding="utf-8") as file:
            value = json.load(file)
    except (OSError, json.JSONDecodeError) as exc:
        raise ModelUnavailable("locked model catalog is unavailable") from exc
    if value.get("schema_version") != "inference.models.lock.v1" or not isinstance(value.get("models"), list):
        raise ModelUnavailable("locked model catalog has an unsupported schema")
    return value


def catalog_models() -> list[dict[str, Any]]:
    return load_catalog()["models"]


def resolve_model(model_id: str, revision: str, operation: str) -> dict[str, Any]:
    for entry in catalog_models():
        if entry.get("model_id") == model_id and entry.get("revision") == revision:
            if operation not in entry.get("operations", []):
                raise UnsupportedOperation("locked model does not support this operation")
            if entry.get("family") not in _FAMILY_MODULES:
                raise UnsupportedOperation("locked model family has no adapter")
            return entry
    raise UnsupportedInput("model ID and immutable revision do not match the lock")


def choose_model_dir(root: Path, entry: Mapping[str, Any]) -> Path:
    root = root.resolve()
    candidates = [root / str(entry.get("catalog_id", ""))]
    model_id = str(entry.get("model_id", ""))
    candidates.append(root / model_id)
    candidates.append(root / model_id.replace("/", "--"))
    # A direct model directory is useful for a single-model installation.
    candidates.append(root)
    for candidate in candidates:
        try:
            resolved = candidate.resolve(strict=True)
        except OSError:
            continue
        if not resolved.is_dir() or not resolved.is_relative_to(root):
            continue
        required = entry.get("files", [])
        if required and all((resolved / spec["path"]).is_file() for spec in required if isinstance(spec, dict)):
            return resolved
    # Return the canonical catalog path; the adapter will report a typed unavailable error.
    return candidates[0]


def expected_dimension(entry: Mapping[str, Any], request: Mapping[str, Any]) -> int:
    output = entry.get("output", {})
    dimension = output.get("dimension")
    if not isinstance(dimension, int) or dimension <= 0:
        return 0
    truncate_to = request.get("settings", {}).get("visual", {}).get("truncate_to")
    allowed = output.get("matryoshka", [])
    if truncate_to is None or truncate_to == 0:
        return dimension
    if truncate_to not in allowed or truncate_to > dimension:
        raise UnsupportedInput("requested truncation is not supported by the locked model")
    return int(truncate_to)


def make_adapter(entry: Mapping[str, Any], context: AdapterContext) -> Any:
    module_name, class_name = _FAMILY_MODULES[str(entry["family"])]
    try:
        module = importlib.import_module(module_name)
        adapter_class = getattr(module, class_name)
    except (ImportError, AttributeError) as exc:
        raise ModelUnavailable("model adapter dependencies are unavailable") from exc
    return adapter_class(context)


def capability_report(model_dir: Path) -> dict[str, Any]:
    catalog = load_catalog()
    capabilities: list[dict[str, Any]] = []
    dependencies = {
        "siglip2": ["torch", "transformers", "PIL"],
        "wemm": ["torch", "transformers", "qwen_vl_utils", "PIL"],
        "clap": ["torch", "transformers", "soundfile", "numpy"],
        "yamnet": ["tensorflow", "soundfile", "numpy"],
        "campplus": ["torch", "funasr", "soundfile", "numpy"],
        "eres2netv2": ["torch", "funasr", "soundfile", "numpy"],
        "pyannote": ["torch", "pyannote.audio", "soundfile", "numpy"],
    }
    for entry in catalog["models"]:
        model_path = choose_model_dir(model_dir, entry)
        files = entry.get("files", [])
        local_files = bool(files) and all((model_path / item["path"]).is_file()
                                          for item in files if isinstance(item, dict))
        available = (entry.get("artifact_status") not in {"pending_download", "blocked_gated"}
                     and local_files)
        for dependency in dependencies.get(entry.get("family"), []):
            try:
                available = available and importlib.util.find_spec(dependency) is not None
            except (ImportError, ModuleNotFoundError, ValueError):
                available = False
        output = entry.get("output", {})
        backend = entry.get("runtime", {}).get("backend", "unknown")
        for operation in entry.get("operations", []):
            capabilities.append({"operation": operation, "model_id": entry.get("model_id"),
                                 "model_revision": entry.get("revision"),
                                 "schema_version": "inference.v1",
                                 "dimension": output.get("dimension"), "backend": backend,
                                 "available": bool(available)})
    return {"schema_version": "inference.capabilities.v1", "capabilities": capabilities}
