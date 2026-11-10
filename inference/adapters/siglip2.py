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
        strategy = settings["strategy"]
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

        if strategy == "native_video":
            if source["kind"] != "asset":
                raise UnsupportedInput("native_video requires an asset input")
            raise UnsupportedInput("SigLIP 2 does not expose a native-video embedding method")

        images: list[Any] = []
        metadata: list[tuple[str, int, int, int]] = []
        if source["kind"] == "image":
            images.append(self._open_image(source["path"]))
            metadata.append((str(settings.get("shot_id", "")), -1, 0, 0))
        else:
            frames = settings.get("frames", [])
            if not frames:
                raise UnsupportedInput("shot_frame_pooled requires staged frame paths")
            shot_id = str(settings.get("shot_id", ""))
            for index, frame in enumerate(frames):
                self.context.cancel_check()
                images.append(self._open_image(frame["path"]))
                metadata.append((shot_id, index, int(frame["timestamp_ms"]), index))
        encoded = processor(images=images, return_tensors="pt")
        encoded = {key: value.to(self.context.device) for key, value in encoded.items()}
        with torch.inference_mode():
            if not hasattr(model, "get_image_features"):
                raise ModelUnavailable("locked SigLIP model has no image embedding method")
            raw = tensor_to_numpy(model.get_image_features(**encoded))
        vectors = normalized_vectors(raw, self.context.dimension)
        records: list[dict[str, Any]] = []
        if source["kind"] == "image":
            records.append(self._record(vectors[0], metadata[0][0], -1, 0, 0, 0))
        else:
            for vector, (shot_id, sample_index, timestamp, _) in zip(vectors, metadata):
                records.append(self._record(vector, shot_id, sample_index, timestamp,
                                            timestamp, timestamp))
            pooled = normalized_vectors(vectors.mean(axis=0), self.context.dimension)[0]
            interval = source.get("interval") or {"start_ms": 0, "end_ms": source["duration_ms"]}
            timestamp = int(interval["start_ms"])
            records.append(self._record(pooled, str(settings.get("shot_id", "")), -1,
                                        timestamp, int(interval["start_ms"]), int(interval["end_ms"])))
        self.context.progress(0.95, "encoding_complete")
        return {"kind": "visual_embedding", "vectors": records}

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
