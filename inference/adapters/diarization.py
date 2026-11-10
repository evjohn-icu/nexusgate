"""Whole-recording diarization from an already installed local pyannote artifact."""
from __future__ import annotations

import math
from typing import Any, Mapping

from .base import (AdapterContext, ModelUnavailable, ResourceExhausted, UnsupportedInput,
                   checked_model_path, decode_audio, import_dependency, interval_from_input,
                   slice_audio)


class DiarizationAdapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._pipeline: Any | None = None
        self._torch: Any | None = None

    def _load(self) -> tuple[Any, Any]:
        if self._pipeline is None:
            self.context.stage(0.02, "model_loading")
            try:
                torch = import_dependency("torch", "PyTorch")
                pyannote = import_dependency("pyannote.audio", "pyannote.audio")
                model_path = checked_model_path(self.context)
                pipeline_class = getattr(pyannote, "Pipeline", None)
                if pipeline_class is None:
                    pipeline_class = import_dependency("pyannote.audio", "pyannote.audio").Pipeline
                # A local path only and no token argument: gated model access is never attempted.
                pipeline = pipeline_class.from_pretrained(str(model_path))
                pipeline.to(torch.device(self.context.device))
            except ModelUnavailable:
                raise
            except Exception as exc:
                raise ModelUnavailable("gated pyannote model is not installed and usable locally") from exc
            self._pipeline, self._torch = pipeline, torch
            self.context.stage(0.18, "model_loaded")
        return self._pipeline, self._torch

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        if request["input"]["kind"] != "asset":
            raise UnsupportedInput("diarization requires an audio asset")
        source = request["input"]
        pipeline, torch = self._load()
        audio, rate = decode_audio(source["path"], target_rate=16000)
        decoded_ms = math.floor(len(audio) * 1000 / rate)
        asset_start, asset_end = interval_from_input(request, decoded_ms)
        audio = slice_audio(audio, rate, asset_start, asset_end)
        waveform = torch.from_numpy(audio.copy()).to(torch.float32).unsqueeze(0)
        try:
            annotation = pipeline({"waveform": waveform, "sample_rate": 16000})
            if hasattr(annotation, "speaker_diarization"):
                annotation = annotation.speaker_diarization
            raw_turns: list[tuple[int, int, str]] = []
            for segment, _, speaker in annotation.itertracks(yield_label=True):
                start = max(asset_start, asset_start + math.floor(float(segment.start) * 1000))
                end = min(asset_end, asset_start + math.ceil(float(segment.end) * 1000))
                if end > start:
                    raw_turns.append((start, end, str(speaker)))
        except Exception as exc:
            if "outofmemory" in type(exc).__name__.lower() or "resourceexhausted" in type(exc).__name__.lower():
                raise ResourceExhausted("diarization exhausted device memory") from exc
            raise ModelUnavailable("pyannote whole-recording diarization failed") from exc
        raw_turns.sort(key=lambda item: (item[0], item[1], item[2]))
        overlaps = [False] * len(raw_turns)
        for left in range(len(raw_turns)):
            for right in range(left + 1, len(raw_turns)):
                if raw_turns[right][0] >= raw_turns[left][1]:
                    break
                if max(raw_turns[left][0], raw_turns[right][0]) < min(raw_turns[left][1], raw_turns[right][1]):
                    overlaps[left] = overlaps[right] = True
        turns = [{"start_ms": start, "end_ms": end, "local_speaker_id": speaker,
                  "confidence": None, "overlap": overlaps[index]}
                 for index, (start, end, speaker) in enumerate(raw_turns)]
        self.context.progress(0.95, "diarization_complete")
        return {"kind": "diarization", "turns": turns}
