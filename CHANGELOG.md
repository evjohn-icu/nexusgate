# Changelog

## Unreleased

- **The public asset detail no longer hands out absolute disk paths** (audit
  F3-03). `GET /api/v1/assets/{id}` returned `thumbnail_path` and
  `proxy_path` verbatim — absolute locations under the Hub's data directory,
  carrying the operator's username and cache layout — to every trusted-read
  caller: any LAN peer, and any agent token from any network. The handler's
  own redaction block, two lines above, cleared the location's absolute path,
  the file id and the coordinates, and promised that absolute paths stay
  behind the administrator boundary; these two fields were simply never added
  to it. Nothing in the tree reads them — the browser and the Skill both fetch
  artifacts through `/assets/{id}/thumbnail` and `/proxy`, which serve the
  bytes without naming the file — so they are now cleared outright.

- **Three gates now hold the docs to the code** (audit's root-cause batch).
  Fourteen documentation drifts were repaired by hand on 2026-08-30, but
  nothing stopped the fifteenth: `scripts/check-doc-refs.sh` only validates
  that `file:line` references in `docs/` are in range, and checks no semantic
  claim at all. Added: `TestAgentCapabilitiesMatchesSkillMD` /
  `...MatchesAPIContractDoc` (the live `/api/v1/agent/capabilities` response
  must equal the action lists `SKILL.md` and `api-contract.md` print);
  `TestSkillsDocumentedEndpointsExistInRouteInventory` /
  `TestAgentReachableRoutesAreDocumentedOrAllowlisted` (both directions —
  a renamed route leaves a dangling doc reference, and a new agent-reachable
  route must be documented or carry a written reason in an allow-list that is
  itself checked for staleness); `TestMCPToolsMatchUsageDoc` (registered tool
  names equal the doc's table, and each tool's documented HTTP method and path
  fragments are found in the handler's own source via `go/ast`); and
  `scripts/check-tool-count.sh`, wired into CI, which fails closed when any of
  the four manifests claiming a tool count disagrees with `AddTool(`.

- **Narrowing a filter refines the search instead of discarding it** (audit
  U3-05, found while writing the U3-04 browser test). Every filter control on
  the library page carried `onchange="load()"`, and `load()` is the asset
  browse listing. Once the search became shot-first, that meant the single
  gesture a user makes to refine a result set was the one that threw it away:
  the shot cards vanished and the unfiltered asset list came back. The
  handlers simply predated the refactor. All nine controls, plus Apply, Clear,
  chip removal and collection selection, now go through `refreshResults()`,
  which re-runs the search when there is a query and is exactly the old browse
  listing when there is not.

- **New files under `cmd/timingdex-mcp/` are visible to git again** (audit
  G1-07).
  `.gitignore` carried a bare `timingdex-mcp` for the built binary, which also
  matched the `cmd/timingdex-mcp` *directory*, so every new file added there
  was silently invisible to `git status` and `git add` — already-tracked files
  kept working, so it only ever bit new ones, which is the worst way for this
  to fail. Anchored to `/timingdex-mcp` (and `/timingdex-corpusgen`, which had
  the same problem), matching the `/timingdex` two lines above it that was
  always right.

- **Worker enrollment and job status say whose mistake it was** (audit F7-02,
  F7-03). Two reachable Worker-surface refusals answered 500: an unknown job
  id on `GET /api/v1/admin/worker-jobs/{id}`, and an enrollment carrying a
  valid pairing token with no `name` or `platform`. Both are the caller's
  mistake, and "internal error" sent the operator to read Hub logs for
  something the Hub had answered correctly; they are now 404 and 400, with
  the 400 naming the missing fields. The enrollment classifier that decided
  between them was `strings.Contains(err.Error(), "pairing token")` — the one
  thing this repository forbids everywhere else, and whose failure mode is
  that rewording a message silently reclassifies it. The three refusals now
  carry sentinels. `ErrPairingTokenInvalid` moved from `internal/app` to
  `internal/domain` (with an alias left behind, so one condition keeps one
  identity) because only the repository can decide it without a race and the
  repository cannot import `internal/app`. An incomplete registration is
  deliberately not the 401 "enrollment rejected": that answer would send an
  operator off to regenerate a pairing token that was fine.

- **"Similar shots" respects the filters on screen** (audit U3-04). It is a
  separate code path from the search POST and carried neither facets nor the
  asset context, so a shot the operator had just filtered out of the result
  list could come straight back through the drawer. `GET
  /api/v1/shots/{id}/similar` now takes the same asset-context filter the
  browse listing does, under the same query-string param names
  (`date_from`/`region`/`camera`/`session`/`status`), and 400s on an
  unparseable date or an unknown status rather than compiling it into a WHERE
  clause that matches nothing.

- **`get_shot` no longer has a size cliff** (audit M1-04). It was the last
  transcript-bearing MCP tool still decoding through `do()`'s 1 MiB bound;
  it now uses `getLarge` like `get_asset`, `get_transcript` and
  `get_timeline`. The audit called the embedded word list "unbounded",
  which overstates it — `GetShot` reads only the words inside the shot's own
  time range, so it cannot grow with asset length. The cliff is real anyway:
  nothing anywhere caps how long a shot may be, one locked-off take is one
  shot, and roughly twenty thousand aligned words crosses the bound. Past it
  the tool returned "unexpected end of JSON input", which gives an agent no
  next step.

- **The Worker setup wizard announces which step you are on** (audit U4-03).
  `aria-current="step"` was written into the markup on step 1 and `goStep()`
  only ever rewrote `className`, so a screen reader was told "step 1 of 4"
  for the whole flow — including on the page that mints a one-time pairing
  token, where knowing you have reached the generate step matters. Passed
  steps now read as done through `.steps span.is-done`, a rule the shell has
  styled since v0.32 that this page never set. The step nav is a labelled
  landmark, the four panels are labelled groups, and the four containers the
  wizard writes into are live regions. Unlike `/library-roots`, the visual
  sequence was never broken here: `.step-panel{display:none}` was already
  declared.

- **A Hub with no providers says so instead of shrugging** (audit X1-01).
  `classifyJobFailure` had no branch that could ever return
  `JobFailureCategoryConfiguration`; the constant's own comment said "No
  sentinel produces it yet". A cold-start Hub with nothing configured therefore
  landed all five of its terminal failures in `unknown`, and `/api/v1/issues`
  showed an unlabeled bucket with no repair link — while the progress page has
  carried the `configuration` → `/providers` mapping and its five translations
  all along, waiting for a producer nobody wrote. The three provider-channel
  sentinels that mean "the deployment is incomplete"
  (`ErrProviderChannelNotConfigured`, `errProviderChannelSecretMissing`,
  `errProviderChannelMultiframeUnsupported`) now map to it. Deliberately not
  included: a spent key (401/402/403), which stays `provider_auth` because the
  deployment is fine and the credential is dead, and `ErrNoRoute` /
  `ErrNoAvailable`, which clear on their own once a cooldown expires.

- **The `/library-roots` wizard shows one step at a time** — a defect the audit
  did not find and the accessibility pass surfaced. `goStep()` toggles `.active`
  on `.step`, and nothing in the shell or the page ever declared
  `.step{display:none}`, so all four panels rendered at once: a first-time
  visitor met an empty mount-point field, an empty verify result and a scan
  panel before typing anything, under a numbered nav describing a sequence that
  was not happening. `worker_setup_page.go` had this right with `.step-panel`.

- **The `/library-roots` wizard is usable without sight** (audit U2-05). The
  largest and most complex page in the product — and the one a new user meets
  first — had zero `aria-*` and zero `role=` across 445 lines. Every container
  the wizard writes an outcome into is now a live region, so the feedback loop
  is no longer silent: without it a button is pressed, text appears somewhere,
  and nothing is announced. The step nav is a labelled landmark whose
  `aria-current` moves with the wizard, each panel is a labelled group, and
  passed steps read as done rather than pending.

- **A shared search link comes back as a search** (audit U3-01). `loadLibrary`
  put the restored query back into the box and then ran the browse listing, so
  refreshing or sending someone a result URL showed a different thing than the
  sender saw. The collection was never restored from the URL at all. It now
  restores the collection too and, when a query is present, waits for
  `loadSessions`/`loadCollections` before running the search — without that
  wait, `search()` would call `syncURL()` while the session and collection
  selects were still empty and rewrite the address bar without the very params
  it was restoring.

- **Search results past the first page are reachable** (audit U3-02). The v2
  endpoint has returned `offset`/`has_more`/`next_offset`/`window_exhausted`
  since v0.31; the page asked for 40 rows and said nothing about the rest.
  There is now a load-more control that appends the next page, and a footer
  that reports how many shots are shown. `window_exhausted` gets its own
  sentence rather than being folded into "that is everything": the page ended
  exactly on `MaxSearchWindow` and more may exist past a boundary one request
  cannot cross.

- **`/library-roots` offers a way in when the Hub answers 401** (audit U2-01).
  Every admin call on the wizard collapsed the status code into prose, so a
  401 was indistinguishable from a broken share and there was no way to act on
  it; the health panel labelled *every* failure — including an unreachable Hub
  — as an auth problem. The thrown error now carries the status, each catch
  branches on it, and an auth failure renders a callout whose button opens the
  shared login dialog and retries the step that failed. Revealing that control
  is deliberate even when `admin_auth` is not `required`: the waiver is decided
  per peer from `RemoteAddr`, so an internet peer under `trusted_network` still
  gets a 401 with the only way to authenticate hidden.

- **`/setup` names each failed check and what to do about it** (audit U2-02).
  Six environment checks reported the same bare failure marker and nothing else
  — not which remedy applies, and not that `timingdex doctor` prints the same
  probes with the paths this page deliberately withholds (root paths are
  admin-only everywhere else in the UI). Each failing check now carries its own
  next step, and the status line stops reporting success while checks fail.

- **The worker wizard's failure text is readable** (audit U1-01/U1-02). The
  generate-failure message was the only self-rescue text on the page and it was
  painted a leftover dark-theme salmon on a light panel: 1.66:1, the least
  readable string on the page at the moment it mattered most. It now uses the
  shared contradicted callout (4.8:1 light, 5.5:1 dark). The mount-path label's
  leftover `#bfcae0` (1.65:1) became `--text-muted` (6.1:1). The library page's
  first-run guidance had the same defect in the same shape — three inline hexes
  that `TestPageCSSUsesDesignTokens` cannot see because it only scans `<style>`
  blocks — and now uses `--brand` and the shared primary button.

- **Browser coverage for the two waived admin-auth branches** (audit U4-01).
  The Playwright fixture serves one Hub configured `admin_auth: required`,
  while production's default is `trusted_network`, so the whole v0.33 access UI
  — the Access status cell, the `/providers` and `/workers` callouts, the
  hiding of the login control — had never been through a browser. Three specs
  now stub `GET /api/v1/setup/status`, the single place the shell reads the
  mode from, and assert all three branches. This covers the UI branch only;
  whether the server actually waives the credential is a separate claim, pinned
  by `TestAdminAuthModeRootWriteGuard`.

- **Two silent CSS defects on shared surfaces** (audit U2-03/U1-04).
  `/library-roots` hid its two status spans with a selector list that began
  with a stray `+` combinator; a CSS selector list is not forgiving, so one
  invalid selector voided the whole list and neither span was ever hidden. And
  the shell declared a bare `.panel` rule alongside the `:where()`-wrapped copy
  the v0.32 punchlist added — shellCSS is injected after each page's own
  `<style>`, so the bare rule beat every page rule of the same name and left
  that fix inert. `/setup` asked for `--raised` panels and `/collections` for
  `margin-bottom:0`; both silently lost, and now do not.

- **Probe-stage writes are lease-bound** (audit F5-01). `SaveMediaMetadata`,
  `SaveSpeechClassification` and `SaveAlignment` took no `jobID`/`owner` and
  performed no compare-and-swap, so a holder whose lease had been reclaimed
  mid-job still landed its rows in `media_metadata`, `capture_metadata`,
  `speech_classifications`, `alignment_runs` and `transcript_words`.
  `CompleteJob`'s CAS did not cover them because they commit before it. All
  three now take the lease pair and test it inside their own transaction (or,
  for the single-statement one, inside the INSERT itself, so there is no
  check-then-write window), returning `domain.ErrJobLeaseLost` and writing
  nothing when the lease is gone. Both empty still means "no lease to check",
  which is what the CLI and fixtures rely on. `SaveAlignment` mattered most:
  its words are a provider result two attempts need not agree on, and
  `transcript_words` feeds the evidence gate's speech channel directly.
  `TestStaleLeaseProbeWritesRejected` covers all three against real SQLite.

- **`search_shots` no longer discards filters silently** (audit M1-01). The MCP
  tool type-asserted `filters` to a string and ignored every other type, so an
  agent passing a JSON object — the shape a model produces naturally — had its
  filter dropped and got a wider result set with no error to notice. Objects
  and JSON strings are both accepted now, and an unknown facet key is rejected
  by name instead of being forwarded to a Hub that drops unknown fields — which
  also closes audit M1-03, the same silent-discard failure one level down. That
  rejection is the MCP side only: nothing in the tree sets
  `DisallowUnknownFields`, so a caller reaching the search endpoint over plain
  HTTP still has a mistyped facet ignored. The parameter previously had no test
  coverage at all.

- **`get_shot` reports ASR speech instead of implying silence** (audit M1-02).
  Shot detail returned only forced-alignment words, and `align` is an optional
  stage most assets never run, so an ASR-only shot looked like silent footage.
  It now falls back to the overlapping ASR segments and labels which source it
  used (`transcript_source`: `aligned` / `asr` / absent), the same vocabulary
  `GET /api/v1/assets/{id}/transcript` uses. Segment timing is reported in a
  separate field so it can never be read as word timing.

- **A mistyped library-root path is a 400, not a 500** (audit X1-02). `POST
  /api/v1/roots` answered `internal_error` for a path that does not exist or is
  a file rather than a directory, sending the real reason only to the Hub log —
  the operator who mistyped a path was told the server broke. Both now carry
  the new `app.ErrRootPathInvalid` sentinel and answer 400 with a message that
  names the path and the problem.

- **The retrieval benchmark is reproducible again** (audit F4-02). Five
  consecutive runs now produce identical numbers, matching the frozen baseline
  exactly. The cause was the fixture, not the engine: `seedGoldenCorpus` left
  shot ids to `idgen.New()`, and shot id is the ranking tie-breaker, so every
  exactly-tied pair reordered on every run and `v2-weighted`'s AssertionFP
  oscillated between 54 and 55. Production ids are stable, so ranking was never
  affected. Separately — a real defect, though not this one's cause —
  `WeightedBlend.Fuse` summed weighted scores in Go map iteration order, which
  makes equal scores differ by an ULP and defeats a tie-breaker that requires
  exact equality; it now accumulates in sorted signal order.

- **`THIRD-PARTY-LICENSES` covers every linked module** (audit G1-02). Five
  modules compiled into the binary had no entry: `github.com/cenkalti/backoff`
  (MIT), `github.com/geoffgarside/ber` (BSD-3-Clause),
  `github.com/grandcat/zeroconf` (MIT), `github.com/hirochachacha/go-smb2`
  (BSD-2-Clause) and `github.com/miekg/dns` (BSD-3-Clause) — all of which
  require reproducing their copyright notice. The file now lists 24 modules,
  verified against `go list -deps` under linux, windows and darwin, because two
  existing entries are only reachable under a non-linux build.

- **Documentation corrected against the code it describes** (2026-08-29 audit,
  `docs/v0.31-audit-2026-08-29.md` §5). Fifteen claims that had drifted away
  from the implementation were re-verified and rewritten. The ones that changed
  meaning rather than a number:
  - `SECURITY.md` no longer describes `region_label` as a coarsened view of the
    capture coordinates. It is free text set by whoever wrote the row, nothing
    reverse-geocodes into it, and calling it a privacy transform promised a
    guarantee the code does not make.
  - `skills/timingdex/` and the MCP reference no longer promise that the agent
    token is refused with `401` on administrator routes. That holds under
    `hub_security.admin_auth: required` and from a remote network; under the
    default `trusted_network` the guard waives the credential for a trusted
    peer *before* reading the header, so an agent on the Hub's own LAN would
    succeed. The Skill states the boundary as a rule of its own conduct and
    names the setting that enforces it.
  - `openspec/specs/search-retrieval` described asset-level search as unscored
    after change 0024 had already added `ORDER BY bm25(asset_search)` to the
    FTS branch. A stale "single source of truth" is worse than a stale README;
    the requirement now describes both branches and what is still missing
    (a relevance scale comparable *across* them).
  - The README and `deploy/unraid/README.md` claimed a container deployment
    needs a manual admin-auth override, and the README claimed `doctor` cannot
    run before provider keys are configured. Both were fixed in this same
    Unreleased block; only the docs lagged. Compose and the Unraid template
    both ship `TIMINGDEX_HUB_ADMIN_AUTH=required`, both admin-auth settings are
    environment variables (the README said the CIDR list had none), and
    `doctor` is deliberately exempt from the provider validator — a diagnostic
    you must fix the problem to run would be useless.
  - `search_shots` pagination (`offset`/`has_more`/`next_offset`/
    `window_exhausted`) and `asset_filter` are documented for agents for the
    first time, along with two silent-failure modes: `filters` must be a JSON
    *string*, and a misspelled facet key is dropped rather than rejected —
    both return a wider result set that looks correct.
  - `docs/retrieval-benchmark-v030-merged.md` records `v2-weighted`'s
    AssertionFP as `54–55` rather than a fixed number: four consecutive runs on
    one machine gave 54/55/55/54. `WeightedBlend.Fuse` sums in Go map iteration
    order (`fusion.go:46`), so equal scores differ by an ULP and the exact-
    equality tie-breaker (`retriever.go:46`) never fires. No test pins the
    value, but the legacy-compat ranking path wobbles at ties for the same
    reason.
  - `state.md`'s browser-smoke figure is corrected to 50/52; CI's `retries: 1`
    masks two 30-second timeouts, so a green CI run is not evidence against it.
  - `.claude-plugin/marketplace.json` no longer advertises plan drafting; the
    plugin has shipped six read-only tools since the write tools were removed.
  - `CLAUDE.md`'s migration ceiling (`0035`, 35 files, `0034` deliberately
    duplicated), library-page anchor count (19) and dependency list (nine
    direct) match the tree, each with a note on how to re-derive it.

  No behavior changed. The common cause is that `scripts/check-doc-refs.sh`
  validates `file:line` bounds under `docs/` only — nothing reads `CLAUDE.md`,
  `README.md`, `SECURITY.md`, `skills/`, `plugins/` or `openspec/specs/`, and
  nothing anywhere checks a semantic claim.

- **Clean container startup by default**: the shipped Docker Compose and Unraid
  Hub entry points now default `TIMINGDEX_HUB_ADMIN_AUTH=required`, so a fresh
  container demands its generated administrator token on every write instead of
  failing the container guard or waiving writes for the bridge gateway's
  RFC1918 address. `TIMINGDEX_HUB_ADMIN_AUTH_NETWORKS` is split and validated
  by `config.Load` into `hub_security.admin_auth_networks`, letting an operator
  who deliberately selects `trusted_network` name the real client CIDRs.
  Bare-metal defaults are unchanged.

- **`doctor` diagnoses a broken legacy provider config**: an enabled selected
  provider with an unset key environment no longer stops `doctor` before it
  prints anything. The report now carries `✗ legacy provider config: <reason>`
  (and `config_valid`/`config_error` in `--json`) while every operational
  command keeps the fail-fast behavior.

- **Capability-first provider wizard**: the beginner provider dialog asks for
  one capability (video understanding or speech-to-text) and offers only
  grounded key-backed presets with a prefilled editable model; model detection
  is optional (unprobeable/no_models/schema_unknown keep the preset,
  `key_invalid` blocks), one submit creates exactly one channel, and the key is
  never stored in the browser. A no-auth local VLM stays on the legacy
  `providers.local_vlm` config path with in-page guidance.

- **`/setup` reports executable state**: `healthy_root_count`,
  `provider_ready`, `searchable_shot_count` and `search_index_ready` join the
  inventory counts; the next-step and `ready` verdicts now require a healthy
  root, a runnable video route, at least one canonical shot and a ready search
  index (ExifTool stays optional).

- **NAS staging and scan reporting**: `source_staging.mode=copy` stages once
  before the probe and reuses the same versioned cache file for the full-media
  stages. Scans report skipped non-video files (bounded to 20 named extension
  types, the rest folded), the supported extension list, and warn on a
  zero-discovery scan that skipped files, in both `root scan` and
  `/library-roots`.

- **Visible browser feedback**: settings/collections mutation status is now a
  visible shared callout (`role=status`, `aria-live=polite`); the `/progress`
  issue repair link is a category→action map with localized labels instead of
  a blanket providers link; the jobs, library-root health and Tags tables
  scroll inside the shared `.table-scroll` without overflowing the document at
  375px; every dialog/overlay shares Escape-to-close, contained Tab and opener
  focus restore; shot-result cards are keyboard-activatable; the shell Labs
  group and provider option labels are localized through the catalogs.

- **Structured search keeps every library filter**: the shot-search POST now
  carries an `asset_filter` (captured date with exclusive upper bound, region,
  camera, session, status) beside `facets`, and the candidate universe
  constrains every recall channel, including `similar` mode.

- **Pagination and resource semantics**: Search responses report
  `offset`/`limit`/`has_more`/`next_offset`/`window_exhausted` with global
  ranks; the six legacy list endpoints (`/api/v1/assets`, `/jobs`,
  `/repurpose/plans`, `/tags/unresolved`, `/tags/proposals`, `/shoot-sessions`)
  accept optional `offset` and answer with `X-Timingdex-Limit`/
  `X-Timingdex-Offset`/`X-Timingdex-Has-More`; parent/child endpoints distinguish
  a known parent with zero children (`200 []`) from an unknown parent (404 with
  `action: check_the_identifier`); successful empty lists serialize `[]`, never
  `null`. MCP `getTimeline` moves onto the 64 MiB decode path, `search_shots`
  takes an optional `offset`, `inspectLibrary` reports setup/status and
  jobs/summary, and MCP/Worker errors decode a shared `internal/apiclient`
  envelope exposing the stable `code`. An unset `TIMINGDEX_BASE_URL` now
  defaults to `https://127.0.0.1:8787` (requiring `TIMINGDEX_HUB_FINGERPRINT`
  unless the URL is an explicit loopback/link-local HTTP).

- **Generated Worker installer works against the default self-signed Hub**: the
  POSIX/PowerShell scripts embed the server-derived certificate fingerprint,
  SPKI pin and binary SHA-256, create a private 0700 config directory, download
  with the pairing token pinned to the Hub, verify the checksum before renaming,
  and enroll with `--fingerprint` before `--pairing`. A non-consuming
  `WorkerPairingValid` backs a strict bootstrap route that rejects a presented
  redeemed token even from trusted networks, and the setup page retries context
  after an `timingdex:admin-auth-changed` login.

- **CLI usage synopsis**: `timingdex` help now enumerates
  `search rebuild|rebuild-embeddings`, `cache inspect|gc|verify|repair-derived`,
  and the Worker enrollment `--root`/`--cache`/`--config` flags.


- **Browser multilingual UI**: the 11 inline HTML pages (library, setup,
  progress, workers, worker-setup, library-roots, repurpose, tags, providers,
  collections, settings) now render in Simplified Chinese (the default),
  Japanese, US English, French or Spanish from one embedded locale catalog. A
  first visit negotiates from `Accept-Language`; a language selector in the
  shared sidebar writes a non-sensitive `timingdex_locale` preference cookie
  (SameSite=Lax, one-year, no browser storage) that overrides negotiation on
  later loads. Dynamic copy calls the injected `tdT`/`tdPlural`/`tdFormat*`
  helpers; API error envelopes are localized client-side through
  `tdApiErrorMessage` without changing any API field, code, action or
  English message. CLI, MCP, Agent and Worker output is unchanged. Root
  storage diagnostics gained an additive structured form
  (`RootWarningDetails`, `warning_details` beside the existing `warnings`) with
  four stable codes, while Doctor/CLI English output stays byte-for-byte the
  same.

- **Optional admin auth (`hub_security.admin_auth`)**: the Hub administrator
  credential is now optional. `required` keeps the previous always-demand
  behaviour; `trusted_network` (the new default) waives the password for peers
  inside `hub_security.admin_auth_networks` — LAN peers skip it, internet peers
  still need it — and `off` never demands it. The waiver is decided from the
  peer address alone, so behind a reverse proxy or a published Docker port every
  peer looks RFC1918: a containerised Hub running `trusted_network` without an
  explicit `admin_auth_networks` now refuses to start rather than silently
  waiving the password for the whole internet. The nine `/api/v1/worker/*`
  routes and the `provider_operations` trust chain are unchanged — Worker
  authentication is not relaxed by this.

### v0.31.0-alpha — footage capability provider & release preparation

- **Shot-level speech search no longer requires forced alignment**: the
  transcript channel now sources per asset from the strongest available timing
  — aligned words when they exist, otherwise the ASR transcript's timed
  segments (materialized into a new `asr_segments` table at `SaveTranscript`
  time, with a migration backfill for existing libraries). ASR-only assets are
  now searchable by spoken phrase with shot-level placement and
  possible/transcript evidence, joining the description channels in fusion. A
  0-0 placeholder segment still contributes no timing, so untimed ASR stays
  asset-level. Mixed CJK/ASCII multi-component phrases over ASR segments keep
  the honest no-result behaviour rather than over-matching.
- **Footage capability provider**: Timingdex now exposes its library to
  transcript-driven AI editing frontends as a product-neutral capability
  contract. New `GET /api/v1/assets/{id}/transcript` returns the asset's
  word-level timeline transcript: `source=aligned` carries the forced-alignment
  word stream (the strongest timing evidence) with `text`/`segments` promoted
  from it, `source=asr` falls back to the ASR sentence segments, and an asset
  with neither answers `404 not_found` rather than an empty 200 — so an editor
  can tell "no transcript" from "silent footage". The word stream stays
  contiguous across shot boundaries. The Agent contract bumps v0.14 → v0.15:
  `read_transcript` joins `allowed_actions`, and the skills package
  (`SKILL.md`, `references/api-contract.md`) is re-versioned and re-validated
  in the same change. The route guard matrix also gains the previously missing
  `GET /api/v1/admin/hub/worker-setup/library-roots` admin row.
- **MCP**: `timingdex-mcp` gains a sixth tool `get_transcript(asset_id)`,
  plus certificate fingerprint pinning for cross-machine Hubs:
  `TIMINGDEX_HUB_FINGERPRINT` (printed by `timingdex serve`) pins the Hub's
  self-signed leaf certificate over `https://`. An https base URL without a
  fingerprint now fails at startup instead of silently accepting an arbitrary
  certificate; `http://` remains available for local development. Added a
  Claude Code plugin marketplace (`.claude-plugin/marketplace.json` +
  `plugins/claude/`) that declares `timingdex-mcp` over stdio and passes
  through `TIMINGDEX_BASE_URL`, `TIMINGDEX_AGENT_TOKEN` and
  `TIMINGDEX_HUB_FINGERPRINT`; the plugin ships no binary.
- **Design round — the browser UI now closes its core workflows**: the
  Repurpose workspace gained a plan inbox, deep links, revision history and
  per-candidate proxy preview, so an agent-drafted plan can be discovered,
  reviewed and human-approved after refresh; `/workers` is the front door to
  the setup wizard (dead button removed); the Library has one search entry,
  keyboard-operable shot timeline, and a mobile nav; Provider channels gain
  full edit/enable/disable/delete management and Tags governance is wired with
  error recovery; Collections distinguish dynamic views from pinned shot
  lists; and every served page consumes a shared design-token vocabulary.
- Release-closure work makes browser smoke deterministic and repository-owned,
  runs it over the fixture's HTTPS session boundary, pins Playwright and
  gitleaks, and makes browser regressions blocking once the clean-run gate is
  green.
- Search v2 batches transcript evidence, speech-phrase validation, and result
  context lookups; candidate sorting is stable `O(N log N)` for full scans.
  API wire compatibility, ranking, evidence semantics, and the `query_hash`
  bytes remain unchanged.
- HTTP route registration is grouped behind an auditable typed inventory, and
  the browser shell makes the core Library/Search-to-reuse path distinct from
  Advanced / Operator links without removing capabilities.
- The repository adds a central `VERSION` declaration, synchronized
  `v0.31.0-alpha` deployment references, a native release-build helper, real
  SQLite scale benchmark/evaluation scaffolding, and offline Search relevance
  evaluation data. Measured results and the remaining publication-only gates
  are recorded in [`docs/v0.31-release-notes.md`](docs/v0.31-release-notes.md).
- Completed the browser/Pipeline P0 hardening: API scans still trigger queued work
  after partial scan errors, concurrent Pipeline triggers coalesce into a joined
  follow-up pass, and `serve` waits for background Pipeline work before closing the
  repository. Browser administration now has bounded in-memory Sessions, login
  throttling, sensitive-response `no-store` headers, and Worker Setup refreshes its
  path details when the shared Session logs in or out.

- v0.30 review-fix round 的目标与升级/运维检查见
  [`docs/v0.30-review-fix-round.md`](docs/v0.30-review-fix-round.md)。各领域细节仍见
  成本参考值、部署、Worker setup 路径脱敏和 Search evidence correctness 文档。
- Analysis now enqueues an idempotent `JobIndex` successor after every successful analyzer
  path, keeping asset-level FTS in sync without rebuilding it inside the canonical commit.
  Added offline `timingdex search rebuild` to repair all assets with canonical analysis or
 successful transcripts; it never re-runs models.
- `root scan` now starts the existing single-run Pipeline after discovery: the API reports
  whether it started or found an existing pass, while the CLI waits for its synchronous pass
  before exiting. Browser administration now uses a short-lived HTTPS HttpOnly session with
  same-origin Origin and CSRF checks; CLI, Agent, and Worker Bearer authentication remains
  unchanged.
- **Real-machine deployment hardening (107 / NAS / agent-plan)**:
  - **Fail-fast provider validation**: `timingdex serve`/`pipeline run` now refuse to start
    when a selected provider (`asr_primary`, `vision_primary`, `repurpose_primary`, …) is
    enabled but its `api_key_env` did not resolve. Previously the Hub came up "healthy" with a
    dead ASR/vision route and only failed on the first real job. A selected-but-disabled block
    stays allowed (the shipped default selects providers while leaving them disabled).
  - **Orphan lease reclaim at startup**: a killed `serve`/`pipeline run` left `running` jobs
    with still-valid leases that stalled the queue for the whole lease TTL (30 min on derive).
    Hub-local executors now register a heartbeat row (`pipeline_executors`, migration 0035) and
    `HealOnStartup` reclaims any `running` job whose local executor is dead immediately, while
    never touching a live process's jobs or worker-pinned work.
  - **Provider visibility**: `GET /api/v1/agent/capabilities` now carries a `providers` object
    (`asr`/`vision`/`repurpose`/`tag_curator`/`embedding`/`alignment`) listing the selected
    primary/fallbacks and the enabled blocks (`name`, `protocol`, `model`, never credentials),
    so an operator can see at a glance what the Hub is actually running.
  - 部署运维要点（SSH 里用 `pkill -x timingdex`、`/tmp` 满、key 环境变量、`pipeline run`
    单轮语义）写入 [v0.31 部署指南](docs/v0.31-deployment.md)。
- **模型通道一键配置**：`/providers` 新增通道时，选择已知 Provider 会自动带出 well-known
  端点（火山 Agent/Coding Plan、火山视频、千问、Gemini、StepFun、本地 VLM），并在模型字段
  旁提供「读取模型」——用表单里刚输入的 key 经 Hub 发一次非计费 `GET {endpoint}/models`，
  把当前账号可用模型拉进下拉（火山 agent/coding plan、qwen token plan 等 OpenAI 兼容端点
  直接可用），省去手查端点与模型 ID。新增 Hub-admin 门控路由
  `POST /api/v1/admin/provider-channels/probe-models`；key 仍只沿管理员 HTTPS 请求方向流动，
  不落浏览器存储、不回显，返回的模型 ID 均按 key 脱敏，超长/超量截断。另加「一键配置」
  向导：**Plan 预设行只填 Key**——火山 Agent/Coding Plan、Qwen Token Plan、StepFun ASR
  各自建好它能服务的能力通道，端点与模型已按探测结果预填且可改；**自定义端点**填
  Endpoint + Key 后程序探测可用模型、用户勾选该端点担当的角色（Repurpose / Tag / Embedding /
  视频理解），模型同样预填可改。用户全程不需要读模型列表，也没有探测按钮；已配置的能力
  跳过、未填 Key 的行跳过，全部为纯前端编排，复用既有建通道与探测端点。
- **素材目录状态表加「提示」列**：`/library-roots` 的健康表对每个已添加根显示
  `timingdex doctor` 同源的存储建议（网络挂载提示、SMB/NFS 根建议 `source_staging.mode
  =copy`、可写挂载提示），`GET /api/v1/roots/health` 每行新增 `warnings` 字段——原本只能
  在 CLI 里看到的 NAS 性能建议，现在网页上就能看到。

## v0.30.0-alpha — 2026-08-10（成本参考值）

- 成本配置改名为每日 / 每月成本参考值（`daily_cost_guide`、
  `monthly_cost_guide`）。它们是运营观察指标，不是调用上限；分析和转写
  不会因为账本累计超过参考值而暂停或推迟。
- 成本账本继续在模型提交后追加事后估算，明确为成本参考而非账单记录；汇总
  API 字段为 `today_estimate` 与 `month_estimate`。
- 设置 API 在一个版本内兼容读取旧的 `daily_budget` 与 `monthly_budget` 字段，
  但响应和页面只使用新的成本参考命名。历史 `budget_exhausted` 持久化类别保留，
  新流程不再产生。
- **缓存维护 CLI**：新增 `timingdex cache inspect`、`gc`、`verify` 与
  `repair-derived`。`inspect` 汇总各类缓存、孤儿目录和可重建空间；`verify`
  双向核对数据库 artifact 行与文件；`gc` 只处理明确指定的 scratch、可重建
  派生物或孤儿目录，默认仅预览，实际删除必须 `--yes`。数据库、Provider 密钥、
  原片和 `cache/sources/` 始终受保护；删除可重建派生物会为已完成分析的素材
  重新排队派生，不会重新计费模型调用。`repair-derived` 可按硬件 profile
  清除错误派生结果并重新派生，dry-run 不产生副作用，维护期间协调并发 pipeline。
- **分析提交与 model run 一致性**：model run 的创建、阶段转换和 canonical
  analysis/shots 写入均受 lease 所有权保护，失去 lease 的执行者不能覆盖新持有者
  的结果，也不会留下可重复的 model run。迁移 0031 让失败 run 可重试，并以
  capability/provider/model/input/prompt/schema 组合去重非失败 run；对已有数据的
  重建可安全执行。
- **搜索索引维护**：分析成功后幂等地排队 `JobIndex`，`timingdex search rebuild`
  可离线重建拥有 canonical analysis 或成功 transcript 的资产索引，失败会传播而
  不是静默吞掉 I/O 错误，且不会重新运行模型。元数据通道采用稳定排序；分页会
  正确覆盖 `offset+limit` 的候选窗口，不会因为前页偏移而提前截断结果。
- **Embedding 防护**：文本 embedding 拒绝维度错误、非有限值和无效向量，重建
  计数只报告实际写入的 shot；embedding provider 的响应与 metadata 通道均有界，
  embedding 仍只是 retrieval 信号而不是 evidence。`timingdex search
  rebuild-embeddings` 继续只重建 embedding 层，不重跑 VLM analysis。
- **Evidence 与短语匹配**：检索中的 CJK 短语改为按完整短语、顺序和可接受的
  分词边界匹配，部分或乱序 token 只能参与召回，不能升级为 evidence；证据冲突时
  显式否定优先于正向结构化观察，结果保持 `contradicted`，不会把沉默误报为缺失
  内容。
- **素材位置与采集元数据**：迁移 0030 为每个 asset 强制一个 canonical primary
  location，并按文件存在性与 root 健康状态确定优先级；迁移 0033 保存稳定的
  probe 文件修改时间，完整扫描才执行删除/重现协调，移动或重新出现的文件不会
  因路径变化产生错误身份。重新探测时保留更强的采集时间来源和置信度，capture
  metadata merge 会保留冲突信息而不以较弱来源覆盖较强来源。
- **Collections 顺序约束**：迁移 0032 将已有 collection shot positions
  重排为从零开始的连续序列，并加上 collection 内 position 唯一约束；追加和删除
  在原子操作中维护顺序，不再产生重复或空洞位置。
- **Worker 信任边界**：heartbeat 不再用自动探测结果覆盖 enrollment 时声明的
  `provider_operations`；心跳只能更新运行时能力，不能凭空授予 Provider 访问权。
  WebDAV 请求改为使用 request-local handler，消除并发请求之间共享 Prefix/FileSystem
  状态造成的竞态与跨空间响应风险。
- **密钥与凭证安全**：secretstore rekey 改为带 journal 的原子流程，分阶段写入、
  崩溃恢复，并在失败时保留可恢复状态；旧数据密钥备份到
  `provider-secrets/store.key.pre-rekey`。Hub/Worker token 与 Worker config 文件
  现在对非 regular file、symlink 和不安全权限 fail closed，父目录权限也收紧，
  保存后原子验证。Worker provider proxy 会清除 URL 中的 userinfo/query/fragment、
  所有 extra-header 值及请求体中的 Provider 凭证，同时保留可分类的错误状态；
  下游错误写入 job 时也有界，避免上游回显把密钥持久化。
- **API 输入边界**：JSON 请求采用有界读取和严格尾部解码，尾随 JSON、未消费内容
  与超大 body 不再被静默接受；路由 inventory/coverage 测试覆盖 catch-all，避免
  新增 endpoint 遗漏认证或方法边界。

## v0.28.2 — 2026-08-10

- **Worker setup path redaction**: the trusted-read worker setup context now returns
  only library root IDs. Absolute paths are available only through the Hub-admin
  route `/api/v1/admin/hub/worker-setup/library-roots`, which propagates repository
  failures instead of returning an empty list. The wizard keeps the admin token in
  page memory while loading those details.
- Bumped the agent API contract version to `v0.14` for the changed read contract.
- **Token and credential file hardening**: Hub admin/agent tokens and Worker
  configs now use raw bearer tokens in exact-mode files, reuse them across
  restarts, reject symlinks/non-regular files/insecure permissions, and commit
  atomically with post-verification and parent-directory fsync. Corrected the
  access-control specification so only pairing/node token digests are stored.
## v0.28.2-alpha — 2026-08-10（Search evidence correctness）

- **Evidence conflict precedence**：positive structured observations no longer
  mask an explicit description negation; the result is `contradicted` and cites
  both sources. Negated queries distinguish observed forbidden content from
  stated absence without asserting absence from silence.
- **Exact speech phrases**：aligned speech retrieval now validates the complete
  phrase in order, with ASCII whole-word matching, CJK segmentation tolerance,
  no span reuse, and a 1500 ms maximum adjacent-span gap. Partial and reordered
  token hits remain retrieval-only and cannot become evidence.
- Bumped the Search v2 profile to `v2-profile-2` and added API, SQLite, and
  matcher regression coverage.

## v0.28.1-alpha — 2026-08-09（发布前修复）

- **修复 `RebuildAutomaticShootSessions` 多 location asset 撞
  UNIQUE(asset_id, session_id)**：同一文件经多个路径链入一个 root
  （镜像目录 / 硬链接副本）时，location JOIN 产生重复行，会话重建把同一
  (asset, session) 插两次，`pipeline run` 整体中断（实测复现）。修复：
  聚合契约层按 asset 去重（一个文件 = 一条 capture）+ 重建 SELECT
  `GROUP BY a.id`。回归测试
  `TestRebuildAutomaticShootSessionsDeduplicatesMultiLocationAssets`（sqlite
  层，真 DB + 约束）与 `TestAggregateSessionsDeduplicatesSameAsset`
  （capture 层）。

## v0.28.0-alpha — 2026-08-08（Text Embedding 通道）

Search v2 的第五检索通道：真正的文本 embedding，retrieval_generation 从
概念落地为可运行层。

- **TextEmbedding 通道**：`search.TextEmbedder` 批量接口与
  `providers.Embedder` 结构性一致（OpenAI-compatible / Gemini 适配器
  零改动直接满足）；`shot_text_embeddings` 表（每 shot 一行、model 标记、
  float32 LE blob、source_text_hash 驱动增量重建）；稠密通道阈值
  `embeddingCutoffFraction`（0.8 × 通道最高相似度）防止近正交噪声长尾
  冲垮 RRF。
- **自动增量 + 全量重建**：pipeline 提交后 hook（分析/精修两个提交点）
  只重嵌 derived 文本变化的 shot，错误只记日志绝不失败分析 job；
  `timingdex search rebuild-embeddings` 全量重建。换 embedding 模型 =
  重建 derived 层，**绝不重跑 VLM analysis**。
- **语义 profile 调整**：semantic `{heuristic 0.45, text_embedding 0.35,
  lexical 0.20}`、fact 含 `text_embedding 0.15`；speech/creative 不参与。
  evidence gate 零改动（embedding 是检索信号，不是证据）。
- **Benchmark**：72 query 第六管道 v2-rrf-gate-embed（fake embedder +
  真实存储路径）：R@10=0.986、RetrievalFP=0、AssertionFP=0，与 v0.27
  gate 基线完全持平——通道加性成立。httptest 真实适配器 roundtrip、
  hook 增量语义、模型切换测试。详见
  docs/retrieval-benchmark-v028-text-embedding.md。
- **文档**：docs/search-architecture.md embedding 从 future 升 v1。

## v0.27.0-alpha — 2026-08-08（Search Architecture v2 + Evidence Gate + Selection 骨架）

把检索系统从"一个 hybrid 端点 + 一个 0.70/0.30 权重"升级为分层的
local-first footage retrieval and selection engine：**召回可以大胆，
声称必须有证据**。

- **Search Architecture v2**（`internal/search`，纯 Go）：query compiler
  （离线受控词表，ASCII 整词纪律：carefree≠car / raining≠train；位置化
  否定：没有人的海边空镜 → mustNot person，海边保持正向）+ intent router
  （auto/fact/speech/semantic/similar/creative，无 LLM）+ 四通道召回
  （lexical 加权 bm25 / heuristic semantic / transcript 时间重叠 /
  metadata 仅文件名）+ fusion（WeightedBlend 兼容 + RRF k=60）。
- **Evidence Gate**：confirmed（结构化观察字段）/ possible（仅描述或口述）
  / contradicted（shot 自己否定）/ unknown（**绝不是"确认无人"**）。
  fact/negative 查询的 must 无证据即排除，部分支持降权；
  missing≠negative 语义在 API 层强制诚实。
- **Selection 骨架**：near-time 近重复硬跳过（A001 10.1s/10.9s/11.5s 不
  能霸占 top-10）+ 同 asset / 同 session 软罚；creative 更高 diversity。
- **结构化 API**：`POST /api/v1/search/shots`（mode/diversity/evidence/
  context/facets + search_id/query_hash/result_rank，为未来反馈挂钩）。
- **旧端点内部接管**：GET hybrid 由 v2 compat 路径服务，compat 相等性
  测试对全部 legacy golden query 断言 ID 序列与分数（≤1e-12）一致；
  UI/MCP/repurpose planner 行为不变。
- **UI**：搜索改走 v2 端点，结果卡片渲染"为什么命中"证据行（✓ 确认 /
  可能 / 未确认 / ✗ 矛盾），unknown 与否定项绝不显示成"确定"。
- **Benchmark**：legacy 语料 + 6 家族 v2 hard negatives 共 72 query，
  5 pipeline × per-intent 指标 + RetrievalFP/AssertionFP。gate 管道：
  RetrievalFP 3→**0**、AssertionFP 56→**0**、R@10 0.972→**0.986**
  （不降反升，gate 把被噪音挤掉的 relevant shot 释放回 top-10）。
  详见 docs/retrieval-benchmark-v027-search-v2.md。
- **文档**：docs/search-architecture.md（长期架构：canonical shot truth、
  检索/断言分离、future embedding/OCR/temporal/sequence）、
  docs/v0.27-search-architecture-goal.md、api-contract 增补结构化搜索 +
  evidence 语义。

## v0.26.0-alpha — 2026-08-08（检索基准 + 控制界面产品化）

公开前一轮：把「系统说的话必须是真的」从口号变成可测量的门禁，并把十个
独立后台页面收敛成同一个 Re:Footage 控制界面。

- **检索 golden set 扩到全量语料**：8 asset / 12 query → **40 asset / 62
  query**（`internal/repository/sqlite/retrieval_golden_corpus_test.go`），
  10 个 adversarial 家族全覆盖（后半段对象、全局标签污染、中英同义、
  矛盾全局、景别区分、窗口边界钉时间、overlap 去重、定时 speech、
  纯否定断言、干扰语料）。权重扫描加入 **RRF**（k=60）。指标：
  lexical-only R@10=0.871 vs 语义 blend 0.984，**全 blend FP=0**；
  默认 0.70/0.30 维持不变。详见 docs/retrieval-benchmark-v026-alpha.md。
- **修掉被测出来的 semantic false-positive assertion**：64 维 FNV 哈希
  向量在 query 与 shot **零共享 token** 时仍给出 0.33 余弦（维度碰撞
  噪声），使无关 shot 进入所有含语义权重的 top-10——正是「搜汽车命中
  没有汽车的时间段」这类错误。修复：无共享语义 token 时语义分强制为 0
  （alias-canonical token 算共享）。这是打分契约，不是权重微调。
- **eval 工具修复**：`timingdex-eval score` 的位置索引 flag 校验 bug
  （传 `--labels` 却检查 `label`，导致正确调用报 "--labels is required"）
  重写为显式校验 + 回归测试；`score` 输出新增按信号归因的
  semantic/lexical false positives。新增 `cmd/timingdex-corpusgen`
  生成可复现的离线 eval 语料（lavfi 合成 clips + ground_truth.json）。
- **shot 检测真正 merge**：`Normalize` 从 stretch 改为 merge（stretch 在
  相邻边界下无空间，150ms 闪帧此前会残留为 canonical shot 并多付一次
  VLM 调用）。规则：唯一短 shot 扩至全资产；首个并入后一个；其余并入
  前一个；连续短 shot 全溶解。9 个回归测试（含 ffmpeg adjacent 形态）。
- **channel fail-fast**：`/providers` 通道配 `openai_multiframe` 协议
  在构建期即拒绝并指名 remedy（此前会运行到 "multiframe summary call
  requires at least one frame" 才失败）。
- **UI 产品化**（无框架、无 npm、单二进制不变）：
  - 统一 App Shell：十个页面共用同一 header/导航/配色/管理 Token 输入
    （仍 memory-only）+ **全局状态条**（Hub / Pipeline / Providers /
    Workers 四格，点击直达对应页）；
  - **shot-first 搜索**：搜索返回镜头级结果卡片（文件名、start–end、
    缩略图、description、tags/objects/actions/mood、匹配度），不再是
    整素材卡片；
  - **Shot 预览抽屉**：点击时间轴镜头或结果卡片 → 抽屉内 proxy 自动
    seek 到 start_ms 并播放该区间，不离开当前查询/筛选/滚动位置；
  - 筛选收敛：高频条件默认显示，语义 facets 收进「高级筛选」折叠
    （素材级标注保留）；
  - /progress 操作台化：新增「正在处理」hero（文件名 + 阶段 +
    尝试次数），危险操作按钮层级区分；
  - Tag Curator 收敛进统一深色主题。
- **检索正确性附带**：hybrid shot 结果与 jobs 列表带资产文件名（join
  主 location，无需二次请求）；评估基准、eval CLI、corpusgen 均离线。
- **生产 wiring 修复（P0，实测暴露）**：Pipeline 此前拿到的是 channel
  runtime 的裸包装器，不暴露 multiframe 选择器——multiframe 编排在生产
  路径上从未运行，纯帧 provider 报 "does not support video preparation"
  而非进入 detector 模式（只有直连 Router 的测试是绿的）。新增
  `pipelineVideo` 桥（channel 包装 + legacy router 的 multiframe 表面），
  `TestServiceWiring*` 钉死装配路径。
- **alias 子串假阳性修复（P0，实测暴露）**：`semanticTokens` 的 alias
  匹配用 `strings.Contains`——"carefree" 含 "car"、"train" 含 "rain"，
  搜 "car" 会命中无车辆证据的镜头。ASCII alias 词改为整词匹配（CJK
  词保持子串——中文无词边界）。golden 语料新增陷阱 asset +
  硬门禁，discovery 单测钉死。

**Migration**：无 schema 迁移。旧素材无需 reanalyze（检索打分变化是
检索期的，不触碰已提交的 shot 行）。语义分 token-overlap 门禁在检索期
生效，已提交向量无需重建。

## v0.25.1 — 2026-08-08（Local Multiframe Analysis v1）

Roadmap 第一阶段：把 shot 时间轴从 LLM 手里拿出来。Timingdex 负责检测与采样，
VLM 只负责描述画面。目标部署：插上一张 8–16GB 消费级 GPU 就能在后台慢慢索引。

- **`openai_multiframe` 协议**：`local_vlm.protocol` 支持
  `openai_multiframe`（llama.cpp / LM Studio / vLLM / SGLang 的 OpenAI 兼容
  chat/completions 表面）。端点永不接收整段视频；Timingdex 对每个 shot 采样
  2/4/6 帧（boundary-aware 默认，10/35/65/90% 位置）、按 shot 切 transcript、
  每 shot 一次模型调用，模型只返回纯元数据（无时间字段）。
- **确定性 shot 检测**：`shot_detection` 配置块，两种 detector——
  `external_command`（PySceneDetect wrapper，stdin/stdout JSON 契约，与
  forced aligner 同款）与内置 `ffmpeg_scene`（零新依赖）。Timingdex 硬校验：
  单调、非重叠、界内、最短 300ms、上限 2000；违规=永久失败。detector 身份
  进入 model_run 的 request_json，换命令/阈值自动重跑分析。
- **双模式编排**：配置了 detector 时走纯 detector 模式（detector 出边界 +
  每资产一次 summary 调用 + 逐 shot 精修）；未配置时回退双 pass（现有 VLM
  window analysis 出边界和资产级 analysis，multiframe 逐 shot 精修并用
  `ReplaceAssetShots` 替换 shot 行）。两个 pass 各留一个 model_run，provenance
  可审计；精修失败保留 pass-1 结果（降级而非丢失）。纯 multiframe 链且无
  detector 时报永久配置错误并指名 remedy。
- **资产级字段**：per-shot shot_size/camera_motion/quality/usable_as 聚合
  （众数/最差/并集）折叠进资产级 analysis；audio_type/has_speech 诚实声明为
  帧模型不可判（has_speech 由 transcript 判定，audio_type 留 summary 推断）。
- **eval 工具**：`cmd/timingdex-eval`（内部 `internal/eval`）——真实 clips 进
  隔离数据目录、走真实 Pipeline（probe→derive→analyze→index），`score` 用
  产品同款 hybrid 检索 + golden 同口径指标输出 R@10/P@10/FP/RT factor/帧数
  对比表与逐 query 明细。离线工具，不进 CI。
- 附带修复：`GetAssetDetail` 的 analysis 从未成功反序列化（`has_speech` 以
  0/1 数字出现在 json_object 里，Go bool 拒绝）——API 详情页的资产级分析
  一直是 nil，现修复。

使用：`timingdex-eval run --corpus ./corpus --data-dir ./eval/qwen --label
qwen3vl-4b && timingdex-eval score --corpus ./corpus --data-dir ./eval
--labels qwen3vl-4b,gemini-flash`。Worker 路径不在本版本范围（multiframe 是
Hub 本地 GPU 路径；worker 继续走 proxy 的 openai_video）。
## v0.25.0 — 2026-08-07（shot truth / transcript timeline / retrieval correctness）

GPT review 轮的 P0/P1 正确性修复，主线是"一个 shot 的 metadata 必须来自这个
shot 的证据"。全部 5 条 finding 在代码中验证属实（其中 2 条比描述更严重，见下），
并首次引入 retrieval golden set 作为检索回归基准。

- **P0 shot 元数据污染（真修复）**：`ToAssetShots` 不再把 whole-asset
  Objects/Actions/Mood 复制进没有观测的 shot——08:20 出现的 car 不再污染
  00:10 的 shot。FTS / semantic vector / hybrid / similar / repurpose 候选
  全部受益。Gemini prompt 的 shots schema 补 per-shot objects/actions/mood；
  prompt_version `footage-analysis-v3`→`v4`、schema `asset-analysis/v1`→`v2`
  打破 model_run 缓存。回归测试断言 asset 全局 car + 空 shot 在 FTS 与 hybrid
  下均不命中。
- **P0 transcript 时间轴（比描述更严重）**：Qwen/Volcengine ASR 产出单条
  `{0,0}` 伪 segment，拆窗时被 `EndMS <= window.StartMS` 在**每一个**窗口
  （含第一个）丢弃——长视频的 ASR 完全到不了模型。`align` 的
  `transcript_words` 写完就死（仓库无任何读路径）。现在：transcript 时间轴
  分类（0-0 伪 segment 不算时间戳）；新增 `GetAlignmentWords` 读路径；
  analyze 优先消费 alignment 词级时间轴；拆窗时无时间轴的文本一律不传
  （单调用模式仍传全文，本机 mimo 配 external_command aligner 即可恢复
  长视频时序）。
- **P1 MCP 远程 403（changelog 声称与实现不符）**：v0.23.1 声称
  "inspectLibrary 带 agent token"，实际 commit f21eb42 只改了 stderr 日志，
  代码仍以空 token 调 `requireTrustedRead` 路由。本轮真正修复：`/hardware`
  与 `/search/shots/hybrid` 带 agent token，`/health` 保持匿名；远程无 token
  403 的 httptest；本条 changelog 即为对旧声称的更正。
- **P1 hybrid 权重首次测量**：`0.70*Semantic + 0.30*Lexical` 自初版未动过。
  新增 retrieval golden set（8 个 adversarial case：后半段对象、全局 tag 污染、
  中英文同义、全局 tag 相反、wide/close-up 混排、窗口边界、重叠去重、ASR 定时
  区间），sweep 结果：lexical-only R@10=0.667（丢 4/12 跨语言查询），任意带
  heuristic 项的 blend R@10=1.000 且 false-positive=0——默认保持 0.70/0.30。
  `semantic-hash-v1` 常量改名 `HeuristicVectorModel`（存储值不变，无 migration）。
- **P1 facet 语义**：shot search 的 shot_size/camera_motion/audio_type/quality/
  usable_as 均解析自素材级 `asset_analysis`，参数重命名为 `asset_*` 前缀，
  旧名保留为别名；UI 标注"景别(素材级)"等；close-up shot 被素材级 wide 命中
  现在是文档化语义而非 bug。
- **P1 reanalysis 机制（新增）**：`timingdex reanalyze --asset <id> | --root <id> |
  --all [--reason]`。nonce 化 input hash 打破 job/model_run 双重 dedup，产生新
  model_run、canonical 切换、FTS/vector 重建；旧 run 留在 model_runs 可审计；
  `reanalysis_requests` 表记录 who/why；不需要删库刷新旧分析。
- **P2 lease stage TTL**：本地 pipeline 固定 2 分钟租约常被 derive/long analyze/
  transcribe 超过，第二个执行者 reclaim 造成重复付费调用。改为按类型 TTL
  （probe 2m / derive 30m / transcribe 15m / analyze 20m / 默认 2m），worker
  derive 初始租约 2m→30m；lost-lease 竞态测试保持全绿。

**Migration**：无需 schema 迁移；新增 `reanalysis_requests` 表随迁移自动创建。
**旧素材需要 `timingdex reanalyze --all`** 才能吃到 v4 prompt 的新 shot 语义
与时间轴逻辑。

## v0.24.1 — 2026-08-05（luna 交叉复核修复批 + 第三方裁决收尾）

GPT-5.6 Luna（xhigh）四轮交叉复核 + deepseek-v4-pro 第三方裁决后的修复。
Luna 四轮抓到的问题前三轮均为真实 P1/P2，全部修复；第四轮经第三方裁决
判为 gold-plating（防御性滚雪球），仅采纳 P3 收尾。

- **app**：identity 解析 goroutine 带 ctx 贯穿（secrets.Has 加 context）；锁序
  统一消除死锁反转；快速路径刷新 fingerprint；model 成对校验；同 root 并发
  scan 计数隔离；RunUntilIdle 有限重试（3 次）；scan 失败按 root 计数 + 去重。
- **凭证**：redactSecrets 重写为 JSON 感知递归（转义嵌套/深层嵌套/顶层字符串
  标量/编码形式全覆盖）；BaseURL/URL 序列化剥离 userinfo + query 凭证（大小写
  不敏感 + fragment 清除）；longToken 收紧不误伤 UUID；LeaseAudit/volcasr
  安全格式化；ExtraHeaders 全脱敏。
- **API**：decodeStrictJSON 严格解码（同一 Decoder 二次 Decode 要求 io.EOF，
  覆盖 Decoder.Buffered 盲区）；enrollWorker 补尾部检查；413/400 语义区分；
  serveArtifact 双根（DataDir+CacheDir）；collection 重复 409；denied_actions
  补全。
- **repository/domain**：assignment sentinel 下沉产生点；collection UNIQUE →
  sentinel；chunk 测试成员归属断言。
- **licenses**：libc musl 子节补全作者名单 + 第三方声明（TRE/数学库/ARM
  memcpy/DES/blowfish/smoothsort/public-header）。
- **P3 收尾（第三方裁决采纳）**：顶层 JSON 字符串标量脱敏；resultCh/merge
  误导注释修正。

Docker 镜像 tag：`timingdex:v0.24.1`。

## v0.24.0 — 2026-08-05（全量 review 修复批：8 reviewer 覆盖全部 21 包）

8 子代理并行审查全库（5.4 万行/251 文件）后的 P1/P2 修复，覆盖依赖合规、
API 加固、性能、密钥安全、测试覆盖。

- **依赖合规（P1）**：THIRD-PARTY-LICENSES 真正落盘重写（上次声称修复未生效）
  ——19 依赖全覆盖、删 nhooyr/x-exp、版本对齐 go.mod；CI race 加 `-count=1`、
  timeout-minutes 15、测试合并两步；Dockerfile 基础镜像 1.23→1.25。
- **API 加固（P1）**：workerCompleteJob 加 MaxBytesReader(32KB)；4 处错误回显
  改 writeError（enrollWorker/workerCompleteJob/setWorkerJobAssignment/
  saveCollection）——DB 错误原文不再泄漏给调用方。
- **API（P2）**：serveArtifact 加 cacheDir 前缀防御；denied_actions 补 5 个
  admin-only 写路由；新增 `DELETE /api/v1/admin/webdav/spaces/{id}`。
- **app 健壮性**：provider channel identity 调用 2s 超时（原 Background 无限
  阻塞）；ScanLibraryRoot 连续失败周期性 Error；RunUntilIdle 日志风暴抑制；
  buildClusters 空输入守卫。
- **repository 性能/正确性**：RebuildSearch 不再静默吞 I/O 错误；
  ListProviderChannels N+1 改批量；时间戳解析失败加 debug 日志。
- **media 卫生**：ffmpeg stderr 截断 4KB（防 last_error_message 膨胀）；删
  死代码 firstExifString；probeReadRate 加 5s 超时。
- **密钥安全**：Credential 实现 GoStringer/Formatter（`%+v` 不再泄漏
  APIKey，含测试）；worker 错误消息 redactSecrets 过滤；postJSON 限长；
  UploadArtifact goroutine 感知 ctx 取消；volcasr APIKey 加 `json:"-"`。
- **测试覆盖**：cmd/timingdex 17.4%→55.9%（run() 子命令分发 table-driven）；
  internal/remote 新增 12 个 wire type 往返测试。

Docker 镜像 tag：`timingdex:v0.24.0`。

## v0.23.1 — 2026-08-05（review 修复批）

8 子代理全库审查后的修复（P1/P2/P3 全收），集中于 v0.23 新增的剪辑 agent 接入层。

- **数据竞争修复**：webdavspace 共享 `webdav.Handler` 并发写 Prefix/FileSystem
  改为按 space 缓存 handler（并发请求不再可能跨空间泄露/404）。
- **WebDAV 错误路径**：重复账号 409、空字段/坏 JSON 400、未知 space 404、无效
  kind 400、DELETE、ListSpaces；Service 层 sentinel 错误 + handler 映射。
- **THIRD-PARTY-LICENSES**：重写覆盖 go.mod 全部 19 依赖（删 nhooyr/x-exp，
  补 mcp-go/x-crypto/x-net/x-text/jsonschema/cast/uritemplate/coder-websocket
  许可证）——二进制分发合规恢复。
- **MCP 边界**：~~`inspectLibrary` 带 agent token（远程 403 修复）~~ —— 该声称
  不实（commit f21eb42 仅改 stderr 日志，代码仍以空 token 调用
  `requireTrustedRead` 路由），v0.25.0 已真正修复；错误消息截断
  256B；文档标注 `request_source_media` 是唯一 admin token 工具。
- **页面 XSS 面**：进度页/tags 页 `esc()` 补全 5 字符转义 + `log()` 转义。
- **测试补全**：只读全入口（RemoveAll/Rename/O_TRUNC/O_APPEND）、Revoke、路径
  穿越、并发 race、MCP 5xx/4xx/空结果、账号排序。
- **P3 批量**：dirFile.Readdir 空列表、接口去重、integrity_test 独立文件、
  未知空间 401 统一、LOG_FORMAT 非法值回退 warn。

Docker 镜像 tag：`timingdex:v0.23.1`。

## v0.23.0 — 2026-08-05（剪辑 agent 接入：MCP + 按需 WebDAV 交付）

剪辑 agent 接入的两层能力：`cmd/timingdex-mcp`（MCP 语义检索，Codex 等客户端
可接入）+ 按需 WebDAV 交付空间（素材经软链按需可见、只读、流式）。

### MCP server（cmd/timingdex-mcp）：独立 stdio 二进制，Codex 等 MCP 客户端
  可接入。5 个剪辑语义工具：`inspect_library` / `search_footage` /
  `create_edit_plan` / `revise_edit_plan` / `request_source_media`。agent token
  用于计划类，admin token 用于 WebDAV 软链（该端点 admin-only）。启动不写
  stderr（MCP stdio 严格要求）；基于 `mark3labs/mcp-go`。

### 按需 WebDAV 交付空间

空间初始为空，素材经软链（虚拟映射）按需可见；只读（PUT/MKCOL 拒绝）、
字节从 NAS 原文件流式读取、真实路径不泄露、未请求素材 404。账号 bcrypt
哈希落库（`golang.org/x/crypto`），虚拟 FileSystem 基于 `golang.org/x/net/webdav`。

### 管理端点 + skills

admin 建账号/建空间/软链 asset（`/api/v1/admin/webdav/*`），WebDAV Basic Auth
挂载于 `/spaces/{id}/...`。skills：SKILL.md 补 MCP 优先工作流；references 补
`mcp-usage.md`；`mcp/.mcp.json.example` 供 Codex 加载。

新增依赖：`mark3labs/mcp-go`、`golang.org/x/net`、`golang.org/x/crypto`。

## v0.22.0 — 2026-08-05（优化轮：依赖卫生、测试覆盖、CI 门禁、搜索、可观测性）

三轮并行优化的合集（openspec changes 0021–0033），覆盖：依赖升级、测试覆盖
提升、CI 门禁强化、素材级搜索相关度、分面多选、Rekey CLI、Worker 离线检测、
结构化日志、SQLite integrity 检查。Docker 镜像 tag：`timingdex:v0.22.0`。

### S1 — 依赖卫生 + 测试覆盖 + CI 门禁

- **依赖升级**：`modernc.org/sqlite v1.37.1 → v1.56.0`（落后 19 个 minor，纯 Go
  sqlite 的稳定性/性能修复）；`nhooyr.io/websocket` → `github.com/coder/websocket`
  （原库已归档停更，API 完全兼容，仅 import path 迁移，0 残留）；go 指令
  1.23 → 1.25（按 sqlite v1.56 要求）。
- **测试覆盖**：`internal/ingest`（无人值守扫描核心）覆盖率 3.5% → **82.4%**
  （25 个新测试：嵌套递归、扩展名过滤、变更/删除上报、错误传播、ctx 取消）；
  `cmd/timingdex` 0% → **14%+**（usage/parseWorkerMounts/repeatedFlag/hubTLSFiles
  纯函数补测）；`internal/providers/common` → 60%+（URL 拼接、client 超时、
  认证 header 组合、ReadError 未截断路径）。
- **CI 门禁**：`go test -race` 从 `./internal/...` 扩为全量 `./...`；新增覆盖率
  门槛 step（全量语句覆盖率 ≥ 65%，低于即红，输出低覆盖包 top5）。

### S2 — 搜索相关度、分面多选、Rekey CLI

- **素材级搜索相关度**：`SearchFiltered` 的 FTS 兜底分支加
  `ORDER BY bm25(asset_search)`——素材命中按相关度降序（此前为 FTS5 内部 docid
  顺序），tag 精确命中仍优先于 FTS 文本匹配；golden SQL 测试基线同步更新，
  新增 bm25 相关度排序验证测试。
- **页面分面多选**：景别/运镜/音频/画质/可用性 5 个分面控件从单选改为
  `<select multiple>`，多选值 `join(',')` 传入（后端 `facetWhere` 本就支持
  `IN(...)` 多值），素材类型保持单选；清除筛选同步清空多选。
- **secretstore Rekey CLI**：新增 `timingdex secrets rekey`——轮换数据加密密钥、
  全量重加密、旧 key 备份到 `provider-secrets/store.key.pre-rekey`；复用
  `EnsureAdminToken` 取凭证，无 store/token 时报错不 panic；docs 补轮换操作说明。
- **0027（设计结论）**：`providers.*` → `/providers` 通道双轨经查为刻意 Worker
  信任边界（Worker 不读通道，防通道密钥推给远端节点），关闭为设计决策不迁移。

### S3 — 离线检测、结构化日志、SQLite integrity

- **Worker 离线检测**：`ListWorkers` 按 `last_seen_at` 距今是否超过 90s 派生
  offline 状态（此前心跳把 status 写死 online 永不回落，挂掉的节点永远显示
  "在线"）；阈值 = 3× 默认 30s 心跳间隔，容忍丢 1–2 个心跳；revoked 不被覆盖。
- **结构化日志**：`TIMINGDEX_LOG_FORMAT`（text|json，默认 text）+
  `TIMINGDEX_LOG_LEVEL`（debug|info|warn|error，默认 info）环境变量配置
  slog handler；无变量时行为与默认一致，非法值回退并告警。
- **SQLite integrity**：`timingdex doctor` 增加 `PRAGMA integrity_check`，
  健康库输出 `sqlite: ok`，损坏库报告错误并非零退出。

## v0.21.0 — 2026-08-04（首个 GitHub release）

合并 `hardware-and-mounts` 全量（v0.20 NAS 挂载 + v0.21 无人值守巡检/时间线导出，此前均未发布）与 2026-08-04 全库审查批（OpenSpec 落地、API 硬化、model_runs 边界、21 个 change，见下方对应小节）。Docker 镜像 tag：`timingdex:v0.21.0`。

### v0.21 — Unattended Library and Timeline Export

See `docs/v0.21-unattended-and-export.md`,
`docs/v0.21-provider-deployment.md` and
`docs/v0.21-retrieval-and-search.md`.

- Hybrid shot search ranked its lexical dimension backwards. SQLite's `bm25()`
  returns more-negative values for stronger matches, and the conversion to a
  0–1 score was `1/(1+|bm25|)` — monotonically *decreasing* in match strength —
  while the blend adds it as `0.70*semantic + 0.30*lexical`. So every weaker
  textual match outscored every stronger one. Measured on a fixture where two
  shots carry identical semantic vectors and only the text differs, a shot whose
  description says "rain" four times lost to one that says it once. The
  conversion is now `|bm25| / (2.2 + |bm25|)`, strictly increasing, with the
  saturation point a named constant rather than an accident of the old
  denominator.
- **That fix changes result ordering, and it changes how much the text half
  counts.** The old curve pushed genuine matches toward zero and only produced
  large values at bm25's floor, so the stated 0.30 lexical weight was worth about
  a twentieth of the blend in practice; a strong match contributed 0.045 and now
  contributes 0.216. The weights themselves are untouched — `0.70/0.30` has not
  moved since the initial commit and `docs/v0.14.1-security-retrieval-checkpoint.md`
  already reserves them for a benchmark that has not been run. Expect adjacent
  pairs to swap where two shots are semantically close and one is textually
  richer; the ordering was consistently wrong in that case, not randomly.
- Chinese text sitting next to a digit or a Latin letter is searchable again.
  The segmenter scanned an ASCII word run while the rune was a "word character",
  and Go counts CJK as a letter, so a run that started on ASCII swallowed the
  Chinese that followed it and emitted no bigrams — while the query side always
  builds a bigram phrase. `2024年春节的素材`, `用A7S3拍的空镜` and `iPhone拍摄的画面`
  each indexed as a single opaque token and could not be found by any of the
  words in them. Pure-Chinese text was unaffected, which is why this survived:
  the failure needed a digit or a Latin letter in the sentence, and a transcript
  full of dates, resolutions and camera models has one everywhere. Camera reel
  names (`A001_C002.mp4`) still index as one token, deliberately — the fix yields
  only at a CJK boundary, so the underscore stays a word character.
- Migration 0022 re-tokenizes the existing index, because the fix above changes
  what indexing *would* produce and nothing about the rows already written.
  It flips `fts_index_state`'s existing `cjk_bigram_v1` key back to `pending`
  rather than adding a key: that flag only ever meant "the bigram shadow tables
  exist", never "the segmenter that filled them was right", and `Migrate` rebuilds
  whenever it reads anything but `ready`. The rebuild runs inside `Migrate`,
  before the Hub serves anything, and costs a one-time startup pause measured at
  250ms for 10,000 assets and 1.4s for 50,000 with a transcript each. An
  interrupted rebuild rolls back to the pre-migration rows with the flag still
  `pending`, so the next start retries it rather than leaving a half-built index.
- `docs/v0.21-provider-deployment.md` writes down which provider plan to buy and
  how to pool several keys on one channel, because nothing in the code says it:
  the recommended entry plan has a hard monthly cap and no overage, so the worst
  case is the library pausing rather than a surprise bill, and several accounts'
  keys added as members of one channel multiply the allowance while the pool
  skips whichever is spent. It also records the trap that the provider name on a
  channel is only the adapter — which wire protocol to speak — while the
  endpoint, model and auth come from the channel record, so the built-in default
  for a provider is not the only endpoint it can reach. Reading the default as
  the only option is what makes "video understanding cannot run on the plan"
  look true when it is not.

- A deterministic 4xx from a channel-routed provider call no longer burns the
  full retry ladder. `redactError` rebuilt the error with `errors.New` on the
  flattened text, which destroyed the chain, so `isRetryableJobError`'s
  `errors.As` never found the `*common.StatusError` it classifies by and an
  invalid key or a rejected model was retried three times against a paid
  provider on every job. The obvious fix — wrapping the original with `%w` — was
  rejected because it hands any caller of `errors.As` a `StatusError` whose
  `Body` is the raw upstream response, and a relay can echo the request key back
  into that body. The status is instead rebuilt from redacted parts: the chain
  is classifiable and no unredacted string survives anywhere in it for a caller
  to print into `jobs.last_error_message`.

- The library page grew a second filter row exposing the six controlled
  vocabularies and a duration range, which `/api/v1/assets` had accepted for a
  while with nothing in the browser able to reach them. The options are
  generated in Go from the `normalize` vocabularies instead of being typed into
  the page's JavaScript, and every value carries a Chinese label that a test
  requires, so a vocabulary addition either grows the control or fails the
  build rather than rendering an English slug in a Chinese UI. Duration is
  entered in seconds and converted before it is sent. A non-ok response used to
  collapse into the empty-library state, which turned `parseFacetFilter`'s
  deliberate 400 — a misspelled facet must not look like having no footage —
  back into exactly that; the server's message is now shown as an error.
- The replacements that assemble that page are a slice rather than inline
  `strings.Replace` calls, and a test replays them requiring each anchor to
  match exactly once and the replay to reproduce the shipped page byte for
  byte. A stale anchor is otherwise a silent no-op: the page still builds,
  still serves, and the feature is simply gone, with no compile or runtime
  error to notice.

- The processing-summary strip and the card list no longer describe different
  sets of assets. They receive the same query string, but only the list
  understood the facets, because `AssetCollectionFilter` was `AssetCardFilter`
  minus them. It now embeds `FacetFilter`, which `encoding/json` flattens into
  the same persisted object; every facet field is `omitempty`, so a collection
  saved earlier has no facet keys, unmarshals to the zero value that matches
  everything, and returns what it always did. The summary query gained the
  `asset_analysis` join the facet predicates need — a LEFT join, because an
  inner one would drop every asset with no analysis row, which in a freshly
  scanned library is nearly all of them, and the strip would read near-zero with
  no filter applied at all.
- Two adjacent inconsistencies went with it: an unparseable `date_from` was
  silently dropped while a misspelled facet answered 400, the same failure class
  decided both ways inside one request; and `ListAssetCardsInCollection` built
  its filter by hand instead of through `collectionToAssetCardFilter`, so a
  saved collection carrying facets would have narrowed its summary but not its
  cards.

- A provider channel now rejects two members sharing a label instead of letting
  SQLite do it. A comment claimed the opposite was allowed; the schema has said
  `UNIQUE(channel_id, label)` since migration 0013, and that false sentence is
  what made the earlier label-fallback bug look deliberate. The consume-once
  fallback had stopped the clobber but produced a member list the database
  cannot store, so typing a name twice earned a bare constraint error naming no
  label. The check runs before anything is written, so a rejected patch leaves
  the secret store untouched, and the message names the label and nothing else.
  The test covering this previously ran against a fake repository that stored
  two same-label members happily — it asserted an outcome real SQLite refuses.

- A provider-channel write no longer echoes a downstream layer's error text to
  the browser. The create handler passed `err.Error()` through unconditionally,
  and `SaveProviderChannel` reaches both SQLite and the secret store, so a
  duplicate label arrived in the response body as
  `UNIQUE constraint failed: provider_channel_members.channel_id, … (2067)` and
  anything else those layers said would have arrived the same way. Its sibling
  had the opposite fault: the update handler collapsed every failure into one
  generic sentence, so the message naming the duplicated label never reached the
  operator who caused it. Validation failures now wrap a sentinel, and only text
  wrapping it is written to a response — the same shape `writeExportError`
  already used. The duplicate-label check itself moved to one function both
  paths call, so create and update cannot drift on what "duplicate" means.

- A facet-narrowed search is now exact rather than nearly right. The library
  page's search box called the one search endpoint with no facet support, then
  intersected its ids client-side against a separate 300-row card fetch — two
  independently capped windows whose overlap silently shrank as the library
  grew, with nothing on screen to distinguish a dropped hit from no match.
  `SearchFiltered` folds the facet predicate into each query as a correlated
  `EXISTS` guard, so `LIMIT` counts facet-matching rows instead of being applied
  first and filtered afterwards. A zero `FacetFilter` builds no guard at all, so
  the unfiltered path's SQL is byte-identical to what it was and keeps the query
  plans the `UNION`-of-single-predicates comment was written to protect; a test
  captures the SQL actually sent and diffs it against that baseline.
- `GET /api/v1/assets` accepts `ids=` so those hits are fetched directly instead
  of re-derived from a capped listing. An over-cap id list is a 400 rather than
  a truncation, and an id list with no explicit limit sizes the limit to itself
  — silently dropping ids is the failure the parameter exists to remove, and it
  would have reappeared one field over. Rendering order is unchanged: it follows
  the card query, not search relevance, which the tag and text branches do not
  currently share a comparable score for.

- `timingdex serve` can now rescan every library root on a timer and drain the
  queue behind it, so footage dropped onto a share is indexed without anyone
  running `root scan` and `pipeline run`. Off by default
  (`library_supervisor.enabled`, `scan_interval_minutes`, default 15, floored
  at 1): upgrading an existing install must not make it start working — and
  spending — on its own.
- Polling rather than fsnotify, deliberately. The target deployments keep
  footage on SMB/NFS shares where inotify either does not fire for changes made
  by another host or is unavailable, and the Docker media bind uses `rslave`
  propagation, so a share can appear and vanish underneath the bind while the
  Hub runs. A watcher that misses one of those fails silently — the footage is
  simply never indexed and nothing says so. A timer's worst case is being one
  interval late.
- The unattended pass — the whole pass, not just the scan — respects the
  throttle's off-peak window: walking a NAS tree is itself load, and starting a
  pipeline pass outside those hours begins paid analysis nobody asked for.
  Anything a human triggers is ungated, and the per-job levers inside
  `RunUntilIdle` are untouched. `/progress` reports `held_off_peak` and
  `held_until` so a supervisor working exactly as configured is not
  indistinguishable from one that has died.
- The pass runs inline on the loop's own goroutine and drives the pipeline via
  the new `Service.TryRunPipeline`, not `StartPipeline` — the latter detaches
  onto `context.Background()` by design, which would keep making paid Provider
  calls after the server stopped serving. `serve` now joins the loop before
  returning, which is only meaningful because nothing is detached.
  `TryRunPipeline` shares the single-run guard with the `/progress` button, so
  an operator-triggered run and the supervisor cannot drive the same disk at
  once.
- When every key on a capability's route fails at once — on the recommended
  plan that is a spent monthly quota answering 429 everywhere — the job is now
  parked on wall-clock time for five hours and the attempt that lease consumed
  is handed back, instead of being retried through its whole budget inside ten
  seconds and failed permanently for an outage. Each of those retries is a paid
  call. `/progress` shows these as awaiting quota, with a button
  (`POST /api/v1/pipeline/resume-deferred`) to release them early. The state is
  reported as the Hub-assigned constant `provider_route_exhausted` rather than
  upstream text, which can embed a truncated response body and stays behind the
  admin token.
- Stored timestamps sort correctly again. `formatTime` emitted
  `time.RFC3339Nano`, whose layout strips trailing zeros from the fraction, and
  every timestamp in this database is compared as a **string** by SQL. Variable
  width means lexicographic order stops matching chronological order:
  `…34.5123Z` compares *greater* than `…34.51234Z` because `'Z'` outranks `'4'`.
  Measured at 0.3412% of consecutive `time.Now()` pairs; zero after the fix. It
  is a known Go footgun (golang/go#19635), and notably neither `mattn/go-sqlite3`
  nor `modernc.org/sqlite`'s own `time.Time` conventions are sortable either, so
  there was no driver default to fall back on.
- The visible symptom was `LeaseNextJob` occasionally returning nothing for a job
  that was due, which self-heals on the next poll and made this look minor. It
  was not the whole footprint: the capture-date and shoot-session range filters
  behind the browse UI could silently exclude an asset with no retry to correct
  it, and `ORDER BY created_at DESC LIMIT 1` — how the newest transcript, derived
  artifact and library summary are chosen — could pick a stale row that stays
  picked until something newer is written.
- Migration `0021` rewrites every existing row in place, because the far worse
  state is one column holding both formats at once. It covers 81 columns and is
  idempotent: a correct value is exactly 30 characters, so the guard skips rows
  already converted. A test reads the live schema from `sqlite_master` and fails
  if any timestamp column is missing from that migration — a column added later
  and forgotten would silently reintroduce the mixed-format state this fixes.
  `assets.missing_since` is in it: a timestamp column whose name a naive `_at`
  sweep would have missed.
- No `time.Parse` call site changed. `time.RFC3339Nano` reads the fixed-width
  form back to the identical instant, which is what made the fix one constant
  and one migration instead of a rewrite of every read.
- The controlled vocabulary is now something you can filter on. `asset_type`,
  `shot_size`, `camera_motion`, `audio_type`, `quality` and `usable_as`, plus
  `min_duration_ms`/`max_duration_ms`, are accepted by the asset browse endpoint
  and by all three shot-search endpoints. Values within one parameter are OR'd
  (`shot_size=wide,medium`), different parameters are AND'd, and a facet narrows
  a text query rather than replacing it. Every one of these fields was already
  being extracted, validated and stored — none of it was reachable.
- An unrecognised facet value is a 400 naming the parameter, not an empty
  result. A silent no-match on a typo is indistinguishable from an empty
  library, which is the worst possible answer to give someone who is looking for
  footage they know they have.
- All six fields live only in `asset_analysis`, one row per asset, so a
  shot-level search resolves them through the shot's asset. Duration does not
  work that way: the same two bounds mean the asset's probed length when
  browsing and the individual shot's span when searching shots, and the type
  that carries them says so.
- A rescan now enqueues only the assets it actually changed, instead of walking
  every asset in the database and re-enqueuing all of them. The old sweep read
  `ListAssets(100000)` across *all* roots and called `EnqueueAsset` per asset —
  two queries each — on every pass; since a probe job's input hash is derived
  from the location's absolute path and mtime, every one of those inserts was
  ignored unless one of those two had moved. Harmless when a scan was something
  an operator typed; continuous churn now that the supervisor runs one every 15
  minutes. `UpsertScannedFile` reports whether the file it just wrote changed
  anything the pipeline keys on, and the scan carries those ids out on
  `ScanResult.ChangedAssetIDs` (hidden from JSON — the struct is a response
  body and keeps its shape).
- Assets that never got a probe job at all are caught up by a single bounded
  query per root rather than by the sweep, and enqueue failures are now logged
  instead of discarded — previously a failed enqueue was silent and the next
  full sweep was what happened to fix it. Jobs that exist but failed are
  deliberately still out of scope: the old sweep did not revive them either
  (`INSERT OR IGNORE` against an existing row is a no-op whatever its state),
  and requeuing failed work stays an explicit operator action.
- An approved repurpose plan can now leave the Hub as something an NLE reads:
  `GET /api/v1/repurpose/plans/{id}/export.edl` (CMX3600) and
  `.fcpxml` (FCPXML 1.9), with download buttons on `/repurpose` that appear
  only once a plan is approved. Sections keep the order and the selections a
  human approved; nothing is re-ranked or re-selected at export time.
- Both routes require the Hub administrator token and reject the agent token,
  and `export_timeline` is now declared in `denied_actions` on
  `GET /api/v1/agent/capabilities`. An FCPXML embeds the absolute path of
  every original, which is precisely what `access_original_media_paths`
  denies, and an EDL is the artifact someone cuts with — so both sit on the
  human side of the approval boundary rather than in the agent allowlist.
  `skills/timingdex/references/api-contract.md` says the same thing, so a
  Skill reading the handshake does not attempt a route that will 401.
- A plan that is not approved returns 409 rather than a document. A plan whose
  selected shot sits on an asset the pipeline has not probed returns 422
  naming the file — by basename, not by path, because the same text reaches
  the log, which is not admin-gated.
- Camera reel metadata is preferred over the filename when naming a source,
  since that is the identifier an assistant editor matches against. Two
  originals that carry the same reel id (two cards both labelled A001 is
  ordinary) are disambiguated before `nleexport` sees them, because its own
  collision handling works on distinct input strings and would silently
  relink one shot to the other's file.
- An expired lease is now reclaimable, and closing a job out now proves who
  holds it first. `LeaseNextJob` leases `state IN ('pending','failed','running')`
  where `lease_expires_at<=now`; before it was `('pending','failed')` only, so a
  job abandoned by a dead Worker sat in `running` forever, with no recovery but
  hand-written SQL. Acquisition was always a correct compare-and-swap —
  `LeaseNextJob` re-checks expiry inside its UPDATE, so two racing leasers
  cannot both win. That made reclaiming safe, and reopened a hole the follow-up
  closed: the four Hub-local completion writes (`CompleteJob`,
  `FailJobTerminally`, `RetryJob`, `DeferJob`) had no ownership predicate at all
  — `WHERE id=?` — while their Worker-side twins in `remote_jobs.go` carried one
  all along. It was reachable, not theoretical: the Hub-local pipeline mints one
  lease of a fixed, never-renewed two minutes and then runs a stage
  synchronously that outlives it routinely (a software x264 proxy of a long
  clip, a windowed analysis). A paired Worker on `derive`, or a second Hub
  process (`serve` and `pipeline run` are both documented), reclaims the job,
  and the original holder's write then overwrites the new holder's row — the
  same job runs twice, and on `analyze` that is a second paid provider call.
  `SaveArtifact` needed the same predicate for the same reason and got it,
  inside its `INSERT ... SELECT` rather than behind a preceding read. A lost
  race is contention, not a broken database, so the run logs it and moves on,
  recognising the case through `domain.ErrJobLeaseLost` rather than by matching
  error text.

### v0.20 — NAS Mounting

- Documented the NAS-mounting decision ladder in
  `docs/v0.20-nas-mounting.md`: given the Hub runs in a container as a fixed
  unprivileged uid 10001, the least-friction path is still to mount the
  share on the host first and bind it in — the same choice Jellyfin, Immich,
  PhotoPrism, Frigate and Navidrome all make, because a decoder needs cheap
  random access and every userspace/FUSE alternative (davfs2, sshfs, a Go
  SMB client) degrades seek. `docker-compose.yml`'s media bind now sets
  `bind.propagation: rslave` and binds a *parent* directory (`/mnt/remotes`
  on Unraid, `/mnt` on bare metal) rather than one specific share, so a
  share mounted on the host *after* the container is already running
  becomes visible inside it without a restart — the previous default,
  `rprivate`, made that permanently invisible until the container was
  recreated. `rslave` is one-directional and Linux-only; Docker Desktop
  ignores it rather than erroring.
- Both Unraid templates now ship the media bind as `Mode="ro,slave"` and
  default it to `/mnt/remotes`. Unraid's Access Mode control offers slave as
  a first-class choice ("Read Only slave") for precisely this case, so the
  propagation does not have to be smuggled through `ExtraParams` the way the
  GPU device flag is — it stays an ordinary `Config Type="Path"` entry the
  operator can edit in the WebGUI. It matters more on Unraid than anywhere
  else: Unassigned Devices remounts its remote shares when the array stops
  and starts, so even a share that was present at container start goes
  stale behind a private bind.
- Ranked the Docker-native volume alternatives rather than treating them as
  equivalent. NFS via the `local` volume driver's `type: nfs` option is the
  recommended docker-native path because NFS carries no password at all —
  exports are authorised by client IP — so nothing sensitive is ever written
  anywhere. SMB via the same driver is documented as a fallback, not a
  default: the `local` driver calls `mount(2)` directly rather than the
  `mount.cifs` helper, so it cannot accept `credentials=` — the password has
  to go inline in `o:`, where it lands in cleartext in `docker volume
  inspect` and in `opts.json` on disk (docker/cli#2802, open since
  2020-10-20). A related leak, the same password appearing in mount-failure
  error text, was moby#43596 and was fixed by moby#43597; the storage
  exposure is not a tracked bug, it is how the syscall path works, and it
  remains. That is a real regression against this project's own boundary —
  provider keys live only in the encrypted `secretstore` and never reach
  SQLite, an API response or a log line — which is why `internal/mount`'s
  existing host-side SMB guidance (a 0600 `credentials=` file, which the
  kernel helper *does* support) stays the recommended way to mount SMB at
  all.
- Recorded, with reasons, what stays out of scope: mounting inside the
  container (needs `CAP_SYS_ADMIN` or a bind-mounted `docker.sock`, the
  latter equivalent to host root and a direct defeat of running as uid
  10001); a userspace SMB client such as `go-smb2` (gives an `io.Reader`,
  not the path FFmpeg needs to seek on, and pulls in a dependency beyond the
  stdlib plus the two libraries this project allows itself); AFP (no AFP
  server has shipped in macOS since macOS 11, and its only maintained Linux
  client is FUSE-based, carrying the same privilege cost as mounting inside
  the container); WebDAV/SSHFS (same FUSE privilege cost, same weak
  random-access performance); rclone's Docker volume plugin (capable, but a
  separately-installed, separately-updated moving part outside this
  project's own release); and iSCSI (block-level, single-initiator, the
  wrong shape for a shared library).
- The `/library-roots` wizard now detects whether the Hub is running
  containerised and frames its mount commands as host-side steps when it is,
  since a container has no business acquiring `CAP_SYS_ADMIN` to mount
  anything itself. Containerised, it also renders a Compose `driver_opts`
  stanza for the share that was actually typed — one stanza, not a menu,
  because a `RootInspection` knows only the single address it was given and
  cannot invent an NFS export path for a NAS reached at an SMB one. An NFS
  address therefore yields the credential-free form with nothing to warn
  about; an SMB address yields the fallback form with its caveat rendered
  *above* the YAML, since an operator who has scrolled past the block to
  copy it has already pasted the password, plus a note that re-running the
  wizard against an NFS export avoids the trade entirely.
- README's NAS section now points at the new document for the reasoning
  instead of re-explaining it, and keeps the `source_staging.mode: copy`
  advice unchanged — copy-mode staging is independent of how the share
  reaches the Hub in the first place.
- Fixed the shipped authentication defaults for the four Volcengine plan
  provider blocks (`volc_agent_plan`, `volc_coding_plan`, and their
  `_embedding` counterparts) in both `internal/config` and
  `config.example.json`: they now send `Authorization: Bearer <key>` instead
  of `X-Api-Key` with a bare key. An Agent Plan account was verified to
  accept only the bearer form and to answer the previous default with 401 —
  a failure that surfaces against the operator's own key and therefore reads
  as a bad key rather than a bad default, which is why the workaround people
  found was to rebuild the same routes as generic `openai_chat` /
  `openai_embeddings` channels whose blank auth fields fall through to
  Bearer. The Coding Plan blocks are corrected by inference, not
  measurement: same Ark host and API family, different path prefix, and no
  Coding Plan account was available to test. Both fields are stated
  explicitly rather than left blank because blank is not a single meaning
  here — `common.Endpoint.NewRequest` defaults an empty scheme to Bearer,
  while the Worker JSON proxy in `internal/app` used to send the key
  unprefixed, so a blank scheme would have kept the 401 on the Worker path. An
  existing
  `config.json` still overrides these defaults; README now says which two
  fields to delete. `volc_asr` keeps `X-Api-Key`, which is correct for
  ByteDance's openspeech WebSocket service.

- Worker provider access (`IssueWorkerCredential`, the opt-in direct-credential
  path, and `issueProviderCredentialForProxy`, behind the Hub JSON proxy) stays
  legacy-`providers.*`-only, on purpose: both build their `credentials.Broker`
  from `s.cfg.Providers` alone and deliberately never read
  `internal/providerchannels`, because a Worker getting channel-scoped keys,
  member pools and health state is exactly what the direct-credential path
  being default-deny exists to prevent. What was missing was a way to say so:
  on a Hub configured entirely through `/providers` channels, both paths used
  to fail with the same message a Hub with nothing configured at all would
  give — the proxy path discarded the broker's own detail entirely, down to a
  flat "provider proxy is not configured" — so a correctly-configured channel
  read as a broken one. `Service` now checks whether a channel exists for the
  capability (`ListProviderChannels`, presence only — never a member, secret
  or health state) and wraps the broker's failure in one of two new sentinels,
  `ErrWorkerProviderConfiguredAsChannelOnly` or `ErrWorkerProviderNotConfigured`
  (`internal/app/service.go`, classified in
  `internal/app/provider_channel_operations.go`), matching the
  `errors.Is`-over-message-text shape this repo settled on for the Worker
  lease boundary and the repurpose approval boundary. Neither sentinel's text
  carries anything past a provider/operation name — the broker's own error
  never did either — so nothing new reaches `jobs.last_error_message` that the
  key-redaction boundary in `internal/providers/common` has to account for.
  README and `docs/v0.16-operations.md` and `docs/v0.21-provider-deployment.md`
  now say plainly that `/providers` and Worker access are two different
  configuration surfaces, and that a capability needs its own `providers.*`
  entry before a Worker can reach it either way. The two Worker handlers
  (`workerCredential`, `workerProviderProxy` in `internal/api/server.go`) now
  read those sentinels too: `ErrWorkerProviderConfiguredAsChannelOnly` answers
  403, not the flat 400 both routes gave every such failure before — the
  Worker's request was fine and the capability is genuinely configured, so this
  is a standing policy refusal, the same shape as the neighbouring
  `AllowWorkerProviderCredentials`-disabled 403. `ErrWorkerProviderNotConfigured`
  answers 503: nothing about the request is wrong, the Hub simply has nothing
  configured for the capability yet by either method, and configuring one later
  makes the identical request succeed, which 503 signals and 400 does not.
  Neither message is built from `err.Error()` — both are fixed text plus the
  operation name already on the Worker's own path parameter — so rewording
  either sentinel's `errors.New` string cannot change what the Worker receives.

- A quota-exhausted key on a pooled channel used to take the whole route down
  for every job behind it, on exactly the several-plan-keys-in-one-channel
  deployment `docs/v0.21-provider-deployment.md` recommends.
  `providerpool.ClassifyFailure` classified a failure by probing for a
  status-bearing error through an interface, but every adapter's error carries
  its status in a *field*, which satisfies no interface — the probe returned 0
  for every real provider failure, and classification fell through to matching
  digits in "provider returned HTTP %d", right by accident for most of 4xx/5xx
  and wrong for 402, which matched nothing and fell to the closing
  `NonRetryable`. `Executor.Execute` returns immediately on `NonRetryable`, so
  one member out of credit ended the channel for that job. `*StatusError` now
  exposes its status through a method, and 401/402/403 get a class of their
  own, `MemberSpent`: those answer about the key, not the request, so
  `Pool.complete` retires that member (the same state `SetEnabled(false)`
  produces) instead of cooling it, and the next member in the pool serves the
  call. Retiring a member does not spend one of the channel's three
  per-channel tries — charging the budget for a call that was never going to
  work would let two dead keys hide a live third — and when nothing is left,
  the route reports `ErrRouteExhausted` the same way a fully cooled-down route
  already did, parking the job where `/progress` shows it rather than
  retrying it to a certain, repeated failure.
- Retirement is deliberately not durable. It lives only in the pool's
  in-memory state, scoped to the running process: a revoked key and an
  hour-long IP block look identical from where the pool sits, and only a human
  should decide which one it was, so both editing the channel and restarting
  the Hub clear it and let the key prove itself again. `MemberSpent` never
  comes from matching an error's text either — only from the status a
  `*common.StatusError` carries — because a relay that echoes a permanent-
  sounding phrase into a body must not be able to talk the pool into retiring
  a key that never actually failed. `providerchannels.MemberStatus` gained a
  `Retired` field, and `ChannelStatus.Available` is now `false` once every
  enabled member on a channel has retired.
- `GET /api/v1/admin/provider-channels/status` serves that runtime view, because
  a key that silently stops being selected is worse than one that fails loudly:
  without this, the only evidence an operator had was jobs beginning to park.
  It is a different question from `GET /api/v1/admin/provider-channels`, which
  returns the stored channel rows — retirement is never written to the database,
  so it cannot appear there. The endpoint reports only what it actually knows:
  runtime state lives on an executor built on demand per capability, so a
  capability that has not routed a call since the Hub started, or since its
  channel was last edited, answers `has_runtime_data: false` rather than
  rendering healthy. Every capability reads that way immediately after a
  restart, which is the truth rather than a fault — retirement is per-process by
  design. Nothing secret crosses the boundary: `MemberStatus` carries
  `secret_configured` as a bool and never a ref or a key, and `Channel` has no
  API-key field at all. The `/providers` page still shows configuration only;
  drawing retirement into it is separate work.
- `isRetryableJobError` treats a raw `MemberSpent` status as retryable for the
  one case the executor's own exhaustion check cannot resolve first: a member
  merely saturated by a concurrent caller (its `MaxInflight` held elsewhere)
  was never attempted, so `Pool.Select` returns `ErrNoAvailable` without an
  attempt, `routeFailingEverywhere` correctly declines to call an unattempted
  member exhausted, and the raw spent-key error surfaces here unwrapped
  instead of behind `providerchannels.ErrRouteExhausted`. The classification
  is by status alone, never by the response body, for the same reason the
  digit-matching removal below exists: an upstream message that happens to
  read as permanent cannot be allowed to flip a key the pool has already
  retired back into a fail-permanent job.

- `providerpool.ClassifyFailure`'s status-free fallback no longer treats any
  three-digit run in an error's text as though it named an HTTP status. This
  makes classification of a status-free error narrower, not smarter — it
  removes a pattern match, it does not add one. Every HTTP-speaking adapter
  already reports its status through `*common.StatusError`, classified before
  this fallback ever runs; the fallback exists for adapters with no status to
  carry, and the concrete failure was `internal/providers/volcasr`'s WebSocket
  path, which forwards a Volcengine error payload verbatim into the error
  text. A Volcengine error code such as `45000002` contains "500" as a plain
  substring with no relationship to HTTP semantics, so a permanent provider
  rejection (an unregistered app ID) was classified `Retryable` and burned the
  full three-attempt backoff on a call that could not have succeeded on any
  attempt. Classification below the status probe is now vocabulary-only.

- Approving a superseded revision that also has an unselected required
  section now reports the selection defect — "required section needs an
  explicit selection before approval" — rather than "not the latest". Both are
  true of that revision, and the alternative was keeping a second, race-prone
  copy of "which revision is latest" up in `Service` solely to choose between
  the two messages. This is one visible ordering change in a larger move: the
  human-approval boundary CLAUDE.md calls non-negotiable — a plan an approval
  locks, section selections a human must confirm before approval succeeds —
  used to be checked three times across two packages (a `Service` pre-check,
  the repository's write-time recheck, and a second `Service` re-read after
  the write failed, purely to turn the repository's unclassified prose back
  into a 404 or 409). Only one of those three ever ran inside the transaction
  that actually decides, so the other two were guesses that happened to be
  right whenever nothing else was writing at the same time. `domain` sentinels
  (`ErrPlanNotFound`, `ErrPlanImmutable`, `ErrPlanRevisionNotFound`,
  `ErrPlanRevisionNotDraft`, `ErrPlanRevisionNotLatest`) now let the
  repository's own write-time check hand back a classifiable answer directly,
  so the pre-check and the post-write re-derivation — both of which could only
  ever confirm what the write itself already knew — are gone. The same shape
  was applied to the Worker-lease boundary (`domain.ErrJobLeaseLost`, replacing
  seven sites in `remote_jobs.go` that matched "does not own active job" text,
  one of them in slightly different words, plus the artifact-upload rename
  `ErrWorkerArtifactLease`) and to the Repurpose export boundary: a missing
  plan id now wraps `app.ErrPlanNotFound` (an alias of the same
  `domain.ErrPlanNotFound` the revision boundary uses — `GetRepurposePlan`
  returns `(nil, nil)` rather than `sql.ErrNoRows` for a missing plan, so the
  404 previously had no structural marker at all and matched the literal
  phrase "not found") instead of falling to `writeExportError`'s default, and
  every `nleexport` rejection — a mixed frame rate, a shot past the end of its
  file — now wraps `nleexport.ErrInvalidTimeline` instead of being recognized
  by the message prefix `"nleexport: "`. The property all of it buys: rewording
  any one of these errors' `errors.New` string for clarity is now just a
  wording change, not a silent 500. A revision request with an empty section
  list is folded into the same identity (`ErrInvalidRepurposeRevision`) as its
  neighbouring per-section checks, so it now answers 400 instead of falling
  through to a 500 that read like a Hub bug for what is simply a malformed
  request.

- `/worker-setup`'s script had been dead since v0.18: a ternary's `:` was
  written inside the string literal it belonged outside of, so the page
  shipped two adjacent string tokens with no operator between them — a syntax
  error, in a page that keeps all of its JavaScript in one script block, so
  nothing on the page ran. No pairing token, no generated script, none of the
  documented Worker onboarding flow. It stayed invisible for three versions
  because the page's own test greps the rendered HTML for step markers, and
  every marker was still there in a page whose script never parsed — that
  shape of test structurally cannot see a syntax error. The page served,
  nothing errored, the feature was simply gone. A new test now extracts every
  served page's script block and runs `node --check` on it (skipped when
  `node` is absent, the same way the media integration test skips without
  `ffmpeg`), so the next syntax error like this one fails naming the route
  instead of shipping silently again.

