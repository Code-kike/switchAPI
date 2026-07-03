# switchAPI — 技术设计（design.md）

> 前置阅读：`prd.md`（范围与决策总表）、`CONTEXT.md`（术语）、`docs/adr/0001-0005`。
> 本版已吸收 M0 全部 8 项研究结论（`research/01`–`08`，2026-07-03/04 完成，含真实中转站实测）。
> 标注 `(研究#N)` 的条目在对应研究文档中有实证与来源；实现时以研究文档为细则。

## 1. 仓库布局（monorepo）

```
switchAPI/
├── go.mod                  # 单一 Go module：github.com/Code-kike/switchAPI（go1.25+，本机 ~/sdk/go1.26.4）
├── cmd/
│   ├── hub/main.go
│   └── agent/main.go
├── internal/
│   ├── hub/                # api、realtime、store、pricing、backup、importer、auth
│   ├── agent/              # proxy、forward、usagebuf、pairing、appconfig（写 CC/Codex 配置）
│   └── shared/             # 协议消息类型、加密工具、版本
├── web/                    # React + TS + Vite + Tailwind + shadcn/ui（M3 搭建，构建产物 embed 进 hub）
├── desktop/                # Tauri 2 壳（M3 搭建）
└── docs/adr/
```

## 2. 数据模型（Hub 权威，SQLite）

- `providers`：id、name、protocol(`anthropic`|`openai`)、base_url、api_key_enc（AES-GCM）、
  model_redirects(JSON 映射)、cost_coefficient(默认 1.0)、preset_id、sort、备注。
  同一中转站的两种协议端点 = 两条 Provider 记录（protocol 即 App 归属）。
  **base_url 存储惯例**（研究#2/#5，与用户既有配置及 cc-switch 导入零摩擦）：anthropic 协议**不含** `/v1`
  （如 `https://anyrouter.top`）；openai 协议**含** `/v1`（如 `https://relay.example/v1`）。预设模板按此预填。
- `app_state`：app 主键（`claude-code`|`codex`）、active_provider_id、updated_at、updated_by。
  全局切换语义的落点；预留 device_id 列（NULL=全局，二期设备覆盖用）。
- `fallback_orders`：app、provider_id、position——备选序列。
- `provider_health`（研究#8，Hub 侧仲裁状态）：provider_id、demote_count(n)、cooldown_until、
  needs_attention(401/403 配置类故障标记)、last_probe_at、consecutive_probe_ok。
- `devices`：id、name、platform、token_hash、paired_at、last_seen、revoked。
- `usage_records`：ts、device_id、app、provider_id、model、model_redirected、
  input/output/cache_write/cache_read tokens、duration_ms、status、error_kind、
  **usage_source(`wire`|`estimated`|`none`)**（研究#3 中断矩阵）、request_id(UNIQUE，去重键)。
  **无任何消息内容字段**（第 16 题决策）。四分项统一语义：input=非缓存输入；cache_read=缓存命中；
  cache_write=缓存写入（仅 anthropic 有）；output=全部输出（含 reasoning）。
- `pricing_base`：model、四分项单价（**缓存两列可 NULL**——OpenAI 系无 cache_write 价，NULL 按 0 计）、
  litellm_provider、mode、tiered_prices(JSON，>200k 分层/1h 缓存变体，先记录二期结算)、source、synced_at。（研究#4）
- `pricing_overrides`：provider_id、model、四分项单价。
- `events`：ts、kind(`switch`|`failover`|`pairing`|`backup`|`speedtest`|`probe`…)、payload JSON——事件时间线。
- `settings`：key/value（管理员密码 argon2id 哈希、备份策略、价格同步开关、**pricing_etag**、健康阈值覆盖等）。

## 3. Hub 接口面

REST（`/api/v1`，Session Cookie 鉴权，argon2id 密码）：
- `POST /auth/login`、`POST /auth/logout`。**Session Cookie 必须设 Max-Age**（会话 cookie 在 Tauri WebView
  三平台都不跨重启存活）**且不设 Secure**（WebKit 拒绝 http 下的 Secure cookie；ADR-0005 内网明文 http）。（研究#7）
