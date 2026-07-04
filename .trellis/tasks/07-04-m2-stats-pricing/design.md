# M2 — 技术设计补充（design.md）

> 权威方案 = 父任务 design.md §2/§3/§4/§6；字段规格 = research/03、research/04。本文只记 M2 落地决策。

## 1. 协议扩展（internal/shared/wire）

```go
UsageRecord{ request_id, ts, app, provider_id, model, model_redirected,
             input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
             duration_ms, status, error_kind, usage_source }   // 与 usage_records 表一一对应（device_id 由 Hub 按连接归属）
UsageBatch{ batch_id string, records []UsageRecord }           // 上行
UsageAck{ batch_id string }                                    // 下行，TypeUsageAck = "usage_ack"
```

## 2. Agent 侧管线

- **forward 扩展**（所有权 internal/agent/forward）：
  - decision 增补：request_id(uuid)、app（由前缀推导 anthropic→claude-code、openai→codex）、start 时刻、
    model_redirected（patchModel 的 from→to）；计费路径判定 = POST 且 path 以 `/messages`（anthropic）
    或 `/responses`（openai）结尾——其余透传不挂 tee。
  - tee 泛化：anthropic 解析器保持；新增 **openai Responses 解析器**（找 `response.completed|incomplete|failed`
    的 data 行，`usage` 空值防御，`input = input_tokens − cached_tokens`、`cache_read = cached_tokens`、
    cache_write=0）；openai 协议行缓冲上限放宽至 8MB（response.completed 内嵌完整输出，研究#6 遗留#3）,
    高水位仪表保留；**非流式**：Content-Type application/json 时缓冲响应体（≤8MB）终态解析，两协议同构。
  - Usage 产出补全：Status、ErrorKind（5xx→"upstream_5xx"、破流→"stream_aborted" 等粗分类）、
    UsageSource（正常 wire；破流有部分数字仍 wire+Done=false→error_kind 标注；无数字 none）。
- **usagebuf**（新包 internal/agent/usagebuf）：SQLite 队列表
  `pending(request_id TEXT PK, payload TEXT, created_at INT)`；`Enqueue(Usage)` 经带缓冲 channel +
  单写 goroutine（sink 永不阻塞转发）；`Batcher`：每 2s 或攒 50 条打包 UsageBatch（batch_id=uuid）
  交给发送方，收到 Ack(batch_id) 后按该批 request_id 删除；重连时全部 pending 重新可发（at-least-once）。
- **hubclient 扩展**：出站 channel（心跳 goroutine 统一变为 writer：心跳 + usage_batch 都走它）；
  读循环分发 usage_ack → Batcher；连接建立后触发 Batcher flush（补报积压）。
- **cli 接线**：run 时构造 usagebuf（路径 `~/.switchapi/agent.db`）→ sink 注入 forward.New。

## 3. Hub 侧

- **realtime**：读循环新增 usage_batch → `store.InsertUsageRecords(deviceID, records)`（tx，
  INSERT OR IGNORE by request_id）→ 回 usage_ack；入库条数与忽略条数写日志。
- **store 扩展**：InsertUsageRecords；QueryUsage(filter{from,to,app,provider,model,device,limit,offset})
  → (rows,total)；AggUsage(group by 维度/时间桶) —— SQL 聚合出 token 分项与请求数，费用在 Go 层结算。
- **pricing**（新包 internal/hub/pricing）：
  - `snapshot.json`（embed）：LiteLLM 原表过滤 mode∈{chat,responses}，仅留
    {input_cost_per_token, output_cost_per_token, cache_creation_input_token_cost,
    cache_read_input_token_cost, litellm_provider, mode}；构建脚本留 Makefile target `pricing-snapshot`。
  - `EnsureLoaded(store)`：pricing_base 空则导入快照；`SyncDaily(ctx, store)`：24h ticker + ETag
    （settings: pricing_etag / pricing_sync_enabled）+ upsert-only。
  - `Resolver`：缓存 pricing_base 于内存（同步后失效重载）；`Resolve(model)` 四步匹配；
    `Cost(rec, coeff, override)`：`override ?? base×coeff`，四分项独立、NULL→0；
    全项 NULL/未匹配 → 返回 (0,false)（未知模型，费用记 null）。
- **api 扩展**：`GET /usage`、`GET /stats/summary`、`GET /stats/trend?bucket=hour|day`、
  `GET /stats/breakdown?by=provider|model|app|device`（均带 from/to unix 秒，默认近 7 天）；
  响应含 cost 字段（null=未知模型）；providers 变更（系数/override）即时生效（查询时结算，无需回填）。
- **cmd/hub**：启动时 pricing.EnsureLoaded + go SyncDaily。

## 4. 测试

- forward：openai 流式/非流式/incomplete/usage 缺失/大行；anthropic delta 带输入侧字段；破流 partial 标注。
- usagebuf：enqueue 并发不丢、ack 删除、重启进程队列仍在（重开 db）。
- realtime：batch 入库 + 去重（同 batch 重发） + ack。
- pricing：四步匹配、三层结算、快照导入、同步 upsert-only + ETag 304（httptest 假 GitHub）。
- api：usage 分页筛选、summary/trend/breakdown 形状与 cost。
- e2e 扩展：流式+非流式过代理 → /usage 可查 → 断 Hub 请求积压 → 重连补报无重无漏（request_id 断言）。

## 5. 文件所有权（并行波次）

- W-A（Agent 侧）：internal/agent/forward/**、internal/agent/usagebuf/**、internal/agent/hubclient/**、
  internal/agent/cli/cli.go、internal/shared/wire/wire.go（协议扩展先行、双方契约）
- W-B（Hub 侧）：internal/hub/store/**（新增文件+迁移 0002 若需）、internal/hub/pricing/**、
  internal/hub/realtime/realtime.go、internal/hub/api/**、cmd/hub/main.go
- W-C（串行收尾）：internal/e2e/**、全量检查
