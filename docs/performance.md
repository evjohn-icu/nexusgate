# Performance Measurements

`internal/search/benchmark_test.go` measures the **engine**, never SQLite I/O:
the `ShotStore` contract (`internal/search/store.go`) is implemented by a
scripted in-memory fake, so the numbers cover compile → intent routing →
channels → fusion → evidence gate → selection → response assembly, and the
`TextEmbeddingRetriever`'s O(N·D) cosine full scan. Corpus sizes (1k / 10k /
100k shots) are the library scales the Hub stores behind the same interface.
The fake implements the full `ShotStore` interface (including the batched
`ShotSessions` lookup); sessions and transcript spans are scripted as empty,
embeddings as deterministic rows.

The entries are `func Benchmark` only — they never run under plain
`go test`, so the normal suite is unaffected.

```bash
go test ./internal/search/ -bench . -benchmem -benchtime 1s -run '^$'
```

## Harness notes (why the numbers look the way they do)

- The fake's four retrieval channels each return **all N scripted
  candidates**, ignoring the recall-pool limit. The real SQLite store caps
  every channel at ≤200 candidates (`recallMultiplier` in `service.go`), so
  these measurements are the engine processing a whole library in-process —
  an upper bound, deliberately. A production search pays the same
  per-candidate cost but only ever for the bounded pool.
- The corpus's per-channel scores decline with the index and every channel
  ranks the same order, so the fused RRF list arrives already sorted: the
  benchmark measures the pipeline's O(N) passes and the per-candidate
  evidence work, not a pathological insertion sort over an adversarial order
  (which production never produces either — the pool is capped).
- Each embedding row's cosine against the query declines strictly with the
  index (the vector points along the query direction scaled by `1-i/N` plus
  fixed-norm, query-orthogonal noise), so the scan measures the real
  dot-product hot spot while the post-scan sort sees the deterministic
  `shot_id` order the SQLite store returns.
- The query is `夜晚下雨 有人撑伞` (fact intent: must `person` + `umbrella`,
  should `rain` + `night`) with `IncludeEvidence: true`, so the evidence gate
  computes per-constraint verdicts for every candidate every iteration.

## Measurements

Current run: 2026-08-10; `uname -m`: `x86_64`; CPU: AMD Ryzen 5 5600X
6-Core Processor (12 logical CPUs); `go version`: `go1.26.4 linux/amd64`.
Single-threaded engine timings; total run ~27 s (the 100k search benchmark is
one ~8.7 s iteration — the corpus is just large).

### Current run

| benchmark | ns/op | MB/s | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| `BenchmarkSearch1k` | 113.7 ms | — | 13.5 MB | 139,143 |
| `BenchmarkSearch10k` | 801.6 ms | — | 137.2 MB | 1,390,270 |
| `BenchmarkSearch100k` | 8.72 s | — | 1.38 GB | 13,901,176 |
| `BenchmarkEmbeddingFullScan1k` | 900.5 µs | 1,137 | 485 kB | 2,003 |
| `BenchmarkEmbeddingFullScan10k` | 53.6 ms | 191 | 4.8 MB | 20,003 |
| `BenchmarkEmbeddingFullScan100k` | 458.5 ms | 223 | 48.0 MB | 200,003 |
| `BenchmarkCompile` | 49.0 µs | — | 876 B | 14 |

### Historical baseline

The table below is retained for comparison only. It was measured on the same
machine and toolchain, but without a recorded run date.

| benchmark | ns/op | MB/s | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| `BenchmarkSearch1k` | 59.5 ms | — | 13.3 MB | 135,143 |
| `BenchmarkSearch10k` | 595 ms | — | 135 MB | 1,350,267 |
| `BenchmarkSearch100k` | 6.09 s | — | 1.36 GB | 13,501,177 |
| `BenchmarkEmbeddingFullScan1k` | 413 µs | 2,480 | 485 kB | 2,003 |
| `BenchmarkEmbeddingFullScan10k` | 4.12 ms | 2,483 | 4.8 MB | 20,003 |
| `BenchmarkEmbeddingFullScan100k` | 43.6 ms | 2,347 | 48 MB | 200,003 |
| `BenchmarkCompile` | 5.7 µs | — | 876 B | 14 |

