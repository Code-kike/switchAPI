# M4 — 技术设计（design.md）

> 细则来源：research/08 参数表（阈值/仲裁/探测全部 20 条）、research/05 映射表（E1-E9）、
> 父 design.md §5（流程）/§7（备份导入导出）。本文只固化跨模块契约与落位。

## 1. wire 协议扩展（internal/shared/wire）

```go
TypeHealthReport    = "health_report"     // Agent → Hub（边沿触发）
TypeProbeCmd        = "probe_cmd"         // Hub → 指定 Agent（恢复探测）
TypeProbeResult     = "probe_result"      // Agent → Hub
TypeSpeedtestCmd    = "speedtest_cmd"     // Hub → 全体 Agent（手动测速）
TypeSpeedtestResult = "speedtest_result"  // Agent → Hub

type ErrorSample  { Kind string; TS int64; Status int; LatencyMS int64 }
type HealthReport { App, ProviderID, Kind string /*hard|rate_limit|config*/; Count int;
                    Samples []ErrorSample /*≤5*/ }
type ProbeTarget  { ProviderID, Protocol, BaseURL, APIKey, Model string } // LAN-trust 同 ConfigPush
type ProbeCmd     { ProbeID string; Target ProbeTarget; TimeoutS int }
type ProbeResult  { ProbeID, ProviderID string; OK bool; Status int; LatencyMS int64; Error string }
type SpeedtestCmd    { TestID string; Targets []ProbeTarget }
type SpeedtestResult { TestID string; Results []ProbeResult }
```

**ConfigPush 扩展**：新增 `FallbackRoutes map[string][]AppRoute`（app → 备选序列完整路由，
含解密 key）——本地临时降级的前提（现 FallbackOrders 只有 id 无法本地切换）。字段追加向后兼容。

## 2. Agent 侧（internal/agent）

- **health 包**（新，internal/agent/health）：每 provider 计数器（硬失败连续、300s 新鲜度；
  429 独立 6 次/≥60s；401/403 三连；成功清零），错误样本环形 ≤5；达阈值经回调吐 HealthReport
  （由 hubclient 上行）。判定输入 = forward.Usage（已有 Status/ErrorKind/DurationMS）。
  error_kind 需细化补齐：connect/tls/timeout_first_byte/timeout_idle/stream_aborted/fake_200/http_5xx。
- **forward 超时落地**（proxy.go TODO(M4)）：connect 10s（已有）；流式 ResponseHeaderTimeout 60s、
  流中静默 120s（tee 层 read deadline 看门狗）；非流式 ResponseHeaderTimeout=0 + 总时限 300s
  （context）。按请求体 stream 字段区分（M1 已解析）。
- **本地临时降级**：hubclient 断连状态下 health 达阈 → forwarder 原子切到 FallbackRoutes 中
  下一候选（本地切换 dwell ≥60s）；重连收到 config_push 即对齐（现有逻辑天然覆盖）。
- **probe/speedtest 执行器**（internal/agent/probe）：非流式最小补全（anthropic POST
  {base}/v1/messages、openai POST {base}/responses，max_tokens=1，"ping"，10s 超时）；
  hubclient 分发 probe_cmd/speedtest_cmd → 执行 → 上行结果（复用单 writer 通道）。

## 3. Hub 侧（internal/hub）

- **failover 包**（新）：输入 health_report。流程 = 5s 防抖汇集 → 负证据否决
  （store 查同供应商他设备 30s 内 status 2xx 用量记录）→ 每 App 限速 10s →
  沿 fallback_orders 取下一健康（跳过 cooldown_until 未到者）→ SetAppState +
  failover 事件 + agents.Broadcast + ui 通知；无健康候选 → 仅事件+通知。
  冷却写 provider_health（demote_count++、cooldown_until=300s×2^(n-1)≤3600s）。
  config 类（401/403）→ needs_attention=1 + 事件，不冷却不自动恢复。
