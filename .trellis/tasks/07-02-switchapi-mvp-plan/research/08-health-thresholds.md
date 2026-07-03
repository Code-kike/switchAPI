# Research #08: 健康判定默认阈值与防抖（failover 参数）

- **Query**: 健康判定默认阈值（连续失败次数、超时窗口、恢复探测）与误判防抖；调研 Envoy / LiteLLM / nginx / resilience4j / claude-code-hub 真实参数后给出 switchAPI 默认参数表
- **Scope**: external（官方文档 + 源码）+ 架构推导
- **Date**: 2026-07-03

---

## 结论

### 1. 先行系统参数总览（全部经官方文档或源码核实，置信度 High）

| 系统 | 失败判定 | 阈值 | 窗口/间隔 | 隔离时长 | 恢复机制 |
|---|---|---|---|---|---|
| **Envoy outlier detection** | 5xx + 本地错误（超时/TCP reset/连接失败，默认同桶） | `consecutive_5xx=5` | 分析扫描 `interval=10s` | `base_ejection_time=30s × 连续弹出次数`，上限 `max_ejection_time=300s`；`max_ejection_percent=10%` | 时间到自动放回；主动健康检查成功可提前解除并清零计数 |
| **nginx upstream** | `proxy_next_upstream` 定义，默认仅 `error timeout`（连接/传递/读响应头出错或超时）；`http_5xx/429` 需显式开启 | `max_fails=1`（0=禁用） | `fail_timeout=10s`（双重语义：计数窗口=不可用时长） | 10s | 时间到直接重新参与选择（无半开） |
| **LiteLLM Router** | 429 立即；分钟失败率 >50%（`DEFAULT_FAILURE_THRESHOLD_PERCENT=0.5`）且样本 ≥5；不可重试错误 401/404/408 | 传统模式 `allowed_fails=3`/分钟 | 按自然分钟统计 | `cooldown_time=5s`（`DEFAULT_COOLDOWN_TIME_SECONDS=5`） | 冷却到期自动放回并重置计数；**单部署组默认豁免冷却**（除非全失败且流量 ≥1000 req/min） |
| **resilience4j**（通用熔断器） | 异常谓词（默认所有异常） | `failureRateThreshold=50%`，`minimumNumberOfCalls=100` | `slidingWindowSize=100`（计数型） | OPEN 等待 `waitDurationInOpenState=60s` | HALF_OPEN 放行 `permittedNumberOfCallsInHalfOpenState=10` 次试探，按失败率决定回 CLOSED/OPEN |
| **claude-code-hub**（同领域直接对标） | HTTP≥400（**404 除外**）、fake-200（200 但 SSE 体含错误）、中途断流 `STREAM_ABORTED`（客户端主动中断除外）；网络错误默认**不计入**（`ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS=false`） | `failureThreshold=5`（连续，成功即清零；0=禁用） | 无时间窗（纯连续计数） | `openDuration=30min` | 被动：到期转 half-open，**连续 2 次成功**（`halfOpenSuccessThreshold=2`）关闸，half-open 失败重新开闸 30min；主动：智能探测默认关，开启后每 10s 对 OPEN 供应商发真实小补全（超时 5s），成功提前转 half-open |

关键横向观察（对 switchAPI 直接适用）：

1. **低流量场景只能用"连续失败计数"，不能用失败率窗口**（High）。resilience4j 要求 ≥100 样本、LiteLLM 失败率路径要求 ≥5 样本/分钟、Envoy success_rate 要求 100 请求/interval——单用户编码流量远达不到；Envoy `consecutive_5xx`、nginx `max_fails`、claude-code-hub `failureThreshold` 三家均以连续计数为低流量主判据。
2. **429 在各家分歧明显**（High）：nginx 默认不算失败、Envoy 不算（非 5xx）、LiteLLM 立即冷却（因为它有同组多部署可分流）、claude-code-hub 计入并重试所有 4xx。含义：429 是否算失败取决于"切走的代价"——切换代价越高越应视为退避信号而非故障。
3. **404/资源类 4xx 一律不算供应商故障**（High）：claude-code-hub 源码注释明确"避免把资源/模型不存在当作供应商故障"；LiteLLM 对普通 4xx 不冷却。
4. **客户端主动中断不计失败、上游中途断流计失败**（High）：claude-code-hub 对 `!streamEndedNormally && !clientAborted` 记 `STREAM_ABORTED` 失败。
5. **隔离时长指数递增是防振荡标准手法**（High）：Envoy `base × 连续弹出次数` 封顶 300s；claude-code-hub 用固定 30min（对个人工具过于迟钝）。
6. **恢复判定用"少量连续成功"而非单次**（High）：claude-code-hub 半开需 2 次成功；resilience4j 默认 10 次（有成本压力时取小值）。
7. **无备选时不隔离**（High）：LiteLLM 单部署组默认豁免冷却（可用性优先于保护）——对应 switchAPI"备选序列无健康候选则不切换只通知"。
8. **同领域探测用真实小补全而非 HEAD/models**（High）：claude-code-hub 探测体为真实流式补全（haiku 类小模型，`max_tokens=20`，"ping"→期望 "pong"），因为 `/v1/models` 返回 200 无法证明补全链路（鉴权、配额、模型路由）可用。

