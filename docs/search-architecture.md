# Search Architecture v2 — local-first footage retrieval and selection

Timingdex 的检索系统正在从"一个 hybrid 端点"长成一个真正的
**local-first footage retrieval and selection engine**：既独立承担素材检索，
也成为 ChatCut 等上层创作/剪辑系统的素材 intelligence layer。

核心原则：

> **Recall may be fuzzy. Claims may not.**
> 召回可以大胆，但系统最终声称"这个 shot 里有某个东西/行为/场景"时，
> 必须尽可能有 shot-level evidence 支撑。

本文档描述 v0.27 引入的 Search Architecture v2 的分层结构、各层职责、
诚实性边界，以及未来能力的接入口。实现分布在 `internal/search`（引擎，
纯 Go、无 SQL）与 `internal/repository/sqlite`（`search_v2.go`，ShotStore
持久层）两个包；SQL 永远只在后者。

## 分层

```text
Query
  ↓
Query Compiler（离线，无 LLM）
  ↓
Retrieval Router（启发式 intent）
  ↓
Candidate Recall（通道：lexical / heuristic_semantic / transcript / metadata）
  ↓
Fusion（WeightedBlend / RRF）
  ↓
Evidence Gate（fact/negative intent 强制；其余只计算不拦截）
  ↓
Optional Rerank Interface（本轮为 none）
  ↓
Diversity / Selection（same-asset / same-session / near-time）
  ↓
Search Result + Evidence
```

## Canonical shot truth

`asset_shots` 是 canonical shot：模型观察（VLM 分析）的直接产物，包含
description / objects / actions / tags / mood 与精确时间范围。所有检索
表示（FTS 行、启发式语义向量、text embedding、未来 OCR 索引、rerank cache）
都是 **derived、rebuildable、replaceable** 的：换 embedding 模型绝不能
要求重新 VLM analyze 全部素材，只能重建 derived 层（`timingdex search
rebuild-embeddings`）。这正是
`analysis_generation`（canonical 真值）与 `retrieval_generation`
（FTS/embedding/alias/fusion 配置）分离的意义。

## Query Compiler

`internal/search/compiler.go` + `vocabulary.go`。v1 完全离线：

- 受控词表（multilingual 规范家族）：`汽车/轿车/car/vehicle → car`；
- 匹配纪律与 discovery 包一致：**ASCII 整词匹配**（`carefree` 绝不能 →
  `car`；`raining` 绝不能 → `train`），CJK 子串匹配；
- 对象约束进 `Must`，动作/场景/天气/时间/情绪进 `Should`；`Should`-only
  查询提升为 `Must`；
- 位置化否定："没有人的海边空镜" → `mustNot: [person]`，`海边` 保持正向；
  "empty counter" 是"空的柜台"而不是"没有柜台"（absence descriptor
  只在证据侧生效，不在查询侧否定）；
- 引号 / 「说过」→ speech 短语约束；`shot_...` id → similar intent。

示例（黄金用例，`compiler_test.go`）：

```text
夜晚下雨，有人撑伞走过街道
→ intent: fact
  must:   [{object,person}, {object,umbrella}]
  should: [{action,walking}, {scene,street}, {weather,rain}, {time,night}]
```

## Retrieval Router

`router.go`，确定性启发式，无 LLM：引号/说过 → speech；shot id →
similar；mustNot → fact；mood 无 object 或纯 scene/weather/time →
semantic；"给我找…" → creative；其余 fact。`mode: auto` 是默认；API 允许
显式 mode 覆盖。benchmark 单独报告 router 与人工标注 intent 的一致率。

## Candidate Recall（通道）

`CandidateRetriever` 统一接口；每个通道是一个逻辑检索信号：

| 通道 | 持久层 | 信号 |
| --- | --- | --- |
| Lexical | FTS5 加权 bm25（五列 field weights） | `lexical` |
| HeuristicSemantic | `scoreShotCandidates`（cosine + 共享 token 门） | `heuristic_semantic` |
| Transcript | `transcript_words` 时间重叠 JOIN | `transcript` |
| Metadata | filename 整词匹配（**仅**文件名） | `metadata` |
| TextEmbedding | `shot_text_embeddings`（float32 blob，cosine 扫描） | `text_embedding` |

- 候选统一带 `Signals map[string]float64`，fusion 按信号融合；
- facet 由 semantic 通道（全库打分）定义 universe，其余通道的候选
  不在 universe 内则丢弃；
- **Metadata 通道只匹配文件名**。asset 级 summary/subjects 不得进入通道：
  golden case-2 钉死了"asset-global 标签不得污染从未见过该对象的 shot"；
  metadata 命中永远不是 shot 级证据（evidence gate 忽略它）；
- **TextEmbedding 通道是稠密通道**：embedding 给每个 shot 一个非零 cosine，
  而 RRF 会给每个有排名的候选记分——不设阈值的话，近正交噪声的长尾会
  淹没稀疏通道的强 top rank。因此通道只保留相似度 ≥ 0.8 × 通道自身最高
  相似度的簇（`embeddingCutoffFraction`）。embedding 是检索信号，不是
  证据；换 embedding 模型 = 对 derived 文本重建（`timingdex search
  rebuild-embeddings`），绝不重跑 VLM analysis。

未来通道（同一接口，不重写 SearchService）：VisualEmbedding / OCR。

