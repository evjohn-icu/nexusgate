# Performance Measurements (Synthetic Engine Benchmarks)

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

Machine: AMD Ryzen 5 5600X (12 threads), Go 1.26.4, linux/amd64. Single
threaded engine timings; total run ~16 s (the 100k search benchmark is one
~6 s iteration — the machine was not slow, the corpus is just large).

| benchmark | ns/op | MB/s | B/op | allocs/op |
| --- | --- | --- | --- | --- |
| `BenchmarkSearch1k` | 59.5 ms | — | 13.3 MB | 135,143 |
| `BenchmarkSearch10k` | 595 ms | — | 135 MB | 1,350,267 |
| `BenchmarkSearch100k` | 6.09 s | — | 1.36 GB | 13,501,177 |
| `BenchmarkEmbeddingFullScan1k` | 413 µs | 2,480 | 485 kB | 2,003 |
| `BenchmarkEmbeddingFullScan10k` | 4.12 ms | 2,483 | 4.8 MB | 20,003 |
| `BenchmarkEmbeddingFullScan100k` | 43.6 ms | 2,347 | 48 MB | 200,003 |
| `BenchmarkCompile` | 5.7 µs | — | 876 B | 14 |

## Interpretation

**Search is linear in the candidate pool, and the pool is capped in
production.** ×10 corpus → ×10.0–10.2 time. The per-candidate cost is
~60 µs, dominated by the evidence gate's vocabulary scans (a
`Canonicalize` pass per constraint per field). The real SQLite store hands
the engine ≤200 candidates per channel, so a production search costs
roughly 200 × 60 µs ≈ 12 ms of engine time plus the SQLite retrieval —
comfortable for an interactive UI, with allocation volume in the MBs rather
than the 1.36 GB/op the full-library sweep shows.

**The embedding scan is the production-relevant measurement**, and it is
comfortably SQLite-first: 100k shots × 256-dim = 102 MB of vectors scanned
in 43.6 ms (~2.4 GB/s, alloc-free math; the B/op is the candidate
conversion for rows above the 0.8 cutoff cluster). Linear in N·D (413 µs →
4.12 ms → 43.6 ms). The known O(N·D) hot spot is real but far from the
boundary.

**The ANN threshold is 2 s at 100k** — the point where a full cosine scan
would start feeling like a pipeline stall. Current headroom is ~46×; even a
million-shot library would land near ~0.4 s, still inside the envelope.

**Compile is off the critical path**: 5.7 µs for a typical Chinese query
(fact, negation, speech, creative, shot-id — all panic-safe, pinned by
`compiler_panic_test.go`).

## Recommendation

Keep the SQLite-first full scan for now. The measurements say a 100k-shot
library is 44 ms per embedding query — no ANN needed this round, no vector
DB (the v0.28 design decision stands: embeddings are derived,
rebuildable, model-tagged, and the engine never treats them as evidence).

If/when 100k+ shot libraries with embeddings become the norm **and** the
scan approaches the ~2 s threshold (≈ 4M+ shots at current throughput, or
significantly higher-dimensional models), plug an ANN index at the
`TextEmbeddingRetriever` boundary (`embedding.go`, `Retrieve` → the
`ListShotTextEmbeddings` + cosine loop). The channel already isolates the
scan behind the `ShotStore` interface, so the index swaps in without
touching the engine, the fusion weights, or the evidence gate — a store
that serves ANN results (or an index built inside the store) is the
replacement, and recall stays gated exactly as today.

## Re-measuring

These are single-machine, single-threaded numbers; the shape (linear in N,
~2.4 GB/s scan) matters more than the absolutes. Re-run the command above
on current hardware before tuning on them — and never let the embedding
scan regress without a deliberate reason: it is the one number a library
growth spurt will hit first.
