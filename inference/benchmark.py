"""Offline model benchmark; all timings are local and are not accuracy claims."""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import random
import resource
import select
import signal
import struct
import subprocess
import sys
import tempfile
import time
import wave
from pathlib import Path
from typing import Any, Mapping

from .adapters import (catalog_models, choose_model_dir, expected_dimension, make_adapter,
                       resolve_model)
from .adapters.base import AdapterContext
from .protocol import validate_result


# Kept as a constant so resource rejection is identical to the locked RTX 2070 gate.
CUDA_VRAM_LIMIT_MIB = 7168


def _csv_values(value: str, parser: Any, name: str) -> list[Any]:
    try:
        items = [parser(item.strip()) for item in value.split(",") if item.strip()]
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"invalid {name} list") from exc
    if not items:
        raise argparse.ArgumentTypeError(f"{name} cannot be empty")
    return items


def _catalog_entry(catalog_id: str) -> dict[str, Any]:
    for entry in catalog_models():
        if entry.get("catalog_id") == catalog_id:
            return entry
    raise ValueError(f"unknown locked catalog ID: {catalog_id}")


def _write_wav(path: Path, seconds: float, rate: int = 16000) -> None:
    frames = max(1, int(seconds * rate))
    with wave.open(str(path), "wb") as output:
        output.setnchannels(1)
        output.setsampwidth(2)
        output.setframerate(rate)
        block = bytearray()
        for index in range(frames):
            sample = int(0.12 * 32767 * math.sin(2 * math.pi * 220 * index / rate))
            block.extend(struct.pack("<h", sample))
        output.writeframes(bytes(block))


def _write_images(directory: Path, count: int) -> list[Path]:
    try:
        from PIL import Image
    except ImportError as exc:
        raise RuntimeError("Pillow is required to generate visual benchmark inputs") from exc
    paths: list[Path] = []
    for index in range(count):
        rng = random.Random(6100 + index)
        image = Image.new("RGB", (224, 224))
        image.putdata([(rng.randrange(256), rng.randrange(256), rng.randrange(256))
                       for _ in range(224 * 224)])
        path = directory / f"frame-{index}.jpg"
        image.save(path, format="JPEG", quality=90)
        paths.append(path)
    return paths


def _request(entry: Mapping[str, Any], operation: str, input_value: dict[str, Any],
             settings: dict[str, Any]) -> dict[str, Any]:
    identity = hashlib.sha256((entry["catalog_id"] + operation).encode()).hexdigest()
    return {"schema_version": "inference.v1", "operation_id": "benchmark-" + identity[:24],
            "operation": operation, "model_id": entry["model_id"],
            "model_revision": entry["revision"], "space_id": "benchmark-space",
            "input_hash": identity, "config_hash": hashlib.sha256(b"benchmark-config").hexdigest(),
            "timeout_ms": 3_600_000, "input": input_value, "settings": settings}


def _prepare_sample(entry: Mapping[str, Any], batch: int,
                    directory: Path) -> tuple[dict[str, Any], str]:
    family = entry["family"]
    operation = entry["operations"][0]
    if operation == "visual_embedding":
        frames = _write_images(directory, batch)
        video_path = directory / "visual-input.mp4"
        command = ["ffmpeg", "-nostdin", "-v", "error", "-framerate", "1", "-i",
                   str(directory / "frame-%d.jpg"), "-frames:v", str(batch), "-c:v", "mpeg4",
                   "-y", str(video_path)]
        completed = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, check=False, timeout=60)
        if completed.returncode != 0 or not video_path.is_file():
            raise RuntimeError("ffmpeg could not generate the visual benchmark clip")
        duration_ms = max(1000, batch * 1000)
        shot = {"shot_id": "benchmark-shot", "start_ms": 0, "end_ms": duration_ms,
                "timestamps_ms": [index * 1000 for index in range(batch)]}
        request = _request(entry, operation,
                           {"kind": "asset", "path": str(video_path.resolve()), "duration_ms": duration_ms,
                            "interval": {"start_ms": 0, "end_ms": duration_ms}},
                           {"visual": {"strategy": "shot_frame_pooled", "shot_set_hash": "benchmark-shotset",
                                       "shots": [shot], "truncate_to": 0}})
        return request, operation
    if operation == "audio_embedding":
        seconds = 10 + max(0, batch - 1) * 5
        audio_path = directory / "benchmark-audio.wav"
        _write_wav(audio_path, seconds, 48000)
        request = _request(entry, operation,
                           {"kind": "asset", "path": str(audio_path.resolve()), "duration_ms": seconds * 1000},
                           {"audio": {"window_ms": 10000, "hop_ms": 5000, "pad_tail": True}})
        return request, operation
    if operation == "speaker_embedding":
        duration_ms = batch * 3500 + 1000
        audio_path = directory / "benchmark-audio.wav"
        _write_wav(audio_path, duration_ms / 1000, 16000)
        segments = [{"start_ms": index * 3500, "end_ms": index * 3500 + 3000}
                    for index in range(batch)]
        request = _request(entry, operation,
                           {"kind": "asset", "path": str(audio_path.resolve()), "duration_ms": duration_ms},
                           {"speaker": {"segments": segments}})
        return request, operation
    if operation in {"sound_events", "diarization"}:
        duration_ms = max(3000, batch * 2000)
        audio_path = directory / "benchmark-audio.wav"
        _write_wav(audio_path, duration_ms / 1000, 16000)
        field = "sound_events" if operation == "sound_events" else "diarization"
        request = _request(entry, operation,
                           {"kind": "asset", "path": str(audio_path.resolve()), "duration_ms": duration_ms},
                           {field: {}})
        return request, operation
    raise ValueError(f"no benchmark input builder for operation {operation}")


