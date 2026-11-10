# V1.5 Model Matrix

Status of every model the milestone's inference boundary can name. This file is
the human-readable companion to `inference/models.lock.json`; the lock file is
the machine-readable authority the installer and the Python dispatcher read.
Neither file is generated from a mutable branch: every entry pins an immutable
revision and the SHA-256 of each file the installer may download.

Measurement status is deliberately separate from implementation status. A model
adapter can exist, pass its contract tests and still have no measured accuracy
or memory profile on the reference device. Rows below record what was actually
observed on 2026-11-10; nothing here is inferred from a model card.

## Device and budget

| item | value | evidence |
| --- | --- | --- |
| GPU | NVIDIA GeForce RTX 2070, 8192 MiB, driver 610.88 | `nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader` |
| GPU compute capability | 7.5 (Turing): FP16 tensor cores, no native BF16 | device property |
| Production VRAM ceiling | 7168 MiB peak per process, 1 GiB reserved, concurrency 1 | this plan |
| CPU | AMD Ryzen 5 5600X, 12 threads, 23 GiB RAM | `nproc`, `free -g` |

A profile that peaks above 7168 MiB is rejected rather than made to fit by
silently shrinking media or dropping precision below the model's supported
dtype.

## Locked models

### Visual embedding

| model | revision | files | space | license | decision |
| --- | --- | --- | --- | --- | --- |
| `google/siglip2-base-patch16-224` | `75de2d55ec2d0b4efc50b3e9ad70dba96a7b2fa2` | `model.safetensors` 1 500 800 904 B sha256 `61292338…6a0b` | 768d, L2-normalized, 224×224 RGB, mean/std 0.5, bicubic | Apache-2.0 | default visual space |
| `tencent/WeMM-Embedding-2B` | `bbd6cd4bf52cfc6716f752a2df80b2706720bd95` | `model.safetensors` 5 441 695 216 B sha256 `e1a1ad75…6e59` | 2048d base, Matryoshka `64,128,256,512,1024,2048` (declared in `config.json`), L2-normalized | card declares Apache-2.0 (`license_name: apache-2.0`, `license_link` → LICENSE) while HF’s `license` field is `other`; the LICENSE text governs | strong candidate, gated on device profile |

WeMM is `bfloat16` in `config.json` and ships a custom `Qwen3_5` video/vision
stack (`transformers >= 5.2.0`, `qwen-vl-utils`). Turing has no native BF16, so
the only viable device profile is FP16; whether the hybrid linear-attention
stack runs at all in FP16 on this GPU is an unresolved measurement. The adapter
must therefore report `model_unavailable` — never silently fall back to
SigLIP dims or another space — when the WeMM profile is absent or over budget.

Frame-pooled and native-video encodings are **different spaces** and get
different `space_id`s: pooled frames store per-frame vectors plus a normalized
mean of normalized frames; native video stores one record per clip window.

### Audio/text embedding

| model | revision | files | space | license | decision |
| --- | --- | --- | --- | --- | --- |
| `laion/clap-htsat-unfused` | `8fa0f1c6d0433df6e97c127f64b2a1d6c0dcda8a` | `pytorch_model.bin` 614 525 833 B sha256 `1cd3c601…19bb` | 512d joint, L2-normalized, 48 kHz mono, 10 s window, `repeatpad` upstream overridden by explicit zero-pad | Apache-2.0 | audio/text retrieval and *possible* natural-language sound matching |

The 16 kHz ASR derivative is never upsampled into CLAP. The staged source is
decoded to 48 kHz mono for every window; short tails are zero-padded explicitly
and the override is recorded in the generation's `preprocessing_json`.

### Sound events

| model | revision | artifact | output | license | decision |
| --- | --- | --- | --- | --- | --- |
| YAMNet (AudioSet, 521 classes) | TF-Models code `c14bf9ad91962cf189f9f58db2132c06247fcd53` | official version-1 release via Kaggle Models `google/yamnet/TensorFlow2/yamnet/1` | 16 kHz mono, 0.96 s patches / 0.48 s hop, per-frame 521-class scores | Apache-2.0 weights and code per the AudioSet release | CPU baseline for environmental events |
| PANNs `Cnn14_DecisionLevelMax` | optional comparison only | not locked | clip-level backbone + frame-level decision head | Apache-2.0 code, weights per upstream release | **not selected**; clip-only classification is never reinterpreted as exact boundaries |

