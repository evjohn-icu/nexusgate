"""Shared inference adapter primitives. Model imports are deliberately lazy."""
from __future__ import annotations

import importlib
import io
import math
import subprocess
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Mapping


class InferenceFailure(Exception):
    code = "invalid_output"


class ModelUnavailable(InferenceFailure):
    code = "model_unavailable"


class UnsupportedInput(InferenceFailure):
    code = "unsupported_input"


class UnsupportedOperation(InferenceFailure):
    code = "unsupported_operation"


class InvalidOutput(InferenceFailure):
    code = "invalid_output"


class ResourceExhausted(InferenceFailure):
    code = "resource_exhausted"


class DeadlineExceeded(InferenceFailure):
    code = "deadline_exceeded"


class Cancelled(InferenceFailure):
    code = "cancelled"


@dataclass
class AdapterContext:
    model_dir: Path
    device: str
    dtype: str
    progress: Callable[[float, str | None], None]
    model: Mapping[str, Any]
    cancel_check: Callable[[], None]

    @property
    def dimension(self) -> int:
        return int(self.model.get("output", {}).get("dimension", 0))

    def stage(self, fraction: float, stage: str) -> None:
        self.cancel_check()
        self.progress(fraction, stage)


def import_dependency(module: str, purpose: str) -> Any:
    try:
        return importlib.import_module(module)
    except (ImportError, ModuleNotFoundError) as exc:
        raise ModelUnavailable(f"{purpose} dependency is not installed") from exc


def checked_model_path(context: AdapterContext) -> Path:
    """Return only the catalog-owned, locally installed model directory."""
    status = context.model.get("artifact_status")
    if status in {"pending_download", "blocked_gated"}:
        raise ModelUnavailable("locked model artifact is not available locally")
    root = context.model_dir.resolve()
    if not root.is_dir():
        raise ModelUnavailable("model directory is unavailable")
    required = context.model.get("files", [])
    if not required:
        raise ModelUnavailable("locked model manifest contains no installed files")
    for entry in required:
        relative = entry.get("path")
        if not isinstance(relative, str) or not relative:
            raise ModelUnavailable("locked model file manifest is invalid")
        file_path = (root / relative).resolve()
        if not file_path.is_relative_to(root) or not file_path.is_file():
            raise ModelUnavailable("one or more locked model files are missing")
    return root


def torch_dtype(torch: Any, name: str) -> Any:
    if name == "float32":
        return torch.float32
    if name == "float16":
        return torch.float16
    raise UnsupportedInput("unsupported model dtype")


def load_torch_model(context: AdapterContext, model_class: str, processor_class: str | None = None,
                     *, trust_remote_code: bool = False) -> tuple[Any, Any, Any]:
    """Load only from a fully present local artifact; remote lookup is disabled."""
    torch = import_dependency("torch", "PyTorch")
    transformers = import_dependency("transformers", "Transformers")
    model_path = checked_model_path(context)
    dtype = torch_dtype(torch, context.dtype)
    try:
        model_type = getattr(transformers, model_class)
        kwargs = {"local_files_only": True, "trust_remote_code": trust_remote_code}
        model = model_type.from_pretrained(str(model_path), torch_dtype=dtype, **kwargs)
        processor = None
        if processor_class:
            processor_type = getattr(transformers, processor_class)
            processor = processor_type.from_pretrained(str(model_path), local_files_only=True,
                                                       trust_remote_code=trust_remote_code)
        model.to(context.device)
        model.eval()
    except (OSError, ValueError, RuntimeError, AttributeError) as exc:
        if _is_oom(exc):
            raise ResourceExhausted("model could not fit in the requested device memory") from exc
        raise ModelUnavailable("locked model could not be loaded locally") from exc
    return torch, model, processor


def _is_oom(exc: BaseException) -> bool:
    name = type(exc).__name__.lower()
    return isinstance(exc, MemoryError) or "outofmemory" in name or "resourceexhausted" in name


