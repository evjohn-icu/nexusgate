"""One-request, local-only inference process entry point."""
from __future__ import annotations

import argparse
import base64
import json
import os
import signal
import struct
import sys
import threading
import time
from pathlib import Path
from typing import Any, Mapping

from .adapters import (capability_report, choose_model_dir, expected_dimension, make_adapter,
                       resolve_model)
from .adapters.base import (AdapterContext, Cancelled, DeadlineExceeded, InferenceFailure,
                            ModelUnavailable, ResourceExhausted, UnsupportedInput,
                            UnsupportedOperation)
from .protocol import (MAX_FRAME_BYTES, ProtocolError, read_request, result_frames,
                       validate_result, write_error, write_frame, write_progress)


class _StdoutRedirect:
    """Keep protocol bytes on the original stdout and redirect accidental prints to stderr."""
    def __init__(self, binary: Any) -> None:
        self.buffer = binary

    def write(self, value: str) -> int:
        return sys.stderr.write(value)

    def flush(self) -> None:
        sys.stderr.flush()

    def isatty(self) -> bool:
        return False


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run one local inference.v1 operation")
    parser.add_argument("--model-dir", required=True, type=Path)
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cpu")
    parser.add_argument("--dtype", choices=("float32", "float16"), default="float32")
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--once", action="store_true", help="read one request line (default)")
    modes.add_argument("--probe", action="store_true", help="emit locked model capabilities JSON")
    parser.add_argument("--oom-probe-mib", type=int, help=argparse.SUPPRESS)
    return parser


def _domain_result(request: Mapping[str, Any], entry: Mapping[str, Any],
                   payload: Mapping[str, Any]) -> dict[str, Any]:
    operation = request["operation"]
    source = request["input"]
    settings = request["settings"]
    backend = entry.get("runtime", {}).get("backend", "unknown")
    result: dict[str, Any] = {
        "schema_version": "inference.v1",
        "operation_id": request["operation_id"],
        "operation": operation,
        "model_id": request["model_id"],
        "model_revision": request["model_revision"],
        "input_hash": request["input_hash"],
        "config_hash": request["config_hash"],
        "backend": backend,
    }
    if request.get("space_id"):
        result["space_id"] = request["space_id"]

    if operation == "visual_embedding":
        result["coverage"] = ([{"start_ms": shot["start_ms"], "end_ms": shot["end_ms"]}
                               for shot in settings["visual"]["shots"]]
                              if source["kind"] == "asset" else [])
        result["embeddings"] = [_embedding_record(record, operation, settings, index,
                                                   request["input_hash"])
                                for index, record in enumerate(payload["vectors"])]
    elif operation == "speaker_embedding":
        records = payload["vectors"]
        result["coverage"] = [{"start_ms": record["start_ms"], "end_ms": record["end_ms"]}
                               for record in records]
        result["embeddings"] = [_embedding_record(record, operation, settings, index,
                                                   request["input_hash"])
                                for index, record in enumerate(records)]
    elif operation == "audio_embedding":
        windows = list(payload.get("windows", []))
        query_vector = payload.get("query_vector")
        if query_vector is not None:
            result["coverage"] = []
            windows.append({"sample_index": -1, **query_vector})
        else:
            result["coverage"] = ([{"start_ms": window["start_ms"], "end_ms": window["end_ms"]}
                                   for window in windows])
        result["embeddings"] = [_embedding_record(record, operation, settings, index,
                                                   request["input_hash"])
                                for index, record in enumerate(windows)]
    elif operation == "sound_events":
        source_interval = source.get("interval") or {"start_ms": 0, "end_ms": source["duration_ms"]}
        result["coverage"] = [{"start_ms": source_interval["start_ms"],
                               "end_ms": source_interval["end_ms"]}]
        result["events"] = [dict(event) for event in payload["events"]]
    elif operation == "diarization":
        source_interval = source.get("interval") or {"start_ms": 0, "end_ms": source["duration_ms"]}
        result["coverage"] = [{"start_ms": source_interval["start_ms"],
                               "end_ms": source_interval["end_ms"]}]
        result["turns"] = [dict(turn) for turn in payload["turns"]]
    return result


def _embedding_record(record: Mapping[str, Any], operation: str,
                      settings: Mapping[str, Any], index: int,
                      input_hash: str) -> dict[str, Any]:
    try:
        raw = base64.b64decode(record["floats_b64"], validate=True)
        dimension = int(record["dimension"])
        if len(raw) != 4 * dimension:
            raise ValueError("vector byte count mismatch")
        vector = [value[0] for value in struct.iter_unpack("<f", raw)]
    except (KeyError, TypeError, ValueError, base64.binascii.Error, struct.error) as exc:
        raise InferenceFailure("validated vector could not be serialized") from exc
    result: dict[str, Any] = {"sample_index": record.get("sample_index", index), "vector": vector}
    for key in ("shot_id", "timestamp_ms", "start_ms", "end_ms"):
        if key in record:
            result[key] = record[key]
    if operation == "visual_embedding":
        result["strategy"] = settings["visual"]["strategy"]
        result["source_hash"] = settings["visual"]["shot_set_hash"] or input_hash
    return result




