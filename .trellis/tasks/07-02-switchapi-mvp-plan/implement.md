# switchAPI — 执行计划（implement.md）

> 依赖：`prd.md`（范围/验收）、`design.md`（技术方案）。
> 原则：每个里程碑结束时有可运行、可验证的产物；研究项在用到它的里程碑开工前完成。

## M0 — 研究与地基（前置）✅ 完成（2026-07-04）

- [x] 完成研究项 #1 #2 #3（CC/Codex 指向本地代理写法 + usage 采集点）——实测记录在 `research/01/02/03`
  （#1 verdict=confirmed 且 CC 热生效实锤；#2 发现 Codex 已移除 chat wire；#3 产出三协议→四分项映射表）
- [x] 完成研究项 #6（`research/06` + 可运行原型 `research/06-sse-prototype/`，vet/test/race 全绿）
  ——顺带完成 #4 #5 #7 #8（全部 8 项研究一次做完，结论已回写 design.md）
- [x] 初始化 monorepo 骨架 + CI（go build/vet/test 全绿 @go1.26.4；golangci-lint 待 spec 回填后替换 go vet；
  CI 三平台交叉编译待首次 push 后在 GitHub Actions 上验证）
- [x] 验证：原型代理直连真实中转站（anyrouter.top）完成流式对话——事件间隔原样透传、
  usage 解析成功（记录见 research/06 文末补录）
- 环境备忘：Go 工具链固化于 `~/sdk/go1.26.4`（本机原先无 Go）

## M1 — 数据与转发内核

- [ ] Hub：SQLite store 层（design.md §2 全部表 + 迁移机制）
- [ ] Hub：providers CRUD API + 预设模板（含密钥 AES-GCM 加密落库）
- [ ] Hub：app_state + `POST /switch` + events 记录
- [ ] Hub：ws/agent 通道（hello/config_push/心跳）+ 设备配对（一次性码→token）
- [ ] Agent：转发器（同格式直通 + 本地 token 校验 + 上游 key 注入 + 模型重定向）
- [ ] Agent：配置缓存与断连降级骨架；`agent install|pair` CLI + 系统服务注册
- [ ] Agent：appconfig 模块——首次安装写 CC/Codex 配置指向本机（研究#1#2 的落地）
- 验证：两台机器配对后，在 Hub 上 `curl POST /switch`，两机 CC 下一请求均走新供应商，无需重启终端

## M2 — 统计与计价

- [ ] Agent：usage 流式解析（Anthropic/OpenAI 两协议）+ 本地缓冲队列 + usage_batch 上报（幂等去重）
- [ ] Hub：用量入库、聚合查询 API（summary/trend/breakdown）、明细分页
- [ ] Hub：计价引擎（LiteLLM 快照打包 + 每日同步 + 三层解析 + 缓存分项；研究#4）
- [ ] 断连补报：Agent 离线累积 → 重连全量补传，Hub 端 request_id 去重
- 验证：跑一天真实使用，对照中转站后台账单，折扣系数配置正确时误差可解释；
  中途断开 Hub 两小时，恢复后明细无缺口

## M3 — 双端体验 ✅ 完成（2026-07-08，commit d241f72，子任务 07-05-m3-dual-experience）

- [x] Web SPA：登录、供应商管理（含预设/备选序列拖排）、一键切换、用量仪表盘/趋势/明细、
  设备管理、事件时间线；ws/ui 实时刷新
- [x] Hub：embed 前端产物；ws/ui 通道
- [x] 桌面壳：Tauri 装载 + 首启向导（Hub 地址）+ 托盘快切 + 开机自启 + Agent 托管 + 应急页
  （登录在 Hub 页内完成；Linux cargo build+clippy 绿，三平台打包实机验证留 M4/发布期）
- [x] Docker 镜像 + hub 部署文档；桌面打包配置齐备（bundle 三目标 + externalBin）
- 验证：e2e 双 ws/ui 客户端 1s 内可见切换与新用量（自动化）；手机浏览器+桌面壳同开实测待两机部署

## M4 — 可靠性与迁移

- [ ] 故障自动切换全链路（健康判定/防抖阈值（研究#8）→ Hub 裁决 → 全局切换 → 双端通知）
- [ ] 端点测速（广播指令→各 Agent 自测→按设备展示）
- [ ] 备份快照轮转 + 口令加密导出/导入 + CSV 用量导出
- [ ] cc-switch 一键导入（研究#5）
- [ ] 桌面通知/托盘状态联动；事件时间线补全
- 验证：逐项跑 prd.md 第 5 节全部验收标准，全绿后本任务 finish

## 任务组织建议

M1–M4 各建一个 Trellis 子任务（parent = 本任务），各自 curate `implement.jsonl`/`check.jsonl`
（引用 prd.md、design.md、CONTEXT.md、相关 ADR 与 research/ 产出）；
`.trellis/spec/` 的 backend/frontend 规范在 M0 骨架定型后回填（衔接 00-bootstrap-guidelines）。
