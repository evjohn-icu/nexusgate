# Inference runtime

One Python process per inference operation. The Go executor starts it, writes a
single `inference.v1` request line to stdin, reads bounded JSON frames from
stdout and kills the whole process tree on timeout or cancellation. Nothing here
listens on a socket, and nothing downloads weights at inference time.

## Environments

`models.lock.json` names the runtime digest each model expects. The lock file is
the authority; these files only say how to build an environment that can honour
it.

```bash
# CPU (developers, small evaluations)
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements/torch.in \
    --extra-index-url https://download.pytorch.org/whl/cpu

# CUDA (the reference device is a Turing RTX 2070; cu126 wheels are compatible)
# The measured GPU set is pinned in requirements/torch-cuda.lock
# (torch 2.14.1+cu126, torchvision 0.29.1+cu126, transformers 4.57.1).
uv pip install --python .venv/bin/python -r requirements/torch.in \
    --extra-index-url https://download.pytorch.org/whl/cu126
```

YAMNet needs a separate environment because its SavedModel was exported for
Keras 2 and does not load under Keras 3 defaults:

```bash
uv venv --python 3.12 .venv-tf
uv pip install --python .venv-tf/bin/python -r requirements/tensorflow.in
```

Do not install both stacks into one environment. The two `site-packages` trees
must not see each other.

Record the resolved versions after installing:

```bash
uv pip freeze --python .venv/bin/python > requirements/torch.lock
sha256sum requirements/torch.lock
```

The digest belongs in `models.lock.json` (`environments.torch.digest`). An
environment whose digest does not match the lock is not a runtime the Hub should
route work to.

## Probe

```bash
.venv/bin/python -m inference.dispatch --probe \
    --model-dir /var/lib/nexusgate/models --device cuda --dtype float16
```

The probe reports one capability per locked model it can actually load. A model
whose files are missing, gated, or unsupported by this device is reported with
`available=false` and a reason; the Hub never invents a capability.

## Benchmark

```bash
.venv/bin/python inference/benchmark.py \
    --model visual-siglip2-base --device cuda --dtypes float16 \
    --batch 1,2 --json /tmp/siglip-bench.json
```

The report records cold start, load time, peak VRAM/RSS, P50/P95 warm latency,
output shape/finite/norm checks, a cancellation probe and an explicit
out-of-memory request. A CUDA profile peaking above 7168 MiB is rejected.

## Rules

- Weights are read from `--model-dir`; a missing file is `model_unavailable`.
- Paths come from the request (asset, image, text). The process never reads
  outside the request or its own scratch directory.
- Vectors are finite, normalized and exactly the declared dimension. A zero
  vector is an error, never "no match".
- Audio is decoded by the adapter: CLAP at 48 kHz mono in 10 s windows, YAMNet
  at 16 kHz mono with 960 ms patches, speaker models at 16 kHz with an 80-dim
  fbank. The 16 kHz ASR derivative is never upsampled into CLAP.
- YAMNet postprocessing thresholds are versioned in the lock file's
  `postprocessing` block; changing one changes the generation's config hash.
- Gated models (pyannote Community-1) require access acceptance and a token held
  by the Hub. Without them the adapter returns `model_unavailable`; it never
  substitutes generated audio, a mock, or an unlicensed model.