- `THIRD-PARTY-LICENSES` now lists the license text for the twelve modules
  `go list -deps` shows actually linked into the binary (not everything
  `go.mod` mentions — thirteen more appear only in the test and tooling
  graph). Eight of the twelve are BSD-3-Clause, whose attribution clause
  covers binary redistribution and not only source, so shipping none of them
  was the kind of omission a public repository gets noticed for.

### 2026-08-04 全库审查与 OpenSpec 落地（随 v0.21.0 发版）

- CI 红灯修复：素材根检查对普通空目录不再误报「未挂载的挂载点」——该警告只对已注册的 library root 发出，全量 `go test ./...` 恢复全绿。
- `model_runs` 边界测试：补充了模型输出写入失败、截断和重入场景的覆盖，
  `CommitAnalysisWithShots` 在写入规范表之前增加硬守卫，拒绝越界数据。
- Worker credential 脱敏修复：Worker 凭证路径的若干错误字符串已去除密钥片段，
  与 Hub 侧脱敏边界对齐。
- API 硬化：`writeError` 系列不再将内部错误原文写入响应体；上传与代理端点
  增加 `MaxBytesReader` 限制；`http.Server` 增加读写超时，防止慢客户端占用。
- 分页 tie-breaker：在排序键相同时，分页游标增加确定性的第二排序键，避免
  翻页漂移和重复行。
