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
   never bundled with the application. The release packages
   `nexusgate-inference.tar.gz` beside the binaries — it unpacks as
   `inference/…` with the module, `inference/models.lock.json` and the pinned
   requirements; extract it to a dedicated directory such as `/opt/nexusgate`
   and build the venv against `inference/requirements/torch.lock`. On Docker,
   use the opt-in `inference` image instead (section below).
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

3. Install the model artifacts from the catalog. The Hub serves HTTPS by
   default — `NEXUSGATE_TLS_MODE=auto` generates a self-signed certificate at
   `<NEXUSGATE_DATA_DIR>/hub-cert.pem` on first start — so every call below,
   read or mutation, trusts exactly that generated certificate rather than
   dropping TLS or disabling verification. On a Compose install, copy it out
   first with `docker compose cp hub:/var/lib/nexusgate/hub-cert.pem .` and
   point `--cacert` at the copy:

```bash
curl -sS --cacert "$NEXUSGATE_DATA_DIR/hub-cert.pem" \
  -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  https://127.0.0.1:8787/api/v1/admin/inference/catalog
# then, for each entry you want, echo back its manifest_digest:
curl -sS --cacert "$NEXUSGATE_DATA_DIR/hub-cert.pem" \
  -X POST -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"manifest_digest":"sha256:...","license_accepted":true}' \
  https://127.0.0.1:8787/api/v1/admin/inference/models/visual-siglip2-base/install
```

4. Switch operations on. Every operation defaults to **off**, so an upgrade
   never starts model work by itself:

```bash
curl -sS --cacert "$NEXUSGATE_DATA_DIR/hub-cert.pem" \
  -X PUT -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"enabled":{"visual_embedding":true,"sound_events":true}}' \
  https://127.0.0.1:8787/api/v1/admin/multimodal/settings
```

Saving settings queues missing work for assets that already exist. Enqueue is
idempotent: the task key hashes the operation, the source identity, the
immutable model revision, the preprocessing/runtime/dimension identity and any
consent/profile generation, so pressing save twice creates nothing.

### Enroll a Worker for inference

Pairing tokens issued without grants authorize derive work. Issue a scoped
token from the Hub as an administrator, then use the returned token with the
normal `nexusgate worker enroll` command. Select the inference operations and
maintenance actions that this Worker is allowed to perform.

```bash
curl -fsS --cacert "$NEXUSGATE_DATA_DIR/hub-cert.pem" \
  -H "Authorization: Bearer $NEXUSGATE_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"grants":{"inference_operations":["visual_embedding"],"model_management_operations":["install","probe"]}}' \
  https://127.0.0.1:8787/api/v1/hub/worker-pairings
```

Start the Worker with `--inference-python`, `--inference-module`,
`--inference-models` and the device profile for its locked environment. Add
`"worker_id":"<enrolled-worker-id>"` to the model install request above to
target that Worker. Successful maintenance records the exact locked version
on the Hub. The Worker can lease inference for that version when its
heartbeat reports it available. Re-enroll Workers configured before scoped
pairing to grant them inference access.

## Docker: the opt-in `inference` image

The default image stays lean — no Python interpreter, no torch wheels. The
opt-in `inference` build target layers the locked CPU environment (built from
`inference/requirements/torch.lock`, venv at `/opt/nexusgate-venv`) and the
checked-in module (`/opt/nexusgate`) on the plain runtime image:

```sh
docker build --target inference -t nexusgate:inference .
```

If any pin in the lock cannot be resolved or installed, the build fails — a
half-built environment is never published as a working runtime.

`deploy/docker-compose.inference.yml` is the shipped Compose override. It
switches both services to `nexusgate:inference`, enables the Hub runtime with
`NEXUSGATE_INFERENCE_ENABLED=true` plus `NEXUSGATE_INFERENCE_PYTHON`,
`NEXUSGATE_INFERENCE_MODULE_DIR=/opt/nexusgate` and
`NEXUSGATE_INFERENCE_DEVICE=cpu`, and passes the matching
`--inference-python`, `--inference-module`, `--inference-device cpu` and
`--inference-models` flags through the Worker command:

```sh
NEXUSGATE_MEDIA_ROOT=/path/to/footage \
  docker compose -f docker-compose.yml -f deploy/docker-compose.inference.yml \
  build hub
NEXUSGATE_MEDIA_ROOT=/path/to/footage \
  docker compose -f docker-compose.yml -f deploy/docker-compose.inference.yml \
  up -d hub
# Enroll the Worker as in docs/v0.14-deployment.md, then:
NEXUSGATE_MEDIA_ROOT=/path/to/footage \
  docker compose -f docker-compose.yml -f deploy/docker-compose.inference.yml \
  --profile worker up -d worker
```

The two sides keep separate model directories in separate volumes —
`/var/lib/nexusgate/models` for the Hub, `/var/lib/nexusgate-worker/models` for
the Worker — and neither is created or populated by the image: a fresh Hub can
queue an install before any model directory exists, and weights arrive only
through the hash-verified install path in step 3. A probe against a missing or
empty model directory reports `model_unavailable` entries; it never downloads
anything. The GPU derive path is unaffected: inference here is the CPU
environment, and the `gpu` build target remains a separate choice.

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