def tensor_to_numpy(value: Any) -> Any:
    """Unwrap common Transformers outputs without changing their values."""
    if hasattr(value, "pooler_output") and value.pooler_output is not None:
        value = value.pooler_output
    elif hasattr(value, "last_hidden_state"):
        value = value.last_hidden_state[:, 0]
    elif hasattr(value, "image_embeds") and value.image_embeds is not None:
        value = value.image_embeds
    elif hasattr(value, "text_embeds") and value.text_embeds is not None:
        value = value.text_embeds
    if hasattr(value, "detach"):
        value = value.detach().to("cpu").float().numpy()
    return value


def normalized_vectors(values: Any, dimension: int, *, truncate_to: int | None = None) -> Any:
    """Return finite, normalized float32 vectors; reject zero and wrong-sized data."""
    np = import_dependency("numpy", "NumPy")
    array = np.asarray(values, dtype=np.float32)
    if array.ndim == 1:
        array = array.reshape(1, -1)
    if array.ndim != 2 or array.shape[1] != dimension:
        raise InvalidOutput("model output dimension does not match the locked dimension")
    if not np.isfinite(array).all():
        raise InvalidOutput("model output contains a non-finite value")
    if truncate_to is not None:
        if truncate_to < 1 or truncate_to > dimension:
            raise UnsupportedInput("requested vector truncation is out of range")
        array = array[:, :truncate_to].copy()
    norms = np.linalg.norm(array, axis=1)
    if not np.isfinite(norms).all() or np.any(norms <= 0):
        raise InvalidOutput("model output contains a zero or invalid vector")
    array = array / norms[:, None]
    if not np.isfinite(array).all():
        raise InvalidOutput("normalized model output is not finite")
    return np.asarray(array, dtype="<f4")


def encode_vector(vector: Any) -> dict[str, Any]:
    import base64
    np = import_dependency("numpy", "NumPy")
    array = np.asarray(vector, dtype="<f4").reshape(-1)
    if not np.isfinite(array).all() or not len(array):
        raise InvalidOutput("cannot encode an empty or non-finite vector")
    return {"dtype": "float32", "dimension": int(array.size),
            "floats_b64": base64.b64encode(array.tobytes(order="C")).decode("ascii")}


def decode_audio(path: str, *, target_rate: int | None = None) -> tuple[Any, int]:
    """Decode local audio via libsndfile or PCM WAV only; never shells out to ffmpeg."""
    np = import_dependency("numpy", "NumPy")
    source = Path(path)
    if not source.is_file():
        raise UnsupportedInput("audio input file is unavailable")
    try:
        sf = importlib.import_module("soundfile")
        samples, sample_rate = sf.read(str(source), dtype="float32", always_2d=True)
        audio = np.mean(samples, axis=1, dtype=np.float32)
    except (ImportError, ModuleNotFoundError):
        audio, sample_rate = _decode_pcm_wave(source, np)
    except Exception as exc:
        try:
            audio, sample_rate = _decode_pcm_wave(source, np)
        except Exception as fallback:
            raise UnsupportedInput("audio input is not a supported soundfile or PCM WAV") from fallback
    if sample_rate <= 0 or audio.ndim != 1 or not len(audio) or not np.isfinite(audio).all():
        raise UnsupportedInput("decoded audio is empty or invalid")
    if target_rate is not None and sample_rate != target_rate:
        if sample_rate == 16000 and target_rate == 48000:
            raise UnsupportedInput("CLAP requires source audio, not an upsampled 16 kHz derivative")
        audio = resample_audio(audio, sample_rate, target_rate)
        sample_rate = target_rate
    return np.asarray(audio, dtype=np.float32), int(sample_rate)