- `GET|POST|PUT|DELETE /providers`、`GET /presets`（预设模板勿开 supports_websockets 类旋钮，研究#2）
- `POST /switch`（{app, provider_id}——写 app_state + 事件 + 广播）
- `GET|PUT /fallback-order/{app}`
- `POST /devices/pairing-code`（一次性码，TTL 10min）、`GET /devices`、`DELETE /devices/{id}`（吊销）
- `GET /usage`（分页明细/筛选）、`GET /stats/summary|trend|breakdown`
- `POST /speedtest`（广播指令，各 Agent 自测上报，结果按设备展示）
- `POST /backup/run`、`GET /backups`、`POST /export`（口令加密 JSON）、`POST /import`（含 cc-switch 模式）
- `GET /healthz`（无鉴权，仅存活；亦供桌面壳应急页探测）

实时通道（均为 WebSocket）：
- `/api/v1/ws/agent`（设备 token 鉴权）：Agent hello（版本/平台）→ Hub 全量 config_push；
  此后增量 config_push、speedtest_cmd、**probe_cmd**（恢复探测指令）下行；
  usage_batch、**health_report**（结构：失败计数器快照 + ≤5 条错误样本[种类/时刻/HTTP 码]）、
  speedtest_result、**probe_result** 上行；心跳。（研究#8）
- `/api/v1/ws/ui`（Session 鉴权）：state_changed、event、usage_tick 下行——双端 UI 实时同步的载体。

## 4. Agent 设计

**路由与路径拼接**（研究#1/#2 实证）——监听 `127.0.0.1:9527`（可配）：

| App 侧固定前缀 | 剥离规则 | 上游 URL |
|---|---|---|
| `/anthropic/<rest>` | 剥 `/anthropic` | anthropic 供应商 `base_url + /<rest>`（rest 自带 `v1/messages` 等） |
| `/openai/v1/<rest>` | 剥 `/openai/v1` | openai 供应商 `base_url + /<rest>`（base 自带 `/v1`，rest 为 `responses`/`models`） |

- CC 接管：`~/.claude/settings.json` env 块写 `ANTHROPIC_BASE_URL=http://127.0.0.1:9527/anthropic` +
  `ANTHROPIC_AUTH_TOKEN=<本地 token>`；**官方实锤热生效**（settings 文件被 watch，重启例外仅
  model/outputStyle）→ 接管瞬间运行中的 CC 会话即改走 Agent。（研究#1）
- Codex 接管：`~/.codex/config.toml` 写 `model_provider="switchapi"` + `[model_providers.switchapi]
  base_url="http://127.0.0.1:9527/openai/v1"`、`wire_api="responses"`（chat wire 已被 Codex 移除）；
  本地 token 写 `auth.json.OPENAI_API_KEY`（Codex 自动作为 Bearer 发出；不用 env_key——服务化不可靠）。
  Codex 启动时读配置，**需提示重启运行中会话**。openai 通道只需转发 `POST …/responses` + `GET …/models`
  （/models 失败非致命）。（研究#2）
- **appconfig 写入规格**（研究#1/#2）：手术式最小合并（CC 只动 env 两键；Codex 写 config.toml 供应商块 +
  auth.json）→ 写前时间戳备份（auth.json 可能含用户 ChatGPT OAuth tokens）→ temp+rename 原子替换 →
  可回滚卸载。安装时冲突检查清单：apiKeyHelper、CLAUDE_CODE_USE_BEDROCK/VERTEX、shell 层 ANTHROPIC_*
  export、HTTP(S)_PROXY 需 NO_PROXY 含 127.0.0.1（细则见 research/01 C8）。
- **鉴权链**：校验本地 token——同时接受 `Authorization: Bearer` 与 `x-api-key` 两种头（CC 按 env 变量二选一，
  apiKeyHelper 双头齐发）→ 剥离 → 按协议注入上游（anthropic **双头齐发**——与 CC 处理 apiKeyHelper 值
  的官方行为一致，兼容面最大；openai 仅 Bearer）。（研究#1 C2/C8；M1 已实现）
- **转发器**（研究#6，原型 research/06-sse-prototype 已对真实中转站实测通过）：
  `httputil.ReverseProxy` + **Rewrite 钩子**（Director 已废弃）+ `FlushInterval:-1`；
  Transport = DefaultTransport.Clone() + `DisableCompression=true` + **上游强制 `Accept-Encoding: identity`**
  （保证 usage tee 永不见压缩字节）+ `MaxIdleConnsPerHost≈32`；
  **超时旋钮按流式条件化**（调和研究#6/#8）：connect 10s、TLS 10s；stream 请求 ResponseHeaderTimeout 60s、
  流空闲 120s；非流式 ResponseHeaderTimeout=0、总时限 300s；Agent 自身 http.Server `WriteTimeout=0`。
  模型重定向：仅顶层 `model` 字段字节级拼接改写（json.Decoder Token+InputOffset），修 ContentLength、
  TransferEncoding=nil、GetBody；Content-Encoding/非 JSON/超 32MB 直通不改；无匹配零拷贝。
