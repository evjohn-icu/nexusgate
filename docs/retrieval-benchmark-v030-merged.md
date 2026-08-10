# v0.30.0 检索基准冻结（R2-15）

这是 v0.30 合并线的检索基准版本化基线。基准在以下提交上运行：

- benchmark commit: `220d03b7e55b0af25a8a010bf474007abf8d966d`
- Go: `go1.26.4 linux/amd64`
- corpus: 72 queries / 6 pipelines；包含 legacy golden 语料和 v2 hard-negative 语料，使用同一数据库及 relevant/notRelevant 标注
- router agreement: `45/72`

## Retrieval Golden

`TestRetrievalGolden` 的完整 weight-set 指标：

| weight set | P@5 | P@10 | R@10 | relevant-in-top10 | false-positives |
| --- | ---: | ---: | ---: | ---: | ---: |
| lexical-only | 0.175 | 0.088 | 0.862 | 57/65 | 0 (lexical=0, semantic=0, blend-edge=0) |
| current-70s-30l | 0.203 | 0.102 | 0.985 | 66/65 | 0 (lexical=0, semantic=0, blend-edge=0) |
| 70l-30h | 0.203 | 0.102 | 0.985 | 66/65 | 0 (lexical=0, semantic=0, blend-edge=0) |
| 80l-20h | 0.203 | 0.102 | 0.985 | 66/65 | 0 (lexical=0, semantic=0, blend-edge=0) |
| rrf-k60 | 0.203 | 0.102 | 0.985 | 66/65 | 0 (lexical=0, semantic=0, blend-edge=0) |

## Search v2 Benchmark

`TestSearchV2Benchmark` 的完整 pipeline 指标：

| pipeline | P@5 | P@10 | R@10 | RetrievalFP | AssertionFP |
| --- | ---: | ---: | ---: | ---: | ---: |
| legacy-hybrid | 0.203 | 0.101 | 0.972 | 2 | N/A |
| v2-weighted | 0.206 | 0.103 | 0.986 | 4 | 54 |
| v2-rrf | 0.200 | 0.101 | 0.972 | 3 | 56 |
| **v2-rrf-gate** | **0.206** | **0.103** | **0.986** | **0** | **0** |
| v2-rrf-gate-diversity | 0.203 | 0.101 | 0.979 | 0 | 0 |
| v2-rrf-gate-embed | 0.200 | 0.103 | 0.986 | 0 | 0 |

Router agreement is `45/72`.

### Per-intent

| pipeline | intent | P@5 | P@10 | R@10 | FP |
| --- | --- | ---: | ---: | ---: | ---: |
| legacy-hybrid | fact | 7.600 | 3.800 | 36.000 | 1 |
| legacy-hybrid | speech | 1.200 | 0.600 | 5.000 | 0 |
| legacy-hybrid | semantic | 5.600 | 2.800 | 28.000 | 0 |
| legacy-hybrid | negative | 0.200 | 0.100 | 1.000 | 1 |
| v2-weighted | fact | 7.600 | 3.800 | 36.000 | 3 |
| v2-weighted | speech | 1.400 | 0.700 | 6.000 | 0 |
| v2-weighted | semantic | 5.600 | 2.800 | 28.000 | 0 |
| v2-weighted | negative | 0.200 | 0.100 | 1.000 | 1 |
| v2-rrf | fact | 7.400 | 3.700 | 35.000 | 2 |
| v2-rrf | speech | 1.400 | 0.700 | 6.000 | 0 |
| v2-rrf | semantic | 5.600 | 2.800 | 28.000 | 0 |
| v2-rrf | negative | 0.000 | 0.100 | 1.000 | 1 |
| v2-rrf-gate | fact | 7.600 | 3.800 | 36.000 | 0 |
| v2-rrf-gate | speech | 1.400 | 0.700 | 6.000 | 0 |
| v2-rrf-gate | semantic | 5.600 | 2.800 | 28.000 | 0 |
| v2-rrf-gate | negative | 0.200 | 0.100 | 1.000 | 0 |
| v2-rrf-gate-diversity | fact | 7.400 | 3.700 | 35.500 | 0 |
| v2-rrf-gate-diversity | speech | 1.400 | 0.700 | 6.000 | 0 |
| v2-rrf-gate-diversity | semantic | 5.600 | 2.800 | 28.000 | 0 |
| v2-rrf-gate-diversity | negative | 0.200 | 0.100 | 1.000 | 0 |
| v2-rrf-gate-embed | fact | 7.600 | 3.800 | 36.000 | 0 |
| v2-rrf-gate-embed | speech | 1.400 | 0.700 | 6.000 | 0 |
| v2-rrf-gate-embed | semantic | 5.200 | 2.800 | 28.000 | 0 |
| v2-rrf-gate-embed | negative | 0.200 | 0.100 | 1.000 | 0 |

The diversity pipeline suppressed 1 near-duplicate shot on `red car`.

## Regression Floor

The frozen regression floor is:

- `AssertionFP == 0` for the fact and negative gated buckets.
- `R@10` must remain within 1 percentage point of this baseline.

The v0.27 Search v2 artifact still matches this baseline for the reference pipeline:
`v2-rrf-gate = 0.206 / 0.103 / 0.986 / 0 / 0` for `P@5 / P@10 / R@10 / RetrievalFP / AssertionFP`.

## Reproduction

```bash
go test ./internal/repository/sqlite -run 'TestRetrievalGolden|TestSearchV2Benchmark' -count=1 -v
```
