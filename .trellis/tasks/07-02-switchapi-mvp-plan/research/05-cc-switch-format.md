# Research #05: cc-switch 现行数据格式与导入映射表

- **Query**: cc-switch (github.com/farion1231/cc-switch) 当前磁盘数据格式 → switchAPI 一键导入器（M4）字段映射
- **Scope**: mixed（上游仓库源码 + 本机 ~/.cc-switch 实测）
- **Date**: 2026-07-03
- **对应 design.md 标注**: §7 `[研究#5]`

---

## 结论

### C1. 现行存储格式 = SQLite，不再是 config.json（置信度：High）

- 上游 cc-switch **v3.8.0（2025-11-28）起**弃用单一 `config.json`，改为 **`~/.cc-switch/cc-switch.db`（SQLite）**；首次启动自动迁移旧 JSON 并将其改名为 `config.json.migrated` 留档。
- 当前最新版 **v3.16.5（2026-07-01）**，SQLite `PRAGMA user_version = 11`（`SCHEMA_VERSION` 常量）。迭代极快（3.16.x 一个月内 3 个 patch），schema 版本还会继续涨。
- 配置目录可被 `~/.cc-switch/app_paths.json` 的 `app_config_dir_override` 字段重定向 —— **importer 必须先读该文件**（本机即存在此文件，值恰好还是 `/home/orion/.cc-switch`）。

### C2. providers 表结构与 settings_config 内嵌格式（置信度：High）

`providers` 表（主键 `(id, app_type)`，id 为 UUID 字符串）：

```sql
CREATE TABLE providers (
    id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
    settings_config TEXT NOT NULL,          -- JSON，按 app 形状不同（见下）
    website_url TEXT, category TEXT,        -- category: 'official'|'aggregator'|'custom'|...
    created_at INTEGER,                     -- 毫秒时间戳
    sort_index INTEGER, notes TEXT, icon TEXT, icon_color TEXT,
    meta TEXT NOT NULL DEFAULT '{}',        -- JSON ProviderMeta（camelCase 字段）
    is_current BOOLEAN NOT NULL DEFAULT 0,  -- 每 app_type 至多一行为 1
    in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
    cost_multiplier TEXT NOT NULL DEFAULT '1.0',   -- v2 迁移遗留列，运行时几乎不写
    limit_daily_usd TEXT, limit_monthly_usd TEXT, provider_type TEXT,
    PRIMARY KEY (id, app_type)
);
-- 伴随表 provider_endpoints(provider_id, app_type, url, added_at)：多端点测速候选
```

`app_type` 取值：`claude` / `codex` / `gemini` / `claude-desktop` / `opencode` / `openclaw` / `hermes`。**我方只关心前两种。**

