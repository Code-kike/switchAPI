# M3 双端体验 — 技术设计（design.md）

> 输入：父任务 design.md §1/§3/§8/§9、research/07（Tauri 全实证）、M1/M2 已有代码。
> 本文固化跨 worker 契约；实现细节以对应研究文档为细则。

## 1. ws/ui 实时通道（Hub 侧）

**落位**：`/api/v1/ws/ui` 路由放在 **api 包内**（session 中间件就在那里，鉴权零成本；
浏览器同源 WS 升级自动带 cookie）。未登录升级请求 → 401。

**消息契约**（新增 `internal/shared/wire/ui.go`，复用 `wire.Envelope{type,data}`，只有下行）：

```go
const (
    TypeUIStateChanged = "state_changed" // 切换后：全量 app→provider 映射
    TypeUIEvent        = "event"         // 新事件入库后
    TypeUIUsageTick    = "usage_tick"    // usage_batch 入库后
)
type UIStateChanged struct { Rev int64; Apps map[string]string } // app -> active provider_id
type UIEvent struct { ID int64; TS int64; Kind string; Payload json.RawMessage }
type UIUsageTick struct { Inserted int; LastTS int64 }
```

**接线**：
- api.Server 内建 `uiHub`（conn 集合 + 互斥锁 + 逐 conn 写超时 5s，写失败即摘除）；
  暴露 `NotifyState() / NotifyEvent(ev) / NotifyUsage(n, lastTS)` 与 `CloseUI()`（接入优雅退出）。
- 触发点：`handleSwitch` 成功后 NotifyState+NotifyEvent；配对/吊销等写 events 处 NotifyEvent；
  usage 入库在 realtime（ws/agent）——realtime.Hub 增 `SetUsageNotifier(interface{ UsageInserted(n int, lastTS int64) })`，
  cmd/hub 与测试里注入 api.Server。inserted==0（全重复）不推送。
- 语义：usage_tick/event 是**失效通知**（前端收到后 refetch），不携带全量数据——避免 SQL 双份口径。

## 2. Hub embed（SPA 托管）

- 新包 `internal/hub/webui`：`//go:embed all:dist`；`dist/index.html` 占位页**入库提交**
  （无 node 时 go build 可过），真实构建用 `make web` 把 `web/dist/*` 拷入覆盖（CI/Docker 不提交，
  本地构建后 `git checkout` 还原占位）。`.gitignore`：`internal/hub/webui/dist/*` + `!.../index.html`。
- Handler：静态文件命中即返回（含 assets/ 指纹缓存 Cache-Control）；未命中且非 `/api`/`/healthz`
  → 返回 index.html（SPA 客户端路由 fallback）。
- cmd/hub 根 mux 挂载顺序：`GET /api/v1/ws/agent`→realtime、`/api/`→api、`GET /healthz`→api、
  `/`→webui。（Go 1.22 mux 最长前缀优先，`/` 兜底不遮蔽。）

## 3. Web SPA（`web/`）

- 栈：Vite + React + TS + Tailwind + shadcn/ui + @tanstack/react-query + react-router +
  recharts（趋势图）+ @dnd-kit（备选序列拖拽）。包管理 pnpm。