### 2. switchAPI 推荐默认参数表（可直接进 design.md）

失败分类（Agent 侧，随每个真实请求记录）：

| 类别 | 具体信号 | 处理 |
|---|---|---|
| **硬失败**（计入连续失败计数） | DNS 解析失败、connect refused、TLS 握手错误、连接超时、首字节超时、流中静默超时、HTTP 500/502/503/504、529（Anthropic overloaded）、上游中途断流（非客户端中断）、fake-200（2xx 但流内以 error 事件终止） | `hard_fail_count++`；成功请求清零 |
| **软失败**（独立计数，退避信号） | HTTP 429 | 不进硬计数；连续 429 达到独立阈值才升级为不健康 |
| **配置类失败**（独立通道） | HTTP 401/403 | 不自愈：连续 3 次 → 触发 failover + 供应商标记 `needs_attention`（UI 提示查 key），探测成功或用户编辑后解除 |
| **不计数** | 400/404/413/422（请求/模型问题）、客户端主动中断、Agent 自身错误 | 只进用量明细 `error_kind`，供 UI 展示 |

参数表：

| # | 参数 | 默认值 | 依据 | 用户可配置 |
|---|---|---|---|---|
| 1 | `failure_threshold` 连续硬失败上报阈值 | **3** | LiteLLM `allowed_fails=3`；nginx=1 对全局切换的爆炸半径过敏感；Envoy/cch=5 意味着用户被阻塞 5 次请求，个人工具过迟钝 | 是 |
| 2 | `failure_freshness_window` 计数新鲜度 | **300s**（距上次失败超过则计数清零） | 纯连续计数无时限会把"昨天 1 次 + 今天 2 次"误累积；nginx fail_timeout=10s 对低流量过短 | 是（高级） |
| 3 | `connect_timeout` | **10s** | cch `FETCH_CONNECT_TIMEOUT` 默认 30s 偏长（用户在终端干等）；Envoy 生态惯用 5–10s | 是 |
| 4 | `first_byte_timeout`（流式 TTFB） | **60s** | cch 允许 1–180s（默认不限）；thinking 模型/慢中转 TTFB 可 >30s，取保守 60s | 是 |
| 5 | `stream_idle_timeout`（流中静默） | **120s** | cch 范围 60–600s；SSE ping 事件间隔通常 <60s，120s 静默基本可判死流 | 是 |
| 6 | `nonstream_total_timeout` | **300s** | cch 范围 60–1800s；CC/Codex 主流量为流式，非流式取中值 | 是 |
| 7 | `rate_limit_escalation` 429 升级规则 | 连续 **6** 次且首末间隔 ≥ **60s** → 视为不健康 | LiteLLM 立即冷却（多部署低代价）与 cch 计入（有 20 次切换预算）之间取高门槛：全局切换代价高，且 CC 自带 429 退避重试 | 是（可整体关闭） |
| 8 | `health_report` 上报模式 | 边沿触发：计数达阈值立即上报，携带最近 ≤5 条错误样本（kind/ts/status/latency）；恢复时上报 recovered | Envoy consecutive 判定即时生效（inline ejection）；证据随报告走，Hub 无需拉取 | 否 |
| 9 | Hub 防抖汇集窗口 | **5s**（收到首个 health_report 后等 5s 汇集其他设备并发报告再裁决） | 自定；把"同设备连续上报"改为一次性携带证据 + 短汇集，避免拖长用户阻塞时间 | 否 |
| 10 | Hub 多设备仲裁规则 | **反证否决制**：若其他设备在报告时刻前 **30s** 内经同一供应商有**成功**请求 → 否决 failover，改发"设备网络异常?"通知；否则**单设备达标即可切换** | 单用户场景通常仅 1 台设备活跃，"多设备一致"作为必要条件会让 failover 永远无法触发；活跃设备的新鲜成功记录是唯一可靠的反证 | 否 |
| 11 | `failover_rate_limit` | 每 App 两次自动 failover 间隔 ≥ **10s**；单次故障沿备选序列最多遍历一圈 | 防级联风暴；连续切换到下一候选不属于振荡（是级联故障），但需限速 | 否 |
| 12 | `provider_cooldown` 降级供应商冷却 | **300s × 2^(n-1)**，上限 **3600s**（n=连续被降级次数；期间不被自动选中） | Envoy `base_ejection_time × n` 上限 300s 的指数思想 + cch 固定 30min 的量级折中；探测有真金成本，不宜太短 | 是（基准值） |
| 13 | 无健康备选时 | **不切换**，保持当前供应商 + 双端通知"全部备选不可用" | LiteLLM 单部署豁免：可用性优先，切到已知坏候选无意义 | 否 |
| 14 | 恢复探测执行者 | **Hub 下发 probe_cmd → 指定单台在线 Agent 执行**（优先上报故障的设备，轮换） | 架构原则"AI 流量不绕经 Hub"（ADR-0001），Hub 网络位置与 Agent 不同，其探测结果不代表请求路径；单台执行避免 N 台设备并发烧钱 | 否 |
| 15 | `probe_interval` | **60s** 起，指数 ×2，上限 **900s**，±20% jitter | cch 默认 10s 但默认关闭（成本原因）；真实补全计费，60s 起步够快（用户已在备选上工作，不阻塞） | 是 |
| 16 | 探测请求形态 | **非流式最小补全**：`max_tokens=1`、单条 "ping" 消息、超时 10s、走该供应商当前生效模型（或供应商配置的探测模型） | cch 用真实流式补全 max_tokens=20；HEAD//v1/models 无法证明鉴权+配额+补全链路；max_tokens=1 成本可忽略 | 是（探测模型） |
| 17 | `recovery_threshold` 恢复判定 | 连续 **2** 次探测成功 → 标记 healthy、清冷却 | cch `halfOpenSuccessThreshold=2`；r4j 默认 10 次对计费探测过奢侈 | 是 |
| 18 | `auto_failback` 自动切回 | **默认关**：恢复后仅双端通知 + 一键切回按钮 | 防振荡最强手段；备选已可用时自动切回收益小、打断风险大；Envoy/nginx 的"自动放回池子"语义是负载均衡池，与我们"全局改指向"语义不同 | 是 |
| 19 | 中途断流计数 | 上游断流 = 1 次硬失败；**不做流中自动重试**（响应已部分交付客户端，SSE 无法透明重放） | cch `STREAM_ABORTED` 计入 + 客户端中断除外；与 ADR-0002 直通语义一致 | 否 |
| 20 | Agent 本地临时降级（Hub 断连时） | 沿用同一套阈值本地判定；本地切换间隔 ≥ **60s**；重连后对齐全局状态 | 断连时无仲裁者，用更长 dwell 补偿误判风险 | 否 |

