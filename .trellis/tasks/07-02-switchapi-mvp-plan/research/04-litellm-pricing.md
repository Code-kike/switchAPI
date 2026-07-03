# Research #04: LiteLLM 价格表（model_prices_and_context_window.json）作为 pricing_base 数据源

- **Query**: prd.md 研究项 #4 — LiteLLM 价格表字段映射（含 cache_creation/cache_read 单价）、同步频率与快照打包
- **Scope**: mixed（外部：GitHub 原始文件实测下载 + 官方文档；内部：本机 ~/.claude 会话日志中真实出现的模型名）
- **Date**: 2026-07-03
- **对应 design.md 标注**: `[研究#4]`（§6 计价引擎、§2 pricing_base/pricing_overrides 表）

---

## 结论

### (a) 数据源 URL、体积与更新频率

1. **权威 URL 确认**（置信度 **High**）：
   `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`
   实测下载成功（2026-07-03），HTTP 200。LiteLLM 官方文档明确以此 URL 作为 hosted 价格表供外部 `register_model()` 引用，属于官方支持的公开消费方式，非内部实现细节。
2. **当前体积**（High，实测）：**1,579,065 字节（约 1.51 MiB）**，共 **2,929 个顶层 key**（其中 1 个是 `sample_spec` 文档条目，须跳过）。gzip 后约 200-300KB 量级，打包进二进制毫无压力。
3. **更新频率**（High，实测 commit 历史）：**接近每日多次**。Atom feed 显示 2026-06-18 ~ 2026-06-30 的 13 天内该文件有 **20 次 commit**（单日最多 4 次），内容包括新模型上架（如 2026-06-30 "add Claude Sonnet 5"）、价格修正、条目清理。→ 每日拉取一次与上游变更节奏匹配，无需更频繁。

### (b) 字段名、单位与陷阱

4. **四分项字段名精确确认**（High，实测条目）：
   - `input_cost_per_token`
   - `output_cost_per_token`
   - `cache_creation_input_token_cost`（缓存写，5 分钟 TTL 档）
   - `cache_read_input_token_cost`（缓存读）

   全部为 **USD / 单 token**（如 `3e-06` = $3/百万 token）。计价时 `金额 = tokens × 单价`，无需再除以 1000/1000000。
5. **实测条目引用**（节选，2026-07-03 快照）：

   ```jsonc
   // ① Anthropic 官方 Sonnet 4.5（带日期的精确名，中转站 message_start 返回的就是它）
   "claude-sonnet-4-5-20250929": {
     "input_cost_per_token": 3e-06,               // $3/MTok
     "output_cost_per_token": 1.5e-05,            // $15/MTok
     "cache_creation_input_token_cost": 3.75e-06, // = 1.25 × input（5m TTL 写）
     "cache_read_input_token_cost": 3e-07,        // = 0.1 × input
     "cache_creation_input_token_cost_above_1hr": 6e-06,          // = 2 × input（1h TTL 写）
     "input_cost_per_token_above_200k_tokens": 6e-06,             // >200k 上下文分层价
     "output_cost_per_token_above_200k_tokens": 2.25e-05,
     "cache_creation_input_token_cost_above_200k_tokens": 7.5e-06,
     "cache_read_input_token_cost_above_200k_tokens": 6e-07,
     "litellm_provider": "anthropic",
     "mode": "chat",
     "max_input_tokens": 200000, "max_output_tokens": 64000
   }

   // ② OpenAI Codex 模型 —— 注意 mode 是 "responses" 而非 "chat"！
   "gpt-5.1-codex": {
     "input_cost_per_token": 1.25e-06,
     "output_cost_per_token": 1e-05,
     "cache_read_input_token_cost": 1.25e-07,
     // 没有 cache_creation_input_token_cost —— OpenAI 缓存写免费
     "input_cost_per_token_priority": 2.5e-06,    // priority 处理档，忽略
     "litellm_provider": "openai",
     "mode": "responses",
     "supported_endpoints": ["/v1/responses"]
   }

   // ③ Haiku 4.5（无 200k 分层，结构最简）
   "claude-haiku-4-5-20251001": {
     "input_cost_per_token": 1e-06, "output_cost_per_token": 5e-06,
     "cache_creation_input_token_cost": 1.25e-06,
     "cache_read_input_token_cost": 1e-07,
     "cache_creation_input_token_cost_above_1hr": 2e-06,
     "litellm_provider": "anthropic", "mode": "chat"
   }
   ```