**claude 记录的 settings_config** = 一份完整的 Claude Code `settings.json` 快照（切换时整体写入 `~/.claude/settings.json`）：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://...",          // ← base_url
    "ANTHROPIC_AUTH_TOKEN": "sk-xxx（明文）",      // ← api key（主字段）
    "ANTHROPIC_MODEL": "...", "ANTHROPIC_DEFAULT_OPUS_MODEL": "...", // 模型钉扎（客户端语义）
    "...其余任意用户 env..."
  },
  "hooks": {...}, "permissions": {...}, "enabledPlugins": {...}, "statusLine": {...}, "...": "..."
}
```

上游官方 key 提取链（`provider.rs::resolve_usage_credentials`）：`env.ANTHROPIC_AUTH_TOKEN → ANTHROPIC_API_KEY → OPENROUTER_API_KEY → GOOGLE_API_KEY`，取**首个非空**（预设会播种"存在但为空"的占位字段，不能只判存在性）。

**codex 记录的 settings_config** = auth.json 内容 + config.toml 全文字符串：

```json
{
  "auth": { "OPENAI_API_KEY": "sk-xxx（明文）" },
  "config": "model = \"gpt-5.5\"\nmodel_provider = \"anyrouter\"\n[model_providers.anyrouter]\nbase_url = \"https://.../v1\"\nwire_api = \"responses\"\n...（含 mcp_servers/projects 等无关段）"
}
```

上游官方 base_url 提取（`codex_config.rs::extract_codex_base_url`）：解析 TOML，取**顶层 `model_provider` 指向的 `[model_providers.<id>].base_url`**，回退顶层 `base_url`；**绝不读非激活的 model_providers 段**。key 提取：`auth.OPENAI_API_KEY`，回退 config 内 `experimental_bearer_token`。

**meta（ProviderMeta，camelCase）关键字段**：`costMultiplier`(字符串十进制)、`apiFormat`("anthropic"|"openai_chat"|"openai_responses"，claude 专用)、`providerType`("codex_oauth"|"github_copilot")、`authBinding`、`usage_script`、`endpointAutoSelect`、`commonConfigEnabled`、`customUserAgent`、`localProxyRequestOverrides` 等。

**当前供应商**：`providers.is_current=1`（每 app 一行），settings.json 里 `currentProviderClaude` / `currentProviderCodex` 是上游自带的冗余镜像（本机两处一致）。
**故障转移序列**：`in_failover_queue=1` 的行按 `ORDER BY COALESCE(sort_index, 999999), id ASC`（`dao/failover.rs`）。

### C3. API Key 为明文存储，且 DB 文件权限宽松（置信度：High）

- settings_config JSON 内的 key 全部**明文**；上游 database 层无任何加密（keyring/AES 均未使用；代码中 encrypt 命中仅为代理转发的 TLS/内容转换）。
- 本机实测：`cc-switch.db` 权限 **644（组/其他可读）**，settings.json 为 600。DB 内除供应商 key 外，claude settings_config 还夹带 `GITHUB_PERSONAL_ACCESS_TOKEN` 等第三方敏感 env —— 导入器**只提取所需字段，绝不整段搬运 settings_config**。

### C4. 历代格式与迁移链（importer 可能遇到的全部形态）（置信度：High）

| 代际 | 版本区间 | 位置/形态 | 结构要点 |
|---|---|---|---|
| v1 JSON | ≤ v3.1.x（2025-09 前） | `~/.cc-switch/config.json` | 顶层直接 `{providers: {id: Provider}, current: "id"}`，仅 claude 一个 app |
| v2 JSON | v3.2.0 – v3.7.1（2025-09-13 ~ 2025-11-22） | 同上 | `{"version": 2, "claude": {providers, current}, "codex": {...}, "mcp": ..., "prompts": ..., ...}` —— app 管理器 serde flatten 在顶层，**不是**嵌套在 "apps" 键下 |
| SQLite | v3.8.0+（2025-11-28 起，现行） | `~/.cc-switch/cc-switch.db` | 上表 schema；内部 user_version 0→11 链式迁移（v2 加统计/成本列，v4 OpenCode，v9-10 Hermes，v11 rollup 加 request_model 维度） |

- v1→v2 自动迁移**仅存在于 v3.2.x**；现行版本检测到 v1 直接报错拒载（提示装 3.2.x 或手改）。
- JSON→SQLite 迁移后遗留物：`config.json.migrated`（内容=v2 JSON）、更早的 `config.v1.backup.<ts>.json`、`config.json.bak`。
- 迁移时 `meta.custom_endpoints` 被抽出到 `provider_endpoints` 表；`cost_multiplier` 等列由 v1→v2 schema 迁移补齐。

### C5. 本机 ground truth（置信度：High，2026-07-03 实测，只读）

- 用户实际运行的是**自研 fork `Code-kike/cc-switch-web`**（基于上游 3.16.2 的 Web 部署形态，README 明确"复用 ~/.cc-switch 数据"，二进制 `~/.local/bin/cc-switch-web` 常驻），**不是**上游 Tauri 桌面版。
- `~/.cc-switch/` 现状：`cc-switch.db`（10.4MB，大头是 proxy_request_logs 用量日志）+ `settings.json`(600) + `app_paths.json` + `backups/`（10 份 `db_backup_*.db` 轮转）+ `skills/`。**无 config.json / config.json.migrated** —— SQLite 原生安装，无 JSON 遗留。
- DB `user_version = 10`（fork 基于 3.16.2，尚未有上游 v11 的 rollup 改动）；**providers 表列集与上游 v3.16.5 完全一致**（实测 schema 相同）。
- 存量：claude 9 条（current=anyrouter/linuxdo 账号）、codex 11 条（current="local any" → `http://127.0.0.1:23000/v1`）、另有 gemini 1 条、hermes 1 条（不导入）。claude 侧 5 条、codex 侧 5 条在 failover 队列中。
- 值得注意的存量形态（导入器测试用例来源）：
  - 同一站点（anyrouter.top）多账号 = 多条记录复用同一 base_url —— 合法，逐条导入；
  - `Nvidia` claude 记录 `meta.apiFormat = "openai_chat"`（依赖 cc-switch 代理做协议转换）；
  - `Local (127.0.0.1:23000)` / `local any` 指向本机端口 23000 的另一层代理（回环风险）；
  - codex 的 config TOML 里混有 `mcp_servers`、`projects.*.trust_level`，且个别 MCP env 内嵌第三方 accessToken。

