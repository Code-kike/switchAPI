# M4 可靠性与迁移（PRD）

> 父任务 prd.md §4 MVP #4/#5/#6、§5 验收；实现细则以 `research/08`（健康阈值参数表 20 条）与
> `research/05`（cc-switch 映射表 + E1-E9 边界）为准——两文档即规格，本 PRD 不重复。

## Goal

MVP 收官：故障自动切换全链路（判定→仲裁→切换→通知→恢复探测）、端点测速、
备份/口令加密导出导入/CSV、cc-switch 一键导入。完成后逐项核对父 prd.md §5 验收清单。

## Requirements

1. **健康判定（Agent）**：按研究#8 失败四分类（硬失败连续 3 次 + 300s 新鲜度；429 连续 6 次跨 ≥60s；
   401/403 三连 → needs_attention；4xx 业务错不计）；成功清零；达阈值边沿触发 health_report
   （携 ≤5 条错误样本）。四类超时落地（connect 10s / TTFB 60s / 流中静默 120s / 非流式总 300s，
   兑现 M1 的 TODO(M4)）。
2. **Hub 仲裁与切换**：5s 防抖汇集；**负证据否决**（他设备 30s 内同供应商成功 → 只通知不切换）；
   沿备选序列取下一健康者（跳过冷却中）；被降级者冷却 300s×2^(n-1) 封顶 3600s；
   无健康候选只通知；每 App 限速 10s；写 failover 事件 + Agent 推送 + ws/ui 通知。
3. **恢复探测**：Hub 轮转指定单台在线 Agent 发 probe_cmd（非流式 max_tokens=1，10s 超时；
   60s×2 封顶 900s ±20% 抖动）；连续 2 次成功 = 恢复（清冷却 + 事件 + 通知）；**不自动切回**。
4. **本地临时降级**：Hub 断连时 Agent 按缓存备选序列本地判定切换（同阈值，本地切换间隔 ≥60s），
   重连后对齐全局状态。ConfigPush 需扩展携带备选供应商完整路由（含 key，LAN-trust 同现状）。
5. **端点测速**：POST /speedtest 广播 → 各在线 Agent 对全部供应商发探测请求 → 按设备×供应商
   展示延迟/可用性（SPA 设备页）；写事件。
6. **备份**：`VACUUM INTO` 每日 + 结构性变更后防抖 5min；保留 10 份；POST /backup/run、GET /backups。
7. **导出/导入**：schema 版本化 JSON（供应商/备选序列/app_state/覆盖价）；含密钥强制
   scrypt+AES-256-GCM 口令加密；明文导出需 UI 二次确认；新 Hub 导入还原（key 重加密落库）。
   CSV 用量导出（带筛选）。
8. **cc-switch 一键导入**：SPA 上传 `cc-switch.db`（或 v2 config.json）→ Hub 按研究#5 映射表
   逐行导入（按列名读、meta 优先）；E1-E9 逐条跳过并报告原因；is_current→app_state、
   failover 队列→备选序列、costMultiplier→折扣系数。
9. **UI 联动**：SPA 收 failover/probe 事件弹 toast（桌面壳装载同一 SPA 即同享通知）；
   供应商页显示健康状态（冷却中/needs_attention）；设置页承载备份/导出/导入/cc-switch 导入；
   恢复通知后一键切回=既有切换入口。

## Constraints

- 探测/测速流量 Agent 直连供应商，绝不经 Hub（ADR-0001）；probe/speedtest 指令携解密 key
  下发沿用 ConfigPush 的 LAN-trust 模型。
- 导出文件含明文 key 时强制口令加密；cc-switch 源数据只提取映射表字段，
  settings_config 其余内容（含第三方机密）绝不入库。
- 中途断流不做流内自动重试（SSE 无法透明重放，ADR-0002）。

## Acceptance Criteria

- [ ] e2e：打断当前上游 → 连续硬失败达阈 → health_report → Hub 沿备选切换 → Agent 新路由 +
  ws/ui 收到 failover 事件与 state_changed；上游恢复 → probe 连续 2 次成功 → recovered 事件（不切回）。
- [ ] 负证据否决与无健康候选两分支有单测覆盖；冷却指数与限速有单测。
- [ ] 测速：e2e 或集成测试中 speedtest 指令下发 → 结果按设备返回 API 可查。
- [ ] 导出→全新 Hub 导入 roundtrip：供应商（key 解密一致）/备选序列/覆盖价还原；错误口令拒绝。
- [ ] cc-switch 导入单测：本机四类形态 fixture（多账号同站/openai_chat 转换类/localhost/空 key）
  ——可映射项全导入、跳过项逐条带原因（父 prd 验收 #7 口径）。
- [ ] CSV 导出内容与 /usage 一致；备份轮转保留 10 份。
- [ ] SPA：设置页四功能可用；failover toast；供应商健康标记；`pnpm build` 绿。
- [ ] 全量门禁绿（go build/vet/test/race/gofmt + pnpm build + cargo clippy）+ CI 绿。

## 遗留（不阻塞本期）

- 父 prd §5 全清单的两机实机验收（部署后人工）；桌面系统级原生通知（SPA 内 toast 已覆盖双端可见性）。