def _run_one(model_dir: Path, device: str, dtype: str, protocol_output: Any) -> int:
    operation_id = ""
    cancel_event = threading.Event()
    deadline_event = threading.Event()
    deadline_timer: threading.Timer | None = None
    started = time.monotonic()
    request: dict[str, Any] | None = None
    try:
        request = read_request(sys.stdin.buffer)
        operation_id = request["operation_id"]
        entry = resolve_model(request["model_id"], request["model_revision"], request["operation"])
        allowed_dtypes = entry.get("runtime", {}).get("dtypes", ["float32"])
        if dtype not in allowed_dtypes:
            raise UnsupportedInput("requested dtype is not supported by the locked model")
        allowed_backends = load_backends(entry)
        if device not in allowed_backends:
            raise UnsupportedInput("requested device is not supported by the locked model")
        root = model_dir.resolve()
        chosen = choose_model_dir(root, entry)
        deadline_timer = threading.Timer(request["timeout_ms"] / 1000.0, deadline_event.set)
        deadline_timer.daemon = True
        deadline_timer.start()

        def check_cancelled() -> None:
            if deadline_event.is_set() or time.monotonic() - started >= request["timeout_ms"] / 1000.0:
                raise DeadlineExceeded("inference deadline exceeded")
            if cancel_event.is_set():
                raise Cancelled("inference cancelled")

        def progress(fraction: float, stage: str | None = None) -> None:
            check_cancelled()
            write_frame({"type": "progress", "operation_id": operation_id,
                         "fraction": max(0.0, min(1.0, float(fraction))),
                         **({"stage": stage} if stage else {})}, protocol_output)

        previous_handler: Any = None
        if hasattr(signal, "SIGTERM"):
            previous_handler = signal.getsignal(signal.SIGTERM)
            def on_terminate(_signum: int, _frame: Any) -> None:
                cancel_event.set()
            signal.signal(signal.SIGTERM, on_terminate)
        try:
            progress(0.0, "starting")
            expected = expected_dimension(entry, request)
            if expected <= 0:
                raise ModelUnavailable("locked model has no usable output dimension")
            context = AdapterContext(model_dir=chosen, device=device, dtype=dtype,
                                     progress=progress, model=entry, cancel_check=check_cancelled)
            adapter = make_adapter(entry, context)
            payload = adapter.infer(request)
            check_cancelled()
            validate_result(request["operation"], payload, expected)
            domain_payload = _domain_result(request, entry, payload)
            check_cancelled()
            frames = result_frames(operation_id, domain_payload)
            for frame in frames:
                write_frame(frame, protocol_output)
            return 0
        finally:
            if previous_handler is not None:
                signal.signal(signal.SIGTERM, previous_handler)
    except BaseException as exc:
        if isinstance(exc, KeyboardInterrupt):
            exc = Cancelled("inference cancelled")
        if isinstance(exc, InferenceFailure):
            code = exc.code
            message = str(exc)
        elif isinstance(exc, MemoryError) or "outofmemory" in type(exc).__name__.lower():
            code, message = "resource_exhausted", "inference exhausted available memory"
        elif isinstance(exc, TimeoutError):
            code, message = "deadline_exceeded", "inference deadline exceeded"
        else:
            code, message = "model_unavailable", "local model inference failed"
        try:
            write_error(operation_id, code, message)
        except BaseException:
            # A broken pipe means the caller already left; never print non-protocol output.
            return 0
        return 0
    finally:
        if deadline_timer is not None:
            deadline_timer.cancel()


def load_backends(entry: Mapping[str, Any]) -> list[str]:
    runtime = entry.get("runtime", {})
    backends = runtime.get("backends")
    if isinstance(backends, list):
        return [str(item) for item in backends]
    return ["cpu", "cuda"]


def _oom_probe(mib: int, output: Any) -> int:
    if mib <= 0:
        write_error("benchmark-oom-probe", "unsupported_input", "probe size must be positive")
    else:
        write_error("benchmark-oom-probe", "resource_exhausted",
                    f"explicit oversized allocation request ({mib} MiB) rejected by the memory guard")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    real_stdout = sys.stdout
    protocol_output = real_stdout.buffer
    sys.stdout = _StdoutRedirect(protocol_output)  # type: ignore[assignment]
    if args.oom_probe_mib is not None:
        return _oom_probe(args.oom_probe_mib, protocol_output)
    if args.probe:
        report = capability_report(args.model_dir)
        # The Go executor dispatches on the frame type; a bare capabilities
        # object would be rejected as an unknown frame.
        report["type"] = "capabilities"
        protocol_output.write(json.dumps(report, ensure_ascii=False, sort_keys=True,
                                         separators=(",", ":")).encode("utf-8") + b"\n")
        protocol_output.flush()
        return 0
    return _run_one(args.model_dir, args.device, args.dtype, protocol_output)


if __name__ == "__main__":
    raise SystemExit(main())
