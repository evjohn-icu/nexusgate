# Re:Footage

**You have terabytes of footage you can't find anything in. This fixes that.**

[中文说明 → README_CN.md](README_CN.md)

`Re:Footage` is the product; `nexusgate` is the binary, the module and the
config namespace.

---

## The problem

Your drive or NAS has years of clips on it. `DJI_0284.MP4`. `A001_C007.mov`.
`IMG_4471.MOV`. You know there's a good sunset drone shot in there somewhere.
Finding it means scrubbing through folders for forty minutes, and usually you
give up and shoot something new.

Filenames don't tell you what's in a clip. Neither do folders. The only thing
that does is watching it.

## What this does

It watches your footage for you — once — and remembers.

It reads your existing folders (**never writes to them**), splits each clip
into shots, and builds a searchable index down to the individual shot. Then you
ask for what you want in plain language and get back the clip *and the
timecode*:

```
"drone shot pulling back over a coastline at sunset"
→ travel/DJI_0284.MP4    00:12–00:24    ← the shot, not just the file
→ b-roll/A001_C007.mov   01:47–02:03
```

Everything runs on your own machine. Your footage never leaves it.

**Concretely, you get:**

- **Shot-level search** — over what's visible, what's said, and metadata, with
  an evidence gate that separates "this ranked high" from "this actually
  contains what you asked for". It will answer *unknown* rather than guess.
- **Proxies and thumbnails** — generated once, so browsing stays instant even
  when the originals are 16 GB ProRes on a spinning disk.
- **Transcripts** — searchable as complete phrases, not loose keywords.
- **A browser UI, a CLI, an HTTP API and an MCP server** — use whichever fits.

## What this is not

Being straight with you, because the alternative wastes your evening:

- **This is alpha.** The current release is `v0.38.0-alpha`. It works. It is
  not finished. It will change.
- **Don't expose it to the internet.** It is built for your own machine or a
  trusted LAN. The Hub refuses to start if it detects the careless version of
  this inside a container.
- **The smart parts need a model provider.** Scanning, proxies, thumbnails,
  metadata search and shot detection are fully local. Transcription and visual
  understanding call a provider you configure — or a local model you run
  yourself. Without one you still get a browsable, deduplicated, proxy-backed
  library; you just don't get semantic search.
- **It is not an editor** and not a media manager. It finds things. Your NLE
  does the rest — there is a timeline export for that.

---

## Install

### Docker — shortest path, nothing to install

```bash
git clone https://github.com/evjohn-icu/nexusgate.git && cd nexusgate
NEXUSGATE_MEDIA_ROOT=/path/to/footage docker compose up -d --build hub
```

The image compiles Go and brings its own FFmpeg and exiftool, so the complete
prerequisite list is: Docker. This is also the shortest path on Windows.

Your footage appears inside the container at `/media/library` — use *that*
path when you add it below, not the host path.

### From source

```bash
git clone https://github.com/evjohn-icu/nexusgate.git && cd nexusgate
go build -o nexusgate ./cmd/nexusgate
```

You will need:

| | |
|---|---|
| **Go** | 1.25.5 — `go.mod` is the truth if this line ever drifts |
| **ffmpeg / ffprobe** | any recent build. 5.1+ for the read-rate limiter (`-readrate` does not exist before that; older builds skip that one setting with a warning instead of failing every render) |
| **exiftool** | optional. Without it, capture metadata is whatever ffprobe exposes |

No database server, no web framework, no frontend build step. Nine direct
dependencies, all listed in `go.mod`.

---

## First run

```bash
export NEXUSGATE_DATA_DIR="$PWD/.nexusgate-data"

./nexusgate doctor                     # what is missing? fix what it names first
./nexusgate root add /path/to/footage  # read-only. nothing is ever written there
./nexusgate root list                  # copy the root id it prints
./nexusgate root scan <root-id>        # walk, fingerprint, then process
./nexusgate serve                      # opens on https://127.0.0.1:8787
```

Then open the browser UI.

