# Research #02 — Codex CLI 自定义供应商配置（config.toml / auth.json / wire_api / usage）

- **Query**: prd.md 研究项 #2 — Codex 如何指向自定义 OpenAI 兼容供应商；config.toml `model_providers` 字段、auth.json 结构与使用时机、配置读取时机、wire_api 对 Agent 直通的路径要求、两种 wire 格式下的 usage 字段。
- **Scope**: mixed（本机 ground truth + openai/codex 源码@rust-v0.142.5 + 官方文档/讨论区 + openai-python 官方 SDK 类型）
- **Date**: 2026-07-03
- **本机版本基准**: codex-cli **0.142.5**（`codex --version`），源码核对基于同版本 tag `rust-v0.142.5`

---

## 结论

### C1. 自定义供应商的标准写法（置信度：High）

`~/.codex/config.toml` 顶层 `model_provider = "<id>"` 选择 `[model_providers.<id>]` 表中的条目。**内置 id（`openai`、`amazon-bedrock`、`ollama`、`lmstudio`）不可被同名覆盖**（合并逻辑用 `or_insert`，同名配置直接被忽略），所以 switchAPI 必须使用自定义 id（如 `switchapi`）。

`ModelProviderInfo` 在 0.142.5 的完整字段（`#[schemars(deny_unknown_fields)]`，未知字段会被 strict-config 诊断）：

| 字段 | 类型/默认 | 说明 |
|---|---|---|
| `name` | String，默认 "" | UI 显示名 |
| `base_url` | Option\<String\> | 兼容 API 基址；缺省时按登录态取 `https://api.openai.com/v1` 或 ChatGPT backend |
| `env_key` | Option\<String\> | 存 API Key 的**环境变量名**；设置后该变量必须非空，否则报错 |
| `env_key_instructions` | Option\<String\> | env_key 缺失时给用户的提示 |
| `experimental_bearer_token` | Option\<String\> | 直接内联 Bearer token（官方注释：不建议，供程序化使用） |
| `auth` | Option（command 型） | 外部命令产出 bearer token（含 refresh_interval_ms） |
| `aws` | Option | Bedrock SigV4（与 env_key/bearer/requires_openai_auth 互斥） |
| `wire_api` | 默认 `"responses"` | **只剩 `"responses"` 一个合法值**（见 C4） |
| `query_params` | Option\<map\> | 追加到 URL 的查询参数（Azure `api-version` 场景） |
| `http_headers` | Option\<map\> | 每请求附带的固定头 |
| `env_http_headers` | Option\<map\> | 头名 → 环境变量名，变量非空才附带 |
| `request_max_retries` | 默认 4（上限 100） | HTTP 请求重试 |
| `stream_max_retries` | 默认 5（上限 100） | SSE 断流重连 |
| `stream_idle_timeout_ms` | 默认 300_000（5 分钟） | 流空闲超时 |
| `websocket_connect_timeout_ms` | 默认 15_000 | WS 连接超时 |
| `requires_openai_auth` | 默认 false | true 时首跑弹登录界面、凭据存 auth.json |
| `supports_websockets` | 默认 false | true 时走 Responses-over-WebSocket 传输（Agent MVP 不支持，保持缺省） |

选择链：CLI `-c model_provider=...` / `--profile` 的 `profiles.<name>.model_provider` → 顶层 `model_provider` → 默认 `"openai"`；查不到 id 直接报错 `Model provider `<id>` not found`。`[profiles.<name>]` 可携带 model、model_provider、model_reasoning_effort 等一整套（profile_toml.rs）。另有专用旋钮 `openai_base_url`（config.toml 键）可只改内置 openai 供应商的基址。项目级 `.codex/config.toml` **禁止**设置 `model_provider`/`model_providers`（denylist，防仓库劫持凭据流向）。

### C2. auth.json 结构与生效时机（置信度：High）

`$CODEX_HOME/auth.json`（默认 `~/.codex/auth.json`）结构（storage.rs `AuthDotJson`）：

