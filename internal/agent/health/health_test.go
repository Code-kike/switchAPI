package health

import (
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

func newTest(t *testing.T) (*Tracker, *[]wire.HealthReport, *time.Time) {
	t.Helper()
	var got []wire.HealthReport
	now := time.Unix(1_800_000_000, 0)
	tr := New(DefaultConfig(), func(r wire.HealthReport) { got = append(got, r) })
	tr.now = func() time.Time { return now }
	return tr, &got, &now
}

func hard(status int) forward.Usage {
	return forward.Usage{ProviderID: "p1", App: "claude-code", Status: status, ErrorKind: "upstream_5xx"}
}

func TestHardThresholdEdgeTriggered(t *testing.T) {
	tr, got, _ := newTest(t)
	tr.Observe(hard(502))
	tr.Observe(hard(502))
	if len(*got) != 0 {
		t.Fatalf("reported before threshold: %v", *got)
	}
	tr.Observe(hard(502))
	if len(*got) != 1 || (*got)[0].Kind != wire.HealthKindHard || (*got)[0].Count != 3 {
		t.Fatalf("report = %+v", *got)
	}
	if n := len((*got)[0].Samples); n != 3 {
		t.Fatalf("samples = %d, want 3", n)
	}
	// 边沿触发：继续失败不再重复上报。
	tr.Observe(hard(502))
	tr.Observe(hard(502))
	if len(*got) != 1 {
		t.Fatalf("re-reported without recovery: %v", *got)
	}
	// 成功清零后可再次触发。
	tr.Observe(forward.Usage{ProviderID: "p1", App: "claude-code", Status: 200})
	tr.Observe(hard(502))
	tr.Observe(hard(502))
	tr.Observe(hard(502))
	if len(*got) != 2 {
		t.Fatalf("second episode not reported: %v", *got)
	}
}

func TestFreshnessWindowResetsCount(t *testing.T) {
	tr, got, now := newTest(t)
	tr.Observe(hard(500))
	tr.Observe(hard(500))
	*now = now.Add(6 * time.Minute) // 超过 300s 新鲜度
	tr.Observe(hard(500))
	if len(*got) != 0 {
		t.Fatalf("stale counts accumulated: %v", *got)
	}
	tr.Observe(hard(500))
	tr.Observe(hard(500))
	if len(*got) != 1 {
		t.Fatalf("fresh streak should report: %v", *got)
	}
}

func TestRateLimitEscalation(t *testing.T) {
	tr, got, now := newTest(t)
	mk := func() forward.Usage {
		return forward.Usage{ProviderID: "p1", App: "codex", Status: 429, ErrorKind: "http_429"}
	}
	// 6 次但未跨 60s → 不升级。
	for i := 0; i < 6; i++ {
		tr.Observe(mk())
	}
	if len(*got) != 0 {
		t.Fatalf("escalated too early: %v", *got)
	}
	// 第 7 次时首末已跨 60s → 升级一次。
	*now = now.Add(61 * time.Second)
	tr.Observe(mk())
	if len(*got) != 1 || (*got)[0].Kind != wire.HealthKindRateLimit {
		t.Fatalf("rate report = %v", *got)
	}
}

func TestConfigFailuresAndIgnoredClasses(t *testing.T) {
	tr, got, _ := newTest(t)
	auth := forward.Usage{ProviderID: "p1", App: "claude-code", Status: 401, ErrorKind: "http_auth"}
	tr.Observe(auth)
	tr.Observe(auth)
	// 客户端中断与 4xx 业务错不影响任何计数。
	tr.Observe(forward.Usage{ProviderID: "p1", Status: 200, ErrorKind: "client_abort"})
	tr.Observe(forward.Usage{ProviderID: "p1", Status: 404})
	tr.Observe(auth)
	if len(*got) != 1 || (*got)[0].Kind != wire.HealthKindConfig || (*got)[0].Count != 3 {
		t.Fatalf("config report = %v", *got)
	}
}

func TestProvidersAreIndependent(t *testing.T) {
	tr, got, _ := newTest(t)
	tr.Observe(hard(500))
	tr.Observe(hard(500))
	other := forward.Usage{ProviderID: "p2", App: "claude-code", Status: 503, ErrorKind: "upstream_5xx"}
	tr.Observe(other)
	if len(*got) != 0 {
		t.Fatalf("cross-provider contamination: %v", *got)
	}
	tr.Observe(hard(500))
	if len(*got) != 1 || (*got)[0].ProviderID != "p1" {
		t.Fatalf("report = %v", *got)
	}
}
