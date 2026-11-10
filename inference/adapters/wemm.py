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
        truncate_to = settings["truncate_to"] or None
        if source["kind"] == "text":
            tokens = processor(text=[source["text"]], max_length=512, truncation=True,
                               padding=True, return_tensors="pt")
            encoded = self._to_device(tokens)
            with torch.inference_mode():
                raw = self._features(model, "get_text_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}

        if source["kind"] == "image":
            images = [self._open_image(source["path"])]
            encoded = self._to_device(processor(images=images, return_tensors="pt"))
            with torch.inference_mode():
                raw = self._features(model, "get_image_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}
        if source["kind"] != "asset":
            raise UnsupportedInput("unsupported WeMM visual input")

        shots = settings["shots"]
        if settings["strategy"] == "native_video":
            if not hasattr(model, "get_video_features"):
                raise ModelUnavailable("installed WeMM code has no native video embedding method")
            shot = shots[0]
            frames = [self._extract(source["path"], timestamp) for timestamp in shot["timestamps_ms"]]
            try:
                # The selected temporal frames are one clip input and one native video forward pass.
                encoded = processor(videos=[frames], return_tensors="pt")
            except Exception as exc:
                raise UnsupportedInput("installed WeMM processor cannot construct a native video input") from exc
            encoded = self._to_device(encoded)
            with torch.inference_mode():
                raw = self._features(model, "get_video_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            self.context.progress(0.95, "encoding_complete")
            return {"kind": "visual_embedding", "vectors": [
                self._record(vectors[0], shot["shot_id"], -1, shot["start_ms"],
                             shot["start_ms"], shot["end_ms"])]}

        records: list[dict[str, Any]] = []
        for shot_index, shot in enumerate(shots):
            self.context.cancel_check()
            images = [self._extract(source["path"], timestamp) for timestamp in shot["timestamps_ms"]]
            try:
                encoded = processor(images=images, return_tensors="pt")
            except Exception as exc:
                raise UnsupportedInput("installed WeMM processor rejected staged video frames") from exc
            encoded = self._to_device(encoded)
            with torch.inference_mode():
                raw = self._features(model, "get_image_features", encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            for sample_index, (vector, timestamp) in enumerate(zip(vectors, shot["timestamps_ms"])):
                records.append(self._record(vector, shot["shot_id"], sample_index, timestamp,
                                            shot["start_ms"], shot["end_ms"]))
            pooled = normalized_vectors(vectors.mean(axis=0), vectors.shape[1])[0]
            records.append(self._record(pooled, shot["shot_id"], -1, shot["start_ms"],
                                        shot["start_ms"], shot["end_ms"]))
            self.context.progress(0.20 + 0.72 * (shot_index + 1) / len(shots), "encoding_shots")
        return {"kind": "visual_embedding", "vectors": records}

    @staticmethod
    def _extract(path: str, timestamp_ms: int) -> Any:
        from .base import extract_video_frame
        return extract_video_frame(path, timestamp_ms)

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