（以上推荐值本身为设计判断，标注 Medium 置信度；其引用的先行系统数值均 High。）

---

## 证据与来源

### Envoy outlier detection（两个官方页面互证）

- `consecutive_5xx` 默认 5（0=禁用）；`interval` 默认 10s；`base_ejection_time` 默认 30s，实际弹出时长 = base × 连续弹出次数，封顶 `max_ejection_time`（默认 300s）；`max_ejection_percent` 默认 10%；`enforcing_consecutive_5xx` 默认 100%；`consecutive_gateway_failure`（502/503/504）默认 5 但 enforcing 默认 0%（仅记录）；success_rate 判定需 `minimum_hosts=5`、`request_volume=100`/interval、stdev 因子 1.9；`failure_percentage_threshold` 默认 85% 且 enforcing 默认 0%。[Envoy proxy docs (v1.39.0-dev), https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/cluster/v3/outlier_detection.proto]
- 错误分类：外部错误（上游返回 5xx）与本地错误（超时、TCP reset、无法连接）默认**同桶**计入 consecutive_5xx；弹出为被动健康检查，主动健康检查成功可解除弹出并清零计数；健康期间弹出倍数随 interval 递减。[Envoy proxy docs, https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/outlier]

### nginx upstream

- `max_fails` 默认 **1**（0=禁用计数），`fail_timeout` 默认 **10s** 且双重语义（失败计数窗口 = 不可用时长）。[nginx docs, https://nginx.org/en/docs/http/ngx_http_upstream_module.html]
- 何为"失败"由 `proxy_next_upstream` 决定，默认 **`error timeout`**（建连/传请求/读响应头出错或超时）；`http_500/502/503/504/429` 等需显式加入。[nginx docs, https://nginx.org/en/docs/http/ngx_http_proxy_module.html]

### LiteLLM Router（官方文档 + main 分支源码互证）

- 文档明示默认：`allowed_fails: 3`、`cooldown_time: 5s`；冷却触发条件表：429 立即、当分钟失败率 >50%、不可重试错误 401/404/408。[BerriAI LiteLLM docs, https://docs.litellm.ai/docs/routing "Cooldowns" 节]
- 源码常量：`DEFAULT_FAILURE_THRESHOLD_PERCENT=0.5`、`DEFAULT_ALLOWED_FAILS=3`、`DEFAULT_COOLDOWN_TIME_SECONDS=5`、`DEFAULT_FAILURE_THRESHOLD_MINIMUM_REQUESTS=5`（失败率路径最小样本）、`SINGLE_DEPLOYMENT_TRAFFIC_FAILURE_THRESHOLD=1000`。[BerriAI/litellm main, litellm/constants.py L26-80]
- 冷却决策源码（`_is_cooldown_required` / `_should_cooldown_deployment`）：4xx 中仅 429/401/408/404 触发冷却，其余 4xx 一律不冷却；**单部署组默认不冷却**（除非失败率 100% 且流量 ≥1000/min）。[BerriAI/litellm main, litellm/router_utils/cooldown_handlers.py L40-220]
- 注意：docs.litellm.ai/docs/proxy/reliability 页示例写 `cooldown_time: 30`，为示例值而非默认值（routing 页与源码一致确认默认 5s）。

### resilience4j（官方文档 + 源码互证）

- `failureRateThreshold=50%`、`slidingWindowSize=100`（计数型）、`minimumNumberOfCalls=100`、`waitDurationInOpenState=60s`、`permittedNumberOfCallsInHalfOpenState=10`、`slowCallRateThreshold=100%`、`slowCallDurationThreshold=60s`。[resilience4j docs, https://resilience4j.readme.io/docs/circuitbreaker]；[resilience4j/resilience4j master, CircuitBreakerConfig.java L43-52]

### ding113/claude-code-hub（main 分支源码，直接对标系统）

**确认其实现了完整三态熔断器**（closed/open/half-open，按供应商粒度，配置可按供应商覆盖）：

- 默认参数：`failureThreshold: 5`、`openDuration: 1_800_000ms`（30 分钟）、`halfOpenSuccessThreshold: 2`。[src/lib/redis/circuit-breaker-config.ts L23-26]
- 计数语义：closed 态失败 `failureCount++`，**任一成功清零** → 事实上是连续失败计数；达阈值 OPEN 30min；到期转 half-open，连续 2 成功关闸，half-open 失败重新 OPEN。[src/lib/circuit-breaker.ts recordFailure/recordSuccess/isCircuitOpen]
- 失败口径：HTTP ≥400 计入但 **404 显式排除**（源码注释："避免把资源/模型不存在当作供应商故障"）；fake-200（200 状态但 SSE/JSON 体含错误）计入；流未正常结束且非客户端中断记 `STREAM_ABORTED` 计入；**客户端中断不计**；**网络/系统错误默认不计入熔断**（`ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS` 默认 false，但仍标记该供应商本次请求内不再选中）。[src/app/v1/_lib/proxy/response-handler.ts L739-810、forwarder.ts L1928-1958、src/lib/config/env.schema.ts L140]
- 重试/切换：所有 4xx 均重试（含 401/403/429，源码注释明示）；每供应商默认尝试 2 次（`PROVIDER_DEFAULTS.MAX_RETRY_ATTEMPTS=2`，范围 1-10）；最多切换 20 个供应商（`MAX_PROVIDER_SWITCHES=20` 保险栓）；网络错误等 100ms 换端点重试。[forwarder.ts L184、L3596、provider.constants.ts L31]
- 恢复探测（"智能探测"）：默认**关闭**（`ENABLE_SMART_PROBING=false`）；开启后每 10s（`PROBE_INTERVAL_MS=10000`）对 OPEN 态供应商发探测，超时 5s（`PROBE_TIMEOUT_MS=5000`），成功则提前转 half-open。[src/lib/circuit-breaker-probe.ts L1-25]
- 探测请求形态：真实补全——Anthropic 协议为流式 `/v1/messages`、`max_tokens: 20`、消息 "ping, please reply 'pong'"、默认模型 claude-haiku-4-5；OpenAI 兼容为非流式 `/v1/chat/completions`、`max_tokens: 20`；成功判据为响应含 "pong"。[src/lib/provider-testing/utils/test-prompts.ts L28-135]
- 超时：TCP 连接 30s（`FETCH_CONNECT_TIMEOUT`）、响应头/体 600s；流式首字节/流中静默/非流式总超时默认 0（不限制），可配范围分别 1–180s / 60–600s / 60–1800s。[env.schema.ts L155-157、provider.constants.ts L46-66]

（本节全部为源码直读，单一来源即权威；仓库 tarball 于 2026-07-03 自 codeload.github.com 获取 main 分支。）

---

## 对 design.md 的影响

对照 design.md 中标注 `[研究#8]` 的两处假设：

1. **§4 Agent 健康判定"滑动窗口连续失败计数 + 超时率"** → **needs change（部分成立）**：
   - "连续失败计数"成立（三家先行系统同做法），但应补"新鲜度窗口 300s"（第 2 条参数）防陈旧累积；
   - "超时率"应**删除**：单用户流量下所有基于比率的判定（resilience4j 需 ≥100 样本、LiteLLM ≥5/分钟）都会失真或永不触发。超时直接归入硬失败参与连续计数即可；
   - 需补充失败分类四档（硬失败/软失败 429/配置类 401/不计数），及 fake-200 与中途断流的判定——design.md 目前完全未提。
2. **§5 Hub 校验"短窗口内要求同设备连续上报或多设备一致"** → **contradicted（按字面实现会失效）**：
   - "多设备一致"不能作为必要条件：单用户通常只有 1 台设备在产生流量，其余设备无证据可报，等待一致会让 failover 永远无法触发；
   - "同设备连续上报"拖长用户阻塞时间；
   - 应改为**反证否决制**（参数表第 9、10 条）：单设备达标即可切换，除非其他设备 30s 内有同供应商成功请求（此时判定为该设备本地网络问题，只通知不切换）；5s 防抖窗口仅用于汇集并发报告。
3. 需要在 design.md 落地的新增内容：
   - §2 数据模型：`providers` 增加健康参数覆盖列（或 `settings` 存全局默认）；Hub 内存/表需维护供应商冷却状态（cooldown_until、连续降级次数 n）与 `needs_attention` 标记；
   - §3 ws/agent 消息：`health_report` 需定义 payload（provider_id、计数、错误样本 ≤5 条）；新增 `probe_cmd` 下行 / `probe_result` 上行（或并入 speedtest 消息族但语义应分开——探测是自动闭环，测速是手动触发）；
   - §5 故障切换流程：补"无健康备选不切换只通知""每 App failover 限速 10s""恢复后默认不自动切回（通知 + 一键切回）"；
   - §4 Agent：转发器需实现四类超时（connect 10s / TTFB 60s / 流中静默 120s / 非流式总 300s）并把超时归类为硬失败。

总体判定：**confirmed-with-changes**（"Agent 上报 → Hub 裁决"框架与连续失败计数方向被先行系统证实；"超时率"与"多设备一致"两个具体假设需按上文修改）。

---

## 遗留不确定性

1. **Claude Code / Codex 客户端自身的重试行为**（属研究 #1/#2 范围）：CC 对 5xx/429 自带重试与退避，会影响 Agent 侧观测到的失败节奏（阈值 3 实际对应用户几次可见失败）——建议实现后实测校准。置信度：该影响存在为 High，具体节奏未测。
2. **Anthropic 529 (overloaded_error)** 归入硬失败基于其官方错误码文档记忆，本次未独立抓取核实（Medium）；实现时按"所有 >=500 状态码"处理即自然覆盖。
3. 推荐值第 7 条（429 连续 6 次 / 60s 升级）无先行系统直接对应（LiteLLM 立即冷却、cch 与 5xx 同桶），是基于全局切换代价的自行设计（Medium），建议做成可配置并在真实中转站上观察误判率。
4. claude-code-hub README 宣传"最多 3 次故障转移"与源码 `MAX_PROVIDER_SWITCHES=20` 不一致，取源码为准（README 可能滞后或指旧版本）；不影响本研究结论。
5. 探测成本：`max_tokens=1` 仍计输入 token（约十几 token/次），按上限频率（900s 间隔）连续宕机 24h 约 100 次探测，成本可忽略；但若用户配置昂贵探测模型需 UI 提示（Low 风险）。