```json
{
  "auth_mode": "apikey | chatgpt（可省略）",
  "OPENAI_API_KEY": "sk-...（API-key 登录模式）",
  "tokens": { "id_token": "JWT", "access_token": "JWT", "refresh_token": "...", "account_id": "..." },
  "last_refresh": "RFC3339 时间戳",
  "personal_access_token": "...", "agent_identity": {}, "bedrock_api_key": {}
}
```

本机实测：`~/.codex/auth.json` 仅含 `{"OPENAI_API_KEY": "<REDACTED...c988>"}`（61 字节，API-key 模式，无 tokens）。

**鉴权解析优先级**（对每个请求，model-provider/src/auth.rs `resolve_provider_auth`）：

1. `env_key` 指定的环境变量 → `Authorization: Bearer <值>`；
2. `experimental_bearer_token` / `auth`（command 型）；
3. 全局 AuthManager 的 CodexAuth（`CODEX_API_KEY` 环境变量需显式启用；否则读 auth.json：API-key 模式发 `Bearer <OPENAI_API_KEY>`，ChatGPT 模式发 `Bearer <access_token>` + `ChatGPT-Account-ID` 头）；
4. 都没有 → **不带任何鉴权头**（UnauthenticatedAuthProvider）。

**关键结论**：自定义供应商即使 `requires_openai_auth = false` 且无 `env_key`，AuthManager 仍被无条件传入（thread_manager.rs / turn_context.rs），auth.json 里的 `OPENAI_API_KEY` **会作为 Bearer 发给自定义 base_url**。`requires_openai_auth` 只控制"首跑是否弹登录界面 + 账号状态 UI"，不控制"是否回落 auth.json"。本机 cc-switch 的十余条 codex 供应商记录全部依赖此行为（auth.json 写站点真实 key + model_providers 无 env_key），且实际可用——行为得到源码与生产使用双重印证。

鉴权头在 provider `http_headers` 之后以 `HeaderMap::insert` 写入，**会覆盖** `http_headers` 里的同名 `Authorization`；但自定义头（如 `X-SwitchAPI-Token`）不受影响。auth.json 由 AuthManager 启动时读入并缓存，运行中仅在登录/登出/token 刷新等内部路径 `reload()`；外部改写 auth.json 对**运行中的**会话不保证立即生效。

`preferred_auth_method` 在 0.142.5 源码中已不存在（0 处引用），是历史遗留键（本机 config.toml 中仍有，属无效残留）。`codex login --api-key <key>` 写入 `{"auth_mode":"apikey","OPENAI_API_KEY":...}`。

### C3. 配置读取时机（置信度：High，建议实现期做 5 分钟实测复核）

- 每次 `codex` 进程启动（TUI/exec/doctor）通过 `load_config_or_exit` 走 ConfigLayerStack 读取合并：系统层 `/etc/codex/config.toml` → 用户层 `~/.codex/config.toml` → 项目层（受 denylist 限制）→ managed/cloud 层 → CLI `-c` 覆盖。
- **无 config.toml 文件监听**：workspace 里的 file-watcher crate（notify 8.2.0）只用于 app-server 的工作区文件/skills 监听；TUI 启动后唯一的重新加载点是启动期 personality 迁移（tui/src/lib.rs L1115），不是运行时热重载。
- 结论：**改 config.toml 只对下一次启动的 codex 进程生效；运行中的会话不感知**。对 switchAPI 无碍——切换发生在 Agent 内部，codex 配置一次写死指向 127.0.0.1；只有首次安装接管时需要用户重启正在运行的 codex 会话。

### C4. wire_api 与 Agent 必须直通的端点（置信度：High）

**`wire_api = "chat"` 已被移除**：官方 2026-01 公告弃用、2026-02 初完全移除（讨论区 #7782，维护者确认"Support for 'chat' won't be removed until Feb 1"）；0.142.5 的 `WireApi` 枚举只剩 `Responses`，反序列化 `"chat"` 直接报错并指向该讨论。cc-switch 本机所有 codex 记录也均为 `wire_api = "responses"`。