YAMNet's own README pins its dependency: the SavedModel was exported for Keras 2
and does not load under Keras 3 defaults. Its environment is therefore separate
from every PyTorch environment, and its weight artifact is downloaded by the
installer with a manifest-pinned hash rather than resolved through a framework
loader at inference time.

### Diarization

| model | revision | license | decision |
| --- | --- | --- | --- |
| `pyannote/speaker-diarization-community-1` | `3533c8cf8e369892e6b79ff1bf80f7b0286a54ee` | CC-BY-4.0 declared, gated | preferred diarizer, **not installable here** |
| NVIDIA `diar_sortformer_4spk-v1` | not locked | CC-BY-NC-4.0 | **excluded**: non-commercial weights cannot satisfy the production gate |
| Nemotron-3-Diarization | not locked | not verified | fallback candidate only if Community-1 access cannot be satisfied; its 8-speaker limit would be an explicit `unsupported_input` condition |

Direct observation on 2026-11-10: an unauthenticated GET of a file inside
Community-1 returns HTTP 401, so no access acceptance exists in this
environment. The adapter must return `model_unavailable` with the missing-gate
reason; it must not substitute generated tones, a mock diarizer, or an
unlicensed model.

### Speaker embedding

| model | revision | file | dimension | decision |
| --- | --- | --- | --- | --- |
| CAM++ `iic/speech_campplus_sv_zh-cn_16k-common` | file `campplus_cn_common.bin` at ModelScope revision `013c18884dff…` | 28 036 335 B, sha256 `3388cf5f…ada8` | 192 (`config.yaml: embedding_size: 192`), 16 kHz | benchmark candidate, tie-break advantage |
| ERes2NetV2 `iic/speech_eres2netv2_sv_zh-cn_16k-common` | file `pretrained_eres2netv2.ckpt` at ModelScope revision `1cf80d41fb34…` | 71 768 231 B, sha256 `0eb40571…963c` | 192 (`configuration.json: embed_dim: 192`), 16 kHz | benchmark candidate |

Selection rule: highest recall among thresholds whose precision on the
calibration split is ≥ 95 %, ties broken by lower measured latency and then by
CAM++. Without a valid calibration threshold the service returns raw candidate
similarity with state `unknown` and never auto-groups.

### Speech recognition and alignment

| model | decision |
| --- | --- |
| existing configured ASR provider | retained as default |
| `Qwen/Qwen3-ASR-0.6B` | optional, separate Transformers environment, offered only after measured runtime and licence checks |
| `Qwen/Qwen3-ForcedAligner-0.6B` | optional, same conditions |

Neither optional model changes a default in this milestone.

## Explicit non-inclusions

| model | reason |
| --- | --- |
| V-JEPA 2 | research-stage encoder, no production adapter required |
| TimeLens2 | research-only temporal grounding |
| SAM 3.1 | research-only segmentation |
| OpenFLAM | research-only |
| Qwen3-VL-Reranker | not required; reranking stays with the existing reranker |
| WeMM audio | the model has no audio path; audio is never routed to it |

Listing a model here means the catalog never offers it and no adapter may
pretend it exists. Absence from the lock file is the enforcement point.

## Measurement log

| model | cold start | peak VRAM | peak RSS | warm latency | output checks | measured on |
| --- | --- | --- | --- | --- | --- | --- |
| SigLIP 2 base (FP32 CPU) | pending | n/a | pending | pending | pending | — |
| SigLIP 2 base (FP16 CUDA) | pending | pending | pending | pending | pending | — |
| WeMM-Embedding-2B (FP16 CUDA, pooled frames) | pending | pending | pending | pending | pending | — |
| WeMM-Embedding-2B (FP16 CUDA, native video) | pending | pending | pending | pending | pending | — |
| CLAP unfused (FP32 CPU / FP16 CUDA) | pending | pending | pending | pending | pending | — |
| YAMNet (CPU) | pending | n/a | pending | pending | pending | — |
| CAM++ (CPU) | pending | n/a | pending | pending | pending | — |
| ERes2NetV2 (CPU) | pending | n/a | pending | pending | pending | — |
| Community-1 | blocked | blocked | blocked | blocked | blocked | gated, HTTP 401 |

Every `pending` row is a gate that has not passed. The reference GPU is busy
with an unrelated process at implementation time, so GPU rows stay unmeasured
until `inference/benchmark.py` can run exclusively; CPU rows are measured with
the same tool. A row is only replaced by numbers copied from an actual run of
`inference/benchmark.py --json <path>` against the locked revision, and the
run's device string is recorded with it.
