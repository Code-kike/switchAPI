# M3 — 执行计划（implement.md）

> 所有 worker：禁止 git commit；禁止 `go mod tidy`（W-D 收尾统一）；Go = `~/sdk/go1.26.4/bin/go`；
> Rust/cargo 在 `~/.cargo/bin`（须自行入 PATH）；前端包管理用 pnpm。
> 契约（wire/ui.go 消息、webui 挂载、分工所有权）以本任务 design.md 为准，先读再动手。

## W0（主会话先行）✅

- [x] `internal/shared/wire/ui.go`：TypeUIStateChanged/UIEvent/UIUsageTick 三消息定型
- [x] `internal/hub/webui/`：embed 骨架 + 占位 dist/index.html + .gitignore 规则；根 mux 重排（cmd/hub）
- [x] `web/` 脚手架：Vite8+React+TS+Tailwind4+shadcn（组件预装）、dev proxy(:8080)、@ 别名；pnpm build 绿

## W-A Hub ws/ui + embed 完整化 ✅ 主会话完成（子代理两连败于中转站 429/ECONNRESET，改内联实现）

- [x] api：uiHub（session 鉴权升级/多客户端/每连接 writer goroutine + 16 缓冲慢客户端摘除/CloseUI）
  + NotifyState/NotifyEvent/UsageInserted
- [x] 触发点接线：handleSwitch（broadcast 后 NotifyState 取新 rev）；全部 AppendEvent 站点改走
  s.event()（入库+UI 推送，store.AppendEvent 改返回插入行）；realtime.SetUsageNotifier（inserted==0 不推）
- [x] webui Handler：静态命中/assets immutable 缓存/SPA fallback no-cache/不遮蔽 api+healthz；单测 2 个
- [x] cmd/hub：webui 挂载（W0）+ SetUsageNotifier + CloseUI 接入优雅退出
- [x] api 单测 4 个：401 拒升级、switch→state_changed+event、usage_tick 广播+死客户端隔离、CloseUI

## W-B Web SPA ✅ 主会话完成（子代理两连败 429，改内联实现）

- [x] api 层 + TS 类型（对照 Go handler 手写）+ 401 全局跳转；ws 客户端重连(1s→30s)+invalidate 映射
- [x] 登录页（引导设密提示）；布局（桌面侧边栏/移动底部导航，响应式）
- [x] 供应商页：CRUD+预设+key 留空不改+折扣系数+模型重定向编辑+备选序列拖拽(@dnd-kit)+一键切换
- [x] 仪表盘：切换卡片+summary 卡片+trend 图(recharts 时/日)+breakdown 四维度；cost=null 显"未知"、
  零请求显"—"
- [x] 明细页（分页+五筛选+重定向标注+来源标注）、设备页（配对码 TTL 倒计时+指引+吊销）、事件时间线（中文摘要）
- [x] `pnpm build`（tsc -b + vite）绿；**真浏览器冒烟**（chrome-devtools）：登录→建供应商→切换→
  事件摘要全链路点通，控制台零错误；修 base-ui Select 需 items 映射显示 label 的三处

## W-C Tauri 壳 ✅ 主会话完成（子代理两连败 429/ECONNRESET，改内联实现）

- [x] 工程手写（Cargo.toml/tauri.conf.json/capabilities/图标程序化生成/ui 静态引导页，无脚手架 CLI）
- [x] 首启向导→healthz 探测(3s)→navigate(Hub)；应急页（agent_status 白名单读取——绝不碰
  token/api_key、agent_ctl install/start/stop，Rust 代取免 CORS）
- [x] Agent 托管：current_exe 锚定包内→复制 ~/.switchapi/bin(0755)→install/start/stop；
  sync_version 版本对齐（停→覆盖→启）；Windows UAC 留 TODO(M4)
- [x] 托盘（cookies_for_url 复用登录态、30s 重建菜单、快切、降级最小菜单）+ single-instance +
  autostart(--minimized) + 关窗隐藏到托盘；updater 仅 conf 骨架（createUpdaterArtifacts=false）
- [x] bundle 配置（externalBin/appimage+dmg+nsis）；`cargo build` + `clippy -D warnings` + fmt 全绿
  （tauri 2.11.5 / rustc 1.96.1——1.85 因 zvariant 需 ≥1.87 已 rustup 升级）
- 实现注记：tauri-build 在 cargo build 期即校验 externalBin 存在（与预期不同）——binaries/ 需先放入
  triple 命名的 agent（go build 产物拷贝即可，已 gitignore）

## W-D 收尾（主会话，串行）

- [x] e2e 扩展：双 ws/ui 客户端——switch 后 1s 内两端 state_changed（rev>0）；第 5 次请求→双端
  usage_tick(Inserted==1)；hubProc 接线 SetUsageNotifier+CloseUI（全测 5.5s 绿）
- [x] Makefile（web/clean-web 新增）+ Dockerfile（多阶段+非 root+GOPROXY ARG+/data 预建属主）+
  compose（命名卷）+ .dockerignore + docs/deploy.md
- [x] CI：web job 接入（pnpm + tsc + vite build）
- [x] `make web` 后集成构建：真实 SPA embed 的 hub 冒烟（healthz/SPA fallback/assets 强缓存/api 401 全过）
- [x] docker build + 容器 healthz/SPA/api 三探通过（发现并修 /data 卷属主与容器内 GOPROXY 两坑）
- [x] 全量门禁：`go mod tidy`（零漂移）、build/vet/test、race（forward+e2e）、gofmt、pnpm build、
  cargo build+clippy
- [x] commit + push + CI 确认（d241f72，run 28918948641 success，含首跑 web job）

## 验证命令

```bash
GO=~/sdk/go1.26.4/bin/go
$GO build ./... && $GO vet ./... && $GO test ./... && $GO test ./internal/agent/forward/ ./internal/e2e/ -race
cd web && pnpm build          # tsc --noEmit && vite build
cd desktop/src-tauri && PATH=$HOME/.cargo/bin:$PATH cargo build
docker build -t switchapi-hub . && docker run --rm -d -p 18080:8080 switchapi-hub && curl -f localhost:18080/healthz
```

## 遗留（验收后人工/后续里程碑）

- 手机浏览器 + 桌面壳双端同开 1s 可见性实测（两机部署后）
- 三平台桌面打包实机验证、updater minisign 启用、Windows 服务 UAC 细节（M4/发布期）