## Synthetic-engine interpretation

**Search is broadly linear in the candidate pool, and the pool is capped in
production.** ×10 corpus → ×7.0–10.9 time in this run; the 1k result is a
short, noisy sample, while the 10k → 100k step is ~10.9×. The large-corpus
per-candidate cost is ~87 µs, dominated by the evidence gate's vocabulary scans (a
`Canonicalize` pass per constraint per field). The real SQLite store hands
the engine ≤200 candidates per channel, so a production search costs roughly
200 × 87 µs ≈ 17 ms of engine time plus the SQLite retrieval —
comfortable for an interactive UI, with allocation volume in the MBs rather
than the 1.36 GB/op the full-library sweep shows.

**The embedding scan is the production-relevant measurement**, and it is
still SQLite-first but has materially less headroom: 100k shots × 256-dim =
102 MB of vectors scanned in 458.5 ms (~223 MB/s, alloc-free math; the B/op
is the candidate conversion for rows above the 0.8 cutoff cluster). Linear in
N·D at the larger scales (53.6 ms → 458.5 ms). The known O(N·D) hot spot is
real and should be watched as libraries grow.

**The ANN threshold remains 2 s** — the point where a full cosine scan would
start feeling like a pipeline stall — but current headroom is only ~4.4×. At
the measured rate, a million-shot library would take roughly 4.6 s, so ANN
becomes relevant well before that scale if this throughput persists.

**Compile is off the critical path**: 49.0 µs for a typical Chinese query
(fact, negation, speech, creative, shot-id — all panic-safe, pinned by
`compiler_panic_test.go`).

## Synthetic-engine recommendation

Keep the SQLite-first full scan for now. This recommendation applies to the
in-memory engine benchmark above; the on-disk SQLite measurements are recorded
in the release section below. The synthetic engine measures a 100k-shot
embedding query at 459 ms. No ANN index or vector database is implemented (the
v0.28 design decision stands: embeddings are derived, rebuildable, model-tagged,
and the engine never treats them as evidence).

If/when 100k+ shot libraries with embeddings become the norm **and** the
scan approaches the ~2 s threshold (≈ 435k shots at current throughput, or
significantly higher-dimensional models), plug an ANN index at the
`TextEmbeddingRetriever` boundary (`embedding.go`, `Retrieve` → the
`ListShotTextEmbeddings` + cosine loop). The channel already isolates the
scan behind the `ShotStore` interface, so the index swaps in without
touching the engine, the fusion weights, or the evidence gate — a store
that serves ANN results (or an index built inside the store) is the
replacement, and recall stays gated exactly as today.

## Re-measuring

These are single-machine, single-threaded numbers; the shape (linear in N,
~223 MB/s scan) matters more than the absolutes. Re-run the command above
on current hardware before tuning on them — and never let the embedding
scan regress without a deliberate reason: it is the one number a library
growth spurt will hit first.

## Real SQLite scale harness

`internal/repository/sqlite/scale_benchmark_test.go` complements the fake
engine benchmark with a deterministic on-disk corpus. It writes canonical
shots, the FTS5 projection, heuristic semantic vectors, locations, and
256-dimension float32 text embeddings in one seed transaction. Seeding is
outside the benchmark timer. The fixture is synthetic benchmark input only;
it is not a model-output or production write path.

The harness measures these scenarios against the same database:

- `lexical`: field-weighted FTS5 retrieval;
- `hybrid`: legacy SQLite lexical plus heuristic semantic scoring;
- `fact/evidence`: Search v2 fact mode with the evidence gate;
- `context`: Search v2 fact mode with evidence and previous/next context;
- `embedding`: text-embedding materialization from SQLite and the Go cosine
  full scan.

The repository does not currently expose a SQLite statement counter, so this
harness reports timing and allocations rather than inferred query counts.
Separate store call-count tests and SQLite batch-equivalence tests enforce the
fixed-round-trip Search contract; adding a benchmark-only database wrapper
would distort the implementation being measured.

