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

## 2026-07-04（下） — push 补推 + M2 统计与计价完成（任务 07-04-m2-stats-pricing）

- 用户修好凭据：M0+M1 已推 GitHub，CI 三平台首跑 success（run 28691757544）。
- M2 组织：W0 主会话固化 wire 契约（UsageRecord/Batch/Ack）+ LiteLLM 快照过滤嵌入（2204 模型/355KB）；
  W-A/W-B 两子代理并行**均一次全绿**（中转站已恢复）——W-A: 双协议解析/usagebuf 队列/hubclient 单 writer；
  W-B: 入库去重/计价引擎（四步匹配+三层结算+ETag 同步）/stats API。
- 跨代理契约协调：W-A 定义 Model=请求名、ModelRedirected=重定向目标 → 主会话转发给 W-B →
  W-B 全链路按 effectiveModel 分组计价；W-C 补齐 model= 筛选口径。
- W-C（主会话）：e2e 扩展验证断连补报无重无漏 + 真实快照费用结算；修 e2e fake 不回显模型的测试缺陷。
- flake 记录：TestSwapMidStreamAtomic 44 次运行 1 次负载敏感偶发（closed network connection@tidy 峰值），
  无法复现，不掩盖，观察 CI。
- 遗留：跑一天真实对账验收（部署后）；M3 双端体验未开工。

## 2026-07-05/08 — M3 双端体验完成（任务 07-05-m3-dual-experience）

- 收尾归档：M1/M2 task.json 标 completed 并 archive/2026-07（task.py finish 只清指针，状态需手写）。
- **子代理 5 派 5 亡**（429/ECONNRESET，中转站对新会话持续拒绝；主会话 API 正常）——W-A/W-B/W-C
  含重试全部零产出阵亡，最终全部由主会话内联实现（M1 模式重演；用户随后清停全部后台代理）。
- W0：wire/ui.go 三消息契约、webui embed（占位页保证无 node 可 build）、web 脚手架（Vite8/React19/
  Tailwind4/shadcn）。W-A：ws/ui 通道（api 包内、session 中间件顺带鉴权、每连接 writer goroutine、
  慢客户端摘除）、AppendEvent 改返回插入行、全事件点 s.event() 入库即推送、realtime.SetUsageNotifier。
- W-B：SPA 六页全量 + ws 失效通知 invalidate；chrome-devtools 真浏览器冒烟全链路点通（控制台零错）；
  踩坑：新版 shadcn/base-ui Select 的 onValueChange 是 string|null 且显示 label 需 items 映射。
- W-C：Tauri 2.11 壳一次编译通过 + clippy -D warnings 零警告；rustc 1.85→1.96（zvariant 需 ≥1.87）；
  tauri-build 在 cargo build 期即校验 externalBin（研究#7 未覆盖）——binaries/ 需先放 agent 副本。
- e2e 扩展验证 PRD 1s 可见性：双 ws/ui 客户端 switch 后 1s 内 state_changed、第 5 请求 usage_tick。
- Docker 两坑：容器内 proxy.golang.org 不通（GOPROXY 构建参数化）；非 root + 匿名卷 /data root
  属主致 SQLite 打不开（镜像内预建+chown；compose 改命名卷）。
- 教训：后台命令 `cmd | tail` 吞退出码——两个"成功"的构建实为失败；此后一律显式回显 $?。
- 遗留：三平台打包实机验证/updater 签名/Windows UAC（M4）；手机+桌面双端同开实测（部署后）；
  spec/backend 回填继续挂 00-bootstrap-guidelines。

---

---

---

