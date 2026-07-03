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
	TypeUsageBatch = "usage_batch" // reserved for M2
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

// UsageBatch is a placeholder for the M2 usage pipeline.
type UsageBatch struct {
	Records []json.RawMessage `json:"records"`
}
