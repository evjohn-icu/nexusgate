"""Locked model catalog and lazy adapter registry."""
from __future__ import annotations

import importlib
import importlib.util
import json
from pathlib import Path
from typing import Any, Mapping

from .base import (AdapterContext, InferenceFailure, ModelUnavailable, UnsupportedInput,
                   UnsupportedOperation)

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


# Operations that write vectors into an embedding space. Their lock entry must
# name a positive output dimension; interval-producing operations keep their
# complete structured result validation with dimension 0.
EMBEDDING_OPERATIONS = frozenset({"visual_embedding", "audio_embedding", "speaker_embedding"})

# The closed set of typed failure codes the Go executor understands.
TYPED_CODES = frozenset({"model_unavailable", "unsupported_operation", "unsupported_input",
                         "invalid_output", "resource_exhausted", "deadline_exceeded",
                         "cancelled"})

# Dependency modules whose mere presence is checked by the ordinary inventory
# probe. Loading them is deliberately left to the adapters themselves.
_FAMILY_DEPENDENCIES = {
    "siglip2": ["torch", "transformers", "PIL"],
    "wemm": ["torch", "transformers", "qwen_vl_utils", "PIL"],
    "clap": ["torch", "transformers", "soundfile", "numpy"],
    "yamnet": ["tensorflow", "soundfile", "numpy"],
    "campplus": ["torch", "funasr", "soundfile", "numpy"],
    "eres2netv2": ["torch", "funasr", "soundfile", "numpy"],
    "pyannote": ["torch", "pyannote.audio", "soundfile", "numpy"],
}

_BACKEND_MODULES = {"torch": "torch", "tensorflow": "tensorflow"}


def load_backends(entry: Mapping[str, Any]) -> list[str]:
    """Backends the locked entry may execute on; the lock may narrow this."""
    runtime = entry.get("runtime", {})
    backends = runtime.get("backends")
    if isinstance(backends, list):
        return [str(item) for item in backends]
    return ["cpu", "cuda"]


def _cuda_backend_available(entry: Mapping[str, Any]) -> bool:
    """Verify the configured CUDA runtime, not merely the lock's device list.

    A CPU-only wheel or a missing backend module cannot serve a configured
    CUDA device. Only a CUDA request reaches here, so CPU probes never import
    the heavy backend.
    """
    module_name = _BACKEND_MODULES.get(str(entry.get("runtime", {}).get("backend", "")))
    if module_name is None:
        return False
    try:
        module = importlib.import_module(module_name)
    except (ImportError, ModuleNotFoundError):
        return False
    try:
        if module_name == "torch":
            return bool(module.cuda.is_available())
        if module_name == "tensorflow":
            return bool(module.config.list_physical_devices("GPU"))
    except Exception:
        return False
    return False


def validate_runtime_profile(entry: Mapping[str, Any], device: str, dtype: str) -> None:
    """The single shared runtime-profile gate.

    Used by the capability inventory, the explicit model probe and ordinary
    execution alike, so one request can never run a profile the lock does not
    permit. Raises UnsupportedInput for a profile outside the lock's rules and
    ModelUnavailable when a configured CUDA device has no working backend.
    """
    allowed_dtypes = entry.get("runtime", {}).get("dtypes", ["float32"])
    if dtype not in allowed_dtypes:
        raise UnsupportedInput("requested dtype is not supported by the locked model")
    allowed_backends = load_backends(entry)
    if device not in allowed_backends:
        raise UnsupportedInput("requested device is not supported by the locked model")
    if device == "cuda" and not _cuda_backend_available(entry):
        raise ModelUnavailable("configured CUDA runtime is not available in this environment")


def require_positive_dimension(operation: str, expected: int) -> int:
    """Embedding operations need a known vector width; interval results do not.

    sound_events and diarization keep their complete structured result
    validation with dimension 0; visual, audio and speaker embeddings cannot
    write a usable space without a positive locked dimension.
    """
    if operation in EMBEDDING_OPERATIONS and expected <= 0:
        raise ModelUnavailable("locked model has no usable output dimension")
    return expected


def _dependency_available(module_name: str) -> bool:
    try:
        return importlib.util.find_spec(module_name) is not None
    except (ImportError, ModuleNotFoundError, ValueError):
        return False


def _positive_locked_dimension(output: Mapping[str, Any]) -> int:
    """Locked vector width; a missing or non-integer dimension reports 0."""
    dimension = output.get("dimension")
    if not isinstance(dimension, int) or isinstance(dimension, bool) or dimension < 0:
        return 0
    return dimension


def _capability_row(entry: Mapping[str, Any], operation: str, dimension: int,
                    backend: str, available: bool, code: str | None) -> dict[str, Any]:
    row: dict[str, Any] = {
        "operation": operation,
        "model_id": entry.get("model_id"),
        "model_revision": entry.get("revision"),
        "schema_version": "inference.v1",
        "dimension": int(dimension) if isinstance(dimension, int) and not isinstance(dimension, bool) else 0,
        "backend": backend,
        "available": bool(available),
    }
    if not available:
        row["unavailable_code"] = code if code in TYPED_CODES else "model_unavailable"
    return row


