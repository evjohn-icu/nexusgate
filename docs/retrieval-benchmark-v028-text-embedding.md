# v0.28.0-alpha 检索基准报告（Text Embedding 通道）

`TestSearchV2Benchmark` 新增第六管道 **v2-rrf-gate-embed**：在 golden +
v2 语料（72 query）上，以确定性 fake embedder（256 维 token-hash 向量，
与 discovery 启发式正交）经真实存储路径（`UpsertShotTextEmbeddings` →
`ListShotTextEmbeddings` → cosine retriever）seed 全部 shot 向量后对比。
fake embedder 只验证通道集成与排序正确性——真实模型（OpenAI-compatible /
Gemini）由 `internal/providers/embedding` 适配器提供，经 httptest fixture
验证（`TestTextEmbeddingRoundtrip`），真实增益需线上模型实测。

## Before / After

| pipeline | P@5 | P@10 | R@10 | RetrievalFP | AssertionFP |
| --- | --- | --- | --- | --- | --- |
| v2 RRF + gate（v0.27 基线） | 0.206 | 0.103 | 0.986 | 0 | 0 |
| **v2 RRF + gate + embed** | **0.200** | **0.103** | **0.986** | **0** | **0** |

embedding 通道加入后召回与 FP 完全持平：通道是**加性的**——语义查询获得
第二个正交检索信号，fact/negative 的 evidence gate 语义不变（embedding
从不成为证据），speech/creative 不参与（profile 权重为 0）。

## 通道设计验证（benchmark 抓出的真问题，已修）

1. **稠密通道冲垮 RRF**：embedding 给每个 shot 一个非零 cosine，RRF 给
   每个有排名的候选记分 → 近正交噪声长尾淹没稀疏通道强 top rank，
   R@10 0.986 → 0.611。修复：`embeddingCutoffFraction = 0.8`——只保留
   相似度 ≥ 通道自身最高相似度 80% 的簇（`embedding.go`）。
2. 通道降级验证：embedder 未配置 / 库中无该 model 向量 / provider 未
   配置（`ErrProviderChannelNotConfigured` → no-op 适配器）→ 通道静默
   缺席，pipeline 与 v0.27 行为一致。

## 持久层验证

- `TestTextEmbeddingRoundtrip`：httptest OpenAI-compatible 端点 → 真实
  `embedding.Provider` → upsert → list → cosine 召回排序（CLAUDE.md
  httptest 纪律，无真实 provider 调用）。
- `TestEnsureShotTextEmbeddingsWritesChangedShots`：post-commit hook 增量
  语义——首轮全嵌、无变化零调用、description 变更只重嵌该 shot。
- `TestRebuildShotTextEmbeddingsIncremental`：全量重建幂等；换模型 =
  每 shot 一行被新 model 覆盖（与 `shot_semantic_vectors` 同模式），
  canonical 分析零触碰。

## 运行

```bash
go test ./internal/repository/sqlite -run TestSearchV2Benchmark -v
go test ./internal/repository/sqlite -run 'TestTextEmbeddingRoundtrip|TestSearchV2CompatMatchesLegacy' -v
go test ./internal/app -run 'TestEnsureShot|TestRebuildShot' -v
```