URL 拼接规则：`url_for_path` = `base_url.trim_end('/') + "/" + path` + query_params。`base_url = "http://127.0.0.1:9527/openai/v1"` 时 Codex 实际访问：

| 端点 | 方法 | 触发时机 | Agent 是否必须支持 |
|---|---|---|---|
| `{base}/responses` | POST（SSE，`Accept: text/event-stream`） | 每轮对话（主通道） | **必须** |
| `{base}/models` | GET（带同样鉴权头） | 模型目录刷新（带本地缓存 TTL + etag） | 应转发；失败仅 error log，回落 bundled/缓存目录，**非致命** |
| `{base}/responses/compact` | POST | 远端压缩 | **不会调用**：`supports_remote_compaction()` 仅对官方 OpenAI/Azure 判定为真 |
| WebSocket 变体 | — | 仅 `supports_websockets=true` | 不配置即不触发 |

请求体要点（ResponsesApiRequest）：`model`、`instructions`、`input[]`、`tools[]`、`stream`、`store`（0.142.5 固定为 `provider.is_azure_responses_endpoint()`，即对我们 = **false**）、`prompt_cache_key`、`reasoning` 等。附加头：`session-id`/`thread-id`/`x-client-request-id`、User-Agent/originator、`http_headers`/`env_http_headers`。限流信息从**响应头**解析（无独立 usage 端点）。

SSE 事件消费（Agent 必须逐事件透传，不可缓冲重组）：`response.created`、`response.output_item.added/done`、`response.output_text.delta`、`response.reasoning_summary_text.delta`、`response.reasoning_text.delta`、`response.custom_tool_call_input.delta`、`response.reasoning_summary_part.added`、`response.failed`、`response.incomplete`、`response.completed`。**流必须以 `response.completed` 结束**，否则 codex 报 "stream closed before response.completed" 并按 `stream_max_retries`（默认 5）重试。流空闲超时默认 5 分钟——Agent 透传时保持上游 keep-alive 字节原样通过即可。

### C5. 两种 wire 格式的 usage 采集点（置信度：High）

**Responses API（Codex 唯一在用）** — `response.completed` 事件 → `response.usage`：

```
input_tokens                      → usage_records.input_tokens（注意：含 cached 部分）
input_tokens_details.cached_tokens → cache_read_tokens
output_tokens                     → output_tokens
output_tokens_details.reasoning_tokens → （计费上含在 output 内，可单列展示）
total_tokens
```

codex 内部映射为 `TokenUsage{input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens, total_tokens}`；官方 SDK `ResponseUsage` 类型与 codex 解析结构一致（details 两个子对象在 SDK 中必填、codex 按 Option 容错——**Agent 解析也应按可缺省处理**，部分中转站可能不发 details）。Responses 格式**没有 cache_write（缓存写）分项**，OpenAI 缓存不单独计写入价，`usage_records.cache_write_tokens` 对 openai 协议恒为 0。`response.failed` 时 `usage` 可为 null。

**chat.completions 流式（仅通用参考，Codex 已不用）** — 请求带 `stream_options: {"include_usage": true}` 时，`data: [DONE]` 前会多一个 `choices: []` 的 chunk，其 `usage` 为全量统计：`prompt_tokens`、`completion_tokens`、`total_tokens`、`prompt_tokens_details.cached_tokens`、`completion_tokens_details.reasoning_tokens`。若客户端未带该参数，流式响应**不含 usage**（Agent 无法旁路补采，除非改写请求体注入该参数——超出 MVP"仅改 model 字段"的边界，且对 Codex 场景不需要）。

### C6. switchAPI 接管 Codex 的推荐写法（置信度：High，与 cc-switch 生产模式同构）

```toml
# ~/.codex/config.toml（安装时写一次）
model_provider = "switchapi"

[model_providers.switchapi]
name = "switchAPI"
base_url = "http://127.0.0.1:9527/openai/v1"
wire_api = "responses"
```

