"""Local-only WeMM image, video, and text embeddings."""
from __future__ import annotations

from typing import Any, Mapping

import torch

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
            encoded = self._encode(model, processor, [{"type": "text", "text": source["text"]}])
            with torch.inference_mode():
                raw = self._features(model, encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}

        if source["kind"] == "image":
            encoded = self._encode(model, processor, [
                {"type": "image", "image": self._open_image(source["path"])}])
            with torch.inference_mode():
                raw = self._features(model, encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            return {"kind": "visual_embedding", "vectors": [self._record(vectors[0], "", -1, 0, 0, 0)]}
        if source["kind"] != "asset":
            raise UnsupportedInput("unsupported WeMM visual input")

        shots = settings["shots"]
        if settings["strategy"] == "native_video":
            shot = shots[0]
            encoded = self._encode(model, processor, [
                {"type": "video", "video": source["path"]}])
            with torch.inference_mode():
                raw = self._features(model, encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            self.context.progress(0.95, "encoding_complete")
            return {"kind": "visual_embedding", "vectors": [
                self._record(vectors[0], shot["shot_id"], -1, shot["start_ms"],
                             shot["start_ms"], shot["end_ms"])]}

        records: list[dict[str, Any]] = []
        for shot_index, shot in enumerate(shots):
            self.context.cancel_check()
            content = [{"type": "image", "image": self._extract(source["path"], timestamp)}
                       for timestamp in shot["timestamps_ms"]]
            encoded = self._encode(model, processor, content)
            with torch.inference_mode():
                raw = self._features(model, encoded)
            vectors = normalized_vectors(raw, self.context.dimension, truncate_to=truncate_to)
            for sample_index, (vector, timestamp) in enumerate(zip(vectors, shot["timestamps_ms"])):
                records.append(self._record(vector, shot["shot_id"], sample_index, timestamp,
                                            shot["start_ms"], shot["end_ms"]))
            pooled = normalized_vectors(vectors.mean(axis=0), vectors.shape[1])[0]
            records.append(self._record(pooled, shot["shot_id"], -1, shot["start_ms"],
                                        shot["start_ms"], shot["end_ms"]))
            self.context.progress(0.20 + 0.72 * (shot_index + 1) / len(shots), "encoding_shots")
        return {"kind": "visual_embedding", "vectors": records}

    def _encode(self, model: Any, processor: Any, content: list[dict[str, Any]]) -> dict[str, Any]:
        """Build the model card's chat-template input and move it to the device."""
        from qwen_vl_utils import process_vision_info

        messages = [{"role": "user", "content": content}]
        text = processor.apply_chat_template(messages, tokenize=False, add_generation_prompt=False)
        images, videos, video_kwargs = process_vision_info(
            messages, image_patch_size=16, return_video_kwargs=True, return_video_metadata=True)
        if videos is not None:
            videos, video_metadata = zip(*videos)
            videos, video_metadata = list(videos), list(video_metadata)
        else:
            video_metadata = None
        encoded = processor(text=text, images=images, videos=videos,
                            video_metadata=video_metadata, return_tensors="pt", **video_kwargs)
        return self._to_device(encoded)

    def _features(self, model: Any, encoded: Mapping[str, Any]) -> Any:
        if not hasattr(model, "embedding"):
            raise ModelUnavailable("installed WeMM code has no embedding method")
        try:
            return tensor_to_numpy(model.embedding(**encoded))
        except Exception as exc:
            if "outofmemory" in type(exc).__name__.lower():
                raise ResourceExhausted("WeMM exhausted device memory") from exc
            raise ModelUnavailable("WeMM embedding failed") from exc

    @staticmethod
    def _extract(path: str, timestamp_ms: int) -> Any:
        from .base import extract_video_frame
        return extract_video_frame(path, timestamp_ms)

    def _to_device(self, values: Mapping[str, Any]) -> dict[str, Any]:
        moved: dict[str, Any] = {}
        for key, value in values.items():
            if hasattr(value, "to"):
                value = value.to(self.context.device)
                if (self.context.dtype == "float16"
                        and getattr(value, "is_floating_point", lambda: False)()):
                    value = value.to(torch.float16)
            moved[key] = value
        return moved

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
