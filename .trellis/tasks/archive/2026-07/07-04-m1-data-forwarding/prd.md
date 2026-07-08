# M1 数据与转发内核

> 父任务：`07-02-switchapi-mvp-plan`（milestone M1）。范围来自父 `implement.md` M1 节；
> 技术方案以父 `design.md`（已含 M0 研究结论）为权威，本任务 `design.md` 只补 M1 特有决策。

## Goal

打通"数据 → 配置分发 → 转发"的最小闭环：Hub 持有权威数据并可切换供应商，
Agent 接收配置推送并把 CC/Codex 的请求直通转发到当前供应商——不含统计、UI、故障切换。

## Requirements

1. **Hub store 层**：父 design.md §2 全部表 + 嵌入式 SQL 迁移（PRAGMA user_version）；
   API Key AES-GCM 加密落库（主密钥首启生成，0600）。
2. **Hub REST**（父 design.md §3）：auth（argon2id + Session Cookie，Max-Age、不设 Secure）、
   providers CRUD、presets、`POST /switch`（写 app_state + events + 广播）、fallback-order GET/PUT、
   devices + pairing-code、healthz。
3. **Hub ws/agent 通道**：设备 token 鉴权、hello → 全量 config_push、切换后增量 config_push、心跳；
   一次性配对码（TTL 10min）→ 长期设备 token（哈希落库、可吊销）。
4. **Agent 转发器**：演化 `research/06-sse-prototype`——按协议前缀路由（`/anthropic/*`、`/openai/v1/*`，
   剥离规则见父 design.md §4 表）、双头本地 token 校验、上游 key 注入、模型重定向、
   条件化超时旋钮；**不含 usage 采集**（M2，但结构上保留 tee 挂点）。
5. **Agent 接入**：hubclient（pair、WS 连接、指数退避重连）、config 快照缓存
   （agent-state.json 0600）、断连时用缓存继续转发（临时降级完整逻辑属 M4，本期只要"断 Hub 不断转发"）。
6. **Agent CLI 与服务**：`agent install|uninstall|start|stop|pair`（kardianos/service；
   Linux user unit / macOS LaunchAgent；Windows 路径按研究#7 预留）。
7. **appconfig**：一次性接管 CC（settings.json env 两键手术式合并）与 Codex
   （config.toml 供应商块 + auth.json）——备份、原子写、可回滚、冲突检查清单（research/01 C8、research/02）。

## Out of Scope（本任务不做）

usage 采集/上报/计价（M2）、Web UI 与桌面壳（M3）、故障切换/健康仲裁/测速/备份导入（M4）、
`provider_health` 表的仲裁逻辑（建表即可，逻辑 M4）。

## Acceptance Criteria

- [ ] `make vet test build` 全绿；e2e Go 测试：起 hub + agent + 两个 fake upstream，
  走通 配对 → config_push → 经 Agent 流式请求命中 upstream A → `POST /switch` →
  下一请求命中 upstream B（进行中请求不中断）
- [ ] SSE 直通质量不回退：沿用原型的间隔保持/断连传播/O(1) 内存测试并全部通过
- [ ] 密钥在 DB 文件中不可见明文（AES-GCM）；agent-state.json 权限 0600
- [ ] appconfig 对本机真实 `~/.claude/settings.json` 的 dry-run 输出正确 diff（不实际写入也算过）
- [ ] 断开 Hub 后 Agent 继续按缓存转发；恢复连接后自动重连并对齐配置
- [ ] 手动验收（部署后）：两台机器配对，Hub 上 curl `POST /switch`，两机 CC 下一请求走新供应商，无需重启终端

## Notes

- 依赖已预置进 go.mod（modernc.org/sqlite、coder/websocket、kardianos/service、x/crypto、google/uuid）；
  实现代理**禁止运行 `go mod tidy`**（收尾 check 统一 tidy 一次）。
- 本机 Go 工具链：`~/sdk/go1.26.4/bin/go`（不在 PATH）。
