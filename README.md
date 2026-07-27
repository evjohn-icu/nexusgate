# Re:Footage

> **Expired Footage, Reclaimed.**

Re:Footage is the product name. `timingdex` remains the compatible binary and
configuration namespace for existing libraries and Workers.

Local-first intelligent video footage library.

`Import → Understand → Search → Select`

## v0.16 — Worker-direct Provider Calls + Operations

Timingdex now supports two explicit execution paths for a paired, trusted
Worker. **Proxy mode** relays small JSON Provider requests through the Hub and
keeps the Provider key Hub-side. It rejects video, audio, images, multipart
uploads and bodies larger than 2 MiB, so it never becomes a NAS media relay.

**Direct mode** is an intentional opt-in for trusted Workers that already see
the NAS media mount. Set `hub_security.allow_worker_provider_credentials` (or
`TIMINGDEX_ALLOW_WORKER_PROVIDER_CREDENTIALS=true`) only after reviewing the
device boundary. The Worker may request a task-bound Provider configuration
only while it owns that job lease and only for a declared operation. The key
is never written to Worker config, SQLite or logs, but a third-party long-lived
key is visible in that Worker’s memory; the five-minute lease is a delivery and
audit window, not upstream-key revocation.

`/providers` now supports edit, test, enable, disable and safe removal of
Provider channels without returning a key to the browser. `/workers` shows
Worker stage/progress, retry/failure history and optional preferred/required
derive-node routing. The library exposes processing state and saved,
non-sensitive Collections alongside its capture-context filters.

See [v0.16 operations and security](docs/v0.16-operations.md) for the mode
selection, deployment limits and real-machine acceptance checklist.

## v0.15 — Capture Memory + Provider Channels

Timingdex now treats a library as material with a capture history, not just a
folder of files. It recognises common Blackmagic, DJI, Sony, Canon, Apple,
GoPro, Insta360, Panasonic, Nikon, Fujifilm, RED and ARRI capture families;
persists source colour/RAW status; and groups compatible captures into
conservative shoot sessions after processing. The library browser can narrow
material by capture date, region label, camera, and shoot session while
retaining the thumbnail → metadata → shot-timeline layout.

Open `/providers` to add a capability-specific model channel. A channel may
contain several keys for the same Provider. Keys are encrypted in the
Hub-only `provider-secrets/` directory (mode `0700`; encrypted files mode
`0600`), never returned by the API, and never written to browser storage,
SQLite or Worker configuration. The Hub applies health-aware retry, cooldown,
load balancing and ordered fallback within each capability. Existing
environment-based provider configuration remains a compatible fallback.

Capture coordinates remain private: the normal asset and session APIs expose
only a region label. An administrator may request a source-precision location
for an individual asset through the protected Hub API when that is necessary.

RAW support is deliberate rather than fictional: Timingdex reliably
identifies BRAW/R3D/ARI/CRM/N-RAW and records the rendering state, but does
not claim to decode proprietary RAW sources without a compatible renderer.
HDR HLG/PQ and supported Log footage are routed through the colour-aware
preview boundary before thumbnails, timelines, or video understanding.

### v0.15 quick start

1. Start the Hub with `timingdex serve` and keep the generated
   `$TIMINGDEX_DATA_DIR/admin-token` private.
2. Open `/setup`, add a read-only library root, then scan it.
3. Open `/providers`, enter the Hub administrator token for the current page,
   and create only the capability channels you intend to use. The Provider
   selector is capability-aware, so it does not offer routes that Timingdex
   cannot execute.
4. Run the pipeline from `/progress`. When analysis completes, browse `/` by
   capture date, region, camera, or shoot session; each row remains
   **thumbnail → material context → usable shot timeline**.
5. Pair a trusted Windows, Linux x64, or Linux ARM64 Worker only when NAS-side
   proxy generation needs a separate machine. See the deployment guide below.

For operations, security boundaries, colour handling, known limits and the
recommended acceptance checklist, see [v0.15 operations guide](docs/v0.15-operations.md).

