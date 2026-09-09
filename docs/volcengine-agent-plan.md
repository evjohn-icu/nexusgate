# 用一个 ¥9.9 的火山 Agent Plan 跑完大部分事情

这份文档回答一个具体问题：**一个月 ¥9.9 的火山引擎豆包 Agent Plan，能把 NexusGate
的哪几段撑起来，怎么配，哪里配不动。**

`docs/v0.21-provider-deployment.md` 讲的是「为什么推荐套餐、通道怎么调度、Key 为什么
不出 Hub」。那份文档在视觉模型这一节留了一句话：「本仓库无法确认套餐是否提供视觉模型
及其模型名」。这份文档把那句话换成实测结果。

下面每个「能」和「不能」都是 2026-09-10 对着真实套餐 Key 跑出来的，不是从火山文档抄的。
模型 ID 会漂，**用之前自己探一次**——怎么探见最后一节。

---

## 一句话结论

一把套餐 Key 能覆盖 NexusGate 四条能力里的三条：**视频理解、语音转写、向量检索**。
覆盖不到的是 Tag 整理和翻新方案要的通用长文本模型——套餐里那两个模型也能干，只是同一
份额度要分给视频，通常不划算。

真正的价值不在省钱，在**硬上限**：当月额度用完即停，没有超量计费。最坏情况是索引暂停，
不是一张意外账单。按量计费下，一个异常脚本或一次全库重扫就能烧出账单。

---

## 套餐 Key 的边界，先说清楚

**套餐 Key 只在套餐端点上有效。**

| | |
|---|---|
| 端点 | `https://ark.cn-beijing.volces.com/api/plan/v3` |
| 鉴权 | `Authorization: Bearer <key>` |
| 同一把 Key 打 `/api/v3`（按量端点） | **401 AuthenticationError** |
| `GET /api/plan/v3/models` | **404** —— 套餐端点不提供模型列表 |

这两条合起来是个坑：把端点写成按量的 `/api/v3`，套餐 Key 会收到 **401**，读起来像
「Key 写错了」，而实际错的是端点。碰到 401 先核端点，再怀疑 Key。

没有 `/models` 意味着**只能按名字逐个试**。下面这张表就是这么来的。

---

## 能用的三个模型（实测）

| 你写的名字 | 实际解析到 | 用途 | 实测过什么 |
|---|---|---|---|
| `doubao-seed-2.0-lite` | `doubao-seed-2-0-lite-260215` | 视频理解 | 单图 ✓ 一条消息多图 ✓ 内联视频 ✓ `response_format: json_object` ✓ `temperature: 0.1` ✓ |
| `doubao-seed-2.0-mini` | `doubao-seed-2-0-mini-260215` | 视频理解（同上） | 同上，全过 |
| `doubao-embedding-vision` | `doubao-embedding-vision-251215` | 向量检索 | 2048 维 ✓ |
| `doubao-seed-asr-2.0` | —— | 语音转写 | 见下面「ASR 是另一套协议」 |

带日期后缀的全名也能用，解析到同一个模型。**建议写不带后缀的别名**，快照更新时不用改配置。

**内联视频是真能看懂内容，不是敷衍。** 拿一个 495 KB 的代理片段发过去，回来的是
「平板正在播放汤姆猫趴在窗台下滑入窗帘的动画，同时搭配讲解……」——画面里确实是这个。

**`doubao-embedding-vision` 在套餐端点上是纯文本向量模型。** 名字里有 vision，但按
OpenAI 多模态那套传 `input: [{"type":"image_url",...}]` 会被拒：

```
InvalidParameter | The parameter `input[0]` ... expected a string, but got map[...]
```

只收字符串数组。这对 NexusGate 没有影响——`shot_text_embeddings` 本来就是逐镜头的
文本向量。但**别在文档或界面上把它说成图像向量**，它在这个端点上不是。

## 用不了的（同样是实测）

这些全部返回 `404 UnsupportedModel: The requested model does not support the agent
plan feature`：

`doubao-seed-2.0`（不带 -lite/-mini 的裸名）、`doubao-seed-1-6-250615`、
`doubao-seed-1-6-flash-250615`、`doubao-seed-1-6-vision-250815`、
`doubao-1-5-vision-pro-32k-250115`、`doubao-vision-pro-32k`、`doubao-pro-32k`、
`doubao-lite-32k`、`doubao-embedding-large`、`doubao-embedding`。

