# invalid_encrypted_content 调查进度

- 日期:2026-08-16(更新于当日 16:30,含修复)
- 现象:OpenCode → AxonHub(localhost:8090,渠道 666 "Aether codex" → Aether localhost:8084)的请求报
  `The encrypted content gAAA...p_1A could not be verified. Reason: Encrypted content could not be decrypted or parsed., code: invalid_encrypted_content`
- 之前已做的改动(未提交,已包含在运行中的二进制):`internal/server/orchestrator/outbound.go` 阻止有状态 Responses 请求的渠道切换/重试(`hasStatefulResponsesRequest`);`deploy/start.sh` 打开 codex/opencode trace。
- **结论:不是粘性/账号轮换问题,是 AxonHub 响应方向把多个加密 reasoning blob 首尾拼接成一个坏 blob 发给客户端,客户端原样重放后被 OpenAI 整单拒绝。已实施防御性修复(见"已实施修复"),根因拼接点已收窄到 `accumulatedReasoningSignature` 的多次写入,但触发路径尚未在隔离环境复现。**

## 一、已确凿的证据链

1. **渠道/账号层面全部稳定,无轮换**
   - 渠道 666 单 API key;trace 19 会话的 6 个连续请求(68918–68923)全部落在渠道 666。
   - Aether 的 usage 表显示失败请求 68923 与之前 5 个成功请求走的是**同一个上游 key** `8ee5d6ec`(fbisvlop01@gmail.com,直连 chatgpt.com/backend-api/codex)。
   - Aether 侧会话亲和正常(`client_session_affinity: session=ses_ffe5ff774dd58ffeeH7x5GtGSV`,由 AxonHub 注入的 `Session-Id` 头 + `prompt_cache_key` 驱动)。

2. **坏 blob 在 AxonHub 响应方向产出时就已经是坏的**
   - 68922(成功响应)存库的 `response_body->output[0].encrypted_content` 长 3684,结尾 `p_1A`,与 68923 报错中的 blob 完全一致 → OpenCode 忠实重放了 AxonHub 返回的内容。
   - 用该 blob 直连 Aether(带相同 session、落在同一上游账号)重放 → 同样报 `invalid_encrypted_content` → 请求方向无罪,blob 本身不可解密。

3. **坏 blob = N 个合法 blob 的精确拼接**
   - 解剖 3684 blob:`gAAA` 出现在 0/1228/2456 位 → 恰好 3 × 1228。
   - Fernet 结构校验:单块 1228(无 padding)合法;3684 非法。
   - 全库扫描(近 2 天):**92 个非法 blob**,长度全部是基础单元(1208/1228)的整数倍(2416、2456、3684、4912、6040、6140、7368、9824…)。
   - 分布在**所有** codex 类渠道(523/666/669/670/717),约占 reasoning item 的 15% → AxonHub 全局性 bug,非 Aether 特有。

4. **上游真实形态 vs 客户端收到(以 68922、68880 两例)**
   - request_executions(上游侧聚合):**3 个独立 reasoning item**,各自带大而不同的合法 blob(4664/4300/4088 与 4748/4452/3832)。
   - requests(客户端侧聚合,即 AxonHub 实际发给 OpenCode 的):**1 个 reasoning item,blob = 3 × 1228 拼接**,id 为 AxonHub 生成的 `item_*`,summary 计数 1。
   - 拼接的 1228 块:与本轮上游大 blob、与同会话历史轮次的响应 blob 均**不匹配**;头部前缀 `gAAAAABqgSq*` 与本会话其他 blob 同源(同账号同时间窗)。1228 是"空 reasoning 封套"的恒定大小——与直接探测 Aether 时 `output_item.added` 上的 provisional blob(1228)一致。
   - 上游(Aether→codex)真实事件顺序(实测抓包):`output_item.added` 带 1228 provisional → summary 增量 → `output_item.done` 带最终大 blob。

5. **运行环境确认**
   - 运行中二进制(/usr/local/bin/axonhub,08-16 03:16 构建)含未提交的 orchestrator 改动(特征字符串验证)→ 由当前源码树构建,包含 6/23 修复 5723ee4f "preserve encrypted reasoning item boundaries (#1901)"。
   - go.mod `replace github.com/looplj/axonhub/llm => ./llm` → llm 模块也是本地代码。
   - pass-through 全局未开启(`isPassThroughEnabled` 走 system 设置,默认关)→ 请求和响应都经 AxonHub 解码-重组。