- 目录：`src/api/`（fetch 封装：401→跳登录；TS 类型以 Go handler JSON 为唯一真源，
  对照 internal/hub/api/*.go 手写）、`src/ws.ts`（重连退避 1s→30s；消息→react-query invalidate）、
  `src/pages/{login,dashboard,providers,usage,devices,events}`、`src/components/`。
- 路由：`/login`、`/`（仪表盘）、`/providers`、`/usage`、`/devices`、`/events`。
  布局：桌面侧边栏 / 移动端底部导航（响应式，手机一等公民）。
- 实时刷新映射：`state_changed`→invalidate state+providers；`usage_tick`→invalidate stats/*+usage；
  `event`→invalidate events。
- dev：vite server.proxy 把 `/api` 与 `/healthz` 代理到本地 hub（ws 需 `ws:true`）。
- 供应商编辑：api_key 留空=不改（M1 语义）；协议创建后不可改；折扣系数/模型重定向 JSON 编辑。
- 费用展示：cost=null 一律显示"未知"标注（绝不显示 0）。

## 4. Tauri 桌面壳（`desktop/`）

research/07 结论逐条落地；**MVP 远程页不调 Tauri IPC**（SPA 双端同构，浏览器无 Tauri），
故 remote capability 注入代码留骨架但默认不需要。

- 前端：极小本地引导页（vanilla TS，非 React）：首启向导（Hub 地址输入→保存）、
  连接中/应急两视图。配置存 `~/.switchapi/desktop.json`。
- 启动流：装载本地页 → 读配置 → Rust 探测 `GET {hub}/healthz`（3s 超时）→ 成功
  `webview.navigate(hub_url)`；失败停在应急页（可重试/改地址）。
- 应急页数据：Tauri command 由 Rust 代取（不开 CORS）——`agent_status()`（读
  `~/.switchapi/agent-state.json` 摘要 + `switchapi-agent status` 输出）、`agent_ctl(action)`
  （install/start/stop，先复制包内二进制到 `~/.switchapi/bin/`）。
- Agent 托管/版本对齐：定位 `current_exe 同目录`（macOS Contents/MacOS、AppImage 挂载点 usr/bin）
  的 `switchapi-agent` → 比对 `--version` → 停→覆盖→启。Windows UAC 路径写注释留 M4。
- 托盘：TrayIconBuilder；菜单=当前供应商（per-App）+ 快切候选 + 打开主窗 + 退出；
  数据源：Rust 侧带 cookie 调 Hub REST（`Webview::cookies_for_url` 复用登录态），
  30s 间隔 + 菜单打开前刷新；未登录时降级为"打开主窗口"。Linux 无左键弹菜单（已知差异）。
- 插件：single-instance（二开聚焦）、autostart（`--minimized` 起最小化）；updater 仅 conf 骨架
  （endpoints/pubkey 占位，`createUpdaterArtifacts` 暂 false，发布期启用）。
- bundle：externalBin `binaries/switchapi-agent`（构建脚本从 go build 产物拷入 target-triple 命名）；
  Linux AppImage / macOS dmg / Windows NSIS per-user 配置齐备，本机只验 `cargo build`。
- Rust 工具链：本机 1.85（Tauri 2 需 ≥1.77.2，满足）；`~/.cargo/bin` 需入 PATH。

## 5. 部署与 CI

- `Dockerfile`（多阶段）：node:22-alpine 构建 web → 拷入 webui/dist → golang 构建 hub
  （CGO_ENABLED=0，modernc 纯 Go）→ 运行层 alpine（含 ca-certificates，LiteLLM 同步要 TLS）；
  `VOLUME /data`，`ENTRYPOINT ["switchapi-hub","-listen",":8080","-data","/data"]`。
- `docker-compose.yml` 示例 + `docs/deploy.md`（Docker/裸机/agent 接入三节）。
- CI：新增 `web` job（pnpm install→typecheck→build）；go 矩阵不动（占位页保证无 node 也绿）；
  desktop 的 rust check 只在 release 流程做（避免每 push 拉全量 crates）。
- `Makefile`：`make web`（构建+拷贝 embed）、`make hub`、`make agent`、`make clean-web`。

## 6. Worker 分工与文件所有权（并行不越界）

| Worker | 范围 | 文件所有权 |
|---|---|---|
| W0 主会话 | wire/ui.go 契约、webui 包骨架+占位、根 mux 重排、web/ 脚手架 | 先行落定后再派发 |
| W-A 子代理 | ws/ui 通道 + usage 通知钩子 + webui Handler 完整化 + 单测 | internal/hub/api/**、internal/hub/realtime/realtime.go、internal/hub/webui/**、cmd/hub/main.go |
| W-B 子代理 | SPA 全部页面/数据层/ws 客户端/构建 | web/** 独占 |
| W-C 子代理 | Tauri 壳全部 | desktop/** 独占 |
| W-D 主会话收尾 | Makefile/Dockerfile/compose/docs/CI、e2e ws/ui 扩展、集成构建、cargo build、commit | 其余全部 |

规则沿用 M2：worker 禁 git commit、禁 `go mod tidy`（W-D 统一）；Go=`~/sdk/go1.26.4/bin/go`。

## 7. 测试策略

- api 单测：ws/ui 未登录 401；登录后收 state_changed（POST /switch 触发）；多客户端广播；
  写失败摘除不炸全局。
- webui 单测：静态命中、SPA fallback、不遮蔽 /api 与 /healthz。
- e2e 扩展（W-D）：两个 ws/ui 客户端 + 既有全链路——switch 后两端 1s 内收 state_changed；
  usage 批次入库后收 usage_tick（Inserted>0）。
- SPA：`tsc --noEmit` + `pnpm build` 作门禁（组件测试不进本期）。
- 桌面：`cargo build` + clippy 通过；交互路径人工验收清单写入 implement.md 遗留。