- **恢复探测**：failover 包内循环——对每个冷却中 provider，60s×2^k 封顶 900s ±20% 抖动，
  轮转选一在线 Agent 发 probe_cmd（realtime 提供 SendTo(deviceID, env) 与在线列表）；
  probe_result 连续 2 OK → 清冷却/demote 保留、consecutive_probe_ok 复位、recovered 事件+通知；
  失败重置连击。**不自动切回**。
- **通知接口统一**：现 realtime.UsageNotifier 扩为 `UINotifier { UsageInserted; EventAppended(store.Event);
  StateChanged() }`（api.Server 已有对应实现，改为实现该接口）；failover 包同样注入。
  store.AppendEvent 返回行已具备（M3）。
- **speedtest**：POST /api/v1/speedtest → realtime 广播 speedtest_cmd（Targets=全部供应商解密路由）
  → 结果落 api 内存 map[testID]map[device][]ProbeResult + speedtest 事件；
  GET /api/v1/speedtest/latest 返回最近一轮（含进行中标记）。
- **store 增量**：provider_health DAO（upsert/查询/清冷却）；无新表（0001 已建全）。

## 4. 备份 / 导出 / 导入（internal/hub/backup + api）

- backup：daily ticker + MarkDirty() 防抖 5min → `VACUUM INTO backups/hub-<ts>.db`，轮转留 10；
  API 写点（providers/switch/fallback/devices 变更处）调 MarkDirty。POST /backup/run、GET /backups。
- export（POST /api/v1/export {passphrase?, include_usage?, plaintext_confirmed?}）：
  payload = {schema:1, providers(含明文 key), fallback_orders, app_state, pricing_overrides}；
  有 key 且无 passphrase → 必须 plaintext_confirmed=true 否则 400；加密格式
  {format:"switchapi-export-v1", kdf:{scrypt N=32768,r=8,p=1,salt}, nonce, ciphertext}（AES-256-GCM）。
- import（POST /api/v1/import {data, passphrase?}）：解密→校验 schema→provider key 用本地
  master key 重加密 upsert（同名/同 id 覆盖策略：id 相同覆盖，否则新建）→ 还原序列/状态→事件。
- CSV：GET /api/v1/usage/export.csv?（同 /usage 筛选参数）流式输出。
- **cc-switch 导入**（internal/hub/importer）：POST /api/v1/import/cc-switch（body=上传的
  db 文件 base64 或 multipart）→ 落临时文件 → modernc sqlite ro 打开 → 按 research/05 映射表
  逐行（**按列名**取值）；v2 config.json 分支同映射；v1 拒绝。输出 {imported:[...],
  skipped:[{name, reason E1-E9}]}；key 明文入库前 AES-GCM。上传方案规避 E7（锁）/E9（override）。

## 5. SPA（web/）

- `/settings` 新页：备份（列表+立即备份）、导出（口令/明文二次确认/下载）、导入（粘贴或上传+口令）、
  cc-switch 导入（上传 .db/.json → 导入/跳过报告表）。
- 供应商页：GET /api/v1/health 合并展示（冷却中倒计时 badge、needs_attention 警示）。
- 设备页：测速按钮 → POST /speedtest → 轮询 latest → 设备×供应商延迟表。
- ws.ts：event 消息 kind∈{failover, probe} → sonner toast（富文本：从哪切到哪/已恢复）。

## 6. 波次（全部主会话内联；上轮子代理 5 派 5 亡，本轮不再派发）

W1 wire+store 契约 → W2 Agent（health/超时/降级/probe 执行器）→ W3 Hub（failover/探测/测速/通知）
→ W4 备份导出导入+importer → W5 SPA → W6 e2e+门禁+提交。每波各自 build/test 绿再进下一波。

## 7. 测试策略

- health 单测：四分类计数/新鲜度/清零/429 升级/401 通道。
- failover 单测：否决/无候选/限速/冷却指数/needs_attention。
- importer 单测：程序化构造 fixture db（研究#5 C5 四类形态）+ v2 json + v1 拒绝。
- export/import 单测：roundtrip + 错误口令 + 明文未确认 400。
- e2e：断 A→failover→B（双端通知）→A 复活→probe 恢复（不切回）；speedtest 往返。
- SPA：pnpm build 门禁（沿用 M3 口径）。