Each scenario has a warm-open and a fresh-open benchmark. Fresh-open means a
new `sql.DB` is opened and closed for each measured iteration; it does not
evict the operating-system page cache, so it is a cold-ish startup measure,
not a claim about cold disk latency. The repository's `Open` defaults are the
measured SQLite mode: WAL, `synchronous=NORMAL`, 5-second busy timeout, and
up to eight open connections.

The 1k sanity test runs in the normal SQLite package test suite. Larger sizes
are opt-in so ordinary CI cannot accidentally materialize a large database:

```bash
# ordinary CI sanity (also included in go test ./...)
go test ./internal/repository/sqlite -run '^TestSQLiteScaleSanity$' -count=1

# release measurements; keep one size/job serial on constrained machines
NEXUSGATE_SQLITE_SCALE_SIZES=10000 \
  go test ./internal/repository/sqlite -run '^$' \
  -bench '^BenchmarkSQLiteScale' -benchmem -benchtime=1x -count=5 \
  -timeout=30m

# required 100k run, and bounded manual 500k attempt
NEXUSGATE_SQLITE_SCALE_SIZES=100000 go test ./internal/repository/sqlite \
  -run '^$' -bench '^BenchmarkSQLiteScale' -benchmem -benchtime=1x -count=5 \
  -timeout=30m
NEXUSGATE_SQLITE_SCALE_SIZES=500000 GOMAXPROCS=1 \
  go test ./internal/repository/sqlite -run '^$' \
  -bench '^BenchmarkSQLiteScale' -benchmem -benchtime=1x -count=1 \
  -timeout=30m
```

Databases are created under `.bench/sqlite/` at the repository root and are
ignored by Git. The harness removes only its exact `scale-<N>.db` file and
SQLite WAL sidecars before reseeding, so an interrupted run cannot silently
reuse a partial corpus. `-count=5` provides stable timing samples; report
`ns/op`, `B/op`, and `allocs/op` from the output. The embedding benchmark's
logical scan volume is `N * 256 * 4` bytes per operation. Record the machine,
Go version, SQLite mode, corpus size, seed shape, and warm/fresh mode with each
release run. A failed or memory-bound 500k attempt is a bounded result to
document, not a reason to put 500k in ordinary CI. ANN, sqlite-vec, mmap,
vector caches, and vector databases remain future options; this harness
intentionally measures the current SQLite-first design.

## v0.31 SQLite measurements

The release run used an Intel N100 (4 CPUs, 7.5 GiB RAM), Linux amd64, Go
1.26.4, `modernc.org/sqlite` v1.56, and the repository defaults of WAL with
`synchronous=NORMAL`. The 1k and 10k values below are approximate medians of
three warm/fresh samples. The 100k values are one stable warm/fresh sample,
not a five-sample median. Fresh-open has the meaning defined above: it opens a
new `sql.DB` but does not evict the operating-system page cache.

| scenario | 1k warm | 1k fresh | 10k warm | 10k fresh | 100k warm | 100k fresh |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| lexical | 5.37 ms | 8.32 ms | 49.81 ms | 54.63 ms | 542.16 ms | 534.30 ms |
| hybrid | 27.77 ms | 42.55 ms | 268.41 ms | 364.70 ms | 3.688 s | 3.677 s |
| fact (evidence) | 80.68 ms | 97.12 ms | 739.40 ms | 830.04 ms | 7.495 s | 8.377 s |
| context | 92.73 ms | 97.95 ms | 740.56 ms | 830.20 ms | 7.485 s | 8.382 s |
| embedding | 17.72 ms | 25.67 ms | 197.12 ms | 200.65 ms | 2.209 s | 2.199 s |

Warm 100k allocation measurements were:

| scenario | B/op | allocs/op |
| --- | ---: | ---: |
| lexical | 65,760 | 1,139 |
| hybrid | 1,054,380,944 | 12,491,212 |
| fact (evidence) | 1,717,258,616 | 18,576,319 |
| context | 1,717,325,088 | 18,578,048 |
| embedding | 720,872,960 | 6,090,107 |

These results establish the measured 100k SQLite boundary for this host and
configuration; they do not predict other hardware or a cold-disk run. The
500k measurement was deferred because of resource/runtime cost. No ANN index
is implemented in v0.31.
