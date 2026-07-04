// Package wire defines the message types exchanged on the Hub↔Agent
// WebSocket channel (/api/v1/ws/agent). Envelope carries a type tag plus a
// raw JSON payload so both ends can evolve independently.
package wire

import "encoding/json"

// Message type tags.
const (
	TypeHello      = "hello"
	TypeConfigPush = "config_push"
	TypeHeartbeat  = "heartbeat"
	TypeUsageBatch = "usage_batch" // Agent → Hub：用量批量上报
	TypeUsageAck   = "usage_ack"   // Hub → Agent：确认落库，Agent 删本地队列
)

// Envelope is the outer frame of every WS message.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope marshals payload and wraps it.
func NewEnvelope(typ string, payload any) (Envelope, error) {
	if payload == nil {
		return Envelope{Type: typ}, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: typ, Payload: raw}, nil
}

// Decode unmarshals the payload into v.
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// Hello is sent by the Agent right after the WS connection is established.
type Hello struct {
	Name     string `json:"name"`
	Platform string `json:"platform"` // runtime.GOOS
	Version  string `json:"version"`
}

// Heartbeat is sent periodically by the Agent; the Hub updates last_seen and
// drops connections that stay silent past its deadline.
type Heartbeat struct {
	SentAt int64 `json:"sent_at"` // unix seconds
}

// AppRoute is the routing target for one App.
//
// APIKey is the DECRYPTED upstream provider key: the Agent must inject it on
// forwarded requests. This rides the LAN-trust model of ADR-0005 — the WS
// channel stays inside the private network and the Agent persists its config
// snapshot with file mode 0600.
type AppRoute struct {
	ProviderID     string            `json:"provider_id"`
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"` // "anthropic" | "openai"
	BaseURL        string            `json:"base_url"`
	APIKey         string            `json:"api_key"`
	ModelRedirects map[string]string `json:"model_redirects,omitempty"`
}

// ConfigPush is the full routing snapshot. The Hub always pushes the whole
// state (tiny payload, trivial reconciliation): Rev increases monotonically
// and the Agent ignores pushes older than what it already holds.
type ConfigPush struct {
	Rev            int64               `json:"rev"`
	Apps           map[string]AppRoute `json:"apps"`            // key: "claude-code" | "codex"
	FallbackOrders map[string][]string `json:"fallback_orders"` // app → provider ids（M4 本地临时降级用）
}

// UsageRecord is one metered request as the Agent reports it — pure metadata,
// NEVER message content (CONTEXT.md: 用量记录). Field semantics follow
// research/03: input excludes cache reads; cache_write is anthropic-only;
// output includes reasoning. device_id is attributed by the Hub from the
// reporting connection, not trusted from the Agent.
type UsageRecord struct {
	RequestID        string `json:"request_id"` // Agent 生成的 uuid，幂等去重键
	TS               int64  `json:"ts"`         // unix 秒，请求开始时刻
	App              string `json:"app"`        // claude-code | codex（由路由前缀推导）
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	ModelRedirected  string `json:"model_redirected,omitempty"` // 重定向后的模型名（未重定向为空）
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	DurationMS       int64  `json:"duration_ms"`
	Status           int    `json:"status"`
	ErrorKind        string `json:"error_kind,omitempty"`
	UsageSource      string `json:"usage_source"` // wire | estimated | none
}

// UsageBatch carries pending records upstream; at-least-once — the Hub
// deduplicates on request_id and acknowledges by batch_id.
type UsageBatch struct {
	BatchID string        `json:"batch_id"`
	Records []UsageRecord `json:"records"`
}

// UsageAck confirms a batch landed (or was deduplicated) so the Agent can
// drop it from the local queue.
type UsageAck struct {
	BatchID string `json:"batch_id"`
}
