# M3 双端体验（PRD）

> 父任务：`07-02-switchapi-mvp-plan`（prd.md §4 MVP #2/#7、§5 验收、implement.md M3 节）。
> 技术基准：父任务 `design.md` §3（接口面）/§8（桌面壳）/§9（交付物）；`research/07`（Tauri 实证）。

## Goal

一套 React SPA 双端复用：Hub embed 托管给浏览器（含手机），Tauri 2 壳装载同一 Hub URL；
ws/ui 实时通道让任一端的切换/新用量在其他端 1 秒内可见；补齐 Docker 部署与桌面打包骨架。

## Requirements

1. **ws/ui 实时通道**（Hub）：Session 鉴权 WebSocket `/api/v1/ws/ui`；下行
   `state_changed`（切换后）、`event`（新事件）、`usage_tick`（用量批次入库后）；多客户端并发、
   断开自动清理、CloseAll 接入优雅退出。
2. **Web SPA**（`web/`，React+TS+Vite+Tailwind+shadcn/ui）：
   - 登录（含首登引导设密码提示）/登出；401 全局跳转登录
   - 供应商管理：列表/新建（预设模板）/编辑/删除、协议标识、折扣系数、模型重定向、
     模型覆盖价占位（M2 API 已有 pricing_overrides 表——本期只读展示可缺省）、备选序列拖拽排序
   - 一键切换：per-App 当前供应商高亮 + 单击切换（协议匹配过滤）
   - 用量仪表盘：summary 卡片（请求/token 四分项/费用+未知费用标注）、trend 图（时/日切换）、
     breakdown（供应商/模型/App/设备维度）
   - 用量明细：分页表格 + 筛选（时间/app/供应商/模型/设备）
   - 设备管理：列表（last_seen/平台）、生成配对码（展示 TTL）、吊销
   - 事件时间线：分页展示 switch/pairing 等事件
   - ws/ui 客户端：自动重连；收 `state_changed`/`usage_tick`/`event` 后精准刷新对应数据
   - 移动端可用（响应式布局，手机浏览器为一等公民）
3. **Hub embed**：SPA 构建产物嵌入 `switchapi-hub` 单二进制；SPA fallback 路由不影响
   `/api/*`、`/healthz`；无 node 环境时 `go build` 仍可过（占位页）。
4. **Tauri 桌面壳**（`desktop/`）：本地引导页 + 首启向导（Hub 地址）→ Rust 侧 `/healthz` 探测 →
   运行时 remote capability 注入 → navigate(Hub)；托盘（当前供应商展示 + 快切 + 打开主窗）；
   autostart、single-instance；Agent 托管（定位包内二进制→复制稳定路径→install/start/stop/status/
   版本对齐）；Hub 不可达应急页（Tauri command 由 Rust 代取本机 Agent 缓存状态）。
5. **部署与打包**：Hub Dockerfile（多阶段：web 构建→go 构建→运行镜像，/data 卷）+ compose 示例 +
   部署文档；CI 接入 web 构建；桌面打包配置齐备（externalBin 携带 agent、三平台 bundle 配置），
   本机验证 Linux `cargo build`。

## Constraints（安全与既有决策）

- Session cookie：Max-Age 必设、Secure 必不设（research/07；已在 M1 实现，勿回退）。
- API key 永不明文出 API（key_last4）；SPA 编辑供应商时留空=保持原 key。
- Agent 仅 127.0.0.1；应急页数据走 Tauri command（Rust 代取），不开 CORS。
- 壳不用 shell 插件 spawn 常驻 Agent（退出即杀）；externalBin 仅分发（research/07 (b)）。
- updater 插件仅留配置骨架，minisign 签名密钥与启用推迟到发布期（M4/release）。

## Acceptance Criteria

- [ ] e2e 自动化：两个 ws/ui 客户端并发在线，REST `POST /switch` 后两端均在 1s 内收到
  `state_changed`；usage_batch 入库后收到 `usage_tick`；未登录 ws/ui 升级被拒（401）。
- [ ] `switchapi-hub` 单二进制直接服务 SPA：登录→供应商 CRUD→切换→仪表盘/明细/设备/事件全页可用
  （构建产物 embed；`go test ./...` 全绿）。
- [ ] SPA 生产构建无 TS 错误（`pnpm build`）；无 node 时 `go build ./...` 仍通过。
- [ ] Tauri 壳 Linux `cargo build` 通过；首启向导→登录→托盘快切→应急页代码路径完整
  （三平台实机打包与 Windows UAC 路径列入遗留）。
- [ ] `docker build` 出镜像且 `/healthz` 探活通过；部署文档可照做。
- [ ] CI 全绿（新增 web 构建步骤 + 既有 go 矩阵）。

## 遗留（不阻塞本期）

- 手机浏览器 + 桌面壳双端同开实测（1s 可见性）——两机部署后的人工验收。
- 三平台桌面打包实机验证、updater 签名启用、Windows 服务 UAC 细节（M4/发布期）。
