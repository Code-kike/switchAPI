# web — Web 控制台（React SPA）

一套 SPA 双端复用（ADR-0004）：Hub embed 托管给浏览器（含手机），Tauri 壳装载同一 Hub URL。

栈：Vite + React + TypeScript + Tailwind 4 + shadcn/ui + TanStack Query + react-router +
recharts + dnd-kit。页面：登录 / 仪表盘（切换+统计）/ 供应商 / 用量明细 / 设备 / 事件。
实时：`src/ws.ts` 连 `/api/v1/ws/ui`，三类失效通知（state_changed / usage_tick / event）
触发 react-query refetch。TS 类型以 Go handler JSON 为唯一真源（`src/api/types.ts`）。

```bash
pnpm install
pnpm dev        # 代理 /api 与 /healthz 到本地 Hub（127.0.0.1:8080，见 vite.config.ts）
pnpm build      # tsc -b && vite build → dist/
```

集成进 Hub 二进制：仓库根目录 `make web`（构建并拷入 `internal/hub/webui/dist`），
之后 `make build`。费用展示约定：cost=null 一律显示"未知"，绝不显示 0。
