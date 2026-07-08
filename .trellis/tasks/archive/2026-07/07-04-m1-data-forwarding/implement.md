# M1 — 执行计划（implement.md）

> 波次间有真实依赖（W2 依赖 W1 的 shared/store/forward），波内并行、文件所有权互斥。
> 所有实现代理：禁止 git commit；禁止 `go mod tidy`（依赖已预置）；Go = `~/sdk/go1.26.4/bin/go`。

## W1（并行 ×2）✅ 完成（2026-07-04，主会话直接实现——子代理连续 429）

- [x] **W1-A hub 数据与共享层**：wire 消息、cryptoutil（AES-GCM/argon2id/token）、store（10 表迁移 + DAO）；测试全绿
- [x] **W1-B agent 转发器**：原型演化（原子路由表、双头校验、协议注入、identity 强制）；8 测试 + race 全绿

## W2（并行 ×2，W1 全绿后）✅ 完成（同上，主会话实现）

- [x] **W2-C hub API + 实时通道**：auth 引导式首登、providers CRUD（密文落库/last4 掩码）、switch 校验+事件+广播、
  配对（一次性码→哈希 token）、ws/agent（快照推送/rev 递增/心跳踢除/吊销即断）+ CloseAll 优雅关闭；handler+WS 测试全绿
- [x] **W2-D agent 接入与接管**：hubclient（快照 0600/退避重连/断连仍转发）、appconfig（CC 手术合并/Codex TOML+auth.json/
  dry-run/备份/回滚/冲突清单）、cli（kardianos user-service/pair/config/run 回环强制）；测试全绿

## W3（串行）✅ 完成

- [x] **W3-E e2e 集成测试**：配对→推送→流式命中 A→切换→命中 B→杀 Hub 仍转发→重启 1s 重连→再切换生效
  + DB 无明文密钥断言 + 状态文件 0600 断言（1.3s 跑完）；顺带修复 Hub 优雅退出不断开 hijacked WS 的真实缺口
- [x] **W3-F 全量检查**（主会话执行）：`go mod tidy` 收尾、全仓 build/vet/test + race、gofmt 清洁、
  真实 `~/.claude/settings.json`/`~/.codex/*` 的 dry-run 验收通过（含冲突警告与 token 脱敏）
- 遗留：验收最后一条"两台机器手动验收"待真实部署时执行；`.trellis/spec/backend` 规范回填待做（trellis-update-spec）

## 验证命令

```bash
GO=~/sdk/go1.26.4/bin/go
$GO build ./... && $GO vet ./... && $GO test ./...
```

## 回滚点

每波结束 = 一个可回滚点（波内失败只回滚该波所有权目录）。
