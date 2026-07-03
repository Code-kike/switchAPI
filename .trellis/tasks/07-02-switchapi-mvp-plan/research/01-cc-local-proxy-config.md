# Research: #01 Claude Code 指向本地代理的配置写法与热生效（cc-local-proxy-config）

- **Query**: prd.md 研究项 #1 — CC 指向本地代理的 settings.json env 写法、鉴权头语义、优先级链、热生效行为、流量覆盖面、安全写入策略
- **Scope**: mixed（官方文档 + SDK 源码 + 本机 CC v2.1.199 二进制勘察 + 本机实盘 settings + cc-switch 源/README）
- **Date**: 2026-07-03（主会话直接执行；两次子代理运行均因基础设施故障失败）
- **对应 design.md 标注**: `[研究#1]`（§4 Agent 设计 · 路由与鉴权链；internal/agent/appconfig）

---

## 结论

### C1. 目标写法：用户级 `~/.claude/settings.json` 的 `env` 块，两个键即可 【置信度: High】

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:9527/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "<switchAPI 本地 token>"
  }
}
```

本机实盘样本即此形态（cc-switch 写入：`ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_BASE_URL=https://anyrouter.top`，结构同构，仅目标不同）。官方对 `env` 的定义：*"Environment variables applied to every session and to subprocesses Claude Code spawns from it."*

### C2. 鉴权头语义：`ANTHROPIC_AUTH_TOKEN` → `Authorization: Bearer`；`ANTHROPIC_API_KEY` → `x-api-key`；至少其一否则 SDK 抛错 【置信度: High】

- SDK 源码：`authToken` 默认读 `ANTHROPIC_AUTH_TOKEN`（client.ts L322/497），构造 `Authorization: Bearer ${authToken}`（L840）；`apiKey` 默认读 `ANTHROPIC_API_KEY`（L312/496），发 `x-api-key` 头（L782）。
- `validateHeaders`（L772-792）：请求头中 `x-api-key` 与 `authorization` 均缺失时抛 *"Could not resolve authentication method"* —— **静态本地 token 的必要性由此成立**（不能留空）。
- 本机 CC v2.1.199 二进制内嵌同款逻辑（字符串勘察逐句吻合）。
- `apiKeyHelper` 生成的值会**同时**以 `X-Api-Key` 和 `Authorization: Bearer` 两个头发送（官方文档原文）。
- **推论**：Agent 校验本地 token 应同时接受两种头（防御性），剥离后按供应商协议注入上游。

### C3. 优先级链：Managed（不可覆盖）> `.claude/settings.local.json` > 项目 `.claude/settings.json` > 用户 `~/.claude/settings.json` 【置信度: High】

值型设置高优先级整体覆盖（官方示例：项目值覆盖用户值）；permission 类跨层合并。SDK 层显式构造参数 > env 读取。settings `env` 与 shell 层 `export` 的相对优先级官方未明示 → 遗留#1（缓解：安装时检测 shell 环境并告警）。

### C4. 热生效：settings 文件被 watch，改动即热重载；重启例外清单只有 `model` 与 `outputStyle` —— `env` 块热生效 【置信度: High】

官方文档原文：*"Claude Code watches your settings files and reloads them when they change, so edits to most keys apply to the running session without a restart... A few keys are read once at session start: `model`, `outputStyle`."* 三重印证：cc-switch README FAQ（*"The exception is Claude Code, which currently supports hot-switching of provider data without a restart"*）+ 本机二进制 per-request `[API:request] Creating client` 日志。
**对本设计的意义**：不仅"一次性写入"成立，**接管安装那一刻运行中的 CC 会话也立即改走 Agent，无需重启**（Codex 则需重启会话，见研究#2）。切换供应商本就在 Agent 内部完成，CC 侧配置自安装后永不再改。

### C5. 流量覆盖面：全部推理 API 调用（messages / count_tokens）走 `ANTHROPIC_BASE_URL` 【置信度: High】