## v0.14 — NAS Hub + Workers

Timingdex can now run as a NAS Hub with paired Windows, Linux x64 or Linux
ARM64 Workers. The Hub owns the database, browser UI, source probing and
secrets; Workers own only local cache, FFmpeg work and a revocable node token.
Original media is mounted read-only, and `probe` reads it directly on the NAS.

Start the Hub with `timingdex serve`; it creates a self-signed HTTPS identity
by default and prints the Worker-pinning fingerprint. On first start it also
creates `$TIMINGDEX_DATA_DIR/admin-token` with mode `0600`. Use that token only
to call Hub management APIs, including the pairing-token endpoint; it must not
be entered into Worker configuration or browser storage. Then enroll a trusted
node:

```sh
timingdex worker enroll --hub https://nas:8787 --fingerprint <fingerprint> \
  --pairing <one-time-token> --name studio-windows \
  --mount <library-root-id>=D:\\NAS\\Footage
timingdex worker run
```

Workers stage original media into their own cache, upload generated thumbnail,
proxy and audio artifacts back to Hub storage, then report completion. The
Hub keeps only the output and workflow state, so those previews remain
available while a Worker is offline. Visit `/workers` to see node and workflow
status.

For a browser-managed Hub, open `/workers`, enter the administrator token for
the current page only, then generate a one-time Worker pairing token. The page
does not save the token in browser storage.

See [NAS Hub and Worker deployment](docs/v0.14-deployment.md) for Docker,
systemd and ARM deployment notes. v0.16 adds an explicitly opted-in trusted
Worker direct path and the safer Hub JSON proxy path; both preserve the rule
that Worker configuration never stores a Provider key.

Timingdex is a **Video Asset Intelligence Layer**, not an AI editor. v0.10
turns a natural-language production brief into a reviewable repurpose plan:
the optional Planner Agent decomposes the brief, while Timingdex controls shot
retrieval, deduplication, ranking and provenance.

## Current runnable vertical slice

1. Add one or more local library roots.
2. Incrementally scan MOV / MP4 / M4V files.
3. Create stable assets using sampled content fingerprints.
4. Probe media with ffprobe and optionally ExifTool.
5. Generate a thumbnail, 720p H.264 proxy and temporary 16 kHz audio.
6. Run a local speech gate.
7. Run the explicitly configured video-understanding provider (or leave jobs
   visibly blocked until one is configured).
8. Normalize controlled vocabulary and atomically commit validated analysis,
   tag links, shots and their search rows.
9. Build a SQLite FTS5 index.

## Model safety boundary

Model output never writes directly to the canonical asset tables.

```text
request/input hash
      ↓
model_runs immutable cache + staging
      ↓
parse + schema/vocabulary validation
      ↓
transactional commit to asset_analysis
      ↓
FTS5 rebuild
```

Failed, timed-out, malformed or incompatible responses remain in `model_runs`
and cannot contaminate trusted analysis, tags, shots or search rows.

## Requirements

- Go 1.23+
- ffmpeg / ffprobe
- ExifTool is recommended but optional for the current pipeline

## v0.6 — hardware-accelerated derived media

v0.6 accelerates the local, read-only **derived-media** stage only. It can use
hardware decode for thumbnail extraction and proxy generation, and hardware
H.264 encoding for the 720p proxy. The original file is never modified;
transcription, Gemini analysis, tag curation and SQLite indexing keep their
existing behavior.

`hardware.mode` chooses the profile:

- `auto` (default): macOS VideoToolbox; on Linux NVIDIA CUDA/NVENC, Intel QSV,
  then VAAPI, in that order.
- `cuda`: NVIDIA discrete GPU decode + NVENC proxy encode.
- `qsv`: Intel integrated/discrete graphics via Quick Sync Video.
- `vaapi`: Linux DRM render device, normally `/dev/dri/renderD128`.
- `videotoolbox`: Apple silicon / Intel Mac hardware media engine.
- `software`: force the existing libx264 path.

