"""Local-only SigLIP 2 visual and text embeddings."""
from __future__ import annotations

from typing import Any, Mapping

from .base import (AdapterContext, ModelUnavailable, UnsupportedInput, encode_vector,
                   load_torch_model, normalized_vectors, tensor_to_numpy)


class Siglip2Adapter:
    def __init__(self, context: AdapterContext) -> None:
        self.context = context
        self._loaded: tuple[Any, Any, Any] | None = None

    def _model(self) -> tuple[Any, Any, Any]:
        if self._loaded is None:
            self.context.stage(0.02, "model_loading")
            self._loaded = load_torch_model(self.context, "AutoModel", "AutoProcessor")
            self.context.stage(0.18, "model_loaded")
        return self._loaded

    def infer(self, request: Mapping[str, Any]) -> dict[str, Any]:
        torch, model, processor = self._model()
        source = request["input"]
        settings = request["settings"]["visual"]
        if source["kind"] == "text":
            tokens = processor(text=[source["text"]], max_length=64, padding="max_length",
                               truncation=True, return_tensors="pt")
            tokens = {key: value.to(self.context.device) for key, value in tokens.items()}
            with torch.inference_mode():
                if not hasattr(model, "get_text_features"):
                    raise ModelUnavailable("locked SigLIP model has no text embedding method")
                raw = tensor_to_numpy(model.get_text_features(**tokens))
            vectors = normalized_vectors(raw, self.context.dimension)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}
        if settings["strategy"] == "native_video":
            raise UnsupportedInput("SigLIP 2 does not expose a native-video embedding method")

        truncate_to = settings["truncate_to"] or None
        if source["kind"] == "image":
            images = [self._open_image(source["path"])]
            encoded = processor(images=images, return_tensors="pt")
            encoded = {key: value.to(self.context.device) for key, value in encoded.items()}
            with torch.inference_mode():
                if not hasattr(model, "get_image_features"):
                    raise ModelUnavailable("locked SigLIP model has no image embedding method")
                raw = tensor_to_numpy(model.get_image_features(**encoded))
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}

        if source["kind"] != "asset":
            raise UnsupportedInput("SigLIP requires image, text, or asset input")
        records: list[dict[str, Any]] = []
        shots = settings["shots"]
        for shot_index, shot in enumerate(shots):
            self.context.cancel_check()
            images = [self._extract(source["path"], timestamp) for timestamp in shot["timestamps_ms"]]
            encoded = processor(images=images, return_tensors="pt")
            encoded = {key: value.to(self.context.device) for key, value in encoded.items()}
            with torch.inference_mode():
                if not hasattr(model, "get_image_features"):
                    raise ModelUnavailable("locked SigLIP model has no image embedding method")
                raw = tensor_to_numpy(model.get_image_features(**encoded))
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