SDK `baseURL`（L368/509）统辖所有 API 方法；cc-switch 整个产品即依赖此机制运作（本机 9 个 claude 供应商日常切换实盘验证）。console/OAuth 侧流量（如 `claude_cli_profile`）不经 relay，且与推理无关；本机已设 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`。`count_tokens` 免费 → 与研究#3 C8 一致：透传但不产生 usage_records。

### C6. 安全写入策略：手术式最小合并，绝不整文件替换 【置信度: High】

本机实盘 settings.json 是重度共享文件（hooks、permissions、statusLine、plugins、GitHub PAT 等 20+ 无关键）。appconfig 必须：读-改-写只动 `env` 内两个键 → 写前时间戳备份 → temp+rename 原子替换。对照组：cc-switch 采用整文件替换模型（provider 行存整份 settings.json），正是本设计要避免的 clobber 模式。

### C7. 与 OAuth 登录的交互 【置信度: Medium】

env 凭据（`ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY` / `apiKeyHelper`）存在时作为会话鉴权源（官方 `forceLoginMethod` 文档将三者并列描述为独立于 OAuth 登录的鉴权路径）。恢复官方 = 移除 env 两键（恢复备份）+ `/login`。

### C8. 安装时冲突检查清单（appconfig 规格） 【置信度: High】

| 检查项 | 动作 |
|---|---|
| `apiKeyHelper` 已设置 | 警告：其值会以双头发送，可能与本地 token 冲突；建议移除或确认 |
| `CLAUDE_CODE_USE_BEDROCK/VERTEX`、`ANTHROPIC_FOUNDRY_*` | 检测到则拒绝接管（流量不走 BASE_URL） |
| shell 层 `export ANTHROPIC_BASE_URL/AUTH_TOKEN/API_KEY` | 警告（优先级未官方明示，遗留#1） |
| `HTTP(S)_PROXY` 存在 | 确认 `NO_PROXY` 含 `127.0.0.1`，否则回环请求可能被送进代理 |
| `ANTHROPIC_CUSTOM_HEADERS` | 无需处理：以请求头形态到达 Agent，直通转发天然保留 |

---

## 证据与来源

1. [Anthropic, Claude Code Settings 官方文档, 取阅 2026-07-03, https://code.claude.com/docs/en/settings.md] — `env` 定义、热重载条款与例外清单（仅 model/outputStyle）、`apiKeyHelper` 双头语义、Managed 优先级表、`forceLoginMethod` 对 env 凭据的鉴权路径描述。（注：docs.claude.com 域名在本机网络不可达，code.claude.com 可达）
2. [Anthropic, anthropic-sdk-typescript `src/client.ts`, 取阅 2026-07-03, https://raw.githubusercontent.com/anthropics/anthropic-sdk-typescript/main/src/client.ts] — L312/322/368/496-521（三个 env 变量默认值）、L772-792（validateHeaders 鉴权互斥校验）、L824/840（Bearer 头构造）
3. 本机 CC v2.1.199（`~/.local/share/claude/versions/2.1.199`，ELF）字符串勘察（READ ONLY）— 内嵌 SDK 同款 validateHeaders/双头构造、鉴权源枚举（none/ANTHROPIC_AUTH_TOKEN/CLAUDE_CODE_OAUTH_TOKEN/apiKeyHelper）、`[API:request] Creating client, ANTHROPIC_CUSTOM_HEADERS present` 日志、全量 `ANTHROPIC_*` 环境变量表
4. 本机 `~/.claude/settings.json` 实盘（cc-switch 写入形态；token 已脱敏 last4=eKHn）
5. [farion1231, cc-switch README（FAQ L252-254、使用说明 L322）, 取阅 2026-07-03, https://raw.githubusercontent.com/farion1231/cc-switch/main/README.md] — "Claude Code 热切换无需重启，其余工具需重启"
6. 交叉引用：`research/05-cc-switch-format.md`（settings_config 键链 ANTHROPIC_AUTH_TOKEN→ANTHROPIC_API_KEY→…）、`research/06-go-sse-passthrough.md` 补录（Bearer 注入对真实中转站实测 200）

---

## 对 design.md 的影响

**判定：confirmed** —— §4 `[研究#1]` 基线假设（`ANTHROPIC_BASE_URL=http://127.0.0.1:9527/anthropic` + 本地 token + 安装时一次性写入）全部成立，且获得升级：CC 端接管与后续一切切换均**热生效**。

需要增补三点：
1. **§4 鉴权链**：Agent 本地 token 校验同时接受 `Authorization: Bearer` 与 `x-api-key` 两种头（C2/C8）。
2. **§4 appconfig**：写入规格 = 手术式合并（只动 env 两键）+ 时间戳备份 + 原子替换 + C8 冲突检查清单；CC 写 `~/.claude/settings.json`（用户级），不碰项目级。
3. **安装/卸载文档口径**：CC 无需重启即接管/恢复；Codex 需重启会话（与研究#2 的结论合并进安装向导文案）。

---

## 遗留不确定性

1. **settings `env` vs shell `export` 的优先级**（Low 影响）：官方未明示。缓解已入 C8 检查清单；M1 appconfig 集成测试补一条实测。
2. **热重载对"进行中请求"的边界**（无影响）：改动是否影响当前流式请求未单测——本设计切换在 Agent 内部，CC 侧配置安装后不再变化。
3. **managed-settings 覆盖**（不适用）：企业管控可锁死 env；单用户个人机场景不涉及，文档标注即可。
