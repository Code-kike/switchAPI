// health.go — M4 可靠性消息族：健康上报（Agent→Hub）、恢复探测与手动测速
// （Hub 下发指令、Agent 直连供应商执行——流量不经 Hub，ADR-0001）。
// ProbeTarget 携解密 key 下发，沿用 ConfigPush 的 LAN-trust 模型（ADR-0005）。
package wire

// Message type tags (M4).
const (
	TypeHealthReport    = "health_report"    // Agent → Hub：达阈值边沿触发
	TypeProbeCmd        = "probe_cmd"        // Hub → 指定 Agent：恢复探测
	TypeProbeResult     = "probe_result"     // Agent → Hub
	TypeSpeedtestCmd    = "speedtest_cmd"    // Hub → 全体 Agent：手动测速
	TypeSpeedtestResult = "speedtest_result" // Agent → Hub
)

// HealthReport kinds（research/08 失败四分类中需要上报的三类；
// 不计数类只进用量明细）。
const (
	HealthKindHard      = "hard"       // 连续硬失败达阈（默认 3）
	HealthKindRateLimit = "rate_limit" // 429 连续 6 次且跨 ≥60s
	HealthKindConfig    = "config"     // 401/403 三连 → needs_attention
)

// ErrorSample is one piece of evidence attached to a HealthReport.
type ErrorSample struct {
	Kind      string `json:"kind"` // connect|tls|timeout_first_byte|timeout_idle|stream_aborted|fake_200|http_5xx|http_429|http_auth
	TS        int64  `json:"ts"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// HealthReport is edge-triggered: sent once when a counter crosses its
// threshold, carrying the most recent samples (≤5) as evidence.
type HealthReport struct {
	App        string        `json:"app"`
	ProviderID string        `json:"provider_id"`
	Kind       string        `json:"kind"`
	Count      int           `json:"count"`
	Samples    []ErrorSample `json:"samples,omitempty"`
}

// ProbeTarget tells an Agent where to send one minimal completion
// (research/08 #16: 非流式 max_tokens=1，HEAD//models 无法证明补全链路).
type ProbeTarget struct {
	ProviderID string `json:"provider_id"`
	Protocol   string `json:"protocol"` // anthropic | openai
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
}

// ProbeCmd asks ONE agent to probe one cooled-down provider.
type ProbeCmd struct {
	ProbeID  string      `json:"probe_id"`
	Target   ProbeTarget `json:"target"`
	TimeoutS int         `json:"timeout_s"` // 默认 10
}

// ProbeResult reports one probe/speedtest attempt.
type ProbeResult struct {
	ProbeID    string `json:"probe_id,omitempty"`
	ProviderID string `json:"provider_id"`
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// SpeedtestCmd is broadcast to every online agent; each tests ALL targets
// from its own network position（按设备展示的依据）.
type SpeedtestCmd struct {
	TestID  string        `json:"test_id"`
	Targets []ProbeTarget `json:"targets"`
}

// SpeedtestResult carries one agent's full result set.
type SpeedtestResult struct {
	TestID  string        `json:"test_id"`
	Results []ProbeResult `json:"results"`
}
