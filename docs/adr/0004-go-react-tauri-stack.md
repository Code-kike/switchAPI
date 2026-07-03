# 技术栈：Go 后端（Hub 与 Agent 同仓双二进制）+ React 前端 + Tauri 桌面壳

Agent 的硬约束是跨平台单二进制、注册为系统服务、低内存常驻透传 SSE 流——Go 在这些点上最省事（交叉编译、kardianos/service、纯 Go SQLite 免 CGO），且 Hub 可将前端产物 embed 成单文件部署。前端一套 React + TypeScript + Vite + Tailwind + shadcn/ui 的 SPA 同时服务 Web 控制台与桌面壳；桌面壳用 Tauri 2，只做 WebView 装载、托盘、开机自启与 Agent sidecar 托管。

## Considered Options

- **全 Rust（axum + Tauri）**：语言最统一、性能最优，但胶水活（WebDAV、价格表解析、余额适配器）迭代慢。被拒。
- **全 TypeScript（Hono + Electron/Tauri）**：可大量参考 claude-code-hub 实现，但 Agent 常驻内存占用高，单二进制分发与系统服务安装体验差，与"每机小守护进程"定位不匹配。被拒。

## Consequences

仓库内 Go / TS / 少量 Rust 三种语言并存，这是接受的代价；Tauri 壳代码量控制在最薄。
