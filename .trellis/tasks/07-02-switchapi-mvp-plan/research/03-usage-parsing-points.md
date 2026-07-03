# Research: #03 透传代理的 token 用量采集点（usage-parsing-points）

- **Query**: prd.md 研究项 #3 — 透传代理 tee 响应流时，Anthropic Messages / OpenAI Responses / OpenAI chat.completions 三种线格式的精确 usage 采集点；累计 vs 增量语义；include_usage 依赖；中断场景；三格式 → 四分项（input/output/cache_write/cache_read）映射表
- **Scope**: mixed（官方 SDK/源码 + 本机 CLI/配置实证）
- **Date**: 2026-07-03
- **对应 design.md 标注**: `[研究#3]`（§4 Agent 设计 · 用量采集）

---

## 结论

### C1. Anthropic Messages 流式：采集点 = `message_start` + 最后一个 `message_delta`，usage 为**累计值（直接覆盖，不要累加）** 【置信度: High】

- SSE 事件序列：`message_start → (content_block_* / ping)* → message_delta → message_stop`。
- **`message_start.message.usage`**：请求开始即给出输入侧完整计量 —— `input_tokens`（非缓存部分）、`cache_creation_input_tokens`（缓存写）、`cache_read_input_tokens`（缓存读），以及一个很小的初始 `output_tokens`（非 0，即使空响应也非 0）。
- **`message_delta.usage`**：`output_tokens` 为必有字段，官方 schema 明确标注 **"The cumulative number of output tokens which were used"** —— 是累计口径。Anthropic 官方 SDK 的流累加器对它做的是**赋值**而非相加（`current_snapshot.usage.output_tokens = event.usage.output_tokens`），这是累计语义的代码级铁证。
- `message_delta.usage` 中 `input_tokens / cache_creation_input_tokens / cache_read_input_tokens` 为**可选**字段（同样标注 cumulative），近期 API 版本会在 delta 中回带/修正输入侧数字（如 server tool use 场景）。**解析策略：出现即整字段覆盖（merge-overwrite），最终记录以最后一次出现的值为准。**
- 通常一次响应只有一个 `message_delta`（携带 stop_reason）；即便出现多个，累计语义保证"取最后一个"永远正确。

### C2. Anthropic 非流式：顶层 `usage` 对象一次性给全 【置信度: High】

- 响应 JSON 顶层 `usage`：`input_tokens`（必有）、`output_tokens`（必有）、`cache_creation_input_tokens`、`cache_read_input_tokens`（可选，未启用缓存时可能缺失/null）。
- 附加字段：`cache_creation`（按 TTL 细分：`ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`）、`service_tier`、`output_tokens_details`（输出中 reasoning 的观测性细分，**output_tokens 仍是计费权威总数**）。
- **关键口径**：官方 schema 原文 —— *"Total input tokens in a request is the summation of `input_tokens`, `cache_creation_input_tokens`, and `cache_read_input_tokens`."* 即 Anthropic 的 `input_tokens` **不包含**两个缓存字段，三者互斥相加才是总输入。

### C3. OpenAI Responses API：采集点 = `response.completed` 事件内嵌的 `response.usage`；`usage` 字段本身可为 null 【置信度: High】

- 流式：`response.created → response.in_progress → response.output_*.delta ... → response.completed`（终态还有 `response.incomplete` / `response.failed`，以及独立的顶层 `error` 事件）。三个终态事件都携带完整 `response` 对象。
- `response.usage` 形状：`input_tokens`、`input_tokens_details.cached_tokens`、`output_tokens`、`output_tokens_details.reasoning_tokens`、`total_tokens`。**usage 在 Response 对象上是 Optional** —— 中转站/异常路径可能缺失，解析必须空值防御。
- 非流式：响应 JSON 顶层同一个 `usage` 对象，无需任何 opt-in。
- Codex 自身的解析器（codex-rs）**只从 `response.completed` 提取 usage**（`resp.usage.map(Into::into)` 容忍缺失）；`response.failed` / `response.incomplete` 被当作错误处理、不取 usage。我们的代理可以更进一步：对 `incomplete/failed` 也尽力读取 `response.usage`（wire 上存在该对象），取不到再降级。
- **无事后补救通道**：Codex 对非 Azure 上游发 `store: false`（源码 `store: provider.is_azure_responses_endpoint()`），且本机配置 `disable_response_storage = true` —— 中断后不能靠 `GET /v1/responses/{id}` 找回 usage。