6. **价格正确性交叉验证**（High）：LiteLLM 数值与 Anthropic 官方定价规则完全吻合——Sonnet 档 $3/$15、Opus 4.5 档 $5/$25、Haiku 4.5 档 $1/$5；缓存读 = 0.1×输入价，缓存写(5m) = 1.25×，缓存写(1h) = 2×；Sonnet 4.5 >200k 分层 = 输入 2×（$6）/ 输出 1.5×（$22.5），分层档的缓存写/读同样按 1.25×/0.1× 派生（7.5e-06 / 6e-07），内部自洽。
7. **陷阱清单**（High，均为实测）：
   - **`mode` 过滤不能只留 `chat`**：Codex 系列（`gpt-5-codex`、`gpt-5.1-codex`、`codex-mini-latest` 等）`mode = "responses"`（全表 82 条）。**必须取 `mode ∈ {chat, responses}`**，否则 Codex 全部丢失。这与 prd 研究清单里"mode 过滤（仅 chat）"的预设**相反**。
   - **`sample_spec` 键**是嵌在数据里的 schema 文档条目，解析时须跳过（value 里 `mode` 字段是一段说明文字）。
   - **provider 前缀**：Anthropic 官方条目**全部是裸键**（`claude-*`），全表 0 个 `anthropic/` 前缀键；OpenAI 官方条目也基本裸键（仅 sora/container 等 4 个 `openai/` 前缀）。前缀键（2,383 个，如 `vertex_ai/claude-sonnet-4-5`、`openrouter/anthropic/claude-opus-4.5`）是其他托管渠道的价，**与我们无关，匹配时不应使用**。
   - **裸键同时存在别名与日期两种形态**：`claude-sonnet-4-5` 与 `claude-sonnet-4-5-20250929` 并存（价格相同）；OpenAI 同理（`gpt-5.1` 与 `gpt-5.1-2025-11-13`）。
   - **OpenAI 条目没有 `cache_creation_input_token_cost`**（缓存写不收费），计价引擎必须把缺失字段按 0 处理，pricing_base 该列允许 NULL。
   - **分层/变体字段**：`*_above_200k_tokens`（长上下文分层）、`cache_creation_input_token_cost_above_1hr`（1h 缓存写）、`*_priority`（OpenAI priority 档）、`search_context_cost_per_query` 等。MVP 至少要认识它们并决定取舍（见"对 design.md 的影响"）。Opus 4.5/4.7/4.8 无 `above_200k` 字段（1M 上下文不加价），与 Anthropic 官方口径一致。
   - **退役模型会被上游删除**：`claude-3-5-sonnet-20241022`、`claude-3-5-haiku-20241022` 已从表中**消失**（claude-3-opus 尚在且带 `deprecation_date: 2026-05-01`）。→ 我们的同步**只能 upsert、绝不删除**，否则历史用量记录会失去价格。
   - 约 300 个 key 含大写字母（多为第三方前缀键），裸键的 Anthropic/OpenAI 条目全小写。

### (c) 模型名映射策略

8. **本机地面真值**（High）：扫描 `~/.claude/projects/**/*.jsonl`（近 30 天）中 assistant 消息实际返回的 `model` 字段：
   `claude-opus-4-8`、`claude-fable-5`、`claude-haiku-4-5-20251001`、`claude-opus-4-7`、`ZhipuAI/GLM-5.2`、`ZhipuAI/GLM-5.1`、`<synthetic>`（CC 本地合成的错误占位，不会出现在代理路径）。
   前四个（官方名，无论别名形态还是日期形态）**全部与 LiteLLM 裸键精确匹配**；`ZhipuAI/GLM-5.2` 是中转站把 claude 请求重定向到 GLM 后返回的真实名——LiteLLM 中只有 `zai/glm-5.1`（尚无 5.2）和 `cloudflare/@cf/zai-org/glm-5.2`（cloudflare 价），**无法可靠匹配**。
9. **推荐匹配规则（按序尝试，命中即止）**（High）：
   1. **精确匹配**裸键（覆盖绝大多数：CC/Codex 官方模型名不管带不带日期都在表里）；
   2. **去日期回退**：剥掉尾部 `-20\d{6}` 再查（防上游未及时收录新日期快照，如 `claude-sonnet-4-6-20260214` → `claude-sonnet-4-6`）；
   3. **取斜杠尾段 + 小写**再走 1/2（`ZhipuAI/GLM-5.2` → `glm-5.2`；对当前 GLM-5.2 仍未命中，但能兜住部分 vendor 前缀写法）;
   4. 全部未命中 → **未知模型**：记 token 不记费 + UI 标注（design.md 已有此语义），用户可用 pricing_overrides 手工补价。
   不建议做更激进的模糊匹配（如子串搜索）——错误匹配比不匹配更糟。

### (d) 许可证与快照打包