规律很清楚：**套餐只放行 seed-2.0 这一代的 lite / mini，以及 embedding-vision。**
上一代和 pro 档一律不在套餐里。

---

## 三条通道怎么配

在 `/providers` 页面加通道，或者打 `POST /api/v1/admin/provider-channels`。Key 填进去
就进加密的 `secretstore`，不进 SQLite、不进响应、不进日志。

### 1. video_analysis —— 视频理解

| 字段 | 值 |
|---|---|
| 能力 | `video_analysis` |
| 服务商 | `volcengine_video` |
| **协议** | **`openai_video`** |
| 端点 | `https://ark.cn-beijing.volces.com/api/plan/v3` |
| 模型 | `doubao-seed-2.0-lite` |

**协议这一格是这份文档最重要的一行。**

`docs/v0.21-provider-deployment.md` 推荐的是多帧路线（`openai_multiframe`：本地确定性
抽帧、只发图片、不上传视频）。那条路线在通道上走不通，会在 `analyze` 阶段直接失败：

```
openai_multiframe is currently supported through providers.local_vlm config only;
provider-channel routing support is not available yet
```

这句报错的措辞比实情窄。多帧协议在 **legacy config 里对三个视频 provider 都可用**——
`qwen_video`、`volcengine_video`、`local_vlm` 共用同一个 `openai_multiframe` 注册分支
（`internal/providers/factory.go`）。真正接不上的是**通道路由**：通道遇到
`openai_multiframe` 直接返回永久错误（`internal/app/provider_channel_runtime.go`）。

所以限制是「多帧 ⊗ 通道」，不是「多帧 ⊗ 非 local_vlm」。想走多帧就得把配置写进
`config.json` 的 `providers.*` 块（Key 用 `api_key_env` 指向环境变量，别写进文件），
放弃 secretstore 和多 Key 池化。

所以套餐 + 通道这条组合只能用 `openai_video`：整段代理内联进请求体。实际影响是每次
分析要上传一次代理文件，比抽帧慢、也更吃套餐额度。代理体积由 `openai_video` 适配器的
内联上限兜底，超限的素材会被跳过而不是发一个几百兆的请求出去。

### 2. embedding —— 语义检索

| 字段 | 值 |
|---|---|
| 能力 | `embedding` |
| 服务商 | `volc_agent_plan_embedding` |
| 协议 | 留空（内置默认 `openai_embeddings`） |
| 端点 | `https://ark.cn-beijing.volces.com/api/plan/v3` |
| 模型 | `doubao-embedding-vision` |

内置默认的 path（`embeddings`）和鉴权（`Authorization`/`Bearer`）已经对了，不用填。

### 3. asr —— 语音转写

ASR 是**另一套协议、另一个主机、另一种鉴权**，别跟上面两条混为一谈。

| 字段 | 值 |
|---|---|
| 能力 | `asr` |
| 服务商 | `volcengine_asr` |
| 端点 | `wss://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_nostream` |
| 模型 | `doubao-seed-asr-2.0` |

适配器自己发 `X-Api-Key: <key>`（**裸 Key，不是 Bearer**）和
`X-Api-Resource-Id: volc.seedasr.sauc.duration`。Resource ID 必须显式指定——语音模型
不支持通过 Auto 或控制台切换。仓库的内置默认已经是这两个值，通道只需要给 Key、端点、
模型名。

实测：31 个素材转写全部成功，零失败，中文识别质量可用（一条站台广播完整还原成
「旅客们请注意，126 次列车即将到达本站。列车停靠在一站台，不要越过安全白线……」）。
其中 7 条 `full_text` 为空——那是真的没人说话的片段，不是失败。分段结果写在
`transcripts.segments_json` 里。

### 没配 ASR 会怎样

**用通道配的话，管线会卡住。** `pipeline.go` 里的分流条件是
`c.SpeechProbability >= 0.5 && p.asr != nil`，读起来像「没有 ASR 就跳过转写」，但通道
运行时的 `asr()` **永远返回一个非 nil 的 wrapper**（`provider_channel_runtime.go`），
所以 `p.asr != nil` 恒真：有语音的素材照样进 `transcribe`，然后因为没有 asr 通道而失败，
它后面的 `analyze` 就一直排不到。只有 legacy config 完全没配 ASR 时，那个 nil 判断才
真的成立。