---

## 字段映射表（cc-switch → switchAPI providers，design.md §2）

**筛选谓词**：只取 `app_type IN ('claude','codex')` 的行；claude → `protocol='anthropic'`，codex → `protocol='openai'`。这与我方"同一中转站两协议端点 = 两条 Provider 记录"的模型一一对应，无需拆合。

| switchAPI 字段 | claude 行来源 | codex 行来源 | 处理 |
|---|---|---|---|
| `name` | `providers.name` | `providers.name` | 直取；同名冲突加后缀 |
| `protocol` | 常量 `anthropic` | 常量 `openai` | 由 app_type 决定 |
| `base_url` | `settings_config.env.ANTHROPIC_BASE_URL` | 解析 `settings_config.config`(TOML)：`[model_providers.<顶层 model_provider>].base_url` → 回退顶层 `base_url` | 复刻上游 extract 逻辑；trim 尾部 `/` |
| `api_key_enc` | `env` 链：`ANTHROPIC_AUTH_TOKEN → ANTHROPIC_API_KEY → OPENROUTER_API_KEY → GOOGLE_API_KEY`（首个非空） | `auth.OPENAI_API_KEY` → 回退 TOML `experimental_bearer_token` | 源为明文，入库前 AES-GCM 加密 |
| `model_redirects` | **不导入**（空 `{}`） | **不导入** | 见 Drop-list #2 |
| `cost_coefficient` | `meta.costMultiplier` ?? 列 `cost_multiplier` ?? `proxy_config.default_cost_multiplier` ?? `1.0` | 同左 | 字符串十进制 → 数值；语义同我方折扣系数（乘基准价） |
| `preset_id` | NULL | NULL | 可选增强：按 base_url 匹配我方预设 |
| `sort` | `sort_index`（NULL 则按 `created_at` 排尾） | 同左 | |
| 备注(notes) | `notes`；可追加 `[imported from cc-switch <date>] website: <website_url>; 模型钉扎: <env.ANTHROPIC_MODEL 等>` | `notes`；可追加 TOML 顶层 `model` | 信息保全但不进结构化字段 |
| `app_state.active_provider_id` | `is_current=1` 的行 | 同左 | 与 settings.json `currentProviderClaude/Codex` 交叉校验，不一致以 DB 为准 |
| `fallback_orders` | `in_failover_queue=1`，`ORDER BY COALESCE(sort_index,999999), id` → position 0..n | 同左 | 语义完全对应我方备选序列 |

### Drop-list（明确不导入，逐项理由）

1. **settings_config 内除 base_url/key 之外的一切**（hooks、permissions、enabledPlugins、statusLine、alwaysThinkingEnabled、其余 env 如 `GITHUB_PERSONAL_ACCESS_TOKEN`/`PATH`/超时变量）——那是设备本地 Claude Code 配置不是供应商属性，且含第三方机密，带入即泄漏面扩大。
2. **`env.ANTHROPIC_MODEL` / `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU,FABLE}_MODEL` 模型钉扎** —— 语义是"客户端请求哪个模型"，不是我方 `model_redirects` 的"代理改写模型名"；直接映射会把 `claude-*→GLM/DeepSeek` 的站点私有映射错误固化进代理层。写入备注供用户手动决定。
3. **codex config TOML 的 `mcp_servers` / `projects.*` / hooks 段** —— 与供应商无关；且内嵌第三方凭据。
4. **`meta.usage_script`**（余额查询脚本，含独立 apiKey/accessToken/火山 AK/SK）—— 属我方二期"余额适配器"范畴，MVP 不导入。
5. **`provider_endpoints` 多端点 + `meta.endpointAutoSelect`** —— 我方单 base_url 模型；只取主 base_url，其余端点写备注。
6. **`icon` / `icon_color` / `website_url` / `category` / `meta.isPartner` / `partnerPromotionKey`** —— 展示性/营销性字段。
7. **`limit_daily_usd` / `limit_monthly_usd`** —— 我方无消费限额功能。
8. **非 provider 表全部**：mcp_servers、prompts、skills、proxy_request_logs、usage_daily_rollups、model_pricing、stream_check_logs、proxy_config —— PRD 明确砍掉配置管家类功能；历史用量不迁移（我方统计从代理路径重新开始）。
9. **`app_type ∈ {gemini, claude-desktop, opencode, openclaw, hermes}` 的行** —— 明确不做的工具。