### C4. OpenAI chat.completions 流式：usage 仅在客户端显式传 `stream_options.include_usage=true` 时出现 【置信度: High】

- 官方语义（schema 原文）：*"If set, an additional chunk will be streamed before the `data: [DONE]` message. The `usage` field on this chunk shows the token usage statistics for the entire request, and the `choices` field will always be an empty array. All other chunks will also include a `usage` field, but with a null value. **NOTE: If the stream is interrupted, you may not receive the final usage chunk.**"*
- 非流式 chat.completions：顶层 `usage` 总是存在（`prompt_tokens` / `completion_tokens` / `total_tokens` + details）。
- **透传代理不能指望客户端主动带 include_usage** —— 但见 C5，此问题对本项目 MVP 实际上已消解。

### C5. 重大事实：Codex 已彻底移除 `wire_api = "chat"`，本机版本亦然 → MVP 的 OpenAI 侧只需实现 Responses 解析 【置信度: High】

- codex 源码 `model-provider-info`：`WireApi` 枚举**只剩 `Responses` 一个变体**；反序列化遇到 `"chat"` 直接报错：*"`wire_api = \"chat\"` is no longer supported. How to fix: set `wire_api = \"responses\"` ... More info: github.com/openai/codex/discussions/7782"*。同时 `ollama-chat` provider 也被移除。
- 官方讨论 #7782 标题：**"Deprecating `chat/completions` support in Codex"**；社区反馈 codex-cli ≤0.80.0 可用、~0.84 起失效。
- **本机实证**：已安装 codex-cli 0.142.5，真实二进制（`~/.codex/packages/standalone/current/bin/codex`）内含 7 处 `discussions/7782` 字符串 —— 移除逻辑已随本机版本生效；且本机 `~/.codex/config.toml` 的自定义中转 provider 正是 `wire_api = "responses"`（经本地代理指向中转站），证明中转站生态已跟进 Responses。
- 推论：App 范围 = CC + Codex（CONTEXT.md），Anthropic 协议下 usage 无条件存在、Codex 只会说 Responses ⇒ **include_usage 问题在 MVP 范围内不存在**。chat.completions 映射仍写入本文（见映射表 3），作为防御性次要解析路径（旧版 Codex / 未来扩展），但不进 MVP 验收。

### C6. include_usage 注入与其它 fallback 的裁决 【置信度: High（对 ADR 的解读）】

若未来支持 chat wire、且客户端未带 include_usage，备选方案评估：

| 方案 | 评估 | 结论 |
|---|---|---|
| 代理向请求体注入 `stream_options.include_usage=true` | 违反 ADR-0002（"仅换 auth 头 + 可选模型名重定向，不解析或改写消息体结构"）。且注入后**响应流会多出一个 choices 为空的终段 chunk**：要么透传给客户端（改变客户端所见响应，老客户端可能解析异常），要么代理吞掉该 chunk（= 篡改响应流，违背"透传字节一致"的测试基线） | **不采纳**。如未来确需，必须走 ADR 修订、显式白名单化这一改写 |
| 响应头取用量 | OpenAI/中转站无每请求 usage 头；`x-ratelimit-remaining-*` 是配额余量、并发下不可归因到单请求。Anthropic 的 `anthropic-ratelimit-*-tokens-remaining` 同理 | **不可行** |
| tokenizer 估算（tiktoken o200k_base 估请求侧 + 累计 delta 文本估输出） | 可行但有误差（尤其 tool call / 图片） | **最后手段**：记录必须打 `usage_source=estimated` 标记，费用页显式标注 |