The detector checks the installed FFmpeg build rather than guessing from the
machine. Run `timingdex doctor` or query `GET /api/v1/hardware` to see the
selected profile. With `allow_fallback: true`, an unsupported codec, driver or
device automatically retries the same operation in software. Hardware and
software proxy/thumbnail cache paths are kept distinct, so a prior artifact is
never mislabelled as a new hardware result.

Example:

```json
{
  "hardware": {
    "mode": "auto",
    "allow_fallback": true,
    "proxy_bitrate_kbps": 1800
  }
}
```

## v0.7 — Multi Video Understanding Provider Layer

Video understanding is now provider-independent:

```text
video file
    ↓
VideoUnderstandingProvider Router
    ├── Gemini VideoProvider
    ├── Qwen VideoProvider
    ├── Volcengine VideoProvider
    ├── Local VLM Provider
    └── configured fallbacks
    ↓
VideoAnalysisResult
    ├── summary / scenes / shots
    ├── objects / actions / mood
    └── raw_tags + confidence
    ↓
legacy StructuredAnalysis compatibility mapping
    ↓
staging → validation → commit → Tag Curator
```

`providers.vision_primary` selects the first provider and
`providers.vision_fallback` lists fallback names in order. Gemini accepts the
new unified response and the previous flat analysis response during migration.
Set `vision_primary` to `none` when you only want to browse an existing
library. Analysis jobs then fail visibly until an enabled provider is chosen;
Timingdex never substitutes a local demo analysis for a missing provider.
The existing v0.6 hardware pipeline, Volcengine ASR/Plan providers, embedding
pipeline, Tag Intelligence and SQLite review boundary remain unchanged.

Shot-level search is the first v0.8 substrate: each provider-generated time
range is stored with its description, tags, mood and provenance. Results return
the original asset plus exact `start_ms` / `end_ms` ranges, so later Repurpose
Plans can reference clips without modifying source media.

## v0.9 — Repurpose Plan MVP

The v0.9 flow is deliberately planning-first:

```text
production brief
    ↓
optional small-LM Planner (or deterministic fallback)
    ↓
material needs
    ↓
Timingdex shot search and ranking
    ↓
reviewable Repurpose Plan with exact time ranges
```

Create a plan:

```bash
curl -X POST http://127.0.0.1:8787/api/v1/repurpose/plans \
  -H 'content-type: application/json' \
  -d '{"brief":"我要做一个深圳城市宣传视频","duration_ms":30000,"style":"城市生活","audience":"品牌客户"}'
```

Retrieve it with:

```bash
curl http://127.0.0.1:8787/api/v1/repurpose/plans/<plan-id>
```

Set `providers.repurpose_primary` to `openai_chat` and enable the
`providers.repurpose` block to use a local or OpenAI-compatible small model.
Set `repurpose_fallback_heuristic` to `false` when planner failure should be
surfaced instead of falling back. The Planner may propose semantic queries and
section durations, but it cannot create asset IDs, shot IDs or timestamps.
v0.9 produces a plan for an editor; it does not perform automatic editing or
NLE export.

## v0.10 — Pre-v1 Intelligence Closure

### Multi-model video understanding

Set `providers.vision_primary` to `gemini`, `qwen_video`,
`volcengine_video`, or `local_vlm`; list alternatives in
`providers.vision_fallback`. Qwen, Volcengine and local routes use the
OpenAI-compatible `video_url` chat shape and all return the same unified shot
schema. Their endpoint, authentication and model remain configuration-driven.

### Hybrid search and discovery

Timingdex now keeps an explainable local **heuristic feature vector** for each
shot's existing model description, tags, objects, actions and mood. It blends
that score with SQLite FTS5; it is not a learned visual embedding. No original
media is uploaded and no vector database is required. Chinese requests use a
parameterized literal-match fallback so short terms such as `雨夜` work with
the existing SQLite index.

