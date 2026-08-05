# Changelog

## Unreleased — 2026-08-05 优化轮（S1：依赖卫生 + 测试覆盖 + CI 门禁）

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

## Unreleased — 2026-08-05 优化轮（S2：搜索相关度、分面多选、Rekey CLI）

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