### 必须处理的边界情形（importer 规格）

| # | 情形 | 判定 | 处理 |
|---|---|---|---|
| E1 | claude 行 `meta.apiFormat ∈ {openai_chat, openai_responses}` | 该供应商依赖 cc-switch 本地代理做跨协议转换；我方 ADR-0002 不做转换 | **跳过 + 报告原因**（本机例：Nvidia） |
| E2 | `meta.providerType/provider_type ∈ {codex_oauth, github_copilot}` 或 `meta.authBinding.source = "managed_account"` | OAuth 托管凭据，无静态 key 可迁 | 跳过 + 报告 |
| E3 | base_url 指向 `127.0.0.1`/`localhost`/局域网代理 | 导入后 Agent→它→上游 形成双层代理甚至回环 | 默认跳过，允许用户勾选强制导入（本机例 2 条） |
| E4 | key 为空/占位（预设未填） | 首个非空链结果为空串 | 跳过 + 报告 |
| E5 | 同 base_url 多账号（anyrouter 多号） | 合法多记录 | 正常逐条导入，不按 base_url 去重 |
| E6 | DB `user_version` 高于 importer 已知版本 | providers 表自 v2 起列集只增不改 | 按**列名**取值（不 `SELECT *` 按位置），未知列忽略；user_version>已验证版本时警告 |
| E7 | cc-switch 进程正在运行（DB 被写） | 本机 fork 即常驻 daemon | 以 `mode=ro` URI 打开，失败则复制文件到临时路径再读 |
| E8 | 只找到 config.json（≤3.7.1 用户）或 config.json.migrated | v2 JSON：`{version:2, claude:{providers,current}, codex:{...}}`，Provider 字段 camelCase（settingsConfig/websiteUrl/sortIndex/inFailoverQueue，meta 内含 costMultiplier） | 走 JSON 解析分支，同一张映射表；v1（顶层 providers+current 无 version/apps 键）直接拒绝并提示过旧 |
| E9 | `app_paths.json` 存在 `app_config_dir_override` | 数据不在默认 `~/.cc-switch` | 先读 override 再定位 db/json |

**导入优先级**：`cc-switch.db`（存在即为唯一权威）→ `config.json`（v2）→ `config.json.migrated`（v2，提示为历史归档）。

---

## 证据与来源

关键结论均有 ≥2 独立来源交叉验证（上游源码 + 本机实测 + 历史 tag/CHANGELOG）：