```bash
curl --get --data-urlencode 'q=rainy city night' http://127.0.0.1:8787/api/v1/search/shots/hybrid
curl http://127.0.0.1:8787/api/v1/shots/<shot-id>/similar
curl http://127.0.0.1:8787/api/v1/discover/rare-shots
```

`rare-shots` is intentionally library-relative: it identifies uncommon visual
semantic combinations, not a universal claim that a shot is high quality.

### Repurpose review workflow

Creating a plan now creates revision 1. Editors can submit revised ordered
sections/candidates and a note, then approve only the latest draft. Approval
locks that plan from further edits so the selection is stable for v1.0 NLE
handoff.

When a required section has no unused candidate but a previously selected
shot is still the only valid match, Timingdex keeps the section complete and
marks that candidate as `reused: true`. This makes a deliberate editorial
trade-off visible instead of silently omitting an ending.

### Processing reliability

Transient pipeline failures are returned to the local queue with a bounded
1s → 2s → 4s (maximum 30s) backoff, up to the existing job attempt limit.
Configuration, validation and missing-input failures remain final so a bad
setup does not consume provider quota repeatedly.

```bash
curl -X POST http://127.0.0.1:8787/api/v1/repurpose/plans/<plan-id>/revisions \
  -H 'content-type: application/json' \
  -d '{"editor_note":"人工调整开场","sections":[{"role":"opening","query":"city","duration_ms":5000,"required":true,"candidates":[]}]}'
curl http://127.0.0.1:8787/api/v1/repurpose/plans/<plan-id>/revisions
curl -X POST http://127.0.0.1:8787/api/v1/repurpose/plans/<plan-id>/revisions/2/approve
```

## Build

```bash
go mod tidy
go test ./...
go build -o timingdex ./cmd/timingdex
```

## First run

```bash
export TIMINGDEX_DATA_DIR="$PWD/.timingdex-dev"

./timingdex doctor
./timingdex root add /path/to/footage
./timingdex root list
./timingdex root scan <root-id>
./timingdex pipeline run
./timingdex serve
```

The scan command enqueues idempotent jobs. `pipeline run` leases and executes jobs until the queue is idle.

## Local review workspace

After `./timingdex serve`, use the local pages below instead of assembling the
workflow from cURL calls:

- `/setup` — choose the video provider, paste an API key only to generate a
  copyable terminal command, and check the running local service. Keys are not
  posted, stored or logged by this page.
- `/progress` — run the pending local queue, poll job counts and errors, and
  retain a small browser-session activity log. It is intentionally not a
  durable audit-log system.
- `/repurpose` — enter an editorial brief, inspect per-section shot candidates
  with exact time ranges and reasons, then approve the latest immutable plan
  revision.
- `/` — the primary footage library: every material row places the thumbnail,
  semantic summary and tags beside a horizontal, shot-level timeline. Timeline
  blocks use the persisted shot start/end ranges, so an editor can see what
  exists at each point in the source before entering a planning workflow.
- `/repurpose` — a secondary planning workspace, reached after material
  discovery rather than replacing the library home page.

### NAS / low-power NAS workflow

Timingdex runs on the editing machine, not on the NAS. Mount the NAS share in
Finder (SMB/NFS), add that mounted folder as a normal library root, and select
the setup page's **NAS / network folder (stage locally first)** option. It adds
`TIMINGDEX_SOURCE_STAGING_MODE=copy` to the launch command.

In copy mode the original source is opened read-only, then copied once to
`$TIMINGDEX_DATA_DIR/cache/sources/` before FFmpeg, ASR, proxy generation or
video understanding begins. The Mac therefore performs all CPU/GPU work and
can continue using a completed local cache entry if the share later disconnects.
The NAS receives no derived files, sidecars or metadata writes. Budget local
disk space for the original files being processed; this cache is disposable and
may be removed manually while Timingdex is stopped.

Equivalent `config.json` setting:

```json
{
  "source_staging": { "mode": "copy" }
}
```