## Fusion

`WeightedBlend`（compatibility 模式，即 legacy 0.70/0.30 混合）与
`RRF`（k=60，一等公民）。legacy `repository.HybridSearchShots` 保持为被
golden 钉死的兼容基线；`search.Service.LegacySearch` 精确复现它，
`TestSearchV2CompatMatchesLegacy` 对全部 golden 查询断言 ID 序列与分数
一致（≤1e-12）——旧 GET 端点 / MCP / repurpose planner 在新引擎内部
接管后行为不变。

## Evidence Gate

`evidence.go` + `gate.go`。每个 constraint 得到：

```text
confirmed    — canonical 出现在结构化观察字段（objects/actions/tags/mood）
possible     — 仅 description（叙述）或 transcript（提及≠视觉）
contradicted — shot 自己显式否定（"no people"、"没有人"）
unknown      — 无任何证据；绝不是"确认无人"
```

fact/negative intent 下 gate 强制：

- must 有 contradicted → 排除；must 全部无证据（unknown）→ 排除；
  mustNot 被观察到（confirmed/possible）→ 排除；
- 部分支持 → downrank（`0.5 + 0.5 × confirmed/musts`）；
- 纯否定查询（只有 mustNot）只受 mustNot 观察检查约束。

**missing ≠ negative**：无 person 证据的 shot 证据状态是 `unknown`，
不是"无人"。API 对 `negated` evidence 的 `unknown` 状态明确禁止被读成
"确认无人"。gate 从不声称 absence。

## Retrieval 与 assertion 分离

semantic similarity → candidate recall，绝不等于 fact proof：
`umbrella similarity = 0.92` 不代表 shot 确定有伞。API 同时输出
`scores`（检索信号）与 `evidence`（观察证据），两者明确分开。

## Rerank Interface

`Reranker` 接口 + `NoneReranker` 默认。未来 text/multimodal reranker 只跑
有界 top-K（recall 50 → rerank 20 → return 10），绝不整库 rerank。

## Selection / Diversity

`selection.go`：检索回答"有什么"，selection 回答"该用什么"。
near-time 近重复（同 asset、Δstart ≤ 5s）**硬跳过**——A001 10.1s/10.9s/
11.5s/12.1s 不得霸占 top-10；同 asset 远处软罚、同 session 软罚，罚分
相对 pool 最高分按 diversity 缩放；creative intent 默认更高 diversity
（0.6）。

## Context expansion

`include_context` 返回 prev/next shot（按 asset 内 ordinal），用于
`[前镜头][命中镜头][后镜头]`。邻接 shot 的 metadata 绝不参与 matched
shot 的评分与 evidence。

## 未来能力（本轮仅边界/文档）

### Text embedding（v0.28 已落地）

`search.TextEmbedder` 批量接口（`Embed([]string) ([][]float64, error)`），
与 `providers.Embedder` 结构性一致：OpenAI-compatible（Ollama/Qwen/local
gateway）与 Gemini `embedContent` 协议直接满足，零适配器代码。存储
`shot_text_embeddings`（每 shot 一行，model 标记，float32 LE blob，
source_text_hash 驱动增量重建）。生成时机：分析提交后自动增量
（post-commit hook，只重嵌 derived 文本变化的 shot）+ `timingdex search
rebuild-embeddings` 全量命令。future 可接 Qwen3-VL-Embedding / CLIP /
SigLIP，核心 domain 不写死模型。

### Visual embedding

**绝不一 shot 一向量**：未来是 multi-vector（frame vector 1..N），查询
"红色跑车"用 max / late interaction，而不是把所有 frame vector 平均导致
局部物体信号被冲淡。

### OCR

独立能力位（street sign / screen text / packaging / subtitle / PPT…），
有自己的 source（`ocr`），**不塞进 description 字段**。

### Temporal / Sequence / Moment / Scene

未来：`TemporalQuery{Steps []SearchQuery, Ordered bool, MaxGapMS int64}`，
基于真实 timeline 组合 Shot A → B → C；搜索结果未来支持 Moment/Sequence/
Scene 粒度（"做饭过程"跨切菜/炒菜/装盘）。本轮只设计不实现。

## Feedback hooks

响应携带 `search_id` / `query_hash` / `result_rank`，未来反馈
（relevant / wrong / previewed / added_to_collection / used_in_edit）
可以精确关联到 query 与 session。本轮无推荐算法、无持久化。

## 兼容性承诺

- `GET /api/v1/search/shots`、`GET /api/v1/search/shots/hybrid` 响应形状
  不变（后者由 v2 compat 路径接管，被相等性测试钉死）；
- MCP `search_footage` 继续走 GET hybrid；
- `TestRetrievalGolden`（legacy 5-blend 硬门禁）不动，且新增
  `TestSearchV2Benchmark`（5 条 pipeline × per-intent 指标 ×
  RetrievalFP/AssertionFP）作为 v2 的回归地板。

## 指标口径

- P@5/P@10 固定 K 分母；R@10 按 query 平均；
- RetrievalFP：notRelevant shot 进 top-10；
- AssertionFP：fact/negative query 的 top-10 结果中存在 must constraint
  证据为 unknown——检索"声称"满足查询但无证据支撑。仅 must 计（should
  的 unknown 合法）。legacy pipeline 无 evidence，报 N/A。