10. **MIT 许可，可打包**（High）：仓库 LICENSE 明确写明"enterprise/ 目录之外的内容按 MIT 授权"；`model_prices_and_context_window.json` 位于仓库根目录，属 MIT 范围。MIT 允许复制、修改、再分发（含商用），唯一义务是**保留版权与许可声明**。
    佐证：litellm 自身就把该文件快照打包进 PyPI 包（`litellm/model_prices_and_context_window_backup.json`，实测存在，1,570,536 字节；官方文档提供 `LITELLM_LOCAL_MODEL_COST_MAP=True` 使用本地副本），说明"打快照进制品分发"是上游自身的既定用法。
    **落地要求**：在我们发行物的 NOTICE/关于页中附 LiteLLM 的 MIT 声明（Copyright (c) 2023 Berri AI）。

### (e) 同步策略与最小字段子集

11. **ETag 条件请求可用，If-Modified-Since 不可用**（High，实测）：
    - 响应带强 `etag`（sha256 形态）；用 `If-None-Match` 复测返回 **HTTP 304、0 字节**——增量同步成本≈一次空请求。
    - 响应**没有 `Last-Modified` 头**，`If-Modified-Since` 无从谈起。
    - `cache-control: max-age=300`（Fastly CDN 5 分钟新鲜度），对每日轮询无影响。
    - 推荐实现：Hub 每日一次（可关，settings 已预留开关）`GET` + `If-None-Match: <上次 etag>`；200 则解析入库并更新 etag/synced_at，304 则只更新 synced_at；失败静默重试次日，不影响主流程（本地永远有可用快照）。
12. **快照打包**：构建期把当日 JSON `go:embed` 进 hub 二进制，首次启动灌入 pricing_base（`source='snapshot'`）；运行期远程同步成功后逐行 upsert（`source='litellm'`）。若追求可复现构建，可从 release tag 拉取（`raw.githubusercontent.com/BerriAI/litellm/<tag>/...`）而非 main。
13. **入库过滤与最小字段子集**（推荐）：
    - 过滤：`mode ∈ {chat, responses}` 且四分项至少有 input+output 两项（跳过 `sample_spec`）。可选二选一：
      a) 只留 `litellm_provider ∈ {anthropic, openai}` → 当前 **144 行**，最小但中转站重定向到 GLM/DeepSeek 等模型时无价；
      b) **全 provider 收录（推荐）** → 当前约 **2,342 行**（SQLite 毫无压力），裸键条目能额外兜住中转站直接返回 `deepseek-chat`、`kimi-k2` 之类官方名的场景。
    - 每行字段：`model_key`（原样存，含前缀键也可存但匹配只用裸键）、`input_cost_per_token`、`output_cost_per_token`、`cache_creation_input_token_cost`(NULL→0)、`cache_read_input_token_cost`(NULL→0)、`litellm_provider`、`mode`、`source`、`synced_at`。
    - **分层价**：建议增加一列 `tiered_prices` JSON（原样搬 `*_above_200k_tokens` 与 `*_above_1hr` 字段，无则 NULL）。MVP 计价可先只用基础四项（>200k 请求占比极低，误差方向是**少算**且可解释）；留字段则二期启用分层结算无需改 schema。

---

## 证据与来源