### C7. 中断 / 半途错误：各阶段已知的 usage 数字 【置信度: High（Anthropic/chat）；Medium（Responses incomplete/failed 的 usage 填充度未实测）】

**Anthropic 流（Claude Code 路径）**

| 流死亡阶段 | 已知 | 未知 | 建议记录 |
|---|---|---|---|
| `message_start` 之前（连接失败/4xx/5xx JSON error） | 无 | 全部 | status=error，四分项 0，usage_source=none |
| `message_start` 之后、`message_delta` 之前（客户端 abort / TCP 断 / SSE `error` 事件如 overloaded_error） | input / cache_write / cache_read（start 中即为最终值）+ 初始 output_tokens（≈1-few） | 真实 output | 输入侧照记；output 记初始值或按已 tee 文本估算并标 estimated |
| 收到最终 `message_delta` 后 | 全部四分项（累计值） | — | 完整记录；即使 `message_stop` 没到也可入账 |
| `message_stop` 后 | 全部 | — | 正常 |

**OpenAI Responses 流（Codex 路径）**

| 流死亡阶段 | 已知 | 建议记录 |
|---|---|---|
| `response.created/in_progress` 阶段（该阶段 response.usage 为 null）客户端 abort / 连接断 | 无 wire 数字 | status=aborted；可 tokenizer 估算（标 estimated）；`store=false` ⇒ 无法事后查询 |
| 顶层 `error` 事件（事件本身不带 usage） | 无 | 同上 |
| `response.failed` | 事件携带 response 对象，`usage` 可能为 null（Codex 弃取；我们尽力读取） | 读到则记 + status=error；读不到 usage_source=none |
| `response.incomplete`（如 max_output_tokens 截断） | 携带 response 对象，usage 通常已填充（未实测，见遗留） | 尽力读取，status=incomplete |
| `response.completed` | `response.usage`（可选字段，缺失需防御） | 正常记录 |

**OpenAI chat.completions 流（防御路径）**：官方明言流被打断就可能收不到最终 usage chunk ⇒ abort = 无数字，只能估算。非流式三协议都在响应 JSON 里，一次性解析。

### C8. 非计费流量必须排除 【置信度: High】

经代理的 `/v1/messages/count_tokens`（CC 会调用）、`GET /v1/models`、CORS/OPTIONS 等不产生计费 usage，不得写入 usage_records（token 计数接口免费，且其响应的 `input_tokens` 不是消费）。转发器按路径白名单区分"计费推理请求"与"其他透传请求"。

---

## 三种线格式 → 四分项映射表（交付物）

> usage_records 四分项列：`input` / `output` / `cache_write` / `cache_read`（design.md §2）。
> 统一语义定义：**input = 非缓存输入**；**cache_read = 从缓存读取的输入**；**cache_write = 写入缓存的输入（仅 Anthropic 存在）**；**output = 全部输出（含 reasoning，计费即按 output 价）**。

### 表 1 — Anthropic Messages（流式取自 `message_start.message.usage`，被 `message_delta.usage` 同名字段覆盖；非流式取顶层 `usage`）

