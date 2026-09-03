# v0.26.0-alpha 检索基准报告（Retrieval Golden Set before/after）

本轮把检索回归基准从 8 个 fixture asset / 12 条 query 扩到 **41 asset / 65
query**（`internal/repository/sqlite/retrieval_golden_corpus_test.go`），并把
权重扫描从 4 个 blend 扩到 5 个（加入 RRF）。同时修掉了一个被测出来的真问题：
**启发式语义向量的"无证据断言"**。

## 方法

- 语料 100% fixture 驱动，走真实提交路径（`StageModelRun` →
  `CommitAnalysisWithShots`），FTS 行、语义向量、asset_analysis 与生产完全
  同构；测试离线、毫秒级、进 CI。
- 每个 adversarial 家族的 shot 文本与 asset 级字段分离——asset 全局标签
  永远进不了 shot 向量（这正是被回归的边界之一）。
- 硬门禁不变：`notRelevant` shot 在**任何** blend 的 top-10 里出现即失败；
  gated blend 丢 relevant 即失败；窗口边界 query 钉死 `start_ms/end_ms`。
- 指标约定：固定 K 分母 P@5/P@10，R@10 按 query 平均，FP 逐条计数并
  按信号归因（lexical / semantic / blend-edge）。

## Before（8 asset / 12 query，旧语料）

| blend | P@5 | P@10 | R@10 | FP |
| --- | --- | --- | --- | --- |
| lexical-only | 0.133 | 0.067 | 0.667 | 0 |
| current 70s-30l | 0.200 | 0.100 | 1.000 | 0 |
| 70l-30h | 0.200 | 0.100 | 1.000 | 0 |
| 80l-20h | 0.200 | 0.100 | 1.000 | 0 |

## After（41 asset / 65 query，新语料 + RRF）

| blend | P@5 | P@10 | R@10 | FP | FP 归因 |
| --- | --- | --- | --- | --- | --- |
| lexical-only | 0.175 | 0.088 | 0.862 | 0 | — |
| current 70s-30l | 0.203 | 0.102 | 0.985 | 0 | — |
| 70l-30h | 0.203 | 0.102 | 0.985 | 0 | — |
| 80l-20h | 0.203 | 0.102 | 0.985 | 0 | — |
| rrf-k60 | 0.203 | 0.102 | 0.985 | 0 | — |

运行：`go test ./internal/repository/sqlite -run TestRetrievalGolden -v`。

## 本轮抓到的真问题：semantic false-positive assertion

新语料加了一条 `train arrival` 查询：relevant 是"train arriving at the
platform" shot，notRelevant 是同一 asset 的"commuters on the platform" shot
——两者零 token 共享。修复前的语义打分（64 维 FNV hashed-token 余弦）在
两者无任何共同 token 的情况下仍给出 **0.33 余弦**（维度碰撞噪声），于是
notRelevant shot 进入了**所有含语义权重的 blend**（含 RRF）的 top-10——
正是"用户搜汽车，结果那个时间段根本没有汽车"这类最危险错误的一个实例。

**修复**（`internal/repository/sqlite/repository.go`
`scoreShotCandidates` + `internal/discovery/features.go` 新增
`TokensForText`）：query 与 shot **无共享语义 token 时语义分强制为 0**
（alias-canonical token 算共享——那是跨语言桥，不是碰撞）。修复后：

- 5 个 blend 全部 FP=0，包括该查询；
- 所有既有查询（CN/EN 同义、双语 fixture、alias 桥）全部保持命中，
  证明门禁没有以牺牲召回为代价；
- 默认权重 **0.70 semantic / 0.30 lexical 维持不变**——数据不再支持
  改权重的理由，噪声问题已在打分层消除。

## 结论与决策

1. **默认 blend 不变**：0.70/0.30 在新语料上 R@10=0.985、FP=0，与
   70l-30h / 80l-20h / RRF 持平；lexical-only 明显更差（0.862，丢 9/66
   relevant），说明启发式语义仍有真实价值。
2. **RRF 不替代加权融合**：两者打平；加权融合实现更简单且现有代码路径
   已稳定，故不切换默认。`HybridSearchShotsRRF` 保留为测量/后续选项。
3. **语义分必须"有证据才说话"**：token-overlap 门禁现在是打分契约的一部分，
   任何 future semantic 方案（真 embedding 除外）都应继承该门禁，否则
   golden set 会立刻再亮灯。
4. **语料可增长**：`retrieval_golden_corpus_test.go` 按家族组织，加 asset
   只需保证新文本不与既有 query 的 notRelevant 碰撞并重跑全量。

## 离线 eval 语料

`cmd/nexusslate-corpusgen` 生成与 golden 家族对应的合成 clips + 
`ground_truth.json`（lavfi 测试源，可复现、无版权、可分发）：

```bash
nexusslate-corpusgen --out ./corpus
nexusslate-eval run  --corpus ./corpus --data-dir ./eval/qwen   --label qwen3vl-4b
nexusslate-eval run  --corpus ./corpus --data-dir ./eval/gemini --label gemini-flash
nexusslate-eval score --corpus ./corpus --data-dir ./eval --labels qwen3vl-4b,gemini-flash
```

`score` 的 RunScore 输出新增 `semantic_false_positives` /
`lexical_false_positives`（FP 按信号归因，与 golden set 同口径），用于回答
"这个 provider 的错到底错在哪一侧"。
