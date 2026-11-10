"""Local-only WeMM image, video, and text embeddings."""
from __future__ import annotations

from typing import Any, Mapping

from .base import (AdapterContext, ModelUnavailable, UnsupportedInput, encode_vector,
                   load_torch_model, normalized_vectors, tensor_to_numpy)


class WemmAdapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._loaded: tuple[Any, Any, Any] | None = None

    def _model(self) -> tuple[Any, Any, Any]:
        if self._loaded is None:
            self.context.stage(0.02, "model_loading")
            self._loaded = load_torch_model(self.context, "AutoModel", "AutoProcessor",
                                            trust_remote_code=True)
            self.context.stage(0.18, "model_loaded")
        return self._loaded

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        torch, model, processor = self._model()
        source = request["input"]
        settings = request["settings"]["visual"]
        truncate = settings.get("truncate_to")
        if source["kind"] == "text":
            tokens = processor(text=[source["text"]], max_length=512, truncation=True,
                               padding=True, return_tensors="pt")
            encoded = self._to_device(tokens)
            with torch.inference_mode():
                raw = self._features(model, "get_text_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}

        if settings["strategy"] == "native_video":
            if source["kind"] != "asset":
                raise UnsupportedInput("native_video requires an asset input")
            if not hasattr(model, "get_video_features"):
                raise ModelUnavailable("installed WeMM code has no native video embedding method")
            try:
                encoded = processor(videos=[source["path"]], return_tensors="pt")
            except Exception as exc:
                raise UnsupportedInput("installed video decoder cannot read the request asset") from exc
            encoded = self._to_device(encoded)
            with torch.inference_mode():
                raw = self._features(model, "get_video_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate)
            interval = source.get("interval") or {"start_ms": 0, "end_ms": source["duration_ms"]}
            start, end = int(interval["start_ms"]), int(interval["end_ms"])
            shot = str(settings.get("shot_id", ""))
            self.context.progress(0.95, "encoding_complete")
            return {"kind": "visual_embedding", "vectors": [
                self._record(vectors[0], shot, -1, start, start, end)]}

        if source["kind"] == "image":
            images = [self._open_image(source["path"])]
            shot_id = str(settings.get("shot_id", ""))
            timestamp = 0
        elif source["kind"] == "asset":
            frames = settings.get("frames", [])
            if not frames:
                raise UnsupportedInput("shot_frame_pooled requires staged frame paths")
            images = [self._open_image(frame["path"]) for frame in frames]
            shot_id = str(settings.get("shot_id", ""))
            timestamp = int((source.get("interval") or {}).get("start_ms", 0))
        else:
            raise UnsupportedInput("unsupported WeMM visual input")
        try:
            encoded = processor(images=images, return_tensors="pt")
        except Exception as exc:
            raise UnsupportedInput("installed WeMM processor rejected the staged images") from exc
        encoded = self._to_device(encoded)
        with torch.inference_mode():
            raw = self._features(model, "get_image_features", encoded)
        vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate)
        records: list[dict[str, Any]] = []
        if source["kind"] == "image":
            records.append(self._record(vectors[0], shot_id, -1, timestamp, 0, 0))
        else:
            interval = source.get("interval") or {"start_ms": 0, "end_ms": source["duration_ms"]}
            for index, (vector, frame) in enumerate(zip(vectors, frames)):
                at = int(frame["timestamp_ms"])
                records.append(self._record(vector, shot_id, index, at, at, at))
            pooled = normalized_vectors(vectors.mean(axis=0), self.context.dimension,
                                        truncate_to=truncate)[0]
            records.append(self._record(pooled, shot_id, -1, timestamp,
                                        int(interval["start_ms"]), int(interval["end_ms"])))
        self.context.progress(0.95, "encoding_complete")
        return {"kind": "visual_embedding", "vectors": records}

    def _features(self, model: Any, method: str, encoded: Mapping[str, Any]) -> Any:
        if not hasattr(model, method):
            raise ModelUnavailable(f"installed WeMM code has no {method} method")
        try:
            return tensor_to_numpy(getattr(model, method)(**encoded))
        except (RuntimeError, ValueError, TypeError, AttributeError) as exc:
            if "outofmemory" in type(exc).__name__.lower():
                from .base import ResourceExhausted
                raise ResourceExhausted("WeMM exhausted device memory") from exc
            raise ModelUnavailable("installed WeMM embedding method failed") from exc

    def _to_device(self, values: Mapping[str, Any]) -> dict[str, Any]:
        return {key: value.to(self.context.device) if hasattr(value, "to") else value
                for key, value in values.items()}

    @staticmethod
    def _open_image(path: str) -> Any:
        try:
            from PIL import Image
            with Image.open(path) as image:
                return image.convert("RGB")
        except Exception as exc:
            raise UnsupportedInput("staged visual frame cannot be decoded") from exc

    @staticmethod
    def _record(vector: Any, shot_id: str, sample_index: int, timestamp: int,
                start: int, end: int) -> dict[str, Any]:
        return {"shot_id": shot_id, "sample_index": sample_index, "timestamp_ms": timestamp,
                "start_ms": start, "end_ms": end, **encode_vector(vector)}