- `secretstore` Rekey 修复：密钥轮换路径的原子写入和回滚逻辑补齐，旧密钥
  备份文件不再残留于失败中途。
- 文档行号门：`docs/` 中文档中的行号引用统一刷新为当前代码实际行号，
  并增加 CI 检查以防止再次漂移。
- P2/P3 批量修复清单：合并置信度边界、Curator 词干规范化、CJK 字符范围收紧、
  JSON tag 补全、volcasr 响应截断与死代码清理、Windows 路径分隔、LEFT JOIN
  语义、cooldown 默认值、页面 JS 健壮性、Worker 产物校验等，低风险集中修复。
- Provider 错误路径测试：补齐各适配器在 4xx/5xx/超时/空响应下的行为覆盖，
  避免错误分类逻辑因缺测回退到文本匹配。
- Skills 契约恢复：`skills/timingdex/references/api-contract.md` 与
  `/api/v1/agent/capabilities` 的 `allowed_actions`/`denied_actions` 重新对齐。
- `openspec/` 引入：新增 `openspec/` 目录，收录本轮审查产生的变更规格与
  设计记录，作为后续变更的参考基线。

## v0.19 — GPU Docker Images and Unraid Deployment

- Added a `gpu` Dockerfile build target (`docker build --target gpu`) that
  layers the Intel/AMD VAAPI userspace on top of the existing runtime image.
  NVIDIA needs nothing baked in here: Debian's stock `ffmpeg` already ships
  `h264_nvenc`/`hevc_nvenc`, and the only reason NVENC fails in a container
  is that the runtime libraries and device nodes have to come from the host,
  matched to its loaded kernel module — baking any NVIDIA `.so` into the
  image would be a version-skew trap the moment the host driver updates.
  `debian:bookworm-slim`'s default sources only enable the `main` component,
  so the `gpu` stage turns on `non-free` before installing
  `intel-media-va-driver-non-free`, `intel-media-va-driver`, `libvpl2`,
  `mesa-va-drivers` and `vainfo` — one package at a time, each allowed to
  fail on its own, the same pattern `deploy/prepare-node.sh` already uses so
  that one renamed package can't take a shared `apt-get` transaction down
  with it. A trailing alias stage keeps a plain `docker build` (no
  `--target`) pointed at the CPU image; without it, adding `gpu` after
  `runtime` in the file would have silently made the GPU image the default.