## 二、已排除的假设

| 假设 | 排除依据 |
|---|---|
| Aether 换上游账号 / 亲和失效 | 同一 provider key;Aether 亲和 key 正常 |
| AxonHub 渠道漂移 / key 轮换 | 全部同渠道同 key;TraceStickyKeyProvider 与本案例无关(单 key) |
| 同渠道重试导致跨尝试拼接 | 68922 时间窗日志无任何 retry 记录;重试也只发生在首个内容事件前(preRead 机制) |
| 请求方向转换损坏 blob | 直连 Aether 重放同样失败;inbound.go `ReasoningSignature: reasoningItem.EncryptedContent` 直传 |
| inbound/outbound 转换器拼接 | 单测矩阵(2 个 rs item、3 个 rs item、provisional+done、done 为空回退、同 id 多次 done、真实抓包 SSE)全部产出**独立合法 item**,无法复现拼接 |
| `accumulatedReasoningSignature` 逻辑漏洞(静态分析) | `handleReasoningSignature` 的两个关闭条件(done 元数据 / sourceID 为空)在所有发射路径下都会触发关闭(仅静态分析,见剩余疑点) |
| 聚合器(aggregator)拼接 | 所有赋值点都是"替换"不是追加 |
| AI SDK / OpenCode 拼接 | opencode processor `reasoning-end` 对 metadata 是**替换**;AI SDK 的 reasoning-start/end 也是按 part id 分开;且坏 blob 在 AxonHub 自己的存库聚合里就已经存在 |
| 中间件丢 TransformerMetadata / 复制事件 | orchestrator 全部 LLM 流中间件(performance、rate-limit、circuit-breaker、live-preview、model-mapper)都是指针透传;`item_*` 生成 id 其实是 summary 增量先开启 item 所致(ensureReasoningItemStarted("") 的正常行为),非元数据丢失 |
| SSE 解码器截断/分行 | go-sse MaxEventSize 32MB;无分行逻辑 |
| vercel/ai 重复处理 response.completed | completed 只取 finishReason/usage,不重新摄入 output |

## 三、矛盾核心(剩余疑点)

同一条上游流,两个消费方结果不同:
- **执行记录聚合**(OutboundPersistentStream 捕获的 raw chunks → `AggregateStreamChunks`):3 个 item、大 blob(done 的值)。
- **流式路径**(同一批事件 → `transformStreamChunk` → llm 流 → inbound):客户端拿到 3 × 1228(疑似 added 上的 provisional)且拼成一块。

即:流式路径发出的签名增量像是取了 **added 的 provisional** 而不是 **done 的最终值**(除非 done 的 encrypted_content 在流式解析中为空,回退 pending),且三个增量在 inbound 被拼进同一个 item——这两点都与单测结果矛盾。**尚未找到两条路径产生差异的原因。**

可能性(未验证):
1. 上游 SSE 存在单测未覆盖的事件形态/顺序(如同一 rs 的 added/done 交错、done 缺 id、额外事件类型),使流式路径走了未测到的分支。
2. 流式路径与聚合路径看到的事件字节不同(某 raw 流中间件在两者之间做了改写)——目前只核实了 pass-through 相关中间件。
3. 生产二进制行为与源码有差异(构建时间/来源存疑但已有特征字符串佐证一致)。

## 四、追加证据(16:00–16:30)

1. **provisional blob 本身可重放**:从抓包的 `output_item.added` 取出 1228 临时 blob 直连 Aether 重放(同 session、同账号)→ HTTP 200。**拼接是唯一致命形态**,简化了修复判定。
2. **单 rs 响应也存在"取值错位"**:请求 68946/68947(经 AxonHub)上游侧聚合 final=1292/1400,客户端却收到 1228(added 上的 provisional);而隔离环境同样事件形态得到的是 final。说明生产管线中流式路径与聚合路径对同一事件的解析结果不同,进一步佐证拼接/取值问题在 orchestrator 层,而非转换器本身。
3. **上游账号按会话亲和分配**(Aether 亲和键含 session),`544ffc07` 与 `8ee5d6ec` 两个账号都出现过;但多账号直连探测的事件形态一致(added+done 两事件),排除"特定账号发三事件形态"假说。
4. **无法在隔离环境复现**:自建 `llm/cmd/diagprobe`(真实 httpclient + codex 出站 + 双轮对话含历史重放)始终干净;转换器单测矩阵(多 rs、provisional+空 done、同 id 多次 done、真实抓包 SSE)全部通过。