1. **上游源码 @ HEAD v3.16.5**（clone 于 2026-07-03，commit `0cda8d4` 2026-07-02）[farion1231/cc-switch, 2026-07, https://github.com/farion1231/cc-switch]
   - `src-tauri/src/database/mod.rs`：`SCHEMA_VERSION = 11`；DB 路径 `get_app_config_dir().join("cc-switch.db")`；迁移前自动备份。
   - `src-tauri/src/database/schema.rs`：providers 建表 SQL 与 v0→v11 全部迁移步骤（v9→v10 Hermes、v10→v11 rollup request_model）。
   - `src-tauri/src/provider.rs`：`Provider` / `ProviderMeta` serde 定义（camelCase 重命名）；`resolve_usage_credentials` 官方 per-app 凭据提取链。
   - `src-tauri/src/codex_config.rs`：`extract_codex_base_url`（激活 model_provider 段优先）、`extract_codex_api_key`。
   - `src-tauri/src/app_config.rs`：`MultiAppConfig`（v2 flatten 结构）；v1 检测与拒载报错文案。
   - `src-tauri/src/lib.rs` L462-485：JSON→SQLite 迁移成功后 `config.json` → `config.json.migrated` 归档。
   - `src-tauri/src/settings.rs` L422-428：`current_provider_claude/codex`（settings.json 镜像字段为上游自带）。
   - `src-tauri/src/database/dao/failover.rs`：failover 队列排序 SQL。
   - `CHANGELOG.md` [3.8.0] 2025-11-28："Moved from single JSON storage to SQLite + JSON dual-layer... first launch auto-migrates config.json to SQLite"。
2. **历史 tag**：`v3.2.0`（2025-09-13）`app_config.rs` v1→v2 自动迁移与 `config.v1.backup.<ts>.json` 备份；`v3.7.1`（2025-11-22，最后一个纯 JSON 版本）`MultiAppConfig` 与现行 v2 结构一致。
3. **本机实测（只读，key 已脱敏）**：`sqlite3 -readonly ~/.cc-switch/cc-switch.db` schema/user_version/全量 provider 行 dump（Python 脱敏至末 4 位）；`settings.json`、`app_paths.json`；文件权限 stat；`ps` 确认 `cc-switch-web` 常驻。
4. **fork 源码**：`/home/orion/Workspace/github/cc-switch-web`（v3.16.2 base）`src-tauri/src/database/{schema,migration}.rs` —— 与上游同构的迁移链（fork 停在 v10），`migration.rs` 展示 MultiAppConfig→SQLite 的逐字段搬运（可作我方 importer 参照实现）。

## 对 design.md 的影响

- **§7 `[研究#5]` "导入：cc-switch 模式读取 ~/.cc-switch/（SQLite 或 config.json）做字段映射"** → **confirmed-with-changes**：
  1. 措辞应改为"**SQLite `cc-switch.db` 为主（v3.8.0+ 现行唯一格式），config.json 仅作 ≤3.7.1 legacy 兜底（v2 结构），v1 明确不支持**"；
  2. 需补充 E1-E9 边界情形（尤其 apiFormat 转换类、OAuth 托管类、localhost 回环类的**跳过+报告**语义）与 `app_paths.json` override 探测；
  3. importer 输出应含"跳过清单+原因"，作为一键导入的验收组成（PRD 验收"cc-switch 存量供应商一键导入成功"在本机数据上的可达口径 = claude 9 条中约 7 条可直接导入、codex 11 条中约 10 条，其余落跳过清单）。
- **§2 数据模型** "同一中转站的两种协议端点 = 两条 Provider 记录" → **confirmed**：与 cc-switch per-app 记录天然同构，映射为纯逐行变换。`model_redirects` 与 cc-switch 无对应物（其模型钉扎是客户端语义）→ 导入后恒为空，符合设计。
- **§10 测试策略**：导入映射单测的 fixture 直接取本文档 C5 的本机形态（多账号同站、apiFormat=openai_chat、localhost、TOML 混杂 mcp/projects 段四类样本）。

## 遗留不确定性

1. **fork 与上游漂移**（Medium）：本机 fork 的 `schema.rs` 与上游 diff 不同、停在 user_version=10、settings.json 未逐字段比对；但 providers 表列集实测与上游一致，导入所需字段无差异。若用户升级 fork 至 v11+ 结构，providers 表不受影响（v11 只动 rollup 表）。
2. **上游演进速度**（High-确认趋势）：3.16.x 一月三发、SCHEMA_VERSION 持续递增；importer 必须按列名读取并容忍未知列/未知 meta 字段（E6），发布前建议对最新 release 重跑一次冒烟。
3. **cost_multiplier 双源权威性**（Medium）：`providers.cost_multiplier` 列为 v2 迁移遗留、现行代码基本不写；运行时计价走 `meta.costMultiplier` + `proxy_config.default_cost_multiplier`。已按"meta 优先、列兜底"设计取值，若两处冲突以 meta 为准（依据：ProviderMeta 注释与 response_processor 测试用法）。
4. **codex `experimental_bearer_token` 回退分支**（Medium）：本机无此形态样本，仅有上游代码依据；importer 实现该回退但标记为低频路径。
5. **Windows/macOS 路径**（Low）：`get_app_config_dir` 恒为 home 下 `.cc-switch`（含 override 机制），未在非 Linux 机器实测——风险低，逻辑与平台无关。