def _command(model_dir: Path, device: str, dtype: str, *, oom_mib: int | None = None) -> list[str]:
    command = [sys.executable, "-m", "inference.dispatch", "--model-dir", str(model_dir),
               "--device", device, "--dtype", dtype]
    if oom_mib is not None:
        command.extend(["--oom-probe-mib", str(oom_mib)])
    return command


def _run_child(request: Mapping[str, Any], model_dir: Path, device: str,
               dtype: str) -> tuple[dict[str, Any] | None, float, float | None, str | None, int]:
    command = _command(model_dir, device, dtype)
    payload = json.dumps(request, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode() + b"\n"
    started = time.monotonic()
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, bufsize=0)
    assert process.stdin is not None and process.stdout is not None
    process.stdin.write(payload)
    process.stdin.close()
    first_result_ms: float | None = None
    load_started: float | None = None
    load_done: float | None = None
    frames: list[dict[str, Any]] = []
    raw_lines: list[bytes] = []
    while True:
        line = process.stdout.readline()
        if not line:
            break
        raw_lines.append(line)
        try:
            frame = json.loads(line)
        except json.JSONDecodeError:
            continue
        if frame.get("type") == "progress":
            if frame.get("stage") == "model_loading" and load_started is None:
                load_started = time.monotonic()
            elif frame.get("stage") == "model_loaded" and load_done is None:
                load_done = time.monotonic()
        elif frame.get("type") == "result":
            if first_result_ms is None:
                first_result_ms = (time.monotonic() - started) * 1000
            frames.append(frame)
        elif frame.get("type") == "error":
            frames.append(frame)
    stderr = process.stderr.read() if process.stderr else b""
    returncode = process.wait()
    # The Go-compatible result digest is validated by the protocol tests/integration owner.
    result_frame = next((item for item in frames if item.get("type") == "result"), None)
    error_frame = next((item for item in frames if item.get("type") == "error"), None)
    latency_ms = first_result_ms if first_result_ms is not None else (time.monotonic() - started) * 1000
    load_ms = None if load_started is None or load_done is None else (load_done - load_started) * 1000
    error_code = error_frame.get("code") if error_frame else None
    if returncode != 0 and not error_code and stderr:
        error_code = "process_exit"
    return result_frame, latency_ms, load_ms, error_code, returncode


def _warm_run(adapter: Any, request: Mapping[str, Any]) -> dict[str, Any]:
    payload = adapter.infer(request)
    return payload