### v0.11 editorial selection

The Repurpose workspace is now an editor-controlled decision surface. For
each section, choose one candidate, optionally lock that choice, exclude weak
alternatives, add an editorial note, and save a new immutable draft revision.
A locked selection can only be changed in a later revision after the user
explicitly unlocks it. Approval rejects a required section that has available
candidates but no explicit choice; material gaps remain visible rather than
being silently approved.

### v0.12 Agent Skill

The project includes a versioned Agent Skill at `skills/timingdex`. It can use
the local API to inspect readiness, retrieve shot evidence, create a draft
plan, and submit a user-directed draft revision. Start a Skill session by
pointing it to that folder and the local server URL. Before any action it calls
`GET /api/v1/agent/capabilities`; the response declares that plan approval,
pipeline execution, provider keys and original-media paths are out of bounds.
The Skill never calls the approval endpoint: approval remains a human action in
the Repurpose workspace.

## API examples

```bash
curl http://127.0.0.1:8787/api/v1/health
curl http://127.0.0.1:8787/api/v1/assets
curl http://127.0.0.1:8787/api/v1/jobs
curl -X POST http://127.0.0.1:8787/api/v1/pipeline/run
curl --get --data-urlencode 'q=demo' http://127.0.0.1:8787/api/v1/search
curl http://127.0.0.1:8787/api/v1/assets/<asset-id>/shots
curl --get --data-urlencode 'q=雨夜街道' http://127.0.0.1:8787/api/v1/search/shots
```

## Provider status

- StepFun SSE and Qwen OpenAI-compatible ASR adapters write immutable transcript runs.
- Gemini native Files API / `generateContent` and OpenAI-compatible relay modes write raw responses to `model_runs`.
- Provider fixtures cover request paths, authentication, payload shapes and response decoding without calling paid APIs.
- Local Web footage-library browser, setup, processing progress, Repurpose
  review, and Tag Curator review pages are runnable.

## AI providers and relay stations

Timingdex supports direct provider endpoints and relay/reverse-proxy endpoints through the same configuration.
Copy `config.example.json` to `$TIMINGDEX_DATA_DIR/config.json` and edit the provider blocks.

Relay-related fields:

- `base_url`: official API or relay base URL.
- `path`: endpoint path exposed by the relay.
- `protocol`: `gemini_interactions` or `openai_chat` for video analysis.
- `auth_header`: usually `Authorization`, or a custom header such as `x-goog-api-key`.
- `auth_scheme`: `Bearer`, another scheme, or `raw` when the key value must be sent without a prefix.
- `extra_headers`: arbitrary headers required by a relay.
- `model`: relay-side model alias; it does not have to match the official model name.

Example Gemini-compatible relay:

```json
{
  "enabled": true,
  "protocol": "gemini_interactions",
  "base_url": "https://relay.example.com/gemini/v1beta",
  "path": "interactions",
  "api_key_env": "TIMINGDEX_RELAY_KEY",
  "model": "gemini-video-fast",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "extra_headers": {"X-Client": "timingdex"}
}
```

Example OpenAI-compatible multimodal relay:

```json
{
  "enabled": true,
  "protocol": "openai_chat",
  "base_url": "https://relay.example.com/v1",
  "path": "chat/completions",
  "api_key_env": "TIMINGDEX_RELAY_KEY",
  "model": "gemini-3.6-flash"
}
```

The OpenAI-compatible video adapter sends a `video_url` data URL. The relay must explicitly support video content; text/image-only OpenAI-compatible gateways are not sufficient.

Current ASR behavior:

1. Speech Gate decides whether transcription is needed.
2. `asr_primary` is called.
3. On provider failure, `asr_fallback` is attempted.
4. Successful transcripts are cached by `asset_id + input_hash`.
5. Vision analysis consumes the transcript when available.

## Volcengine Agent Plan / Coding Plan

Timingdex treats the two Volcengine personal plans as **separate quota and key
domains**, not as aliases of a generic relay:

