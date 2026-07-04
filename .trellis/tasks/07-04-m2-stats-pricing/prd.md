# M2 统计与计价

> 父任务：`07-02-switchapi-mvp-plan`（milestone M2）。范围来自父 `implement.md` M2 节；
> 字段级规格以 `research/03`（usage 采集点/四分项映射/中断矩阵）与 `research/04`（LiteLLM 价格表）为准。

## Goal

打通"采集 → 缓冲 → 上报 → 入库 → 聚合 → 计价"的用量链路：每个经 Agent 的计费请求
产生一条纯元数据用量记录（永不含消息内容），断连不丢、重传不重，Hub 端可按维度聚合并按三层规则折算费用。

## Requirements

1. **Agent usage 解析**（研究#3 映射表为准）：anthropic（message_start/delta 累计覆盖语义，已有）+
   **openai Responses**（response.completed，兜底 incomplete/failed，usage 可 null 防御，
   input_tokens 含 cached 须减法拆分，cache_write 恒 0）+ **两协议非流式 JSON**；
   openai 大响应事件行放宽缓冲上限；每条记录带 request_id（uuid）、app、duration_ms、status、
   usage_source(wire|estimated|none)；非计费路径（count_tokens、GET /models 等）透传但不产记录。
2. **Agent 本地缓冲**：`~/.switchapi/agent.db`（SQLite 队列，request_id 主键）；sink 不阻塞转发路径；
   断连期间只积压不丢弃。
3. **上报协议**：WS usage_batch（batch_id + records）→ Hub 落库后回 usage_ack(batch_id) → Agent 删队列；
   at-least-once，Hub 以 request_id UNIQUE 幂等去重；重连后全量补传积压。
4. **Hub 入库与查询**：usage_batch 入库（INSERT OR IGNORE）+ 归属 device_id；
   `GET /usage`（分页明细+按时间/app/供应商/模型/设备筛选）；
   `GET /stats/summary|trend|breakdown`（四分项 token、请求数、费用；trend 支持 hour/day 桶）。
5. **计价引擎**（研究#4 为准）：内置**过滤后**的 LiteLLM 快照（mode∈{chat,responses}）随二进制分发；
   每日同步（ETag 条件请求、upsert-only 永不删除、可经 settings 关闭）；
   模型名四步匹配（精确→去 -YYYYMMDD→斜杠取尾小写→未知记 token 不记费）；
   三层解析 `overrides[provider,model] ?? base[model] × coefficient(provider)`，四分项独立结算，
   NULL 价按 0；未知模型在明细/聚合中可识别（cost 为 null 而非 0）。

## Out of Scope（本任务不做）

余额查询适配器（二期）、仪表盘 UI（M3，本期只出 API）、tokenizer 估算 fallback（记 usage_source=none 即可，
估算留 M4 打磨）、5m/1h 缓存 TTL 分级计价（tiered_prices 只记录，二期结算）。

## Acceptance Criteria

- [ ] `make vet test build` 全绿；既有 M1 测试（含 e2e、race）零回退
- [ ] e2e 扩展：经 Agent 的流式+非流式请求在 Hub `GET /usage` 可查（四分项/耗时/归属正确）；
  **断 Hub 期间的请求在重连后补报，无缺口、无重复**（request_id 去重验证）
- [ ] fake-upstream 回放覆盖研究#3 C7 中断矩阵关键行：anthropic 多 delta/delta 带输入侧字段、
  openai usage 缺失/incomplete、中途断流（partial 记录 usage_source 标注正确）
- [ ] 计价单测：三层解析优先级、四步模型名匹配（含真实样本 claude-haiku-4-5-20251001 精确命中、
  ZhipuAI/GLM-5.2 落入未知）、OpenAI cached 减法拆分、NULL 价按 0、override 绕过系数
- [ ] 快照导入后 pricing_base 有 claude/gpt 主流模型价格；同步单测（ETag 304 跳过、upsert 不删除）
- [ ] 真实验收（部署后人工）：跑一天真实使用对照中转站后台账单误差可解释；断 Hub 两小时明细无缺口

## Notes

- 依赖零新增（modernc sqlite 已有）；LiteLLM 快照下载过滤后嵌入 `internal/hub/pricing/`。
- Go = `~/sdk/go1.26.4/bin/go`；禁止 `go mod tidy`（收尾统一）；禁止 git commit。
