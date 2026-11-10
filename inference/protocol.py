"""Bounded JSON-lines protocol, request validation, and result framing."""
from __future__ import annotations

import base64
import hashlib
import json
import math
import os
import re
import struct
import sys
from pathlib import Path
from typing import Any, BinaryIO, Mapping

from .adapters.base import InvalidOutput, UnsupportedInput, UnsupportedOperation

MAX_FRAME_BYTES = 32 * 1024 * 1024
MAX_TEXT_BYTES = 16 * 1024
MAX_IMAGE_BYTES = 8 * 1024 * 1024
MAX_IMAGE_PIXELS = 20_000_000
OPERATIONS = {
    "visual_embedding": "visual",
    "sound_events": "sound_events",
    "audio_embedding": "audio",
    "diarization": "diarization",
    "speaker_embedding": "speaker",
}
RESULT_KINDS = {
    "visual_embedding": "visual_embedding",
    "sound_events": "sound_events",
    "audio_embedding": "audio_embedding",
    "diarization": "diarization",
    "speaker_embedding": "speaker_embedding",
}
_HASH = re.compile(r"^[A-Za-z0-9._:/+-]{1,256}$")


class ProtocolError(UnsupportedInput):
    pass


def canonical_json(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"),
                      allow_nan=False).encode("utf-8")