def _cancellation_probe(request: Mapping[str, Any], model_dir: Path, device: str,
                        dtype: str) -> bool:
    command = _command(model_dir, device, dtype)
    encoded = json.dumps(request, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode() + b"\n"
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, bufsize=0)
    assert process.stdin is not None and process.stdout is not None
    process.stdin.write(encoded)
    process.stdin.close()
    deadline = time.monotonic() + 2.0
    saw_start = False
    while time.monotonic() < deadline:
        ready, _, _ = select.select([process.stdout], [], [], 0.1)
        if not ready:
            if process.poll() is not None:
                break
            continue
        line = process.stdout.readline()
        if not line:
            break
        try:
            frame = json.loads(line)
        except json.JSONDecodeError:
            continue
        if frame.get("type") == "progress":
            if frame.get("stage") == "model_loading":
                saw_start = True
                break
        elif frame.get("type") in {"result", "error"}:
            break
    if process.poll() is None:
        try:
            process.send_signal(signal.SIGTERM)
        except OSError:
            return False
    # communicate() flushes stdin if it is still attached; the request was
    # already written and stdin closed above, so detach it first instead of
    # letting subprocess raise ValueError on a closed file.
    process.stdin = None
    try:
        rest, stderr = process.communicate(timeout=5.0)
    except subprocess.TimeoutExpired:
        process.kill()
        process.communicate()
        return False
    lines = rest.splitlines()
    error_code = None
    for line in lines:
        try:
            frame = json.loads(line)
        except json.JSONDecodeError:
            continue
        if frame.get("type") == "error":
            error_code = frame.get("code")
    return saw_start and process.returncode == 0 and error_code == "cancelled"


def _oom_probe(model_dir: Path, device: str, dtype: str) -> str:
    # Deliberately above any practical host/device budget; dispatch's checked probe refuses it.
    _, _, _, code, _ = _run_probe_child(model_dir, device, dtype, 1_048_576)
    return code or "probe_failed"


