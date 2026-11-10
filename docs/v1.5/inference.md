# V1.5 inference operations

Operator guide for the model boundary added in V1.5. It complements
`docs/v1.5/model-matrix.md` (what is locked and why) and
`inference/models.lock.json` (the machine-readable authority).

## What runs where

| surface | executes | notes |
| --- | --- | --- |
| Hub (`nexusgate serve`) | the local Python runtime, when configured | one process per operation; no resident model |
| Worker (`nexusgate worker run`) | the same runtime under the same lock | mounted source paths, own scratch directory |
| Queue | every unit is a `jobs` row of type `inference` | lease, retry and cancellation reuse the existing machinery |

A Hub without a configured runtime still serves the library, the legacy search
and every derive/analyse job. Inference work then waits for a paired Worker;
the admin settings route reports `local_runtime.available=false` instead of
pretending a model exists.

## Enabling it

1. Install a locked environment (see `inference/README.md`). Model weights are
   never bundled with the application.
2. Point `config.json` at it:

```json
"inference": {
  "enabled": true,
  "python_path": "/opt/nexusgate/inference/.venv/bin/python",
  "module_dir": "/opt/nexusgate/inference",
  "model_dir": "/var/lib/nexusgate/models",
  "device": "auto",
  "dtype": "auto"
}
```

3. Install the model artifacts from the catalog:

```bash
curl -sS -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  http://127.0.0.1:8787/api/v1/admin/inference/catalog
# then, for each entry you want, echo back its manifest_digest:
curl -sS -X POST -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"manifest_digest":"sha256:...","license_accepted":true}' \
  http://127.0.0.1:8787/api/v1/admin/inference/models/visual-siglip2-base/install
```

4. Switch operations on. Every operation defaults to **off**, so an upgrade
   never starts model work by itself:

```bash
curl -sS -X PUT -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"enabled":{"visual_embedding":true,"sound_events":true}}' \
  http://127.0.0.1:8787/api/v1/admin/multimodal/settings
```

Saving settings queues missing work for assets that already exist. Enqueue is
idempotent: the task key hashes the operation, the source identity, the
immutable model revision, the preprocessing/runtime/dimension identity and any
consent/profile generation, so pressing save twice creates nothing.

## What each operation produces

- `visual_embedding` — per-shot frame vectors plus one pooled record per shot.
  Sampling instants are chosen by the Hub (boundary-aware 2/4/6 points) and
  clamped to `end_ms-1`; the model never decides where to look. A native-video
  profile is a different space from pooled frames.
- `sound_events` — environmental events on their own timeline. Detection is
  independent of the speech gate: a recording with no speech still gets rain,
  horns and doors. Automatic events are `evidence_status=possible`; a raw
  detector score is never presented as calibrated confidence.
- `audio_embedding` — CLAP windows at 0/5000/10000… ms, 10 s each, explicit
  zero-padding for the tail. The 16 kHz ASR derivative is never upsampled.
- `diarization` — overlapping turns preserved, confidence null while
  uncalibrated, local speaker ids scoped to the recording.
- `speaker_embedding` — only through an opted-in profile, only for clean speech
  of at least 3 s, never for overlapping turns.

## Privacy

Cross-recording speaker comparison is disabled by default and requires the Hub
administrator to enable it:

```
GET/PUT /api/v1/admin/speaker-consent
```

When enabled, each eligible turn is encrypted into the Hub-only secret store;
SQLite stores a reference, never the vector. Deleting a profile or disabling
consent removes the profile, its embeddings, its associations and the
ciphertext. The guarantee is logical deletion — rows, references and files stop
being reachable and are removed — not a claim of physical erasure from SQLite
pages, WAL files or rotated backups.

## Failure codes

Every executor failure is typed, and retry policy reads the code, never the
message:

| code | retryable | meaning |
| --- | --- | --- |
| `model_unavailable` | no | weights missing, gated model without access, runtime cannot load it |
| `unsupported_operation` | no | the runtime does not implement the operation |
| `unsupported_input` | no | media outside a measured profile, bad interval, wrong query kind |
| `invalid_output` | no | wrong dimension, NaN/Inf, zero vector, broken frame stream |
| `resource_exhausted` | yes | device memory or capacity temporarily exhausted |
| `deadline_exceeded` | yes | the operation exceeded its deadline |
| `cancelled` | no | the Hub cancelled the lease or the operator asked |

A production GPU profile must peak at or below 7168 MiB (8192 MiB device minus a
1 GiB reserve). A profile that exceeds it is rejected, never silently shrunk.

## What is not implemented here

- No ANN index and no standalone vector database. Retrieval is exact cosine over
  the active generation, filtered by the canonical shot universe.
- No model downloads at inference time. Missing weights are
  `model_unavailable`; installation is an explicit, hash-verified queue action.
- No biometric vectors in SQLite, model runs, logs, errors or HTTP responses.
- No diarization fallback to a non-commercial model. If the gated diarizer is
  unavailable, the operation reports `model_unavailable`.