- **用量采集**（研究#3 映射表为准）：tee 响应流 O(1) 解析。anthropic：message_start + message_delta，
  **usage 为累计值、同名字段覆盖合并**（禁累加）；openai Responses：response.completed（兜底
  incomplete/failed 尽力读取，usage 可 null 须防御）；两协议**均须支持非流式 JSON 解析**；
  OpenAI `input_tokens` 含 cached_tokens——**四分项换算必须减法拆分**（cached 按 cache_read 计），
  cache_write 恒 0；openai 大响应的 response.completed 单行可超 1MB——tee 对该事件按事件名感知放宽行缓冲上限。
  中断场景按研究#3 C7 矩阵记录 + usage_source 标注。**非计费路径不产生记录**：count_tokens、GET /models、
  OPTIONS 等透传但不入 usage_records。写本地 SQLite 缓冲队列 → 批量 usage_batch 上报，
  at-least-once + request_id 幂等去重；断连期间只积压不丢弃（Codex 断流重试自带新请求，以上游实际请求为准）。
- 本地状态：`~/.switchapi/agent.db`（缓冲队列）+ `agent-state.json`（0600 权限：设备 token、
  最近 config_push 快照含上游 key——内网信任模型，与 ADR-0005 一致）。
- **健康判定**（研究#8 参数表为准）：**连续硬失败计数（默认 3）+ 300 秒新鲜度窗口**（单用户流量下
  比率型指标不可用）。失败四级分类：硬失败（DNS/拒连/TLS/超时/5xx/529/上游中断/假 200）计数；
  429 软失败（连续 6 次且跨 ≥60s 才升级）；401/403 配置类（3 连即上报 + needs_attention，不自动恢复）；
  不计数（4xx 业务错、客户端主动断开）。越限 → health_report 上报，由 Hub 裁决；
  与 Hub 断连时按缓存备选序列本地临时降级，重连后对齐全局状态。
- 系统服务：kardianos/service。Linux systemd **user unit**（UserService=true，提示 loginctl enable-linger）、
  macOS LaunchAgent（RunAtLoad=true，免管理员）、Windows SCM 需**一次性 UAC 提权**（runas 或安装器钩子；
  服务跑 LocalSystem → 状态目录显式指定，CC/Codex 配置写入必须在用户上下文执行）。（研究#7/#8）
  `agent install|start|stop|pair` CLI。

## 5. 关键流程

**手动切换**：UI → `POST /switch` → Hub 写库+事件 → ws/agent 广播 config_push →
各 Agent 原子替换内存路由（进行中请求不中断，下一请求走新上游）→ ws/ui 广播 state_changed。

**故障切换**（研究#8 仲裁模型）：Agent 连续硬失败达阈值 → health_report → Hub **负证据否决仲裁**
（5 秒防抖收集并发上报；单设备达阈值即可触发，**除非**另一设备 30 秒内在同供应商有新鲜成功记录——
判为设备本地网络问题，否决切换）→ 沿备选序列取下一健康供应商（跳过 cooldown 中的；
被降级供应商冷却 300s×2^(n-1) 封顶 3600s）→ 执行常规全局切换 → events + 双端通知。
护栏：**无健康候选 → 只通知不切换**；同一 App 故障切换限速 10s；**不自动切回**（恢复后通知 + 一键切回）。
**恢复探测**：Hub 轮转指定一个在线 Agent 发 probe_cmd（非流式最小补全 max_tokens=1，10s 超时；
60s 间隔 ×2 退避封顶 900s ±20% 抖动）；连续 2 次成功 = 恢复 → 通知。流量不经 Hub，符合 ADR-0001。

**配对**：UI 生成一次性码 → 用户在设备上 `agent pair --hub <url> --code <code>`（或桌面壳引导）→
Hub 换发长期设备 token → Agent 落盘并建立 WS。

## 6. 计价引擎（研究#4 实证）