| # | 断言 | 来源 1 | 来源 2 / 地面真值 |
|---|------|--------|-------------------|
| 1 | 原始 URL 与文件内容 | 实测 `curl` 下载 200，1,579,065B [BerriAI/litellm, 2026-07-03, https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json] | 官方文档以同一 URL 供 `register_model` 消费 [LiteLLM Docs, Token Usage, https://docs.litellm.ai/docs/completion/token_usage] |
| 2 | 更新频率≈每日 | GitHub commit Atom feed：13 天 20 commits [GitHub, 2026-06-18~30, https://github.com/BerriAI/litellm/commits/main/model_prices_and_context_window.json.atom] | 文件内容含 2026-06-30 新增的 Claude Sonnet 5 条目（下载快照自证） |
| 3 | 四分项字段名/单位 | 实测解析 claude/gpt 条目（上文引用） | `sample_spec` 内嵌 schema 说明（同文件）；litellm 文档 register_model 示例用同名字段 |
| 4 | 价格数值正确 | LiteLLM 条目数值（实测） | Anthropic 官方定价（claude-api 权威参考，cached 2026-06-24）：Sonnet $3/$15、Opus 4.5 档 $5/$25、Haiku 4.5 $1/$5、缓存读 0.1×、写 1.25×(5m)/2×(1h)——逐项吻合 |
| 5 | mode=responses 陷阱 | 实测：`gpt-5-codex`/`gpt-5.1-codex`/`codex-mini-latest` 均 `mode:"responses"`，全表 responses 82 条 | `supported_endpoints: ["/v1/responses"]` 字段自证；Codex CLI 走 Responses API（研究#2 域） |
| 6 | 中转站返回名可精确匹配 | 本机 `~/.claude/projects/**/*.jsonl` 实测出现 `claude-opus-4-8`、`claude-haiku-4-5-20251001` 等，均在表中 | 表内同时收录别名键与日期键（实测枚举 20 个 claude-sonnet-4-5* 键） |
| 7 | 第三方重定向名可能无法匹配 | 本机实测 `ZhipuAI/GLM-5.2` | LiteLLM 仅有 `zai/glm-5.1` 及 cloudflare 前缀键（实测枚举） |
| 8 | 退役模型被删除 | 实测：`claude-3-5-sonnet-20241022`/`-20240620`/`claude-3-5-haiku-20241022` 均 ABSENT | 2026-06-27 commit "chore: remove unused keys from model cost map"（Atom feed） |
| 9 | MIT 许可 | LICENSE 原文实测下载 [BerriAI/litellm LICENSE, https://github.com/BerriAI/litellm/blob/main/LICENSE] | litellm 自身将快照打包进 PyPI 包（`litellm/model_prices_and_context_window_backup.json` 实测 HTTP 200，1,570,536B）+ 文档 `LITELLM_LOCAL_MODEL_COST_MAP` |
| 10 | ETag 304 可用、无 Last-Modified | 实测响应头 + `If-None-Match` 复测 304/0 字节（2026-07-03） | `cache-control: max-age=300`、`via: varnish`（Fastly）响应头自证 CDN 行为 |

---

## 对 design.md 的影响

对照 design.md 中 `[研究#4]` 相关假设逐条判定：

| design.md 假设 | 判定 | 说明 |
|----------------|------|------|
| §6 "四分项（input/output/cache_write/cache_read）独立结算" | **confirmed** | 字段存在且单位明确；但 OpenAI 条目无 cache_write 字段，引擎需按 0 处理（NULL 容忍） |
| §6 "LiteLLM 价格表：构建期打包快照 + 运行期每日拉取（可关）" | **confirmed** | MIT 允许打包（附声明）；每日拉取与上游节奏匹配；用 ETag If-None-Match 实现近零成本轮询 |
| §6 "未知模型记 token 不记费并在 UI 标注" | **confirmed（且必要）** | 本机实测中转站返回 `ZhipuAI/GLM-5.2` 无法匹配，该兜底路径一定会被走到 |
| §2 `pricing_base`: "model、四分项单价、source、synced_at" | **confirmed-with-changes** | 建议增列：`litellm_provider`、`mode`（过滤与展示用）、`tiered_prices` JSON（>200k 分层与 1h 缓存写，二期启用）；cache 两列允许 NULL；settings 增存 `pricing_etag` |
| 研究项预设 "mode 过滤仅 chat" | **contradicted** | 必须 `mode ∈ {chat, responses}`，否则丢失全部 Codex 模型 |
| （隐含）同步=全量替换 | **needs change → upsert-only** | 上游会删除退役模型；同步必须只增改不删，保历史记录可计价 |

**需要写入 design.md 的增量**（建议）：
1. §6 计价引擎补一句："价格表入库过滤 `mode ∈ {chat, responses}`；同步 upsert-only；匹配顺序=精确→去日期→斜杠尾段小写→未知兜底。"
2. §2 `pricing_base` 列定义按上表微调；`settings` 增加 `pricing_etag`。

---

## 遗留不确定性

1. **>200k 分层价是否进 MVP 计价公式**（决策项，非事实问题）：我们逐请求记录了 token 分项，技术上可按 `input+cache_read+cache_creation > 200k` 判档；MVP 不做则长上下文请求少算（Sonnet 4.5 输入差 1 倍）。建议 MVP 记录字段、不结算，UI 说明。（置信度不适用——待产品决策）
2. **中转站折扣与 LiteLLM 基准的对账精度**：LiteLLM 是官方价，中转站实际计费还可能有自己的分层/倍率规则，`cost_coefficient` 单乘数未必完全覆盖（prd 验收标准已用"误差可解释"表述，风险已被吸收）。（Medium）
3. **上游 schema 稳定性**：字段命名 2 年内保持向后兼容（新字段只增不改），但无正式 schema 版本承诺；解析器应忽略未知字段、对缺失字段容错。（Medium，基于 commit 历史观察）
4. **`gpt-5.x-codex` 的 `*_priority` 档**：若用户在 OpenAI 侧开 priority processing，实际单价×2，我们按标准档计——影响极小众场景。（High-确认存在该字段，Low-是否有用户命中）
5. PyPI 元数据未声明 license 字段（`license: None`），但仓库 LICENSE 文本是法律权威来源，不构成矛盾。（High）