```json
// ~/.codex/auth.json（安装时写一次；本地 token 即设计中的"本地鉴权链"载体）
{ "OPENAI_API_KEY": "<switchapi 本地 token>" }
```

Codex 对每个请求发 `Authorization: Bearer <本地 token>` → Agent 校验、剥离、注入当前上游真实 key。不用 `env_key`（依赖用户 shell 环境，服务化场景不可靠）；不用 `experimental_bearer_token`（官方标记 experimental 且明文进 config.toml）。备选增强：再加 `http_headers = { "X-SwitchAPI-Token" = "..." }` 作为第二校验因子（不会被鉴权头覆盖）。注意：接管会顶掉用户原有 auth.json（若用户此前用 ChatGPT 订阅登录，需备份提示——cc-switch 同样有此问题）。

---

## 证据与来源

### 本机 ground truth（READ ONLY，密钥已脱敏）

1. `codex --version` → **codex-cli 0.142.5**；`codex --help` 确认 `-c key=value` 覆盖机制与 `codex doctor`（可用于安装后自检）。
2. `~/.codex/config.toml`：活跃配置即"自定义供应商指向本机代理"的实例——`model_provider = "anyrouter"`，`[model_providers.anyrouter] base_url = "http://127.0.0.1:23000/v1"`, `wire_api = "responses"`，**无 env_key**；含遗留键 `preferred_auth_method`（现版本已无此代码）。
3. `~/.codex/auth.json`：`{"OPENAI_API_KEY": "<REDACTED...c988>"}`——API-key 模式最小结构。
4. `~/.cc-switch/cc-switch.db`（SQLite `providers` 表，app_type='codex' 十余条）：cc-switch 的 codex 供应商统一模式 = `settings_config.auth.OPENAI_API_KEY`（站点真实 key，脱敏如 sk-9ac…c988）+ `settings_config.config`（TOML 文本：`model_provider` + `[model_providers.<id>]` name/base_url/`wire_api="responses"`，个别站点加 `requires_openai_auth = true`）；且其"代理接管"模式（proxy_config 表 listen_port=23000）与 switchAPI Agent 架构同构。此表同时是研究 #5 导入映射的直接输入。

### openai/codex 源码（tag `rust-v0.142.5`，与本机版本一致；tarball 校验于 2026-07-03）