def _decode_pcm_wave(path: Path, np: Any) -> tuple[Any, int]:
    with wave.open(str(path), "rb") as wav:
        channels, width, rate, frames = wav.getnchannels(), wav.getsampwidth(), wav.getframerate(), wav.getnframes()
        if channels < 1 or width not in (1, 2, 3, 4) or frames <= 0:
            raise UnsupportedInput("unsupported PCM WAV format")
        raw = wav.readframes(frames)
        if width == 1:
            values = (np.frombuffer(raw, dtype=np.uint8).astype(np.float32) - 128.0) / 128.0
        elif width == 2:
            values = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
        elif width == 3:
            bytes3 = np.frombuffer(raw, dtype=np.uint8).reshape(-1, 3)
            integer = (bytes3[:, 0].astype(np.int32) | (bytes3[:, 1].astype(np.int32) << 8) |
                       (bytes3[:, 2].astype(np.int32) << 16))
            integer = np.where(integer & 0x800000, integer | ~0xFFFFFF, integer)
            values = integer.astype(np.float32) / 8388608.0
        else:
            values = np.frombuffer(raw, dtype="<i4").astype(np.float32) / 2147483648.0
        return values.reshape(-1, channels).mean(axis=1, dtype=np.float32), int(rate)


def resample_audio(audio: Any, source_rate: int, target_rate: int) -> Any:
    np = import_dependency("numpy", "NumPy")
    if source_rate <= 0 or target_rate <= 0:
        raise UnsupportedInput("invalid audio sample rate")
    try:
        signal = importlib.import_module("scipy.signal")
        from math import gcd
        divisor = gcd(source_rate, target_rate)
        result = signal.resample_poly(audio, target_rate // divisor, source_rate // divisor)
    except (ImportError, ModuleNotFoundError):
        length = max(1, round(len(audio) * target_rate / source_rate))
        old = np.arange(len(audio), dtype=np.float64) / source_rate
        new = np.arange(length, dtype=np.float64) / target_rate
        result = np.interp(new, old, audio).astype(np.float32)
    return np.asarray(result, dtype=np.float32)


def slice_audio(audio: Any, sample_rate: int, start_ms: int, end_ms: int) -> Any:
    start = max(0, int(start_ms * sample_rate / 1000))
    end = min(len(audio), int(math.ceil(end_ms * sample_rate / 1000)))
    if end <= start:
        raise UnsupportedInput("requested audio interval is empty")
    return audio[start:end]


def interval_from_input(request: Mapping[str, Any], decoded_duration_ms: int) -> tuple[int, int]:
    item = request["input"]
    duration = int(item.get("duration_ms", decoded_duration_ms))
    interval = item.get("interval")
    if interval:
        start, end = int(interval["start_ms"]), int(interval["end_ms"])
    else:
        start, end = 0, duration
    if end > decoded_duration_ms:
        end = decoded_duration_ms
    if start < 0 or end <= start:
        raise UnsupportedInput("audio interval is outside the decoded asset")
    return start, end

def extract_video_frame(path: str, timestamp_ms: int) -> Any:
    """Decode one requested still through the sanitized ffmpeg executable."""
    source = Path(path)
    if not source.is_absolute() or not source.is_file():
        raise UnsupportedInput("visual asset is unavailable")
    seconds = f"{timestamp_ms / 1000.0:.3f}"
    command = ["ffmpeg", "-nostdin", "-v", "error", "-i", str(source), "-ss", seconds,
               "-map", "0:v:0", "-an", "-sn", "-dn", "-frames:v", "1", "-threads", "1",
               "-vf", "scale=2048:2048:force_original_aspect_ratio=decrease",
               "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1"]
    try:
        completed = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, check=False, timeout=60)
    except FileNotFoundError as exc:
        raise ModelUnavailable("ffmpeg is unavailable for visual frame extraction") from exc
    except subprocess.TimeoutExpired as exc:
        raise DeadlineExceeded("visual frame extraction exceeded its deadline") from exc
    if completed.returncode != 0 or not completed.stdout:
        raise UnsupportedInput("ffmpeg could not decode the requested visual timestamp")
    try:
        from PIL import Image
        with Image.open(io.BytesIO(completed.stdout)) as image:
            if image.format != "JPEG" or image.width * image.height > 20_000_000:
                raise UnsupportedInput("decoded visual frame exceeds supported image bounds")
            return image.convert("RGB")
    except UnsupportedInput:
        raise
    except Exception as exc:
        raise UnsupportedInput("decoded visual frame is not a valid image") from exc