**A word about `root scan`, because the first one surprises people.** It walks
your library and then drains the whole processing queue in the same command. On
a 50-clip library that is minutes. On a 5000-clip NAS it is an overnight job —
it reads every file across the network and transcodes a proxy for each one.
Ctrl-C is safe: what is done stays done, and the next run picks up where it
stopped. Watch the `queued=` number it prints — that is how much work it
actually created.

Later scans are fast. Files whose size and mtime have not changed are never
re-read.

Everything lives under `$NEXUSGATE_DATA_DIR` — database, cache, tokens.
**Deleting that directory is how you reset.** Your footage is untouched by
that, because nothing was ever written next to it.

### The two tokens

The Hub generates two credentials on first start, both mode `0600` inside the
data directory:

- **`admin-token`** — full control. Yours. Never give it to an agent, never put
  it in a Worker config, never in browser storage.
- **`agent-token`** — read and draft only. This is the one you hand to an
  assistant.

On a trusted LAN, reads need no token at all; writes do. From anywhere else,
both do.

---

## Let an agent install it for you

This repository is written to be read by a coding agent, and setting it up is a
good job for one.

```bash
git clone https://github.com/evjohn-icu/nexusgate.git && cd nexusgate
claude   # or any coding agent that can read files and run commands
```

Then tell it, in roughly these words:

> Get this running against my footage at `/path/to/footage`. Read `CLAUDE.md`
> first, then run `nexusgate doctor` and fix whatever it reports before you
> scan anything.

**Why this works rather than being a gimmick:** `CLAUDE.md` at the repo root is
a real architecture document — the layer map, the pipeline, the invariants that
are easy to break. And `nexusgate doctor` is written to be *acted on*: it names
the missing binary, the wrong directory mode, the provider whose key never
resolved. Between the two, an agent has what it needs to take you from a clone
to a running Hub without you learning the CLI first.

Be clear-eyed about what is happening: there is no installer script. The agent
reads the same docs and runs the same commands you would. What you are buying
is not having to do it yourself.

**Two things to tell it.** Give it the **agent token**, not the admin one. And
if it offers to open the Hub to the network, say no.

## Using it with an agent afterwards

Once the library is indexed, an assistant can search it for you.

**MCP** — `cmd/nexusgate-mcp` speaks MCP over stdio and exposes six read-only tools:
`inspect_library`, `search_shots`, `get_shot`, `get_asset`,
`get_timeline` and `get_transcript`. It is a thin HTTP client of your Hub: no
database handle, no access to your media files. Example config in
`skills/nexusgate/mcp/.mcp.json.example`, and a ready-made Claude Code plugin
in `plugins/claude`.

**Agent Skill** — `skills/nexusgate/SKILL.md` is a versioned contract for
editorial research: find reusable material, retrieve shot evidence, draft a
plan. Before acting it calls `GET /api/v1/agent/capabilities`, which declares
what is out of bounds — plan approval, pipeline runs, provider keys and raw
media paths. That declaration is enforced by the routes, not merely promised in
a prompt.

**Approval stays human.** An agent may draft a plan. It cannot approve one.

---

## Going further

Everything above is the short version. When you need more:

| | |
|---|---|
| **NAS and network shares** | a guided mount wizard in the browser UI generates the exact command for your platform — Linux, macOS, Windows and Unraid — plus a ready-made Docker Compose volume definition |
| **Model providers** | configure them in the browser at `/providers`. Keys are stored encrypted and never appear in the database, the API, logs or error messages |
| **Distributed processing** | pair Worker nodes to a Hub so transcoding runs on the machine with the GPU — see `docs/v0.31-deployment.md` |
| **Disk load** | off-peak windows, read-rate limits and a cache size cap, so a scan does not saturate your NAS for hours |
| **Architecture** | `CLAUDE.md` — layer map, pipeline, and the invariants worth knowing before changing anything |
| **Everything else** | `docs/` — deployment, search architecture, per-version release notes |

## Contributing

Read `CONTRIBUTING.md`. The short version: the dependency rule is real (nine
direct dependencies; adding one is a conversation), and `go test ./...` plus
`gofmt -l .` must be clean.

## License

Apache-2.0. See `LICENSE`.