5. `codex-rs/model-provider-info/src/lib.rs`：L54-83 `WireApi` 枚举仅 `Responses`，L49 `CHAT_WIRE_API_REMOVED_ERROR`（deserialize "chat" 即报错）；L86-140 `ModelProviderInfo` 全字段及默认值；L131-136 `requires_openai_auth` 语义注释；L281-297 `api_key()`（env_key 非空校验）；L417-482 内置四供应商与 `merge_configured_model_providers`（L477 `or_insert` → 内置 id 不可覆盖）；L26-33 重试/超时默认值。
6. `codex-rs/model-provider/src/auth.rs`：L78-110 `resolve_provider_auth` 鉴权链（env_key → bearer → CodexAuth → 无鉴权）；L52-63 无鉴权 Provider 注释明确"custom test providers with requires_openai_auth = false"可零鉴权头。
7. `codex-rs/model-provider/src/provider.rs`：L210-243 ConfiguredModelProvider 无条件持有全局 AuthManager；L284-306 models_manager 对任意配置供应商构造 `/models` 端点；测试 L669-706 wiremock 证明对自定义 base_url 发 `GET /models` 且带 Bearer。
8. `codex-rs/model-provider/src/models_endpoint.rs`：`MODELS_ENDPOINT = "/models"`，5s 超时。
9. `codex-rs/models-manager/src/manager.rs`：L196 "backed by bundled models, cache, and /models"；L277-283 刷新失败仅 `error!` 日志、继续用缓存/内置目录（非致命）。
10. `codex-rs/codex-api/src/provider.rs`：L53-75 `url_for_path`（base_url 去尾斜杠拼 path + query_params）。
11. `codex-rs/codex-api/src/endpoint/responses.rs`：L100-102 `path() = "responses"`；L147-152 `Accept: text/event-stream`。`endpoint/compact.rs` L36 `responses/compact`；`core/src/compact.rs` L70 + `model_provider_info.rs` L402-404：远端压缩仅 OpenAI/Azure。
12. `codex-rs/codex-api/src/sse/responses.rs`：L114-156 `ResponseCompleted{usage}` 结构与 `From<ResponseCompletedUsage> for TokenUsage` 映射（`input_tokens_details.cached_tokens`→cached、`output_tokens_details.reasoning_tokens`→reasoning，两个 details 均 Option）；L319-436 全部 SSE 事件分支；L494 "stream closed before response.completed"。
13. `codex-rs/protocol/src/protocol.rs` L2022-2033 `TokenUsage` 五字段。
14. `codex-rs/login/src/auth/storage.rs` L38-61 `AuthDotJson`；L150-151 路径 `$CODEX_HOME/auth.json`；`login/src/token_data.rs` L10-25 `TokenData`；`login/src/auth/manager.rs` L778-780 三个环境变量常量、L848+ `login_with_api_key`、L1979-1992 `auth()` 缓存语义、L2030 `reload()`。
15. `codex-rs/core/src/config/mod.rs` L3386-3408：`openai_base_url` 键、供应商合并、`model_provider_id` 选择链与未知 id 报错；`config/src/profile_toml.rs` L24-45 profile 可设 `model_provider`；`config/src/config_toml.rs` L313-318 `profile`/`profiles`、L382 `openai_base_url`；`config/src/loader/mod.rs` L62-74 项目层 denylist（含 model_providers）。
16. `codex-rs/tui/src/lib.rs` L1060/L1115：启动加载 + 仅 personality 迁移触发的一次性重载；workspace 无 config.toml 文件监听（notify 仅用于 `file-watcher` crate → app-server fs/skills 监听）。
17. `codex-rs/core/src/client.rs` L831：`store: provider.is_azure_responses_endpoint()`。

### 官方文档 / 讨论区 / 官方 SDK（外部交叉验证）