| wire 字段 | 类型/出现 | → 四分项 | 说明 |
|---|---|---|---|
| `usage.input_tokens` | int，必有 | **input** | 已天然不含缓存部分，直接入列 |
| `usage.cache_creation_input_tokens` | int，可选(null→0) | **cache_write** | 缓存写入（计费 1.25x/2x 基价，价表归研究#4） |
| `usage.cache_read_input_tokens` | int，可选(null→0) | **cache_read** | 缓存命中（≈0.1x 基价） |
| `usage.output_tokens` | int，必有 | **output** | message_delta 中为累计值，取最后出现的值**覆盖** |
| `usage.cache_creation.ephemeral_5m_input_tokens` / `.ephemeral_1h_input_tokens` | 可选细分 | 不入列（可存 detail JSON） | 5m/1h 写入价不同 → 影响研究#4 计价精度，MVP 可按总量×5m 价近似 |
| `usage.output_tokens_details` | 可选细分 | 不入列 | output_tokens 已是计费权威总数 |
| `usage.server_tool_use` / `service_tier` / `inference_geo` | 可选 | 不入列 | 与四分项无关 |
| `message_delta.usage.input_tokens` 等输入侧字段 | 可选 | 出现即覆盖 start 值 | SDK 累加器同款 merge-overwrite 策略 |

**校验恒等式**：总输入 = input + cache_write + cache_read（官方口径）。

### 表 2 — OpenAI Responses API（流式取自 `response.completed`（兜底 `response.incomplete`/`response.failed`）的 `response.usage`；非流式取顶层 `usage`）

| wire 字段 | 类型/出现 | → 四分项 | 说明 |
|---|---|---|---|
| `usage.input_tokens` | int | **input = input_tokens − cached_tokens** | ⚠️ OpenAI 的 input_tokens **包含** cached_tokens（子集关系），必须做减法，否则缓存部分被按全价重复计费 |
| `usage.input_tokens_details.cached_tokens` | int（details 整块可能缺失→0） | **cache_read** | 命中缓存的输入（官方计费约 5 折/更低，价表归研究#4） |
| — | 不存在 | **cache_write = 0** | OpenAI 自动缓存、无写入费、无对应字段 |
| `usage.output_tokens` | int | **output** | **已包含** reasoning tokens |
| `usage.output_tokens_details.reasoning_tokens` | int（可能缺失） | 不入列（可存 detail JSON） | output_tokens 的观测性细分；LiteLLM 亦按 output 单价计 reasoning |
| `usage.total_tokens` | int | 不入列 | 仅作校验：total = input_tokens + output_tokens |

**子集语义双重证据**：openai-python schema 将 cached_tokens/reasoning_tokens 定义为 input/output 的 "detailed breakdown"；Codex 自己实现 `non_cached_input() = input_tokens - cached_input_tokens`。

### 表 3 — OpenAI chat.completions（防御性次要路径；流式取最终 usage chunk（仅当客户端带 include_usage），非流式取顶层 `usage`）

| wire 字段 | 类型/出现 | → 四分项 | 说明 |
|---|---|---|---|
| `usage.prompt_tokens` | int | **input = prompt_tokens − cached_tokens** | 同表 2 的子集语义 |
| `usage.prompt_tokens_details.cached_tokens` | int（details 可能缺失→0） | **cache_read** | |
| — | 不存在 | **cache_write = 0** | |
| `usage.completion_tokens` | int | **output** | 含 reasoning（`completion_tokens_details.reasoning_tokens` 为其子集） |
| `usage.completion_tokens_details.*`（reasoning/audio/prediction） | 可选 | 不入列 | 观测性细分 |
| `usage.total_tokens` | int | 不入列 | 校验用 |
| 流式各中间 chunk 的 `usage` | 恒为 null | 忽略 | 只认 choices 为空的终段 chunk |

**通用防御规则**（中转站现实）：`usage` 整块、任一 details 子对象都可能缺失 → 全部按 null→0 处理；负值 clamp 到 0（Codex 同款 `.max(0)`）；缺 usage 时按 C6 估算或记 usage_source=none。

---

## 证据与来源

**Anthropic（官方）**