实测就是这个形状：43 个有代理的素材里 33 个卡在 `transcribe`，`analyze` 只跑了 11 个。
补配 ASR 通道之后要跑一次 `nexusgate pipeline retry-failed` 把它们捞回来——失败过的作业
不会自己重排。

---

## 套餐盖不到的两块

**Tag 整理**：可以走套餐。配一条 `tag_curator` 通道，provider 选 `volc_agent_plan`，
协议 `openai_chat`，同一个端点。代价是它和视频理解吃同一份额度。

**翻新方案（repurpose）走不了套餐通道**：`repurpose` 能力只接受 `openai_chat` 这一个
provider 名，`volc_agent_plan` 不在它的白名单里
（`internal/app/provider_channel_runtime.go` 的 `supportedChannelProvider`）。要用套餐
端点做 repurpose，只能配一条 `openai_chat` 通道、endpoint 指向 `/api/plan/v3`、模型填
`doubao-seed-2.0-lite`——即绕开 `volc_agent_plan` 这个名字，用通用 OpenAI 兼容通道。

想省额度就把这两样留给 heuristic 兜底，它们本来就有非模型的降级路径。

**TTS 用不上**：套餐同时覆盖 `doubao-seed-tts-2.0`（Resource ID `seed-tts-2.0`，
端点在 `openspeech.bytedance.com/api/v3/plan/tts/*`）。NexusGate 没有语音合成阶段，
这部分额度在这个项目里无处可用。列在这里只是为了让你知道它存在，不是建议你去接。

---

## 怎么确认配好了

```bash
# 1. 通道自检：只探端点可达性和密钥就绪，不会发计费请求
POST /api/v1/admin/provider-channels/<id>/test
# 期望 "status":"reachable","secret_ready":true
# 注意 http_status 会是 404 —— 套餐端点没有模型列表，这是预期的，不是故障

# 2. 拿一个素材真跑一遍
nexusgate reanalyze -asset <asset-id> -reason "plan key smoke test"
nexusgate pipeline run

# 3. 看有没有真结果
sqlite3 $NEXUSGATE_DATA_DIR/nexusgate.db \
  "select count(*) from asset_shots; select count(*) from shot_text_embeddings;"
```

**Key 没漏出去的检查**（值得做一次）：

```bash
grep -c '<key 前缀>' $NEXUSGATE_DATA_DIR/nexusgate.db   # 期望 0
ls -ld $NEXUSGATE_DATA_DIR/provider-secrets              # 期望 drwx------
```

## 模型名漂了怎么办

套餐端点没有 `/models`，所以只能按名字探。一条命令，不消耗多少额度：

```bash
curl -s -X POST https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions \
  -H "Authorization: Bearer $ARK_AGENT_PLAN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<要试的名字>","messages":[{"role":"user","content":"hi"}],"max_tokens":4}'
```

- `200` + 回复里的 `model` 字段 → 能用，字段值就是它实际解析到的快照
- `404 UnsupportedModel` → 不在套餐里
- `401 AuthenticationError` → 端点写错了（多半写成了 `/api/v3`），不是 Key 的问题

## 一次真实跑通长什么样

50 个素材的旅行文件夹（DJI + iPhone 混拍，含 4 个 DNG），一把 ¥9.9 的套餐 Key，
三条通道全开：

- `probe` 49 成功、`derive` 43 成功 —— 这两段全本地，不吃套餐
- `transcribe` 31 成功、0 失败
- `analyze` 每条产出资产级摘要 + 镜头级描述，`index` 同步建好检索
- 每个镜头一条文本向量，语义检索可用

分析质量的一个样本，一段车头视角的 DJI 素材：

> A short high-speed forward moving shot from inside a train passing railway
> infrastructure, industrial buildings, and freight containers…
> scene: railway / industrial / outdoor / daytime · usable_as: transition, activity_detail

**慢在哪**：`openai_video` 每次要把整段代理内联进请求体，所以 `analyze` 是整条管线里
最慢的一段，几十个素材要按小时算。这是走通道就必须付的代价——多帧路线快得多，但如前所述
接不上通道。

## 几把 Key 并成一条通道

一条通道可以挂多个 member，每个 member 一把 Key、自己的 weight 和 `max_inflight`。
额度按账号独立计，三个 ¥9.9 账号就是三倍月额度，一把耗尽只退休那一个 member。
调度细节和退休机制见 `docs/v0.21-provider-deployment.md`。
