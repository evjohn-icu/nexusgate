"""YAMNet 16 kHz inference and versioned 100 ms event postprocessing."""
from __future__ import annotations

import math
from typing import Any, Mapping

from .base import (AdapterContext, InvalidOutput, ModelUnavailable, UnsupportedInput,
                   decode_audio, interval_from_input, slice_audio)


class YamnetAdapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._model: Any | None = None
        self._tf: Any | None = None

    def _load(self) -> tuple[Any, Any]:
        if self._model is None:
            self.context.stage(0.02, "model_loading")
            try:
                from .base import checked_model_path, import_dependency
                tf = import_dependency("tensorflow", "TensorFlow")
                model_path = checked_model_path(self.context)
                model = tf.saved_model.load(str(model_path))
            except ModelUnavailable:
                raise
            except Exception as exc:
                raise ModelUnavailable("locked YAMNet SavedModel could not be loaded locally") from exc
            self._tf, self._model = tf, model
            self.context.stage(0.18, "model_loaded")
        return self._tf, self._model

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        if request["input"]["kind"] != "asset":
            raise UnsupportedInput("YAMNet requires an audio asset")
        np = __import__("numpy")
        tf, model = self._load()
        source = request["input"]
        audio, rate = decode_audio(source["path"], target_rate=16000)
        decoded_ms = math.floor(len(audio) * 1000 / rate)
        asset_start, asset_end = interval_from_input(request, decoded_ms)
        audio = slice_audio(audio, rate, asset_start, asset_end)
        try:
            output = model(tf.convert_to_tensor(audio, dtype=tf.float32))
            if isinstance(output, Mapping):
                scores = output.get("scores")
            elif isinstance(output, (tuple, list)):
                scores = output[0]
            else:
                scores = output
            scores = np.asarray(scores, dtype=np.float32)
        except Exception as exc:
            if "outofmemory" in type(exc).__name__.lower() or "resourceexhausted" in type(exc).__name__.lower():
                from .base import ResourceExhausted
                raise ResourceExhausted("YAMNet exhausted CPU memory") from exc
            raise ModelUnavailable("YAMNet inference failed") from exc
        if scores.ndim != 2 or scores.shape[1] != 521 or not np.isfinite(scores).all():
            raise InvalidOutput("YAMNet must return finite per-frame scores for 521 classes")
        duration_ms = asset_end - asset_start
        cells = self._cells(scores, duration_ms, np)
        smoothed = self._smooth(cells, np)
        events = self._events(smoothed, duration_ms, asset_start)
        coverage = [{"start_ms": asset_start, "end_ms": asset_end}]
        self.context.progress(0.95, "postprocessing_complete")
        return {"kind": "sound_events", "events": events, "coverage": coverage}

    @staticmethod
    def _cells(scores: Any, duration_ms: int, np: Any) -> Any:
        cell_count = math.ceil(duration_ms / 100)
        values = np.zeros((cell_count, 521), dtype=np.float32)
        patch_count = scores.shape[0]
        for cell_index in range(cell_count):
            cell_start, cell_end = cell_index * 100, min(duration_ms, (cell_index + 1) * 100)
            first = max(0, (cell_start - 960) // 480 + 1)
            last = min(patch_count - 1, cell_end // 480)
            total = 0.0
            weighted = np.zeros(521, dtype=np.float64)
            for patch_index in range(first, last + 1):
                patch_start = patch_index * 480
                patch_end = min(duration_ms, patch_start + 960)
                overlap = max(0, min(cell_end, patch_end) - max(cell_start, patch_start))
                if overlap:
                    weighted += scores[patch_index].astype(np.float64) * overlap
                    total += overlap
            if total:
                values[cell_index] = (weighted / total).astype(np.float32)
        return values

    @staticmethod
    def _smooth(cells: Any, np: Any) -> Any:
        output = np.empty_like(cells)
        for index in range(len(cells)):
            output[index] = np.median(cells[max(0, index - 1):min(len(cells), index + 2)], axis=0)
        return output

    @staticmethod
    def _events(scores: Any, duration_ms: int, asset_start: int) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        cell_count, class_count = scores.shape
        for class_index in range(class_count):
            active_start: int | None = None
            pending_low: int | None = None
            max_score = 0.0
            for cell_index in range(cell_count):
                value = float(scores[cell_index, class_index])
                cell_start = cell_index * 100
                cell_end = min(duration_ms, cell_start + 100)
                if active_start is None:
                    if value >= 0.35:
                        active_start = cell_start
                        max_score = value
                    continue
                max_score = max(max_score, value)
                if value < 0.20:
                    if pending_low is None:
                        pending_low = cell_start
                    else:
                        events.append({"label": f"yamnet_class_{class_index:03d}",
                                       "start_ms": asset_start + active_start,
                                       "end_ms": asset_start + pending_low,
                                       "raw_score": max_score, "confidence": None})
                        active_start, pending_low, max_score = None, None, 0.0
                else:
                    pending_low = None
            if active_start is not None:
                events.append({"label": f"yamnet_class_{class_index:03d}",
                               "start_ms": asset_start + active_start,
                               "end_ms": asset_start + duration_ms,
                               "raw_score": max_score, "confidence": None})
        events.sort(key=lambda event: (event["label"], event["start_ms"], event["end_ms"]))
        merged: list[dict[str, Any]] = []
        for event in events:
            if event["end_ms"] <= event["start_ms"]:
                continue
            if (merged and merged[-1]["label"] == event["label"] and
                    event["start_ms"] - merged[-1]["end_ms"] <= 300):
                merged[-1]["end_ms"] = max(merged[-1]["end_ms"], event["end_ms"])
                merged[-1]["raw_score"] = max(merged[-1]["raw_score"], event["raw_score"])
            else:
                merged.append(dict(event))
        return [event for event in merged if event["end_ms"] - event["start_ms"] >= 200]