1. Anthropic 官方 claude-api skill 文档（Anthropic 出品，随 Claude Code 分发，cached 2026-06）：SSE 事件表（message_delta "Contains stop_reason, usage"）、curl SSE 示例、prompt-caching 篇 *"Total prompt size = input_tokens + cache_creation_input_tokens + cache_read_input_tokens"*、非流式 `.usage.input_tokens/.usage.output_tokens` 解析示例。
2. [Anthropic, anthropic-sdk-python `types/message_delta_usage.py`, 取阅 2026-07-03, https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/types/message_delta_usage.py] — 四字段全部标注 "The cumulative number of ..."；output_tokens 必有、输入侧可选。（Stainless 由官方 OpenAPI spec 生成，等同 API 文档口径）
3. [Anthropic, anthropic-sdk-python `types/usage.py` 与 `types/raw_message_delta_event.py`, 同上] — Usage 字段全集、cache_creation TTL 细分、"Total input tokens ... summation" 原文。
4. [Anthropic, anthropic-sdk-python `lib/streaming/_messages.py` L503-518, 同上] — 官方累加器对 message_delta.usage 的赋值覆盖 + 可选输入字段 merge 逻辑（累计语义的实现级证据）。
5. [Anthropic, anthropic-sdk-python `types/raw_message_start_event.py` / `types/cache_creation.py`, 同上] — message_start 内嵌完整 Message（含 usage）；ephemeral_5m/1h 字段名。
   （注：platform.claude.com/docs 与 docs.claude.com 文档页在本机网络环境被区域拦截，无法直接引用页面；以上 1+2/3/4 已构成 ≥2 个独立官方来源。）

**OpenAI（官方）**

6. [OpenAI, openai-python `types/chat/chat_completion_stream_options_param.py`, 取阅 2026-07-03, https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py] — include_usage 完整语义与"流中断可能收不到最终 usage chunk"警告原文。
7. [OpenAI, openai-python `types/completion_usage.py` 与 `types/responses/response_usage.py`, 同上] — chat 与 Responses 两套 usage 形状；cached_tokens/reasoning_tokens 定义为 input/output 的 "detailed breakdown"（子集语义）。
8. [OpenAI, openai-python `types/responses/response_completed_event.py` / `response_incomplete_event.py` / `response_failed_event.py` / `response_error_event.py` / `response.py`, 同上] — 三个终态事件均携带完整 Response；`Response.usage: Optional[ResponseUsage]`；status 枚举 completed/failed/in_progress/cancelled/queued/incomplete。

**Codex（官方源码 + 官方讨论）**