def _entry_unavailable(entry: Mapping[str, Any], model_dir: Path,
                       device: str, dtype: str) -> str | None:
    """Typed unavailability reason for one locked entry, or None when usable.

    The ordinary inventory probe may rely on artifact presence, import checks
    and the shared profile gate; it never loads a model. Gated and pending
    entries stay blocked until the lock itself permits use.
    """
    if entry.get("artifact_status") in {"pending_download", "blocked_gated"}:
        return "model_unavailable"
    model_path = choose_model_dir(model_dir, entry)
    files = entry.get("files", [])
    required = [item for item in files
                if isinstance(item, dict) and isinstance(item.get("path"), str)]
    if not required or not all((model_path / item["path"]).is_file() for item in required):
        return "model_unavailable"
    for module_name in _FAMILY_DEPENDENCIES.get(entry.get("family"), []):
        if not _dependency_available(module_name):
            return "model_unavailable"
    try:
        validate_runtime_profile(entry, device, dtype)
    except InferenceFailure as exc:
        return exc.code if exc.code in TYPED_CODES else "model_unavailable"
    return None


def _load_probe(entry: Mapping[str, Any], model_dir: Path, device: str, dtype: str) -> None:
    """Locked, load-only runtime check: verify paths and load, nothing else."""
    chosen = choose_model_dir(model_dir, entry)
    context = AdapterContext(model_dir=chosen, device=device, dtype=dtype,
                             progress=lambda _fraction, _stage=None: None,
                             model=entry, cancel_check=lambda: None)
    adapter = make_adapter(entry, context)
    load = getattr(adapter, "load", None)
    if load is None:
        raise ModelUnavailable("locked model adapter cannot prove a load-only probe")
    load()


def _probe_entry(model_id: str, revision: str) -> tuple[dict[str, Any] | None, bool]:
    """Exact locked entry for a probe identity.

    Returns (entry, locked) where locked is true only when both the model id
    and the immutable revision match the lock. A shared model id at another
    revision keeps the entry so the row can name the operation, but resolves
    to unavailable; a fully unknown identity has no entry at all.
    """
    models = catalog_models()
    for entry in models:
        if entry.get("model_id") == model_id and entry.get("revision") == revision:
            if entry.get("family") not in _FAMILY_MODULES:
                return None, False
            return entry, True
    for entry in models:
        if entry.get("model_id") == model_id:
            return entry, False
    return None, False


def probe_model(model_dir: Path, device: str, dtype: str,
                model_id: str, revision: str) -> dict[str, Any]:
    """One capability row for an exact locked model identity.

    Runs the artifact, dependency and profile checks and, when they pass, the
    locked load-only runtime check. Failures keep a typed unavailable code; an
    identity outside the lock can never satisfy the probe.
    """
    model_id, revision = str(model_id), str(revision)
    entry, locked = _probe_entry(model_id, revision)
    if entry is None:
        # Deliberately operation-less: the Go executor drops the schema-invalid
        # row and reports the model unavailable, so an identity the lock does
        # not pin can never be attested.
        return {"operation": "", "model_id": model_id, "model_revision": revision,
                "schema_version": "inference.v1", "dimension": 0, "backend": "unknown",
                "available": False, "unavailable_code": "model_unavailable"}
    code = _entry_unavailable(entry, model_dir, device, dtype)
    if not locked:
        code = "model_unavailable"
    operation = entry["operations"][0] if entry.get("operations") else ""
    dimension = _positive_locked_dimension(entry.get("output", {}))
    if operation in EMBEDDING_OPERATIONS and dimension <= 0:
        code, dimension = "model_unavailable", 0
    backend = entry.get("runtime", {}).get("backend", "unknown")
    row = _capability_row(entry, operation, dimension, backend, code is None, code)
    # The requested identity governs the row, not the catalog entry's revision.
    row["model_id"] = model_id
    row["model_revision"] = revision
    if row["available"]:
        try:
            _load_probe(entry, model_dir, device, dtype)
        except InferenceFailure as exc:
            row["available"] = False
            row["unavailable_code"] = exc.code if exc.code in TYPED_CODES else "model_unavailable"
        except MemoryError:
            row["available"] = False
            row["unavailable_code"] = "resource_exhausted"
        except Exception:
            row["available"] = False
            row["unavailable_code"] = "model_unavailable"
    return row


def capability_report(model_dir: Path, device: str = "cpu", dtype: str = "float32") -> dict[str, Any]:
    """Inventory report: one schema-valid capability row per locked operation.

    Unavailable models carry a typed `unavailable_code` so the Go executor
    keeps the row instead of dropping it as protocol-invalid.
    """
    catalog = load_catalog()
    capabilities: list[dict[str, Any]] = []
    for entry in catalog["models"]:
        base_code = _entry_unavailable(entry, model_dir, device, dtype)
        backend = entry.get("runtime", {}).get("backend", "unknown")
        for operation in entry.get("operations", []):
            code = base_code
            dimension = _positive_locked_dimension(entry.get("output", {}))
            if operation in EMBEDDING_OPERATIONS and dimension <= 0:
                code, dimension = "model_unavailable", 0
            capabilities.append(_capability_row(entry, operation, dimension, backend,
                                                code is None, code))
    return {"schema_version": "inference.capabilities.v1", "capabilities": capabilities}