def read_request(stream: BinaryIO) -> dict[str, Any]:
    raw = stream.readline(MAX_FRAME_BYTES + 2)
    if not raw:
        raise ProtocolError("missing request line")
    if len(raw) > MAX_FRAME_BYTES + 1 or not raw.endswith(b"\n"):
        raise ProtocolError("request line exceeds the frame limit or is unterminated")
    if len(raw) - 1 > MAX_FRAME_BYTES:
        raise ProtocolError("request line exceeds the frame limit")
    if stream.read(1):
        raise ProtocolError("stdin must contain exactly one request line")
    try:
        value = json.loads(raw[:-1].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProtocolError("request line is not valid UTF-8 JSON") from exc
    if not isinstance(value, dict):
        raise ProtocolError("request must be a JSON object")
    return validate_request(value)


def validate_request(request: Mapping[str, Any]) -> dict[str, Any]:
    required = {"schema_version", "operation_id", "operation", "model_id", "model_revision",
                "space_id", "input_hash", "config_hash", "timeout_ms", "input", "settings"}
    if set(request) != required:
        raise ProtocolError("request fields do not match inference.v1")
    if request["schema_version"] != "inference.v1":
        raise ProtocolError("unsupported request schema version")
    operation = request["operation"]
    if not isinstance(operation, str) or operation not in OPERATIONS:
        raise UnsupportedOperation("unknown inference operation")
    for name in ("operation_id", "model_id", "model_revision", "input_hash", "config_hash"):
        value = request[name]
        if not isinstance(value, str) or not value or len(value.encode("utf-8")) > 512:
            raise ProtocolError(f"invalid {name}")
    space_id = request["space_id"]
    needs_space = operation in {"visual_embedding", "audio_embedding", "speaker_embedding"}
    if not isinstance(space_id, str) or len(space_id.encode("utf-8")) > 512 or (needs_space and not space_id):
        raise ProtocolError("invalid space_id")
    if not _HASH.fullmatch(request["operation_id"]):
        raise ProtocolError("invalid operation_id")
    timeout = request["timeout_ms"]
    if isinstance(timeout, bool) or not isinstance(timeout, int) or not 1 <= timeout <= 3_600_000:
        raise ProtocolError("timeout_ms must be an integer from 1 through 3600000")
    validate_input(request["input"], operation)
    settings = request["settings"]
    field = OPERATIONS[operation]
    if not isinstance(settings, dict) or set(settings) != {field} or not isinstance(settings[field], dict):
        raise ProtocolError("settings must contain exactly the operation's typed settings field")
    validate_settings(operation, settings[field], request["input"])
    return dict(request)


def _integer(value: Any, name: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise ProtocolError(f"{name} must be an integer greater than or equal to {minimum}")
    return value


def _local_path(value: Any, name: str) -> Path:
    if not isinstance(value, str) or not value or "\x00" in value:
        raise ProtocolError(f"{name} must be a local path")
    path = Path(value)
    if not path.is_absolute():
        raise ProtocolError(f"{name} must be absolute")
    try:
        resolved = path.resolve(strict=True)
        if not resolved.is_file():
            raise ProtocolError(f"{name} is not a regular file")
    except OSError as exc:
        raise ProtocolError(f"{name} is unavailable") from exc
    return resolved


def validate_input(value: Any, operation: str) -> None:
    if not isinstance(value, dict) or not isinstance(value.get("kind"), str) or value.get("kind") not in {"asset", "image", "text"}:
        raise ProtocolError("unknown or invalid input kind")
    kind = value["kind"]
    if kind == "asset":
        if set(value) - {"kind", "path", "duration_ms", "interval"} or not {"kind", "path", "duration_ms"} <= set(value):
            raise ProtocolError("asset input fields are invalid")
        _local_path(value["path"], "input.path")
        duration = _integer(value["duration_ms"], "input.duration_ms", 1)
        interval = value.get("interval")
        if interval is not None:
            if not isinstance(interval, dict) or set(interval) != {"start_ms", "end_ms"}:
                raise ProtocolError("asset interval fields are invalid")
            start = _integer(interval["start_ms"], "interval.start_ms")
            end = _integer(interval["end_ms"], "interval.end_ms")
            if end <= start or end > duration:
                raise ProtocolError("asset interval is reversed or outside the asset")
        if operation in {"visual_embedding", "speaker_embedding", "diarization", "sound_events", "audio_embedding"}:
            return
    elif kind == "image":
        if set(value) != {"kind", "path", "digest"}:
            raise ProtocolError("image input fields are invalid")
        if operation != "visual_embedding":
            raise ProtocolError("images are supported only for visual embeddings")
        path = _local_path(value["path"], "input.path")
        if not isinstance(value["digest"], str) or not value["digest"]:
            raise ProtocolError("image digest is required")
        _check_image(path)
    else:
        if set(value) != {"kind", "text"}:
            raise ProtocolError("text input fields are invalid")
        if operation not in {"visual_embedding", "audio_embedding"}:
            raise ProtocolError("text input is not supported for this operation")
        if not isinstance(value["text"], str) or not value["text"].strip():
            raise ProtocolError("input text must be non-empty")
        if len(value["text"].encode("utf-8")) > MAX_TEXT_BYTES:
            raise ProtocolError("input text exceeds 16 KiB")


def _check_image(path: Path) -> None:
    try:
        if path.stat().st_size > MAX_IMAGE_BYTES:
            raise ProtocolError("image input exceeds 8 MiB")
    except ProtocolError:
        raise
    except OSError as exc:
        raise ProtocolError("image input is unavailable") from exc
    try:
        from PIL import Image
        with Image.open(path) as image:
            if image.format not in {"JPEG", "PNG"}:
                raise ProtocolError("image input must be JPEG or PNG")
            width, height = image.size
            if width < 1 or height < 1 or width * height > MAX_IMAGE_PIXELS:
                raise ProtocolError("image exceeds the 20 megapixel limit")
            image.verify()
    except ProtocolError:
        raise
    except Exception as exc:
        raise ProtocolError("image input cannot be decoded") from exc


def validate_settings(operation: str, settings: Mapping[str, Any], input_value: Mapping[str, Any]) -> None:
    if operation == "visual_embedding":
        allowed = {"strategy", "shot_set_hash", "shots", "truncate_to"}
        if set(settings) != allowed:
            raise ProtocolError("visual settings must contain strategy, shot_set_hash, shots, and truncate_to")
        strategy = settings["strategy"]
        if not isinstance(strategy, str) or strategy not in {"shot_frame_pooled", "native_video"}:
            raise ProtocolError("unknown visual embedding strategy")
        shot_set_hash = settings["shot_set_hash"]
        if not isinstance(shot_set_hash, str) or len(shot_set_hash.encode("utf-8")) > 512:
            raise ProtocolError("invalid shot_set_hash")
        truncate = settings["truncate_to"]
        _integer(truncate, "truncate_to")
        shots = settings["shots"]
        if not isinstance(shots, list):
            raise ProtocolError("visual shots must be an array")
        source_kind = input_value["kind"]
        if source_kind == "asset" and not shots:
            raise ProtocolError("asset visual embeddings require at least one shot")
        if source_kind != "asset" and shots:
            raise ProtocolError("image/text queries cannot include shot records")
        if source_kind != "asset" and strategy != "shot_frame_pooled":
            raise ProtocolError("native_video requires an asset input")
        if strategy == "native_video" and len(shots) != 1:
            raise ProtocolError("native_video requires exactly one shot interval")
        input_interval = input_value.get("interval") or {"start_ms": 0, "end_ms": input_value.get("duration_ms", 0)}
        seen: set[str] = set()
        for shot in shots:
            if not isinstance(shot, dict) or set(shot) != {"shot_id", "start_ms", "end_ms", "timestamps_ms"}:
                raise ProtocolError("visual shot fields are invalid")
            shot_id = shot["shot_id"]
            if not isinstance(shot_id, str) or not shot_id or shot_id in seen:
                raise ProtocolError("visual shot IDs must be non-empty and unique")
            seen.add(shot_id)
            start = _integer(shot["start_ms"], "shot.start_ms")
            end = _integer(shot["end_ms"], "shot.end_ms")
            if end <= start or start < input_interval["start_ms"] or end > input_interval["end_ms"]:
                raise ProtocolError("shot interval is reversed or outside the asset interval")
            timestamps = shot["timestamps_ms"]
            if not isinstance(timestamps, list):
                raise ProtocolError("timestamps_ms must be an array")
            if strategy == "shot_frame_pooled" and not timestamps:
                raise ProtocolError("shot_frame_pooled requires at least one frame timestamp")
            for timestamp in timestamps:
                timestamp = _integer(timestamp, "timestamp_ms")
                if not start <= timestamp < end:
                    raise ProtocolError("frame timestamp is outside its shot interval")
    elif operation == "audio_embedding":
        if set(settings) - {"window_ms", "hop_ms", "pad_tail"}:
            raise ProtocolError("unknown audio settings field")
        if settings.get("window_ms", 10000) != 10000 or settings.get("hop_ms", 5000) != 5000:
            raise ProtocolError("CLAP windows are fixed at 10000 ms with a 5000 ms hop")
        if settings.get("pad_tail", True) is not True:
            raise ProtocolError("CLAP short tails must be explicitly zero-padded")
    elif operation == "speaker_embedding":
        if set(settings) - {"segments"}:
            raise ProtocolError("unknown speaker embedding settings field")
        segments = settings.get("segments")
        if not isinstance(segments, list) or not segments:
            raise ProtocolError("speaker embedding requires clean-speech segments")
        duration = input_value.get("duration_ms")
        previous_end = -1
        for segment in segments:
            if not isinstance(segment, dict) or set(segment) != {"start_ms", "end_ms"}:
                raise ProtocolError("speaker segment fields are invalid")
            start = _integer(segment["start_ms"], "segment.start_ms")
            end = _integer(segment["end_ms"], "segment.end_ms")
            if end <= start or end - start < 3000 or end > duration or start < previous_end:
                raise ProtocolError("speaker segments must be non-overlapping 3000 ms regions within the asset")
            previous_end = end
    elif settings:
        raise ProtocolError(f"{operation} settings must be an empty object")


def validate_result(operation: str, payload: Any, expected_dimension: int) -> dict[str, Any]:
    if not isinstance(payload, dict) or payload.get("kind") != RESULT_KINDS.get(operation):
        raise InvalidOutput("adapter returned the wrong result kind")
    if operation in {"visual_embedding", "speaker_embedding"}:
        records = payload.get("vectors")
        if not isinstance(records, list) or not records:
            raise InvalidOutput("embedding result contains no vectors")
        for record in records:
            _validate_vector_record(record, expected_dimension)
            if operation == "visual_embedding":
                if not isinstance(record.get("shot_id"), str):
                    raise InvalidOutput("visual vector is missing shot_id")
                _validate_interval(record, required={"timestamp_ms", "start_ms", "end_ms"}, allow_empty=True)
            else:
                _validate_interval(record, required={"start_ms", "end_ms"})
                if not isinstance(record.get("quality"), dict):
                    raise InvalidOutput("speaker vector is missing quality metrics")
    elif operation == "audio_embedding":
        windows = payload.get("windows")
        if not isinstance(windows, list):
            raise InvalidOutput("audio embedding result is missing windows")
        for record in windows:
            _validate_vector_record(record, expected_dimension)
            _validate_interval(record, required={"start_ms", "end_ms"})
        query_vector = payload.get("query_vector")
        if query_vector is not None:
            _validate_vector_record(query_vector, expected_dimension)
    elif operation == "sound_events":
        events = payload.get("events")
        if not isinstance(events, list):
            raise InvalidOutput("sound event result is missing events")
        for event in events:
            _validate_interval(event, required={"start_ms", "end_ms"})
            if not isinstance(event.get("label"), str) or not event["label"]:
                raise InvalidOutput("sound event label is empty")
            score = event.get("raw_score")
            if isinstance(score, bool) or not isinstance(score, (int, float)) or not math.isfinite(score):
                raise InvalidOutput("sound event score is not finite")
            if event.get("confidence") is not None:
                raise InvalidOutput("uncalibrated sound event confidence must be null")
    elif operation == "diarization":
        turns = payload.get("turns")
        if not isinstance(turns, list):
            raise InvalidOutput("diarization result is missing turns")
        for turn in turns:
            _validate_interval(turn, required={"start_ms", "end_ms"})
            if turn.get("local_speaker_id") is not None and not isinstance(turn["local_speaker_id"], str):
                raise InvalidOutput("local speaker ID must be a string or null")
            if turn.get("confidence") is not None and not _finite_number(turn["confidence"]):
                raise InvalidOutput("speaker confidence must be finite or null")
            if not isinstance(turn.get("overlap"), bool):
                raise InvalidOutput("turn overlap must be boolean")
    try:
        canonical_json(payload)
    except (TypeError, ValueError) as exc:
        raise InvalidOutput("result payload is not JSON serializable") from exc
    return payload


def _validate_vector_record(record: Any, dimension: int) -> None:
    if not isinstance(record, dict) or record.get("dtype") != "float32":
        raise InvalidOutput("vector record dtype must be float32")
    if record.get("dimension") != dimension:
        raise InvalidOutput("vector dimension does not match the locked model dimension")
    encoded = record.get("floats_b64")
    if not isinstance(encoded, str):
        raise InvalidOutput("vector bytes are missing")
    try:
        raw = base64.b64decode(encoded, validate=True)
    except (ValueError, base64.binascii.Error) as exc:
        raise InvalidOutput("vector base64 is invalid") from exc
    if len(raw) != 4 * dimension:
        raise InvalidOutput("vector byte count does not match the locked dimension")
    values = struct.unpack("<" + "f" * dimension, raw)
    if not all(math.isfinite(value) for value in values):
        raise InvalidOutput("vector contains a non-finite value")
    norm = math.sqrt(sum(value * value for value in values))
    if not math.isfinite(norm) or abs(norm - 1.0) > 1e-3:
        raise InvalidOutput("vector is not L2-normalized")


def _finite_number(value: Any) -> bool:
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)


def _validate_interval(record: Any, *, required: set[str], allow_empty: bool = False) -> None:
    if not isinstance(record, dict) or not required <= set(record):
        raise InvalidOutput("result interval fields are missing")
    for key in required:
        value = record[key]
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise InvalidOutput("result interval contains a negative or non-integer value")
    if "start_ms" in record and "end_ms" in record:
        if record["end_ms"] < record["start_ms"] or (not allow_empty and record["end_ms"] == record["start_ms"]):
            raise InvalidOutput("result interval is reversed or empty")


def write_frame(frame: Mapping[str, Any], stream: BinaryIO | None = None) -> None:
    output = stream or sys.stdout.buffer
    try:
        raw = canonical_json(dict(frame)) + b"\n"
    except (TypeError, ValueError) as exc:
        raise InvalidOutput("protocol frame is not JSON serializable") from exc
    if len(raw) > MAX_FRAME_BYTES:
        raise InvalidOutput("protocol frame exceeds 32 MiB")
    output.write(raw)
    output.flush()


def write_progress(operation_id: str, fraction: float, stage: str | None = None) -> None:
    frame: dict[str, Any] = {"type": "progress", "operation_id": operation_id,
                             "fraction": max(0.0, min(1.0, float(fraction)))}
    if stage:
        frame["stage"] = stage
    write_frame(frame)


def result_frames(operation_id: str, payload: Mapping[str, Any]) -> list[dict[str, Any]]:
    complete = dict(payload)
    base = {key: value for key, value in complete.items() if not isinstance(value, list)}
    arrays = {key: value for key, value in complete.items() if isinstance(value, list)}
    single_result = complete
    single = {"type": "result", "operation_id": operation_id, "sequence": 0, "count": 1,
              "final": True, "result": single_result}
    try:
        if len(canonical_json(single)) + 1 <= MAX_FRAME_BYTES:
            digest = "sha256:" + hashlib.sha256(canonical_json(single_result)).hexdigest()
            single["digest"] = digest
            return [single]
    except (TypeError, ValueError) as exc:
        raise InvalidOutput("result is not JSON serializable") from exc
    if not arrays:
        raise InvalidOutput("non-list result fields exceed the frame limit")
    chunk_budget = 10 * 1024 * 1024 if len(arrays) > 1 else 27 * 1024 * 1024
    chunks: dict[str, list[list[Any]]] = {}
    for key, records in arrays.items():
        pieces: list[list[Any]] = []
        current: list[Any] = []
        size = 2
        for record in records:
            try:
                encoded_size = len(canonical_json(record)) + (1 if current else 0)
            except (TypeError, ValueError) as exc:
                raise InvalidOutput("result record is not JSON serializable") from exc
            if encoded_size > chunk_budget:
                raise InvalidOutput("one result record exceeds the frame budget")
            if current and size + encoded_size > chunk_budget:
                pieces.append(current)
                current, size = [], 2
            current.append(record)
            size += encoded_size
        if current or not pieces:
            pieces.append(current)
        chunks[key] = pieces
    count = max(len(parts) for parts in chunks.values())
    partial_results: list[dict[str, Any]] = []
    for sequence in range(count):
        result = dict(base)
        for key, pieces in chunks.items():
            if sequence < len(pieces):
                result[key] = pieces[sequence]
        partial_results.append(result)
    digest_source = b"".join(canonical_json(result) for result in partial_results)
    digest = "sha256:" + hashlib.sha256(digest_source).hexdigest()
    frames: list[dict[str, Any]] = []
    for sequence, result in enumerate(partial_results):
        frame = {"type": "result", "operation_id": operation_id, "sequence": sequence,
                 "count": count, "final": sequence == count - 1, "digest": digest, "result": result}
        if len(canonical_json(frame)) + 1 > MAX_FRAME_BYTES:
            raise InvalidOutput("result frame exceeds 32 MiB after framing")
        frames.append(frame)
    return frames


def write_error(operation_id: str, code: str, message: str) -> None:
    allowed = {"model_unavailable", "unsupported_operation", "unsupported_input", "invalid_output",
               "resource_exhausted", "deadline_exceeded", "cancelled"}
    if code not in allowed:
        code = "model_unavailable"
    text = " ".join(str(message).split())[:2048] or "inference failed"
    write_frame({"type": "error", "operation_id": operation_id, "code": code,
                 "message": text, "retryable": code in {"model_unavailable", "resource_exhausted",
                                                            "deadline_exceeded"}})