- Added commented-out device-passthrough stanzas to both `hub` and `worker`
  in `docker-compose.yml` for Intel/AMD (`/dev/dri` + `group_add`, since
  both containers run as a fixed unprivileged uid and the render node's
  group ownership is host-specific) and NVIDIA
  (`deploy.resources.reservations.devices` plus
  `NVIDIA_DRIVER_CAPABILITIES=all`). Nothing about the existing hardening —
  `ports`, `TIMINGDEX_BIND`, the read-only media binds, the Worker's
  read-only rootfs — changed to add them. The NVIDIA comment calls out the
  Container Toolkit's default `compute,utility` capability set explicitly:
  it does not inject `libnvidia-encode.so.1`, so `h264_nvenc` lists in
  `ffmpeg -encoders` and then fails on the first frame, indistinguishable
  from no GPU at all, until `all` (or `compute,utility,video`) is set.
- Added `deploy/unraid/timingdex-hub.xml` and `deploy/unraid/timingdex-worker.xml`,
  standalone Community Applications templates mirroring the Compose services
  for operators without the (optional) Compose Manager plugin. Device
  passthrough goes through `ExtraParams: --device=/dev/dri:/dev/dri` rather
  than `Config Type="Device"` — the pattern other Unraid GPU-transcode
  templates already rely on more reliably. Both templates' `Overview` fields
  (in Chinese, matching the rest of the product's UI copy) walk through the
  two real Unraid-specific problems this image runs into: Unraid creates
  `appdata` directories owned `root:root`/`nobody:users`, which collides
  with this image's fixed unprivileged uid 10001 and its deliberate absence
  of a PUID/PGID entrypoint wrapper (fixed with a one-time `chown`, not a
  wrapper script — adding one would mean the image doing privileged setup at
  startup, which is exactly what the fixed-uid design avoids); and the
  Worker container's `worker.json` has to exist *before* its first real
  start, which on Unraid means enrolling via a one-off `docker run`, not
  `docker exec` into the persistent container, since a Worker with no
  config exits too fast to exec into.
- Documented which Alder Lake-N parts Debian bookworm's
  `intel-media-va-driver-non-free` 23.1.1 actually covers, because the answer
  differs by SKU rather than by family: N100 (PCI 46D3) has been supported
  since media-driver 22.1.1 and needs nothing extra, while N150 (46D4, the
  Twin Lake refresh) needs 25.1 and therefore `bookworm-backports`. On the
  newer part a hardware encoder lists and then fails on every frame, which
  is indistinguishable from having no GPU until something separates the two
  — which is what the new `verified`/`remedy` reporting is for.
- `README.md`'s Docker section previously said a Linux container sees no
  `/dev/dri` at all, so QSV/VAAPI were simply unavailable under Docker. That
  was true only for the plain image; it now points at the `gpu` target and
  the Compose/Unraid wiring needed to actually hand a container the device.
- Added a `/library-roots` wizard, closing a gap where `/setup` told the
  operator to "add a mount directory on the processing progress page" and
  `/progress` had no such form — the only way to add a root was the CLI. The
  wizard reuses `internal/mount` end to end rather than re-deriving any of
  it: `ParseShare` decides whether the input is a local path or a share,
  `Guidance` produces the per-OS, paste-ready mount commands (credentials in
  a 0600 file, read-only, `nofail`, the WSL `nsenter -t 1 -m` prefix), and
  `RootWarnings` — previously wired only into `doctor` — now also shows up
  before the root is even created. New endpoint `POST
  /api/v1/roots/inspect` (admin-only) answers what the Hub can tell about a
  candidate path without creating anything and without ever listing a
  directory or reporting on any path beyond the one asked about; `createRoot`
  now answers 422 with the same guidance, instead of a generic 500, when
  `AddLibraryRoot` finds an unmounted share, so an API caller that skips the
  wizard is still told what to do. `mount.ParseShare` also had a real
  password leak fixed as part of this: `smb://user:password@host/share` — a
  form any browser address bar accepts — was keeping the password half in
  `Share.User`, which the new inspect endpoint would otherwise have echoed
  straight back into the page that rendered it.

## v0.18 — Disk Load Limits and Worker Onboarding

- Added `timingdex worker run --tray`: a Windows notification-area icon with a
  settings link and a quit item, built on Win32 through stdlib `syscall`. A tray
  icon is one API plus a message pump, and the alternatives each cost the property
  the install wizard depends on — one cross-compiled .exe with no DLLs beside it.
  Qt would need a C++ toolchain and ~50MB of runtime libraries; the CGO tray
  libraries would need a mingw cross-compiler. CI now cross-compiles and vets for
  Windows, since no test on a Linux runner can execute that code.
- Added a loopback settings page on the Worker for the three values the Hub
  cannot push: the hub URL, the pinned fingerprint and the node token. Those are
  the trust anchor — a Worker must already know where to look before it can be
  told anything — so everything else stays Hub-decided. The server binds
  127.0.0.1 only and every route is gated by a random token in the URL, because a
  loopback port is reachable by any process on the machine; a token mismatch
  answers 404 rather than 401, which would confirm the server exists. The node
  token can be replaced but never read back, and submitting it empty keeps the
  existing one so the hub address can be changed without re-pasting a credential
  that is never displayed. Fingerprint validation reuses the client's own
  canonicalisation so the stored form is the compared form.
- Added pipeline throttling, editable at `/settings` and effective on the next
  lease rather than on restart. The pipeline is strictly sequential, so the levers
  are not concurrency: `read_rate` caps FFmpeg's input read speed via `-readrate`
  (an input option, hence placed before `-i`, where it is not silently ignored),
  and a per-job cooldown turns a multi-hour scan from continuous disk load into
  duty-cycled load. Only whole-file reads honour the rate — thumbnail extraction
  decodes one frame, where a cap would only slow the seek. Both default to off so
  an upgrade does not silently slow an existing library.
- `-readrate` support is probed once before use. It arrived in FFmpeg 5.1, and on
  the 4.x builds many NAS and LTS installs still ship, passing it makes FFmpeg
  exit on an unrecognized option before reading a frame — an error matching none
  of `isRetryableJobError`'s permanent phrases, so enabling the throttle would
  have retried every derive through the full backoff chain instead of failing
  usefully. Where the flag is missing the rate limit is skipped with one warning.
- Added off-peak scheduling with two size thresholds: assets above
  `defer_above_bytes` run only inside the window, assets at or below
  `immediate_max_bytes` are exempt from both the deferral and the rate cap. The
  window is judged in the Hub's local time and the settings page shows the Hub's
  own clock, because "01:00" configured from another time zone otherwise means
  something the operator cannot see. Deferral is applied in the lease predicate,
  not after leasing: leasing increments `attempt_count`, so a held job that were
  leased and put back would exhaust its retries long before its window opened.
- The throttle applies to Workers too, decided by the Hub. `remote.WorkerJob`
  carries the source size and rate; a Worker must not be able to opt itself out of
  a limit that protects a disk it shares.
- Added `/worker-setup`, a wizard that generates a ready-to-run install script
  (PowerShell for Windows, POSIX sh for Linux) with hub URL, certificate
  fingerprint, one-time pairing token and library-root mounts filled in. The Hub
  distributes Worker binaries the operator places in
  `$DATA_DIR/worker-binaries/`, so no separate web server is needed on the LAN.
  The script endpoint is admin-only because the script embeds a single-use
  credential, and every request-supplied value is quoted for the target shell.
- Settings now live in a generic `settings(key, value, updated_at)` table.
  Runtime-editable configuration should not need a migration per setting, and
  v0.15 already established SQLite as its home.
- Fixed a stale test: the lease query-plan assertion held its own copy of the SQL
  and kept passing after the size ceiling was added to the real query, asserting
  a plan for a statement nothing executed. The predicate is now shared.

## v0.17 — Retrieval Performance + Agent Credential Boundary

- The unauthenticated read routes (browse, search, thumbnails, proxy, jobs,
  tags, plan inspection, hardware) are now restricted by source network. They
  carry no token so the browser UI works without one on a home LAN, which also
  meant a forwarded port served the entire library to anyone who could reach
  it. The default allowlist is loopback, the RFC1918 ranges, link-local, IPv6
  ULA and the CGNAT range that Tailscale-style overlays assign; override it with
  `hub_security.trusted_read_networks`, where an explicit list replaces the
  defaults rather than extending them and a malformed range fails startup.
  Forwarded headers are deliberately ignored — they are attacker-controlled on a
  directly exposed listener — so a reverse-proxied deployment must filter for
  itself. A valid admin or agent token is admitted from any network: this closes
  anonymous reads without breaking remote use. The HTML pages stay open; they
  hold no library data and are where the token is entered.
- Added a way back for failed jobs: `timingdex pipeline retry-failed`, and a
  "重试失败作业" button on `/progress` backed by
  `POST /api/v1/pipeline/retry-failed` (admin only). Both ways a job stops being
  retried — exhausted attempts and permanent classification — were one-way, and
  `EnqueueJob` is `INSERT OR IGNORE`, so rescanning the library did not revive
  them. Work that failed only because a provider had not been configured yet was
  stranded permanently once the provider was added. Succeeded and running jobs
  are left untouched.
- Permanently failed jobs now report honestly. Marking them terminal by
  exhausting `attempt_count` kept them out of the lease predicate but showed a
  job that ran once as `3/3` on the progress page. A `jobs.terminal` column
  records the fact directly, so the attempt counter is again a count of actual
  runs and the page labels the job as permanently failed. The Worker derive
  lease predicate honours the same flag; it previously would have re-dispatched
  a job the Hub had already given up on.
- Job leasing no longer sorts on every call. `LeaseNextJob` orders by
  `priority DESC, created_at`, an order no `state`-leading index can supply once
  the predicate spans two states, so each lease built a temp b-tree over every
  leasable row — 170ms per lease measured on a 40k-row queue. A partial index
  storing exactly the leasable rows in that order removes the sort; the query
  names it with `INDEXED BY` because SQLite's cost model does not choose it.
- Added a scoped agent credential (`$DATA_DIR/agent-token`, 0600, same atomic
  write and constant-time comparison as the admin token). It is accepted on
  exactly two routes — repurpose plan creation and revision — and refused
  everywhere else. Plan approval and pipeline runs stay admin-only, so the
  `approval_mode: human_required` boundary is now enforced by access control
  rather than by Skill prompt text. Previously the documented agent workflow
  either failed with 401 or required handing the agent the admin token, which
  also unlocked the two actions the capability contract declares denied.
- FTS5 deletes no longer scan the whole shadow table. `asset_search` and
  `asset_shot_search` declare `asset_id UNINDEXED`, so the per-asset delete
  that runs at the end of every analyze was a full index scan, making a
  from-scratch reprocess quadratic in library size. Deletes now resolve the
  FTS rowid through a mapping table first; existing indexes are backfilled by
  the migration.
- Tag search now uses an index. Adding `idx_asset_tag_links_normalized` alone
  was not sufficient — a three-way `OR` spanning `LEFT JOIN`ed tables prevents
  SQLite from pushing any single disjunct down as a seek — so the query is now
  a `UNION` of single-predicate selects, which is set-equivalent and lets every
  branch seek its own index.
- `RebuildSearch` is transactional; its delete and insert could previously be
  interrupted between statements, leaving an asset missing from the search
  index until it was re-analysed.
- Removed two N+1 query patterns: asset browse issued two artifact lookups per
  row (up to 1000 extra queries per page) and shoot-session listing issued one
  lookup per session.
- Deriving an asset now runs `ffprobe` once instead of three times on both the
  Hub and Worker paths, reusing the classification the probe stage already
  stored. Note this also makes derive agree with the metadata the library
  reports: Log footage identified only through capture metadata is now
  consistently reported as needing a LUT instead of being silently rendered as
  ordinary SDR.
- Added CI (build, vet, gofmt, tests, race) and removed dead code:
  `repurposeHTML`, `legacyProvidersHTML`, `legacySetupHTML`, the unregistered
  `listAssets` handler, a redundant `derived_artifacts` index and the unused
  top-level `migrations/` directory. The Tag Curator page now carries the brand
  element its shared page wrapper had been silently failing to substitute.

### Regression Repair — same release, shipped alongside the above

- Restored the browser write surface. v0.14.1 put the Hub admin token in front
  of Tag governance, pipeline runs and Repurpose writes but only taught
  `/workers` to send it, so every write button on `/tags`, `/repurpose` and
  `/progress` had been returning 401 since that release — including plan
  creation, revision and approval, the product's primary workflow. Each page now
  carries a token field and attaches the header at its single fetch choke point;
  the token stays in page memory and is never written to browser storage.
- Opened `GET /api/v1/jobs` to unauthenticated readers so the progress panel
  polls without a token, but `last_error` is now disclosed only to admin
  callers. That field can embed a truncated upstream Provider response body,
  which is where a relay's echoed key would surface; public callers get a
  `has_error` boolean instead.
- Made derived-artifact writes atomic. FFmpeg wrote thumbnails, proxies and
  extracted audio directly to their final paths, so a run killed midway left a
  truncated file that the pipeline's `os.Stat` idempotency check then accepted
  as a finished artifact, silently poisoning every downstream stage until the
  cache was cleared by hand. Output is now published by rename.
- Permanent job failures are now terminal. The pipeline classified errors as
  permanent but still called `CompleteJob`, and the lease predicate accepts
  failed jobs with attempts remaining — so a permanently failed job was re-run
  up to `max_attempts` anyway, immediately and without even the backoff a
  retryable error receives. Provider-channel misconfiguration and unsupported
  capabilities are also classified permanent now instead of burning the full
  retry chain.
- `Weight` and `MaxInflight` now affect Provider-channel selection. Both round
  tripped through SQLite, the admin API and the UI while never reaching the
  pool, so a member configured with a concurrency cap of one still accepted
  unlimited concurrent work.
- Bounded model output before it reaches canonical tables. Analysis summaries,
  tag lists and shot counts had no size limits, so a model stuck in a repetition
  loop could write unbounded text into SQLite and the FTS index. Oversized
  values are truncated on rune boundaries rather than rejected, keeping the
  existing lenient normalisation contract.

## v0.16.0 — Worker Provider Modes + Library Operations

- Added an explicit, default-deny Worker-direct Provider credential path for
  trusted paired nodes. Credentials are lease-owned and memory-only; audit
  records contain no Provider key, and the documented delivery TTL is not
  misrepresented as key revocation.
- Added a Hub-side, JSON-only Worker Provider proxy. It verifies lease
  ownership, rejects media/multipart payloads and bounded responses, and
  redacts Provider keys before returning a response.
- Added Provider-channel update, test, enable, disable and safe removal APIs;
  browser responses never include API keys or secret references.
- Added Worker stage/progress events, retry/failure visibility and manual
  preferred/required derive-node routing, surfaced on `/workers`.
- Added saved, non-secret asset Collections and processing-state summary/filter
  support to the library-first browse experience.

## v0.15.0 — Capture Memory + Provider Channel Management

- Added a vendor-neutral capture-memory model with field provenance/conflict
  preservation, safe automatic shoot-session aggregation and region-level
  browsing. Camera/colour recognition covers Blackmagic, DJI, Sony, Canon,
  Apple, GoPro, Insta360, Panasonic, Nikon, Fujifilm, RED and ARRI families.
- Added RAW/HDR/Log routing metadata. Unsupported proprietary RAW remains
  visibly unrendered rather than being silently treated as ordinary SDR media.
- Added protected `/providers` management for capability-bound Provider
  channels, multiple same-Provider keys, health-aware balancing, retry,
  cooldown and ordered fallback. Provider secrets are encrypted Hub-only
  state, outside SQLite/browser/Worker configuration.
- Kept the Worker security boundary closed: Worker credential endpoints never
  deliver an upstream Provider API key. A credential TTL cannot revoke a
  long-lived key after it has been disclosed, so remote direct-provider
  execution requires a future Hub proxy or genuinely revocable upstream token.
- Tightened Hub secret storage so shared NAS data directories remain usable;
  only the dedicated `provider-secrets/` child is required to be mode `0700`.

## v0.14.2 — CJK Bigram Retrieval

- Replaced the CJK `instr(...)` full-table fallback with shared overlapping
  Bigram tokenization over existing FTS5 `unicode61` tables, including
  phrase-preserving Chinese and Chinese/English mixed MATCH queries.
- Added an idempotent migration/backfill state so existing libraries rebuild
  both asset and shot FTS indexes before the Hub reports the upgrade ready.
- Kept CJK FTS tokens out of the legacy heuristic vector's free-form hash
  space, preserving similar-shot ranking while retaining multilingual aliases.

## v0.14.1 — Hub Security Baseline

- Added a generated mode-0600 Hub administrator token and constant-time
  protection for Worker pairing, Hub management, scans, pipeline execution,
  tag governance, library-summary generation and Repurpose writes.
- Closed Compose's default LAN port exposure; Hub is internal to the Compose
  network until an operator adds an explicit HTTPS reverse proxy.
- Disabled Worker Provider-key delivery by default. When explicitly enabled,
  documentation now calls it a short-delivery lease: its TTL cannot revoke an
  upstream long-lived API key already received by a trusted Worker.

## v0.14.0 — NAS Hub + Worker Execution

- Added a Hub/Worker trust boundary: one-time Worker pairing, revocable node
  tokens, Worker heartbeats, persisted capabilities and a Hub-only SQLite DB.
- Added cross-platform Worker enrollment/config commands for Windows and Linux
  x64/ARM64, plus Docker Compose and generic systemd delivery assets.
- Added Hub self-signed HTTPS identity with certificate fingerprint pinning for
  Workers; Provider keys remain Hub-managed and never enter Worker config.
- Added NAS-direct probe behavior: `probe` reads source metadata in place while
  later media work may use the existing disposable local staging cache.
- Added source-color classification and metadata for SDR, HLG, PQ, Apple Log,
  unknown Log and RAW; RAW previews remain explicitly unavailable until a
  compatible renderer exists.
- Added Worker-local source staging and capability-matched derive execution;
  thumbnail, proxy and audio artifacts upload to Hub-owned `derived/` storage
  with lease ownership and idempotency checks.
- Added opt-in Worker Provider credential delivery. The Hub audits only Worker,
  job, provider, operation and expiry; it does not persist Provider API keys.
- Added a processing-node/workflow page at `/workers`, HDR→SDR Rec.709 preview
  rendering, Apple Log LUT-required handling, and explicit RAW refusal.

## v0.13.2 — Network Source Staging

- Added optional `source_staging.mode: copy` for mounted NAS/network libraries.
  Timingdex copies a source to a local, versioned cache before processing and
  reuses that completed cache on later jobs.
- Kept NAS originals read-only: FFmpeg, ASR, proxy generation and model work
  execute on the local machine; no sidecars or derived files are written back
  to the mounted share.
- Added a NAS/local-source choice to the startup-command page via
  `TIMINGDEX_SOURCE_STAGING_MODE=copy`.

## v0.13.0 — Library-First Footage Browser

- Reframed `/` as the primary Timingdex experience: an editor now sees each
  source as a left thumbnail, central semantic summary, and right-side
  horizontal shot timeline.
- Timeline blocks are populated from persisted `asset_shots` time ranges and
  descriptions; they expose usable footage rather than an invented visual
  summary.
- Kept `/repurpose` as a distinct downstream planning workspace so browsing
  material precedes asking the system to assemble a proposal.

## v0.12.0 — Bounded Agent Skill

- Added a project-bundled `timingdex` Agent Skill for local API-only readiness
  checks, shot retrieval, draft-plan creation and user-directed draft
  revisions.
- Added `GET /api/v1/agent/capabilities`, declaring the allowed Agent actions
  and the human-only approval boundary. Provider keys, original-media paths and
  pipeline execution are explicitly denied.
- Documented the full revision snapshot and lock/unlock contract for agents;
  the Skill never calls the approval endpoint.

## v0.11.0 — Repurpose Selection Workspace

- Turned the local Repurpose result into an editable editorial workspace:
  choose a candidate, lock/unlock the choice, exclude unsuitable alternatives,
  find additional hybrid-search candidates, add a revision note, and save a
  new immutable draft before approval.
- Added selection invariants: a selected or excluded shot must be a section
  candidate; a selected shot cannot be excluded; a locked section cannot be
  changed without an explicit, non-persistent `unlock` action.
- Approval now requires an explicit selection for every required section that
  already has candidate material, while genuine missing-material gaps stay
  visible.

## v0.10.3 — Local Review Workspace

- Added a small local-first workspace: startup configuration guidance, pipeline
  progress with recent job state/errors, and a Repurpose Plan result/review
  page that calls the existing plan/revision/approval APIs.
- Kept provider keys out of browser/server persistence: setup generates a
  copyable terminal command only; configuration remains process-startup based.
- Normalized an empty jobs response to `[]` so a fresh library renders a useful
  zero-state rather than a client error, and suppressed harmless favicon noise
  for the local pages.

## v0.10.2 — Reliability Patch

- Added bounded retry/backoff for transient pipeline failures; configuration,
  validation and missing-input failures stay final and visible.
- Preserved required Repurpose sections when their only valid shot was already
  selected elsewhere, with an explicit `reused` marker on that candidate.
- Added an invalidating in-memory cache for decoded local feature vectors,
  reducing repeated JSON decode work without changing hybrid/similar/rare
  retrieval semantics or claiming a learned embedding.

## v0.10.1 — Correctness Patch

- Removed silent mock video analysis: provider initialization errors now fail
  service startup, and an intentionally unconfigured video provider leaves
  analysis jobs with a visible configuration error.
- Corrected provider routing: only providers that actually prepare remote
  media are prepared; fallbacks receive the portable local proxy rather than a
  foreign provider URI.
- Made validated analysis, AI tag links, shots, FTS rows and local feature
  vectors one SQLite transaction, preserving previously trusted data when a
  new response is invalid.
- Replaced silence-event counting with duration-ratio classification and added
  regression fixtures for long, short, and open-ended silent regions.
- Added Chinese literal-match retrieval for short queries (for example `雨夜`)
  while retaining FTS5 for non-CJK requests. Hybrid's local feature score is
  documented as heuristic rather than a learned embedding.

## v0.10.0 — Pre-v1 Intelligence Closure

- Completed multi-model video understanding routes: Gemini plus configurable
  Qwen, Volcengine Ark and local OpenAI-compatible VLM video endpoints, all
  constrained by the shared `VideoAnalysisResult` contract and Router fallback.
- Added SQLite-first hybrid shot search using explainable local semantic
  features generated from persisted visual analysis, alongside similar-shot and
  library-relative rare-shot discovery APIs.
- Updated Repurpose Plans with append-only revisions, editor notes, explicit
  candidate ordering, and latest-draft approval. Approved plans are immutable.
- Added v0.10 SQLite migrations, API behavior tests, provider contract
  fixtures, and a pre-v1 goal/verification document.

## v0.9.0 — Repurpose Plan MVP

- Added a Brief → Material Needs → Shot Candidates → Repurpose Plan workflow.
- Added optional OpenAI-compatible Repurpose Planner support for a small local
  LM; planner output is constrained to searchable needs and never invents
  asset IDs or timestamps.
- Added deterministic heuristic fallback so plan generation remains available
  without an LLM or when the configured planner fails.
- Added SQLite persistence and APIs for creating and retrieving plans:
  `POST /api/v1/repurpose/plans` and `GET /api/v1/repurpose/plans/{id}`.
- Plans reference exact shot ranges and explain missing required material. This
  release does not auto-edit media or export an NLE timeline.

## v0.8.0 — Shot-level Clip Search substrate

- Persisted provider-generated time ranges as `asset_shots` with descriptions,
  tags, mood, confidence and model-run provenance.
- Added SQLite FTS5 search over shot descriptions and semantic fields.
- Added `GET /api/v1/assets/{id}/shots` and
  `GET /api/v1/search/shots?q=...`.
- Kept original media immutable; search results reference exact source time
  ranges for the upcoming Repurpose Plan workflow.

## v0.7.0 — Multi Video Understanding Provider Layer

- Added a provider-independent `VideoAnalysisResult` contract for summaries,
  scenes, shots, objects, actions, mood, raw tags and confidence.
- Added `VideoUnderstandingProvider`, capability declarations, registry and
  primary/fallback router.
- Adapted Gemini through the unified contract while preserving the old flat
  `StructuredAnalysis` response format during migration.
- Routed the hardware-aware v0.6.1 pipeline through the new provider layer;
  existing Volcengine ASR, Plan providers, embedding, Tag Intelligence and
  SQLite migrations remain intact.
- Added provider-layer tests and configuration support for `vision_fallback`.

## v0.6.1 — Volcengine Plan providers

- Added first-class, quota-isolated configuration for Volcengine **Agent Plan**
  (`/api/plan/v3`) and **Coding Plan** (`/api/coding/v3`) LLM providers.
  Either plan can power the bounded Tag Curator and library-summary Agent.
- Added separate Agent Plan / Coding Plan embedding provider blocks, so the
  semantic tag-cluster pipeline can use a plan's embedding entitlement without
  sharing or changing the corresponding chat configuration.
- Added `volcengine_asr`: native Doubao Seed ASR 2.0 offline WebSocket support
  for imported footage. It emits 16 kHz PCM in compressed binary frames,
  preserves the plan resource ID, and remains compatible with the existing ASR
  fallback chain.
- Added provider/protocol fixtures for Volcengine ASR framing, compressed server
  responses, and error envelopes. No credentials or live paid calls are used by
  tests.
- Documented the concrete provider names, API-key boundaries and safe selection
  recipe. Plan model names stay user-configurable because availability changes.

## v0.6 — Hardware-accelerated derived media

- Added FFmpeg capability detection and an inspectable `GET /api/v1/hardware`
  endpoint plus `timingdex doctor` output.
- Added `auto`, `software`, `cuda` (NVDEC/NVENC), `qsv`, `vaapi`, and
  `videotoolbox` media profiles for thumbnail/proxy work.
- Hardware is used only for decoding frames and encoding the local 720p H.264
  proxy. Originals remain read-only; ASR, Gemini, embeddings and Tag Curator
  are deliberately unchanged.
- Added safe per-operation software fallback and separate cache/profile keys so
  a hardware setting change cannot reuse or relabel an incompatible artifact.

## v0.5.2 — Tag Intelligence

- Added a provider-backed embedding pipeline for normalized unresolved tags.
  The pipeline supports OpenAI-compatible embeddings and Gemini native
  `embedContent`; vectors are stored in SQLite by tag and model.
- Added transparent cosine-similarity candidate clusters. Clusters are a
  discovery aid only: each candidate goes through Tag Curator and becomes a
  normal pending human-review proposal.
- Formalized the Tag Curator as a constrained semantic provider for both tag
  grouping and library summaries. It supports OpenAI-compatible chat endpoints
  (including Qwen/Ollama-style gateways) and Gemini native `generateContent`.
- Added library-level natural-language summaries generated from a bounded
  statistics snapshot: asset count, analysis coverage and approved canonical
  tag frequency. Provider failure safely falls back to a labelled deterministic
  summary when heuristic fallback is enabled.
- Added SQLite migrations, API routes and provider/repository fixture tests.

### Deliberately unchanged

- No vector search over footage.
- No automatic tag merge or database mutation from model output.
- No hierarchy editor or bulk-review UI; both remain v0.6 governance work.
