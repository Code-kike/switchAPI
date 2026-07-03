-- 0001_init.sql — switchAPI Hub 全量初始 schema（父 design.md §2）。
-- 约定：时间戳一律 unix 秒；布尔用 INTEGER 0/1。

CREATE TABLE providers (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    protocol         TEXT NOT NULL CHECK (protocol IN ('anthropic', 'openai')),
    base_url         TEXT NOT NULL,
    api_key_enc      BLOB NOT NULL,              -- AES-256-GCM（主密钥），永不明文
    model_redirects  TEXT NOT NULL DEFAULT '{}', -- JSON: {"请求模型":"重定向模型"}
    cost_coefficient REAL NOT NULL DEFAULT 1.0,
    preset_id        TEXT NOT NULL DEFAULT '',
    sort             INTEGER NOT NULL DEFAULT 0,
    note             TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- 全局切换语义落点：每 App 一行。device_id 为二期"设备级覆盖"预留（M1 恒 NULL）。
CREATE TABLE app_state (
    app                TEXT PRIMARY KEY CHECK (app IN ('claude-code', 'codex')),
    device_id          TEXT,
    active_provider_id TEXT NOT NULL REFERENCES providers (id),
    updated_at         INTEGER NOT NULL,
    updated_by         TEXT NOT NULL DEFAULT ''
);

CREATE TABLE fallback_orders (
    app         TEXT NOT NULL CHECK (app IN ('claude-code', 'codex')),
    provider_id TEXT NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    PRIMARY KEY (app, provider_id)
);

-- Hub 侧故障仲裁状态（研究#8）。M1 仅建表，逻辑属 M4。
CREATE TABLE provider_health (
    provider_id          TEXT PRIMARY KEY REFERENCES providers (id) ON DELETE CASCADE,
    demote_count         INTEGER NOT NULL DEFAULT 0,
    cooldown_until       INTEGER NOT NULL DEFAULT 0,
    needs_attention      INTEGER NOT NULL DEFAULT 0,
    last_probe_at        INTEGER NOT NULL DEFAULT 0,
    consecutive_probe_ok INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE devices (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    platform   TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL UNIQUE, -- SHA-256 hex，token 本体只在配对时下发一次
    paired_at  INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL DEFAULT 0,
    revoked    INTEGER NOT NULL DEFAULT 0
);

-- 纯元数据，永不包含消息内容（PRD 第 16 题决策）。
CREATE TABLE usage_records (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                 INTEGER NOT NULL,
    device_id          TEXT NOT NULL,
    app                TEXT NOT NULL,
    provider_id        TEXT NOT NULL,
    model              TEXT NOT NULL,
    model_redirected   TEXT NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0, -- openai 协议恒 0（研究#3）
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    duration_ms        INTEGER NOT NULL DEFAULT 0,
    status             INTEGER NOT NULL DEFAULT 0,
    error_kind         TEXT NOT NULL DEFAULT '',
    usage_source       TEXT NOT NULL DEFAULT 'wire' CHECK (usage_source IN ('wire', 'estimated', 'none')),
    request_id         TEXT NOT NULL UNIQUE -- at-least-once 上报的幂等去重键
);
CREATE INDEX idx_usage_ts ON usage_records (ts);
CREATE INDEX idx_usage_provider_ts ON usage_records (provider_id, ts);

-- LiteLLM 快照（研究#4）：四分项单价均可 NULL（OpenAI 系无 cache_write 价，NULL 按 0 计）；
-- 同步 upsert-only 永不删除。
CREATE TABLE pricing_base (
    model            TEXT PRIMARY KEY,
    input_cost       REAL,
    output_cost      REAL,
    cache_write_cost REAL,
    cache_read_cost  REAL,
    litellm_provider TEXT NOT NULL DEFAULT '',
    mode             TEXT NOT NULL DEFAULT '',
    tiered_prices    TEXT, -- JSON：>200k 分层 / 1h 缓存变体，M2 记录、二期结算
    source           TEXT NOT NULL DEFAULT '',
    synced_at        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE pricing_overrides (
    provider_id      TEXT NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    model            TEXT NOT NULL,
    input_cost       REAL,
    output_cost      REAL,
    cache_write_cost REAL,
    cache_read_cost  REAL,
    PRIMARY KEY (provider_id, model)
);

-- 事件时间线：switch / failover / pairing / backup / speedtest / probe …
CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      INTEGER NOT NULL,
    kind    TEXT NOT NULL,
    payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_events_ts ON events (ts);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