9. [OpenAI, codex `codex-rs/model-provider-info/src/lib.rs`, 取阅 2026-07-03, https://github.com/openai/codex/blob/main/codex-rs/model-provider-info/src/lib.rs] — WireApi 仅剩 Responses；`"chat"` 反序列化报错常量 CHAT_WIRE_API_REMOVED_ERROR。
10. [OpenAI, codex `codex-rs/codex-api/src/sse/responses.rs`, 同上] — usage 仅从 response.completed 提取（Optional 容忍）；failed/incomplete 按错误处理；ResponseCompletedUsage→TokenUsage 的字段映射。
11. [OpenAI, codex `codex-rs/core/src/client.rs` 与 `codex-rs/protocol/src/protocol.rs`, 同上] — `store: provider.is_azure_responses_endpoint()`；"stream closed before response.completed" 错误；TokenUsage 的 `non_cached_input()/blended_total()` 子集实现。
12. [OpenAI, codex Discussion #7782 "Deprecating `chat/completions` support in Codex", https://github.com/openai/codex/discussions/7782] — 移除公告；社区反馈 0.80.0 可用、之后版本失效。

**本机实证（read-only，优先级最高）**

13. 本机 CLI：Claude Code 2.1.199、codex-cli 0.142.5（`claude --version` / `codex --version`）。
14. `~/.codex/config.toml`：自定义 provider `wire_api = "responses"`、`disable_response_storage = true`（API key 已按规约不记录）。
15. `~/.codex/packages/standalone/current/bin/codex`（真实二进制）：内含 7 处 `discussions/7782` 字符串 → 本机版本已移除 chat wire。

---

## 对 design.md 的影响

design.md §4 `[研究#3]` 原文假设：*"用量采集：tee 响应流解析 usage（Anthropic：message_start/message_delta；OpenAI Responses：response.completed）"* —— **判定：confirmed-with-changes（方向正确，需补充以下修订）**：

1. **【补充语义】** 注明 Anthropic `message_delta.usage` 为**累计值**：解析器对同名字段做覆盖合并（含 delta 中可选出现的输入侧字段），禁止累加。
2. **【补充采集点】** 两协议都必须同时实现**非流式 JSON 响应解析**（顶层 `usage`）；不能假设 100% SSE。
3. **【补充采集点】** Responses 侧除 `response.completed` 外，还应监听 `response.incomplete` / `response.failed`（尽力读取内嵌 `response.usage`）与顶层 `error` 事件（记错误、无 usage）；`usage` 字段可选，必须空值防御 —— Codex 官方解析器即容忍 usage 缺失。
4. **【数据模型】** usage_records 建议增加 `usage_source` 列（`wire` | `estimated` | `none`），与现有 `status`/`error_kind` 配合表达中断场景（C7 矩阵）；可选增加 `usage_detail` JSON 列存 reasoning/TTL 细分（超 MVP 可砍）。
5. **【映射规则】** 计价前的四分项换算必须内置协议差异：Anthropic `input_tokens` 不含缓存（直接入列）；OpenAI `input/prompt_tokens` 含 `cached_tokens`（**必须减法**，否则缓存 token 按全价重复计费，直接违反验收标准"费用误差可解释"）；OpenAI 恒无 cache_write；reasoning 计入 output 不单列。
6. **【范围确认】** OpenAI 侧 MVP 只实现 Responses 解析（与 Codex ≥0.8x 现状、本机 0.142.5、本机中转配置三重一致）；chat.completions 解析降级为防御性次要路径、不进验收。**不注入** `stream_options.include_usage`（违反 ADR-0002 且需改写响应流）；无 usage 时走 tokenizer 估算并打标。
7. **【转发器】** 路径分类：`/v1/messages/count_tokens`、`GET /v1/models` 等非计费端点透传但不产生 usage_records（C8）。
8. **【测试策略§10】** fake-upstream 回放用例需覆盖：Anthropic 多 message_delta / delta 带输入侧字段；Responses usage 缺失 / incomplete / failed；中途断流各阶段（对应 C7 矩阵逐行断言）。
9. **【移交研究#4】** Anthropic `cache_creation` 的 5m/1h TTL 写入价差、OpenAI cached_tokens 折扣价 → 由 LiteLLM 价格字段映射研究确认对应单价字段。

---

## 遗留不确定性

1. **`response.incomplete` / `response.failed` 时 `usage` 的填充率**：wire schema 上 usage 为 Optional，官方未承诺终态必填；Codex 干脆不取。需在 M1 集成测试用 fake-upstream + 真实中转站实测（Medium 置信，不影响架构，只影响 C7 矩阵中两行的数据完整度）。
2. **中转站兼容性面**：非官方上游可能整块缺 `usage`、缺 `*_details`、甚至给出不含 cached 子集语义的自定义数字。本文映射按官方语义 + null→0 防御，样本外行为待接入真实中转站后回归验证。
3. **Anthropic `message_delta` 回带输入侧字段的确切触发条件**（server tool use、计费修正等）未穷举；merge-overwrite 策略对其免疫，风险仅在"是否需要展示中间值"层面（无需）。
4. **Codex 移除 chat wire 的精确版本号**：社区反馈 ≤0.80.0 可用、~0.84 起报错，未在 CHANGELOG 精确定位（GitHub API 配额限制）；不影响结论 —— 本机 0.142.5 已实证移除。若未来要兼容 0.8x 之前的旧版 Codex，才需要激活表 3 路径。
5. **tokenizer 估算精度**：tool_use/图片/system 注入下 o200k_base 估算误差未量化；作为 estimated 记录的展示策略（是否计费）留给产品决策。