`price(model, provider) = pricing_overrides[provider, model] ?? pricing_base[model] × cost_coefficient(provider)`，
四分项独立结算；OpenAI 系缓存列 NULL 按 0。
LiteLLM 价格表：构建期打包快照 + 运行期每日拉取（**ETag/If-None-Match 条件请求**，无 Last-Modified；可关）；
入库过滤 `mode IN ('chat','responses')`（Codex 系模型 mode=responses）；**upsert-only 永不删除**
（上游会移除退役模型）；跳过 sample_spec 键。模型名匹配四步：精确 → 去尾部 `-YYYYMMDD` →
斜杠取尾小写 → 未知模型记 token 不记费并在 UI 标注（实证存在：中转返回 `ZhipuAI/GLM-5.2` 无表键）。
MIT 许可，快照随二进制分发合规（保留 notice）。

## 7. 备份 / 导出 / 导入

- 快照：`VACUUM INTO` 每日一次 + 结构性变更后防抖 5 分钟触发；本地轮转保留 10 份。
- 导出：schema 版本化 JSON（供应商/备选序列/设置；可选含用量明细）；含密钥时强制
  scrypt 口令派生 + AES-256-GCM 整体加密；明文导出需 UI 二次确认。
- 导入：本产品导出文件；**cc-switch 模式**（研究#5 映射表为准）：读 `~/.cc-switch/cc-switch.db`
  （SQLite，v3.8.0+ 唯一现行格式，user_version≤11 按列名容错取值；先读 app_paths.json override；
  **mode=ro 只读打开**——daemon 可能持锁）；`config.json` 仅作 ≤3.7.1 legacy v2 兜底，v1 拒绝。
  映射：is_current→app_state、in_failover_queue 按 sort_index→fallback_orders、
  meta.costMultiplier→cost_coefficient。**跳过并逐条报告原因**（E1-E9）：apiFormat 转换类
  （openai_chat/responses，违反 ADR-0002）、OAuth 托管类（codex_oauth/github_copilot）、
  127.0.0.1 回环 base_url、空 key 占位。密钥源为明文，导入即 AES-GCM 加密落库。

## 8. 桌面壳（Tauri 2）（研究#7 实证）

- WebView 装载**远程 Hub URL**：`WebviewUrl::External` + 运行期 `CapabilityBuilder.remote(hub_url)`
  注入能力（Hub 地址装机时未知）；首启向导配置 Hub 地址 + 登录。
- **Agent 分发**：externalBin 仅作**分发载体**（随包携带），**不作为常驻 sidecar 运行**
  （shell 插件退出杀子进程、AppImage 只读随机挂载、Windows 服务锁 exe 阻断更新）——
  首启复制到稳定路径（`~/.switchapi/bin`）→ 一次性 `agent install`（kardianos 注册系统服务）。
  **桌面壳每次自更新后执行 agent 版本同步**（比对→停→复制→启）。
- 托盘（TrayIcon + 菜单：当前供应商 + 快切 + Agent 状态；Linux 无左键菜单）、autostart 插件、
  updater 插件（minisign 签名 + createUpdaterArtifacts + GitHub latest.json）、single-instance 插件。
- **应急页**：wry 无页面加载失败事件 → 本地 bootstrap 页 + Rust 侧 `/healthz` 探测 + `webview.navigate(hub)`；
  Hub 不可达时经 Tauri command 直连本机 Agent 读缓存状态与本地临时降级开关（避开 CORS）。

## 9. 交付物

- `switchapi-hub`：单二进制（embed 前端）+ Docker 镜像（挂载 `/data` 存 SQLite 与备份）。
- `switchapi-agent`：各平台单二进制 + install 脚本；亦随桌面壳分发（见 §8）。
- 桌面安装包：Windows **NSIS per-user**（配一次性提权钩子）、macOS dmg、Linux 优先 **AppImage**（自更新最顺）。

## 10. 测试策略

计价引擎与导入映射单测（cc-switch fixture 直接采用本机四类样本形态）；转发器集成测试
（httptest 伪上游回放 SSE，断言透传字节一致与 usage 解析——直接演化 research/06-sse-prototype 的
4 个测试；补 TLS/h2 上游用例；覆盖研究#3 C7 中断矩阵逐行断言 + anthropic 多 delta/delta 带输入侧字段 +
responses usage 缺失/incomplete/failed）；Hub API 层 handler 测试；e2e 冒烟：docker-compose 起
hub + fake-upstream + agent，走通 配对→切换→请求→统计→failover 全链路。