## 五、已实施修复(防御性,已过全部测试)

**`llm/transformer/shared/openai.go`**:新增 `SanitizeOpenAIEncryptedContent`——
- 整体是结构合法的 Fernet blob → 原样透传(含带 `=` padding 的 1208 形态);
- 是多个合法 blob 的拼接(`gAAA` 边界切分后每段都合法)→ **只保留最后一段**(与"done 的 final 替换 added 的 provisional"语义一致);
- 其余情况(短值、他家签名如 Anthropic `EqQ...`、截断值)→ 原样透传,保证跨格式场景不受影响。纯"去拼接"守卫,不引入新的丢弃行为。

**`llm/transformer/openai/responses/inbound_stream.go` `closeReasoningItem()`**:发射 `output_item.done` 前对 `accumulatedReasoningSignature` 调用上述守卫;发生截取时打 WARN 日志(便于在生产日志里继续追根因:出现 `split concatenated encrypted reasoning content` 即命中)。

**验证**:
- 单测 `transformer/shared/openai_sanitize_test.go`、回归 `transformer/openai/responses/repro_sanitize_regression_test.go`(3×1228 → 保留最后 1228)全过;
- 用生产真实坏 blob(requests 68922 的 3684)直接验证:正确降为最后 1228 段(该段与上游 provisional 同构,可重放);
- `llm` 模块全量测试通过。

**生效方式**:需要重新构建并重启 AxonHub 才生效(当前运行实例未包含此修复;遵循"不重启开发服务器"约束,由维护者择机部署)。

## 六、下一步(追根因,可选)

1. 部署本修复后,观察 WARN `split concatenated encrypted reasoning content` 的出现频率与 `accumulated_len` 分布(2 块/3 块)。
2. 复现工具已就绪:`llm/cmd/diagprobe`(AETHER_KEY 环境变量,可选 CONSECUTIVE=1)。下一步若要继续追,需在生产开 debug 日志(server.debug=true 或 AXONHUB_SERVER_DEBUG)复现一次,抓 "received response stream event" 原始事件序列,对照 inbound 侧输出定位多次写入的来源。
3. 顺带排查:上游多 rs item 响应(生产中 3 连 rs + 3 fc)的触发条件——可能与 OpenCode task 子代理的多步工具链会话有关,隔离探测未能触发。

## 七、关键文件/数据索引

- 坏样本请求:AxonHub requests 68922(666/luna, 3684=3×1228)、68880(669/luna, 3684)、68883/68865(3684)、68867(4912=4×1228) 等;对应 request_executions 83181(68922)、83135(68880)。
- 单 rs 取值错位样本:68946(上游 1292/客户端 1228)、68947(上游 1400/客户端 1228)。
- 上游事件参考:`/tmp/aether_probe2.sse`、`/tmp/probe_ses_testdiag0010.sse`(直连 Aether 抓包:added 带 1228 provisional、done 带最终大 blob)。
- 诊断工具:`llm/cmd/diagprobe/main.go`(双轮对话 + 历史重放,直连 Aether 走真实 httpclient/转换器链路)。
- 复现测试(保留):`llm/transformer/openai/responses/repro_sanitize_regression_test.go`、`llm/transformer/shared/openai_sanitize_test.go`。
- 相关代码:
  - 发射签名增量:`llm/transformer/openai/responses/outbound_stream.go:446-475`(done,带 pending 回退)、`:217-226`(added,存 pending)
  - 拼接唯一可疑点(已加守卫):`llm/transformer/openai/responses/inbound_stream.go:418`(`accumulatedReasoningSignature.WriteString`);关闭逻辑 `:408-432`;发射+守卫 `closeReasoningItem :726-810`
  - 聚合(两侧共用):`llm/transformer/openai/responses/aggregator.go`
  - Aether 源码:`/tmp/aether-src`(亲和 `apps/aether-gateway/src/client_session_affinity.rs`,粘性 token 提取 `crates/aether-ai/serving/src/candidate_resolution.rs:188`,亲和键含 client_family/api_key/model,TTL 300s)
