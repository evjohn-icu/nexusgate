# v0.27.0-alpha 检索基准报告（Search v2：Gate + Selection Before/After）

`TestSearchV2Benchmark`：legacy golden 语料（22 asset / 65 query）+
v2 语料（6 家族 hard negatives）共 **72 条 query**，同一数据库、同一
relevant/notRelevant 标注，5 条 pipeline 对比。router 一致率：45/72。

## Before / After

| pipeline | P@5 | P@10 | R@10 | RetrievalFP | AssertionFP |
| --- | --- | --- | --- | --- | --- |
| legacy-hybrid（旧 GET 端点实际路径） | 0.203 | 0.101 | 0.972 | 2 | N/A |
| v2 weighted（无 gate） | 0.206 | 0.103 | 0.986 | 4 | 54 |
| v2 RRF（无 gate） | 0.200 | 0.101 | 0.972 | 3 | 56 |
| **v2 RRF + evidence gate** | **0.206** | **0.103** | **0.986** | **0** | **0** |
| v2 RRF + gate + diversity | 0.203 | 0.101 | 0.979 | 0 | 0 |

- **RetrievalFP 3→0**：gate 排除了没有证据支撑的 must 的 shot
  （"搜汽车命中没有汽车的时间段"类错误清零）。
- **AssertionFP 56→0**：无 gate 时 top-10 里 56 条结果声称满足查询但
  至少一个 must 毫无证据；gate 全部拦下。该指标 legacy 无法计算
  （旧响应无 evidence），报 N/A。
- **R@10 不降反升（0.972→0.986）**：gate 移除的噪音 shot 把被挤到
  top-10 之外的 relevant shot（"夜晚下雨，有人撑伞走过街道"的伞人镜头）
  释放回结果——不是以召回换精度。
- **diversity 代价**：R@10 0.986→0.979（"red car"近重复双 shot 中
  第二个被抑制，标记为 relevant 但被 selection 合理让位），换取
  top-10 不再被同一素材的连拍霸占。

## Per-intent（gate pipeline）

| intent | P@5 | P@10 | R@10 | FP |
| --- | --- | --- | --- | --- |
| fact | 7.600 | 3.800 | 36.000（~35 query 总数） | 0 |
| speech | 1.400 | 0.700 | 6.000 | 0 |
| semantic | 5.600 | 2.800 | 28.000 | 0 |
| negative | 0.200 | 0.100 | 1.000 | 0 |

全 intent 族 RetrievalFP=0。无 gate 的 RRF 管道 semantic 族曾有 4 个 FP
与 fact 族 3 个 FP，全部来自 metadata 通道的 asset summary/subjects
匹配（见问题 4）与语义共享 token 碰撞；metadata 通道改为仅文件名后
semantic 族清零，剩余 fact 族 FP 由 gate 拦截。

## 本轮 benchmark 抓到的真问题（全部已修）

1. **词表丢了 `traffic` 家族**：golden query "traffic" 依赖 alias 桥
   （car/vehicle/车流 → traffic 语义），新词表分裂家族导致 gate 误杀
   relevant shot。修复：car 家族并入 traffic/车流/交通。
2. **absence descriptor 误否定名词**："empty counter" 被编译成
   "没有柜台"（mustNot counter），relevant shot 被 gate 排除。
   修复：查询侧只允许直接否定词（没有/无人/no/without）否定；
   empty/abandoned/空 是场景描述词，只在证据侧生效。
3. **"empty" 作为 raw canonical 泄漏**：ASCII pass-through 未排除否定词，
   产生 must [empty, counter] 之类永远无法满足的约束。
4. **metadata 通道重演 golden case-2 污染**：asset summary/subjects 匹配
   把 asset 全部 shot 拉进结果（13 个 RetrievalFP 的来源）。
   修复：metadata 通道仅匹配文件名——asset-global 文本不得进入 shot 检索。
5. **WeightedBlend dedupe 丢信号**：跨通道候选合并时后到通道的信号被
   跳过，融合分只算了一半。修复：合并而非跳过。

## 运行

```bash
go test ./internal/repository/sqlite -run TestSearchV2Benchmark -v
go test ./internal/repository/sqlite -run 'TestSearchV2CompatMatchesLegacy|TestRetrievalGolden' -v
```

compat 相等性：全部 legacy golden query（含 facet 变体）下
`LegacySearch` 与 `HybridSearchShots` 的 top-10 ID 序列与分数
（≤1e-12）完全一致——旧端点接管无行为漂移。
