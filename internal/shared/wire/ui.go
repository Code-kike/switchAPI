// ui.go — Hub→浏览器 下行消息（/api/v1/ws/ui，Session 鉴权，见 M3 design.md §1）。
// 复用 Envelope 外框；usage_tick/event 是失效通知（前端收到后 refetch），不携带全量数据。
package wire

import "encoding/json"

// UI channel message type tags (downstream only).
const (
	TypeUIStateChanged = "state_changed" // 切换后：全量 app→provider 映射
	TypeUIEvent        = "event"         // 新事件入库后
	TypeUIUsageTick    = "usage_tick"    // usage_batch 入库后（inserted==0 不推）
)

// UIStateChanged mirrors app_state after a switch. Apps maps app name
// ("claude-code" | "codex") to the active provider id.
type UIStateChanged struct {
	Rev  int64             `json:"rev"`
	Apps map[string]string `json:"apps"`
}

// UIEvent is one freshly written events row.
type UIEvent struct {
	ID      int64           `json:"id"`
	TS      int64           `json:"ts"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// UIUsageTick tells clients new usage rows landed; they refetch stats.
type UIUsageTick struct {
	Inserted int   `json:"inserted"`
	LastTS   int64 `json:"last_ts"`
}