def _run_probe_child(model_dir: Path, device: str, dtype: str,
                     mib: int) -> tuple[dict[str, Any] | None, float, float | None, str | None, int]:
    started = time.monotonic()
    process = subprocess.run(_command(model_dir, device, dtype, oom_mib=mib),
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
    code = None
    for line in process.stdout.splitlines():
        try:
            frame = json.loads(line)
        except json.JSONDecodeError:
            continue
        if frame.get("type") == "error":
            code = frame.get("code")
    return None, (time.monotonic() - started) * 1000, None, code, process.returncode


def _maxrss_mib() -> float:
    value = float(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
    return value / (1024.0 * 1024.0) if sys.platform == "darwin" else value / 1024.0


def _vram_metrics(device: str, reset: bool = False) -> tuple[float, float]:
    if device != "cuda":
        return 0.0, 0.0
    try:
        import torch
        if not torch.cuda.is_available():
            return 0.0, 0.0
        if reset:
            torch.cuda.reset_peak_memory_stats()
        return (torch.cuda.max_memory_allocated() / (1024 * 1024),
                torch.cuda.max_memory_reserved() / (1024 * 1024))
    except Exception:
        return 0.0, 0.0


def _verify_output(operation: str, payload: Mapping[str, Any], dimension: int) -> dict[str, Any]:
    validate_result(operation, payload, dimension)
    records = payload.get("vectors", [])
    if operation == "audio_embedding":
        records = payload.get("windows", [])
        if payload.get("query_vector") is not None:
            records = [payload["query_vector"]]
    return {"finite": True, "normalized": True, "dimension": dimension,
            "output_records": len(records) if isinstance(records, list) else 0}


def _profile(entry: Mapping[str, Any], model_dir: Path, device: str, dtype: str, batch: int,
             warmup: int, iters: int) -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="inference-benchmark-") as scratch:
        sample_dir = Path(scratch)
        request, operation = _prepare_sample(entry, batch, sample_dir)
        first, cold_start_ms, child_load_ms, first_error, returncode = _run_child(request, model_dir, device, dtype)
        dimension = expected_dimension(entry, request)
        if first is None:
            checks = {"finite": False, "normalized": False, "dimension": dimension,
                      "first_error": first_error or f"exit_{returncode}"}
            return {"schema_version": "inference.benchmark.v1", "model": entry["catalog_id"],
                    "device": device, "dtype": dtype, "batch": batch,
                    "cold_start_ms": cold_start_ms, "load_ms": child_load_ms,
                    "peak_vram_mib": 0.0, "peak_rss_mib": _maxrss_mib(),
                    "latency_ms": {"p50": None, "p95": None}, "checks": checks,
                    "cancelled": False, "oom_code": "probe_not_run", "status": "unavailable",
                    "rejected": False}
        # Construct a resident adapter for repeated warm iterations in this process.
        resolve = resolve_model(entry["model_id"], entry["revision"], operation)
        expected = expected_dimension(resolve, request)
        stages: dict[str, float] = {}
        def progress(_fraction: float, stage: str | None) -> None:
            if stage:
                stages.setdefault(stage, time.monotonic())
        cancelled = _cancellation_probe(request, model_dir, device, dtype)
        oom_code = _oom_probe(model_dir, device, dtype)
        context = AdapterContext(model_dir=choose_model_dir(model_dir, resolve), device=device,
                                 dtype=dtype, progress=progress, model=resolve,
                                 cancel_check=lambda: None)
        adapter = make_adapter(resolve, context)
        if device == "cuda":
            _vram_metrics(device, reset=True)
        # First direct call measures local model load and the first warmup operation.
        start = time.monotonic()
        payload = _warm_run(adapter, request)
        direct_first_ms = (time.monotonic() - start) * 1000
        direct_load_ms = ((stages["model_loaded"] - stages["model_loading"]) * 1000
                          if "model_loaded" in stages and "model_loading" in stages else child_load_ms)
        checks = _verify_output(operation, payload, expected)
        for _ in range(max(0, warmup - 1)):
            _verify_output(operation, _warm_run(adapter, request), expected)
        latencies: list[float] = []
        for _ in range(iters):
            start = time.monotonic()
            measured = _warm_run(adapter, request)
            latencies.append((time.monotonic() - start) * 1000)
            _verify_output(operation, measured, expected)
        sorted_latency = sorted(latencies)
        def nearest_rank(percentile: float) -> float:
            return sorted_latency[max(0, math.ceil(percentile * len(sorted_latency)) - 1)]
        peak_allocated, peak_reserved = _vram_metrics(device)
        peak_vram = max(peak_allocated, peak_reserved)
        rejected = device == "cuda" and peak_vram > CUDA_VRAM_LIMIT_MIB
        checks.update({"first_process_result_ms": cold_start_ms,
                       "resident_first_result_ms": direct_first_ms,
                       "vram_allocated_mib": peak_allocated,
                       "vram_reserved_mib": peak_reserved,
                       "cancellation_probe": cancelled,
                       "oom_probe": oom_code == "resource_exhausted"})
        if rejected:
            checks["rejection_reason"] = "peak VRAM exceeded 7168 MiB"
        return {"schema_version": "inference.benchmark.v1", "model": entry["catalog_id"],
                "device": device, "dtype": dtype, "batch": batch,
                "cold_start_ms": cold_start_ms, "load_ms": direct_load_ms,
                "peak_vram_mib": peak_vram, "peak_rss_mib": _maxrss_mib(),
                "latency_ms": {"p50": nearest_rank(0.50), "p95": nearest_rank(0.95)},
                "checks": checks, "cancelled": cancelled, "oom_code": oom_code,
                "status": "rejected" if rejected else "complete", "rejected": rejected}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Measure offline inference process and warm latency")
    parser.add_argument("--model", required=True, help="locked catalog_id")
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cpu")
    parser.add_argument("--dtypes", default="float32")
    parser.add_argument("--batch", default="1,2")
    parser.add_argument("--json", required=True, type=Path)
    parser.add_argument("--warmup", type=int, default=1)
    parser.add_argument("--iters", type=int, default=3)
    parser.add_argument("--model-dir", type=Path, default=Path("models"))
    args = parser.parse_args(argv)
    if args.warmup < 0 or args.iters < 1:
        parser.error("--warmup must be nonnegative and --iters must be positive")
    try:
        dtypes = _csv_values(args.dtypes, str, "dtype")
        batches = _csv_values(args.batch, int, "batch")
        if any(dtype not in {"float32", "float16"} for dtype in dtypes):
            raise ValueError("dtypes must be float32 or float16")
        if any(batch < 1 for batch in batches):
            raise ValueError("batch values must be positive")
        entry = _catalog_entry(args.model)
        model_dir = args.model_dir.resolve()
        profiles = [_profile(entry, model_dir, args.device, dtype, batch, args.warmup, args.iters)
                    for dtype in dtypes for batch in batches]
    except Exception as exc:
        parser.error(str(exc))
    output = profiles[0] if len(profiles) == 1 else {
        "schema_version": "inference.benchmark.v1", "model": args.model,
        "device": args.device, "profiles": profiles}
    args.json.parent.mkdir(parents=True, exist_ok=True)
    args.json.write_text(json.dumps(output, ensure_ascii=False, indent=2, allow_nan=False) + "\n",
                         encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