18. [OpenAI, openai/codex docs/config.md @rust-v0.50.0, https://raw.githubusercontent.com/openai/codex/rust-v0.50.0/docs/config.md] — `model_providers` 完整官方文档：字段示例（name/base_url/env_key/wire_api/query_params/http_headers/env_http_headers/三个网络参数及默认值 4/5/300000）、"Built-in providers are not overwritten when you reuse their key"、Azure `query_params = { api-version = ... }` 示例。（注：现行 docs/config.md 已改为指向 developers.openai.com 的 stub，该站对本环境 IP 返回 403，故取仓库内历史全量文档 + 同版本源码双源验证。）
19. [etraut-openai (OpenAI Codex maintainer), 2026-01, https://github.com/openai/codex/discussions/7782] — "Deprecating chat/completions support in Codex"：弃用期打警告，"Full removal is slated for early February 2026"，维护者回复确认 Feb 1 后移除；迁移指引 `wire_api = "responses"`。与 0.142.5 源码硬报错一致。
20. [OpenAI, openai-python (官方 SDK，Stainless 从 OpenAPI spec 生成), https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_usage.py] — `ResponseUsage`: input_tokens / input_tokens_details.cached_tokens / output_tokens / output_tokens_details.reasoning_tokens / total_tokens。
21. [OpenAI, openai-python, .../types/completion_usage.py 与 .../types/chat/chat_completion_stream_options_param.py] — `CompletionUsage`（prompt_tokens/completion_tokens/total_tokens + prompt_tokens_details.cached_tokens + completion_tokens_details.reasoning_tokens）；`include_usage`: "an additional chunk will be streamed before the `data: [DONE]` message… `choices` will always be an empty array"，且"流被中断则可能收不到最终 usage chunk"。

---

## 对 design.md 的影响（标注 [研究#2] 条目核对）

| design.md 假设 | 判定 | 说明 |
|---|---|---|
| §4 "Codex `model_providers.switchapi.base_url=http://127.0.0.1:9527/openai/v1`" | **confirmed** | 写法正确且与 cc-switch 生产模式同构；`/v1` 后缀无硬性要求（拼接是 base+path），保留它可与生态惯例一致 |
| §4 "`/openai/*` → 当前 openai Provider" 直通 | **confirmed（范围可收窄）** | 对 Codex 只需 `POST …/responses`（SSE）+ `GET …/models`（非致命）；`/chat/completions` 对 Codex 已无意义（wire_api="chat" 已移除）。按前缀通配直通天然覆盖，无需专门实现 chat 语义 |
| §4 鉴权链"校验本地 token → 剥离 → 注入上游 key" | **confirmed，落点明确** | Codex 侧本地 token 的载体 = `auth.json.OPENAI_API_KEY`（Codex 自动作为 Bearer 发出）；不要用 env_key |
| §6/§2 usage 四分项（含 cache_write） | **needs change（openai 协议）** | OpenAI Responses 无 cache_write 分项：`usage_records.cache_write_tokens` 对 codex/openai 协议恒 0，计价引擎与 UI 需按协议区分四分项 vs 三分项；`input_tokens` 是含 cached 的总量（计费拆分 = cached 部分按 cache_read 价、其余按 input 价） |
| §4 用量采集"OpenAI Responses: response.completed" | **confirmed** | 字段映射见 C5；Agent 解析需把 `*_details` 当可缺省 |
| Agent 转发行为约束（隐含） | **补充** | ① 必须逐事件 flush，流必须完整送达 `response.completed`，否则 codex 判流断并重试（默认 5 次）→ 重试会造成用量重复计费风险，Agent 上报侧要以上游实际发生的请求为准；② 流空闲 5 分钟超时 → Agent 不得吞掉上游心跳；③ 不要在预设模板里开 `supports_websockets` |
| 安装接管流程（隐含） | **补充** | 写 config.toml + auth.json 各一次；config 变更仅下次进程生效 → 安装器需提示"重启正在运行的 codex 会话"；接管前必须备份原 auth.json（可能含用户 ChatGPT OAuth tokens）与 config.toml（cc-switch 亦留 .bak，本机可见 6 个备份文件）；卸载/停用需可回滚 |

## 遗留不确定性

1. **（Low risk）运行中会话对 config.toml 的绝对不感知**：结论来自 0.142.5 代码路径排查（无 watcher、仅启动加载），未做黑盒实测；实现 M1 时用"启动 TUI → 改 base_url → 观察下一轮请求仍走旧地址"5 分钟验证。置信度 High，剩余风险来自未来版本引入热重载。
2. **（Low）`GET /models` 直通的响应格式**：codex 期望其自有 `ModelsResponse`（含 slug/display_name 等扩展字段），第三方中转站多半返回标准 OpenAI `/v1/models` 或 404——两者都会触发"解析/请求失败 → 回落内置目录"，不影响可用性；Agent 只需原样转发。未逐一验证各中转站行为。
3. **（Low）codex 对上游非 2xx 的具体故障语义**（429/5xx 重试节奏、错误 message 透出）只粗读了 RetryConfig（retry_429=false, retry_5xx=true, retry_transport=true, base_delay 200ms）；研究 #8（健康判定阈值）需要更细的失败分类时再深挖。
4. **（Medium）官方现行文档站不可达**：developers.openai.com/codex/config-* 对当前网络 403，无法核对"现行文档措辞"；已用同版本源码（最强证据）+ 仓库历史全量文档 + 官方讨论区三源替代。若后续可访问建议补一次核对。
5. **（Low）auth.json 并发写**：Agent/安装器改写 auth.json 与 codex 自身 `reload()` 的竞争窗口未测；switchAPI 方案下 auth.json 内容恒定（本地 token），实际不构成运行期问题。
