"""CLAP audio/text embeddings with explicit 48 kHz zero-padded windows."""
from __future__ import annotations

import math
from typing import Any, Mapping

from .base import (AdapterContext, ModelUnavailable, ResourceExhausted, UnsupportedInput,
                   decode_audio, encode_vector, interval_from_input, load_torch_model,
                   normalized_vectors, slice_audio, tensor_to_numpy)


class ClapAdapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._loaded: tuple[Any, Any, Any] | None = None

    def _model(self) -> tuple[Any, Any, Any]:
        if self._loaded is None:
            self.context.stage(0.02, "model_loading")
            self._loaded = load_torch_model(self.context, "ClapModel", "ClapProcessor")
            self.context.stage(0.18, "model_loaded")
        return self._loaded

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        torch, model, processor = self._model()
        source = request["input"]
        if source["kind"] == "text":
            encoded = processor(text=[source["text"]], max_length=77, truncation=True,
                                padding=True, return_tensors="pt")
            encoded = self._to_device(encoded)
            with torch.inference_mode():
                raw = self._features(model, "get_text_features", encoded)
            vector = normalized_vectors(raw, self.context.dimension)[0]
            return {"kind": "audio_embedding", "windows": [], "query_vector": encode_vector(vector)}
        if source["kind"] != "asset":
            raise UnsupportedInput("CLAP accepts audio assets or text queries")

        audio, rate = decode_audio(source["path"], target_rate=48000)
        decoded_ms = math.floor(len(audio) * 1000 / rate)
        start_ms, end_ms = interval_from_input(request, decoded_ms)
        local_audio = slice_audio(audio, rate, start_ms, end_ms)
        region_ms = end_ms - start_ms
        window_samples = 10 * rate
        hop_samples = 5 * rate
        windows: list[dict[str, Any]] = []
        starts = range(0, region_ms, 5000)
        for index, offset_ms in enumerate(starts):
            self.context.cancel_check()
            begin = index * hop_samples
            segment = local_audio[begin:begin + window_samples]
            actual_samples = len(segment)
            if actual_samples == 0:
                continue
            if actual_samples < window_samples:
                # This explicit padding overrides the upstream repeat-padding default.
                segment = __import__("numpy").pad(segment, (0, window_samples - actual_samples),
                                                   mode="constant", constant_values=0)
            try:
                encoded = processor(audios=segment, sampling_rate=48000,
                                    return_tensors="pt", padding=True)
            except Exception as exc:
                raise UnsupportedInput("CLAP processor rejected decoded source audio") from exc
            encoded = self._to_device(encoded)
            with torch.inference_mode():
                raw = self._features(model, "get_audio_features", encoded)
            vector = normalized_vectors(raw, self.context.dimension)[0]
            absolute_start = start_ms + int(offset_ms)
            absolute_end = min(end_ms, absolute_start + 10000)
            windows.append({"start_ms": absolute_start, "end_ms": absolute_end,
                            **encode_vector(vector)})
            self.context.progress(0.20 + 0.72 * min(1.0, (index + 1) / max(1, len(range(0, region_ms, 5000)))),
                                  "encoding_windows")
        if not windows:
            raise UnsupportedInput("audio asset contains no complete or partial CLAP window")
        return {"kind": "audio_embedding", "windows": windows}

    def _features(self, model: Any, method: str, encoded: Mapping[str, Any]) -> Any:
        if not hasattr(model, method):
            raise ModelUnavailable(f"locked CLAP model has no {method} method")
        try:
            return tensor_to_numpy(getattr(model, method)(**encoded))
        except Exception as exc:
            if "outofmemory" in type(exc).__name__.lower():
                raise ResourceExhausted("CLAP exhausted device memory") from exc
            raise ModelUnavailable("CLAP embedding failed") from exc

    def _to_device(self, values: Mapping[str, Any]) -> dict[str, Any]:
        return {key: value.to(self.context.device) if hasattr(value, "to") else value
                for key, value in values.items()}
