"""FunASR-backed CAM++ and ERes2NetV2 speaker vectors."""
from __future__ import annotations

import math
from pathlib import Path
from typing import Any, Mapping

from .base import (AdapterContext, InvalidOutput, ModelUnavailable, ResourceExhausted,
                   UnsupportedInput, checked_model_path, decode_audio, encode_vector,
                   import_dependency, interval_from_input, normalized_vectors, slice_audio)


class SpeakerAdapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._model: Any | None = None

    def _load(self) -> Any:
        if self._model is not None:
            return self._model
        self.context.stage(0.02, "model_loading")
        funasr = import_dependency("funasr", "FunASR/3D-Speaker")
        model_dir = checked_model_path(self.context)
        checkpoint = next((model_dir / item["path"] for item in self.context.model.get("files", [])
                           if str(item.get("path", "")).endswith((".bin", ".ckpt"))), None)
        if checkpoint is None or not checkpoint.is_file():
            raise ModelUnavailable("locked speaker checkpoint is missing")
        auto_model = getattr(funasr, "AutoModel", None)
        if auto_model is None:
            raise ModelUnavailable("installed FunASR has no local AutoModel loader")
        try:
            # Both arguments are local paths. No model ID or registry fallback is passed.
            self._model = auto_model(model=str(model_dir), model_file=str(checkpoint),
                                      device=self.context.device, disable_update=True,
                                      disable_pbar=True)
        except Exception as exc:
            if "outofmemory" in type(exc).__name__.lower():
                raise ResourceExhausted("speaker model exhausted device memory") from exc
            raise ModelUnavailable("FunASR could not load the locked local speaker checkpoint") from exc
        self.context.stage(0.18, "model_loaded")
        return self._model

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        source = request["input"]
        if source["kind"] != "asset":
            raise UnsupportedInput("speaker embedding requires an audio asset")
        np = import_dependency("numpy", "NumPy")
        audio, rate = decode_audio(source["path"], target_rate=16000)
        decoded_ms = math.floor(len(audio) * 1000 / rate)
        asset_start, asset_end = interval_from_input(request, decoded_ms)
        segments = request["settings"]["speaker"]["segments"]
        self._check_nonoverlap(segments)
        model = self._load()
        records: list[dict[str, Any]] = []
        for index, segment in enumerate(segments):
            self.context.cancel_check()
            start, end = int(segment["start_ms"]), int(segment["end_ms"])
            if start < asset_start or end > asset_end:
                raise UnsupportedInput("speaker segment lies outside the selected asset interval")
            waveform = slice_audio(audio, rate, start, end)
            clipped = float(np.mean(np.abs(waveform) >= 0.99))
            rms = float(np.sqrt(np.mean(np.square(waveform, dtype=np.float64))))
            rms_dbfs = -120.0 if rms <= 0 else 20.0 * math.log10(rms)
            quality = {"clipped_fraction": clipped, "rms_dbfs": rms_dbfs,
                       "speech_duration_ms": end - start}
            # These regions are deliberately excluded, not represented by a guessed vector.
            if clipped > 0.01 or rms_dbfs < -40.0:
                continue
            try:
                result = model.generate(input=waveform, input_fs=16000, embedding=True)
            except Exception as exc:
                if "outofmemory" in type(exc).__name__.lower():
                    raise ResourceExhausted("speaker embedding exhausted device memory") from exc
                raise ModelUnavailable("FunASR speaker embedding inference failed") from exc
            embedding = self._extract_embedding(result)
            vector = normalized_vectors(embedding, self.context.dimension)[0]
            records.append({"sample_index": index, "start_ms": start, "end_ms": end, "quality": quality,
                            **encode_vector(vector)})
            self.context.progress(0.20 + 0.72 * (index + 1) / len(segments), "encoding_segments")
        if not records:
            raise UnsupportedInput("all speaker regions failed clipping or RMS eligibility")
        return {"kind": "speaker_embedding", "vectors": records}

    @staticmethod
    def _check_nonoverlap(segments: list[Mapping[str, Any]]) -> None:
        ordered = sorted(segments, key=lambda item: int(item["start_ms"]))
        for previous, current in zip(ordered, ordered[1:]):
            if int(current["start_ms"]) < int(previous["end_ms"]):
                raise UnsupportedInput("speaker embedding segments must not overlap")

    def _extract_embedding(self, result: Any) -> Any:
        if isinstance(result, list) and result:
            result = result[0]
        if isinstance(result, Mapping):
            for key in ("spk_embedding", "speaker_embedding", "embedding", "embeddings"):
                if key in result:
                    return result[key]
        if hasattr(result, "spk_embedding"):
            return result.spk_embedding
        if hasattr(result, "embedding"):
            return result.embedding
        raise InvalidOutput("FunASR returned no speaker embedding")
