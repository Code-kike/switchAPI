# switchAPI 部署指南

> 架构回顾（ADR-0001）：**Hub** 部署在常开服务器（内网/Tailscale 可达），是唯一权威数据源；
> 每台开发机安装 **Agent**（本机 127.0.0.1:9527 守护进程），AI 流量直连供应商、不绕经 Hub。
> 安全模型（ADR-0005）：内网明文 HTTP + 密码登录 + 设备 token，不要把 Hub 直接暴露公网。

## 1. 部署 Hub

### 方式 A：Docker（推荐）

```bash
git clone https://github.com/Code-kike/switchAPI && cd switchAPI
docker compose up -d          # 构建镜像并启动，数据落 ./data
curl -f http://127.0.0.1:8080/healthz
```

自行构建镜像：`docker build -t switchapi-hub --build-arg VERSION=v0.x.y .`
运行要点：把 `/data` 挂到持久卷（SQLite、master.key、备份都在里面）；对外只需开放 8080。

### 方式 B：裸机二进制

```bash
make web && make build        # 先构建 SPA 并 embed，再出二进制（需 node22+pnpm、Go 1.25+）
./dist/switchapi-hub -listen :8080 -data /var/lib/switchapi
```

不执行 `make web` 也能跑：API/统计/推送全部可用，仅 Web 控制台显示占位页。

### 首次登录

浏览器打开 `http://<hub 主机>:8080`，**首次输入的密码即被设为管理员密码**（引导式首登）。
先到「供应商」页添加中转站/官方 API（协议、base_url、密钥），再做设备接入。

## 2. 接入设备（每台开发机）

```bash
# 1) 安装 agent 二进制（发布包下载或 make agent 自编译），放入 PATH
# 2) 在 Web 控制台「设备」页生成 6 位配对码（10 分钟有效），然后：
switchapi-agent pair --hub http://<hub 主机>:8080 --code <配对码> --name <设备名>
# 3) 接管 CC/Codex 配置（写前自动备份，可 rollback）：
switchapi-agent config apply          # --dry-run 先看 diff
# 4) 注册为系统服务并启动（Linux systemd --user / macOS LaunchAgent）：
switchapi-agent install && switchapi-agent start
```

验证：`switchapi-agent status`；随后 Claude Code / Codex 的下一个请求即经本机 Agent 代理，
Web 控制台「用量」页应出现明细记录。切换供应商在任一端操作，全设备下一请求即生效。

注意事项：
- Linux 注销后仍需 Agent 运行时：`loginctl enable-linger $USER`。
- Codex 属启动时读配置，接管/切换后需重启运行中的 Codex 会话（CC 热生效无需重启）。
- Hub 宕机不影响各设备正常使用（Agent 走缓存路由），恢复后用量自动补报。

## 3. 桌面壳（可选）

桌面壳（Tauri）装载同一 Web 控制台并提供托盘快切、Agent 托管与应急页；
安装包见 Releases（`desktop/` 目录可自行 `tauri build`）。首启向导中填 Hub 地址即可。
