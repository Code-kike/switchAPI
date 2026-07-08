# M2 — 执行计划（implement.md）

> 所有实现：禁止 git commit；禁止 `go mod tidy`（W-C 收尾统一）；Go = `~/sdk/go1.26.4/bin/go`。
> wire 协议扩展由主会话先行落定（双方契约），W-A/W-B 并行互不越界（所有权见 design.md §5）。

## W0（主会话先行）✅

- [x] wire 协议扩展（UsageRecord/UsageBatch/UsageAck + TypeUsageAck）
- [x] LiteLLM 快照下载 + mode 过滤（2204 模型 / 355KB）落盘 internal/hub/pricing/snapshot.json

## W-A Agent 侧管线（并行）✅ 子代理完成（中转站恢复，一次全绿）

- [x] forward：decision 扩展、计费路径判定、openai Responses 解析器（8MB 行上限/null 防御/cached 减法）、
  两协议非流式解析、Usage 补全 + ToRecord()
- [x] usagebuf：SQLite 队列 + 非阻塞 Enqueue（256 缓冲溢出丢弃+日志）+ NextBatch/Ack/ResetInflight、重启存活
- [x] hubclient：单 writer 统一出站（心跳+batch）、ack 分发、重连 ResetInflight+flush 补报；UseQueue 可选注入（nil=M1 行为）
- [x] cli：run 接线 <state-dir>/agent.db，打开失败优雅降级（转发不受影响）

## W-B Hub 侧（并行）✅ 子代理完成（一次全绿；已按 effectiveModel=ModelRedirected??Model 契约实现）

- [x] store：InsertUsageRecords 幂等、QueryUsage 分页筛选、AggSummary/Trend/Breakdown（SQL 聚合按 eff_model 分组）
- [x] pricing：快照 embed+EnsureLoaded、SyncDaily（ETag/upsert-only/settings 开关）、Resolver 四步匹配+三层结算
- [x] realtime：usage_batch 入库+usage_ack（不 ack 即重传）
- [x] api：/usage、/stats/summary|trend|breakdown（cost=null 表未知模型）；api.New 增 Resolver 参数
- [x] cmd/hub：EnsureLoaded + go SyncDaily

## W-C 收尾（串行）✅ 主会话完成

- [x] /usage 的 model= 筛选口径改为 effective model（与聚合/计价一致）
- [x] e2e 扩展：4 次计费请求（含 Hub 宕机期间 1 次）→ 重连补报"1 inserted, 0 ignored" → total==4
  无重无漏 + 归属/四分项/费用（真实快照结算 claude-haiku-4-5）+ summary 聚合抽查；
  修复 e2e fake upstream 未回显请求模型的测试缺陷
- [x] 全量检查：`go mod tidy`、build/vet/test 全绿、4 包 race 全绿、gofmt 清洁；上一推送的 GitHub CI success
- 遗留观察：TestSwapMidStreamAtomic 在 44 次运行中出现 1 次负载敏感偶发
  （"use of closed network connection"，恰逢 tidy 重下依赖的磁盘/CPU 峰值），隔离/整包/全仓复跑均无法复现——
  不掩盖，CI 若复现再深挖
- 遗留：真实验收（跑一天对账 + 断 Hub 两小时）待部署后人工执行

## 验证命令

```bash
GO=~/sdk/go1.26.4/bin/go
$GO build ./... && $GO vet ./... && $GO test ./... && $GO test ./internal/agent/forward/ ./internal/e2e/ -race
```
