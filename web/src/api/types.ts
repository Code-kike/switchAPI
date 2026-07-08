// TS 类型以 Go handler 的 JSON 输出为唯一真源（internal/hub/api/*.go）。
// cost 为 null 表示"未知模型无法计价"，UI 一律显示"未知"，绝不显示 0。

export type Protocol = 'anthropic' | 'openai'
export type App = 'claude-code' | 'codex'

export const APPS: App[] = ['claude-code', 'codex']
export const APP_LABEL: Record<App, string> = { 'claude-code': 'Claude Code', codex: 'Codex' }
export const APP_PROTOCOL: Record<App, Protocol> = { 'claude-code': 'anthropic', codex: 'openai' }

// providers.go providerView
export interface Provider {
  id: string
  name: string
  protocol: Protocol
  base_url: string
  key_last4: string
  model_redirects: Record<string, string>
  cost_coefficient: number
  preset_id: string
  sort: number
  note: string
  created_at: number
  updated_at: number
}

// providers.go preset
export interface Preset {
  id: string
  name: string
  protocol: Protocol
  base_url_hint: string
  cost_coefficient: number
  note: string
}

// GET /api/v1/state：app → 状态（未切换过为 null）
export type StateResp = Record<
  string,
  { active_provider_id: string; updated_at: number; updated_by: string } | null
>

export interface FallbackResp {
  app: App
  provider_ids: string[]
}

// devices.go deviceView
export interface Device {
  id: string
  name: string
  platform: string
  paired_at: number
  last_seen: number
  revoked: boolean
}

export interface PairingCode {
  code: string
  expires_at: number
}

// usage.go usageRowView（store.UsageRow + cost）
export interface UsageRow {
  id: number
  ts: number
  device_id: string
  app: string
  provider_id: string
  model: string
  model_redirected: string
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  duration_ms: number
  status: number
  error_kind: string
  usage_source: string
  request_id: string
  cost: number | null
}

export interface UsageResp {
  total: number
  rows: UsageRow[]
}

// usage.go totals
export interface Totals {
  requests: number
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_read_tokens: number
  cost: number | null
  cost_unknown_requests: number
}

export interface SummaryResp extends Totals {
  by_app: Record<string, Totals>
}

export interface TrendEntry extends Totals {
  bucket_ts: number
}

export interface BreakdownEntry extends Totals {
  key: string
  name?: string
}

// devices.go eventView
export interface EventRow {
  id: number
  ts: number
  kind: string
  payload: Record<string, unknown>
}

// ---- ws/ui 下行（internal/shared/wire/ui.go） ----

export interface WsEnvelope {
  type: 'state_changed' | 'event' | 'usage_tick'
  payload?: unknown
}

export interface WsStateChanged {
  rev: number
  apps: Record<string, string>
}

export interface WsUsageTick {
  inserted: number
  last_ts: number
}

// ---- M4 可靠性 ----

// store.ProviderHealth
export interface ProviderHealth {
  provider_id: string
  demote_count: number
  cooldown_until: number
  needs_attention: boolean
  last_probe_at: number
  consecutive_probe_ok: number
}

// backup.Info
export interface BackupInfo {
  name: string
  size_bytes: number
  created_at: number
}

// failover.SpeedtestRun
export interface ProbeResult {
  provider_id: string
  ok: boolean
  status?: number
  latency_ms: number
  error?: string
}

export interface SpeedtestRun {
  id: string
  started_at: number
  expected_devices: string[]
  results: Record<string, ProbeResult[]>
}

export interface CCSwitchImportResp {
  imported: Array<{ app: string; name: string }>
  skipped: Array<{ app: string; name: string; reason: string }>
}
