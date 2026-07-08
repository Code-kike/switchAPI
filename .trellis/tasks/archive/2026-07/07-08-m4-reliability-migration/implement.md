# M4 — 执行计划（implement.md）

> 主会话内联分波实现（不派子代理）；Go=`~/sdk/go1.26.4/bin/go`、cargo=`~/.cargo/bin`、pnpm。
> 细则：research/08 参数表、research/05 映射表；契约见本任务 design.md。
> 每波完成即跑 build/vet/test 局部门禁；禁 commit 直到 W6。

## W1 契约与存储 ✅

- [x] wire：HealthReport/ProbeCmd/ProbeResult/SpeedtestCmd/SpeedtestResult + ErrorSample/ProbeTarget；
  ConfigPush.FallbackRoutes
- [x] store：provider_health DAO（upsert 冷却/needs_attention/探测连击、查询、清理）
- [x] realtime：buildPush 填 FallbackRoutes；SendTo(deviceID)/OnlineDevices()

## W2 Agent 侧 ✅

- [x] forward：error_kind 细化（connect/tls/timeout_first_byte/timeout_idle/stream_aborted/
  fake_200/http_5xx）；四类超时条件化（TODO(M4) 兑现）
- [x] health 包：四分类计数器 + 阈值边沿回调 + 单测
- [x] hubclient：health_report 上行；probe_cmd/speedtest_cmd 分发；本地临时降级（断连 + 达阈 →
  FallbackRoutes 下一候选，dwell 60s）
- [x] probe 包：两协议最小补全执行器 + 单测（httptest）

## W3 Hub 侧 ✅

- [x] failover 包：防抖汇集/负证据否决/限速/沿序列选择/冷却指数/needs_attention + 单测
- [x] 恢复探测循环（轮转 Agent、60s×2≤900s±20%、2 连成功恢复、事件+通知）
- [x] UINotifier 统一（UsageInserted/EventAppended/StateChanged）；api.Server 适配
- [x] speedtest：POST /speedtest 广播 + 结果聚合 + GET /speedtest/latest + 事件
- [x] GET /api/v1/health（provider_health 视图）

## W4 备份 / 导出 / 导入 / cc-switch ✅

- [x] backup 包：daily+MarkDirty 防抖 VACUUM INTO、轮转 10；POST /backup/run、GET /backups
- [x] export/import：scrypt+AES-GCM v1 格式、明文需确认、roundtrip 单测（含错误口令）
- [x] CSV：GET /usage/export.csv（复用筛选）
- [x] importer 包：cc-switch.db（ro、按列名）+ v2 json + v1 拒绝；E1-E9 跳过报告；
  fixture 单测（多账号同站/openai_chat/localhost/空 key）
- [x] POST /api/v1/import/cc-switch（文件上传）

## W5 SPA ✅

- [x] /settings 页：备份/导出（明文二次确认）/导入/cc-switch 导入报告表
- [x] 供应商页健康标记（冷却倒计时/needs_attention）；设备页测速面板
- [x] ws event kind failover/probe → toast；pnpm build 绿

## W6 收尾 ✅（全部主会话内联完成）

- [x] e2e：断 A→3 连失败→failover→B（Agent 路由 + ws/ui 双通知）→A 复活→probe 2 连→recovered
  不切回；speedtest 往返；export/import roundtrip
- [x] 全量门禁：go build/vet/test/race + gofmt + pnpm build + cargo clippy + docker build 冒烟
- [x] journal + 父任务清单回写 + commit + push + CI 确认

## 验证命令

```bash
GO=~/sdk/go1.26.4/bin/go
$GO build ./... && $GO vet ./... && $GO test ./... && $GO test ./internal/agent/forward/ ./internal/e2e/ -race
cd web && pnpm build
cd desktop/src-tauri && PATH=$HOME/.cargo/bin:$PATH cargo clippy -- -D warnings
```

## 实施注记

- Transport 级失败（拒连/TLS/超时）原先不产生任何 usage 记录——健康判定最重要的信号缺失，
  已在 ErrorHandler 补记账（status 502/504 + error_kind 分类，markRecorded 防双记）。
- 旧测试 TestAnthropicMidStreamAbort 场景实为客户端挂断，按研究/08 语义改断 client_abort，
  另补真正的"上游断流"用例。
- race 检测抓到 SpeedtestLatest 浅拷贝共享 Results map 与写入竞争 → 深拷贝修复。
- backup 文件名秒级时间戳同秒撞车（VACUUM INTO 要求目标不存在）→ 毫秒精度 + 按名字典序排序。
- docker build 未重跑（Dockerfile 未变，web 构建由 CI web job 覆盖）；cargo clippy 复验绿。