| Plan | Timingdex roles | Base URL |
| --- | --- | --- |
| Agent Plan | Tag Curator / library summary LLM, tag embeddings, Seed ASR 2.0 | `https://ark.cn-beijing.volces.com/api/plan/v3` |
| Coding Plan | Tag Curator / library summary LLM, plan embedding entitlement | `https://ark.cn-beijing.volces.com/api/coding/v3` |

The plan-specific LLM and embedding adapters use the documented
OpenAI-compatible API shape. Seed ASR 2.0 is different: it uses the documented
native WebSocket binary protocol, so it is exposed only as `volcengine_asr` and
is never sent to an OpenAI audio endpoint.

Copy the `volc_*` blocks from `config.example.json`, export only the key for the
plan you actually subscribe to, then select the role-specific providers:

```json
{
  "providers": {
    "asr_primary": "volcengine_asr",
    "asr_fallback": "qwen",
    "tag_curator_primary": "volc_agent_plan",
    "embedding_primary": "volc_agent_plan_embedding",
    "volc_agent_plan": { "enabled": true },
    "volc_agent_plan_embedding": { "enabled": true },
    "volc_asr": { "enabled": true }
  }
}
```

For Coding Plan, select `volc_coding_plan` and/or
`volc_coding_plan_embedding` instead. It has its own API key environment
variable, so Agent Plan traffic cannot accidentally draw down Coding Plan quota.
The exact plan model inventory changes over time: set `model` to a model enabled
for your subscription in the Volcengine console rather than treating the example
model names as a fixed allow-list.

The ASR adapter defaults to offline `bigmodel_nostream`, which fits an imported
footage asset: it converts the derived audio to 16 kHz mono PCM, sends it in
200 ms chunks, and receives the final transcript. It uses
`X-Api-Key`, `X-Api-Resource-Id: volc.seedasr.sauc.duration`, and a unique
connection ID. Its wire-level `request_model` remains `bigmodel` (the catalog
name stored alongside the transcript is `doubao-seed-asr-2.0`). A missing subscription capability or wrong resource ID fails the
ASR run and therefore activates the configured fallback; it cannot silently
fall back to a different, older ASR model.

## v0.4 additions

- Gemini Files API resumable upload and `provider_files` cache.
- Gemini native `generateContent` with reusable `file_uri`; OpenAI-compatible relay mode remains available.
- Optional forced-alignment workflow through a strict external-command JSON contract.
- `alignment_runs` and `transcript_words` staging tables.
- Local footage grid at `http://127.0.0.1:8787/`.
- Asset card/detail and thumbnail/proxy endpoints.

### Alignment command contract

When `providers.alignment.enabled` is true, Timingdex executes the configured command and writes one JSON object to stdin:

```json
{
  "audio_path": "/absolute/path/audio.m4a",
  "language": "zh",
  "text": "完整转写文本",
  "segments": []
}
```

The adapter must write JSON to stdout:

```json
{
  "words": [
    {"start_ms": 1320, "end_ms": 1680, "text": "我们", "confidence": 0.98}
  ]
}
```

This keeps Python, CUDA and forced-aligner dependencies outside the Go process. A Qwen3-ForcedAligner wrapper can implement this contract without changing Timingdex.

### Gemini file cache

The proxy artifact profile hash, asset ID and provider name form the cache key. An ACTIVE file is reused across prompt/schema revisions. Model output still passes through `model_runs` staging before `asset_analysis` is committed.

## v0.5 — Tag Curator

v0.5 adds a governed tag layer without allowing an agent to edit the main database directly.

Flow:

```text
AI raw tags
→ normalize
→ exact canonical/alias resolution
→ unresolved pool
→ bounded Tag Curator
→ staged proposals
→ human approve/reject
→ transactional alias/catalog update
→ affected asset links updated
```

New local page:

```text
http://127.0.0.1:8787/tags
```

New APIs:

