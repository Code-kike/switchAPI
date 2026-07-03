# Journal - orion (Part 1)

> AI development session journal
> Started: 2026-07-02

---

## 2026-07-03/04 — M0 研究与地基执行（任务 07-02-switchapi-mvp-plan）

- Phase 1.3/1.4 补齐：implement.jsonl / check.jsonl 从模板填充为正式清单；任务已 in_progress。
- Workflow 派发 M0：8 研究并行 + 骨架 implement→check 链 + 审计。6 研究一次成功；#3 文档在重试中已落盘（无需重跑）；#1 两次败于基础设施（子代理卡死×6、429），改由主会话直接完成（官方 settings 文档 + SDK client.ts + CC 二进制勘察 + 实盘 settings + cc-switch README 五源印证，verdict=confirmed）。
- 关键发现：CC settings env 块官方实锤热生效（重启例外仅 model/outputStyle）；Codex 已移除 chat wire（只需 /responses+/models）；OpenAI input_tokens 含 cached 须减法拆分；LiteLLM 过滤须 mode IN (chat,responses)、upsert-only；cc-switch 现行=SQLite user_version 11（本机为 fork cc-switch-web，uv=10）；健康仲裁"多设备一致"被推翻→负证据否决。
- 骨架：module github.com/Code-kike/switchAPI，go build/vet/test 全绿（go1.26.4 固化至 ~/sdk/go1.26.4，本机原无 Go——记得把 $HOME/sdk/go1.26.4/bin 入 PATH）。
- M0 验收门通过：SSE 原型（research/06-sse-prototype，新增 REAL_UPSTREAM 模式）对真实中转站 anyrouter.top 流式对话 200，事件间隔原样透传，usage tee 解析 8in/16out，仅耗 24 token。
- 基础设施备忘：anyrouter.top 当晚持续抖动（stall/429/ECONNRESET/520），是子代理批量失败根因；audit 代理两次被杀后由主会话完成交叉审计。
- 交叉审计结论：m1_ready=true，无 critical 矛盾；两个真冲突已调和——①超时旋钮按流式条件化（#6 非流式头部等待 vs #8 TTFB 60s）；②路径拼接规则按协议区分（anthropic base 不含 /v1、openai base 含 /v1，剥离前缀 /anthropic 与 /openai/v1）。
- M0 完成（2026-07-04）：design.md v2 全面回写研究结论（provider_health 表、usage_source 列、pricing_base 扩列、probe_cmd/probe_result 消息、负证据否决仲裁、cookie Max-Age/非 Secure、externalBin 仅分发等）；prd.md 验收#7 改写（可映射全导入+跳过报告）、§6 标记 8/8 完成；implement.md M0 全勾；CONTEXT.md 新增术语 冷却/恢复探测。
- 下一步：等用户确认——是否 git commit M0 checkpoint；是否建 M1 子任务（任务创建需用户同意）开工数据与转发内核。

## 2026-07-04 — M1 数据与转发内核完成（任务 07-04-m1-data-forwarding）

- 用户批准 commit + 建 M1 子任务。M0 commit=24f8b7a（147 文件）；push 被凭据挡（PAT 缺 workflow scope、gh token 失效——待用户修复后补推）。
- M1 全部代码由主会话直接实现（W1/W2 共 4 次子代理派发全部 429 秒杀，anyrouter 持续拒绝子代理会话；本会话 API 正常——疑与并发/新会话建立有关）。
- 交付：shared/{wire,cryptoutil}、hub/{store(10 表迁移+DAO),api(auth 引导式首登/CRUD/switch/配对/事件),realtime(ws/agent+CloseAll)}、agent/{forward(原型演化+动态路由),hubclient(退避重连+0600 快照),appconfig(CC/Codex 接管 dry-run/备份/回滚),cli(kardianos)}、internal/e2e。
- 实现决策偏差（已回写 design.md）：anthropic 上游注入双头齐发（apiKeyHelper 官方先例），弃 per-provider AuthStyle。
- e2e 发现并修复真实缺口：http.Server.Shutdown 不关 hijacked WS → realtime.CloseAll() 接入优雅退出。
- 全绿：9 包 test + forward/e2e race + gofmt + 真实配置 dry-run 验收（正确 diff/脱敏/冲突警告/零写入）。
- 遗留：两台机器手动验收（待部署）；spec/backend 回填；push 补推。M2 未开工。

---

---

