# M1 — 技术设计补充（design.md）

> 权威方案 = 父任务 `07-02-switchapi-mvp-plan/design.md`（v2，已回写研究结论）。
> 本文只记 M1 特有的落地决策，避免复读。

## 1. 包布局与文件所有权（多代理并行的边界）

```
internal/
├── shared/
│   ├── wire/            # WS 消息类型：Hello、ConfigPush、Heartbeat、（占位 UsageBatch）
│   ├── cryptoutil/      # AES-GCM seal/open、主密钥生成与加载(0600)、token 生成/哈希
│   └── version/         # 已存在
├── hub/
│   ├── store/           # sqlite 打开(modernc)、migrations/*.sql 嵌入、各表 DAO、事务助手
│   ├── api/             # http.ServeMux(Go1.22 pattern)、auth 中间件、各资源 handler
│   └── realtime/        # ws/agent：连接注册表、config_push 广播、心跳超时
└── agent/
    ├── forward/         # 转发器（从 research/06-sse-prototype 演化，去掉演示 main）
    ├── hubclient/       # 配对 + WS 客户端 + 重连退避 + 配置快照落盘
    ├── appconfig/       # CC/Codex 接管（研究#1/#2 规格）
    └── cli/             # kardianos 服务注册 + 子命令
cmd/hub/main.go          # wiring：flags(listen、data-dir) → store → api+realtime
cmd/agent/main.go        # wiring：子命令分发 → 服务运行体（forward + hubclient）
```

## 2. M1 特有决策

- **依赖锁定**：modernc.org/sqlite（ADR-0003 无 CGO）、github.com/coder/websocket（nhooyr 后继，
  context 风格）、github.com/kardianos/service、golang.org/x/crypto（argon2id/scrypt）、github.com/google/uuid。
- **迁移机制**：`store/migrations/0001_init.sql`… 按序执行，`PRAGMA user_version` 记录版本；
  连接参数 `_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`。
- **主密钥**：`<data-dir>/master.key`（32B random，0600）首启生成；api_key_enc = AES-256-GCM
  （nonce 前置存储）。
- **Session**：内存 session 表（重启失效可接受，M1）+ Cookie `switchapi_session`
  （HttpOnly、SameSite=Lax、Max-Age=30d、**不设 Secure**——研究#7）。
- **设备 token**：32B random hex 一次性下发，库存 SHA-256 哈希；WS 握手用
  `Authorization: Bearer <token>` 头。配对码 6 位数字，TTL 10min，一次性。
- **config_push 载荷**：全量快照 `{apps: {claude-code: {provider…}, codex: {…}}, fallback_orders, rev}`
  ——rev 单调递增，Agent 以 rev 判断新旧；增量推送也发全量快照（数据量小，简化对账）。
- **转发器演化**：prototype 的 proxy.go/patch.go 平移进 `agent/forward`，改动：
  ①路由表由 hubclient 原子替换（atomic.Pointer[RoutingTable]）；②双头 token 校验；
  ③按协议注入上游鉴权头（anthropic: x-api-key + anthropic-version 透传；openai: Bearer）；
  ④条件化超时（stream 判定：请求体 `"stream":true` 或 Accept: text/event-stream）；
  ⑤usage tee 挂点保留为 no-op 接口（M2 填充）。
- **appconfig dry-run 优先**：`agent config apply --dry-run` 输出 diff；实际写入走
  备份(`<file>.switchapi-bak-<ts>`)→临时文件→rename；`--rollback` 恢复最近备份。
- **测试基线**：迁移幂等测试；DAO CRUD 测试；api handler 测试（httptest）；
  forward 沿用原型 4 测试 + 路由/双头/注入新测试；e2e 见 prd 验收第一条。

## 3. 端口与默认值

Hub 默认 `:8080`（flag `-listen`，data dir flag `-data`，默认 `~/.switchapi-hub/`）；
Agent 转发口 `127.0.0.1:9527`（可配）；Agent 状态目录 `~/.switchapi/`。