```text
GET  /api/v1/tags
GET  /api/v1/tags/unresolved
POST /api/v1/tags/curate
GET  /api/v1/tags/proposals?state=pending
POST /api/v1/tags/proposals/{id}/review
```

Review body:

```json
{"action":"approve","note":"optional"}
```

The heuristic curator is intentionally conservative. The optional small-model provider implements the same proposal contract, but it cannot emit SQL or bypass review/staging.

## v0.5.1 — Engineering closeout

v0.5.1 keeps the v0.5 product boundary and closes four implementation gaps:

1. **Small-model Tag Curator** — an optional OpenAI-compatible local/small model groups unresolved tags. Its output is filtered against the unresolved input set, converted to bounded proposals and still requires human approval. Invalid or invented aliases are discarded.
2. **Heuristic fallback** — provider failures fall back to `heuristic-v1` by default. Set `tag_curator_fallback_heuristic` to `false` to fail the run instead.
3. **Alias-expanded search** — a search for a canonical tag or any approved sibling alias resolves to every asset linked to that canonical tag. Raw tags and FTS content are not rewritten.
4. **Integration and provider fixtures** — tests cover all SQLite migrations, tag proposal approval, alias search, StepFun SSE, Qwen ASR, Gemini native analysis / resumable upload and the OpenAI-compatible Tag Curator.

To enable a local small model, configure an OpenAI-compatible endpoint:

```json
{
  "providers": {
    "tag_curator_primary": "openai_chat",
    "tag_curator_fallback_heuristic": true,
    "tag_curator": {
      "enabled": true,
      "protocol": "openai_chat",
      "base_url": "http://127.0.0.1:1234/v1",
      "path": "chat/completions",
      "model": "your-small-instruct-model"
    }
  }
}
```

The model can only return semantic groups. Timingdex derives database IDs and proposal types itself, rejects aliases not present in the unresolved pool, and writes nothing to the canonical tag catalog until a user approves the staged proposal.

## v0.5.2 — Tag Intelligence

v0.5.2 turns the Curator into a semantic-governance layer while preserving the
v0.5 approval boundary:

```text
Gemini / Vision raw tags (high recall)
  → real embedding API finds candidate groups
  → Tag Curator names / maps only those candidates
  → staged proposals
  → human approve or reject
  → canonical tag knowledge base
```

The embedding API is **not** used to search footage and has no access to raw
video. It embeds normalized unresolved tag strings only. No embedding provider
means clustering is unavailable; Timingdex deliberately does not pretend a
lexical fallback is semantic clustering.

New APIs:

```text
POST /api/v1/tags/clusters?limit=500&threshold=0.86
GET  /api/v1/library/summary
POST /api/v1/library/summary/generate
```

`POST /api/v1/tags/clusters` persists provider vectors and transparent candidate
membership in SQLite, then emits the normal pending proposals. It never applies
a merge automatically. `threshold` is cosine similarity; it is intentionally
conservative by default.

Library summary generation consumes only a statistics snapshot (asset count,
analysis coverage, and top approved canonical tags). With a configured Curator
it returns a constrained JSON draft (`summary`, `themes`, `suitable_for`). If the
Curator is unavailable and heuristic fallback is enabled, a clearly labelled
deterministic summary is stored instead.

### Curator / embedding provider options

The Curator can use an OpenAI-compatible endpoint (`openai_chat`) — suitable for
Qwen-compatible endpoints and Ollama gateways — or Gemini native
`gemini_generate_content`. The same Curator generates library summaries.

Embedding supports `openai_embeddings` (Qwen/Ollama/OpenAI-compatible) and
`gemini_embed_content` (one tag per native Gemini request). Example:

```json
{
  "providers": {
    "embedding_primary": "openai_embeddings",
    "embedding": {
      "enabled": true,
      "protocol": "openai_embeddings",
      "base_url": "http://127.0.0.1:11434/v1",
      "path": "embeddings",
      "model": "nomic-embed-text"
    }
  }
}
```
