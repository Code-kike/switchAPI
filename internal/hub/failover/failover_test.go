package failover

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// ---- fakes ----

type fakeAgents struct {
	mu         sync.Mutex
	broadcasts int
	sent       []wire.Envelope
	online     []string
}

func (f *fakeAgents) Broadcast() { f.mu.Lock(); f.broadcasts++; f.mu.Unlock() }
func (f *fakeAgents) SendTo(_ string, env wire.Envelope) error {
	f.mu.Lock()
	f.sent = append(f.sent, env)
	f.mu.Unlock()
	return nil
}
func (f *fakeAgents) BroadcastEnvelope(env wire.Envelope) {
	f.mu.Lock()
	f.sent = append(f.sent, env)
	f.mu.Unlock()
}
func (f *fakeAgents) OnlineDevices() []string { return f.online }
func (f *fakeAgents) broadcastCount() int     { f.mu.Lock(); defer f.mu.Unlock(); return f.broadcasts }

type fakeUI struct {
	mu     sync.Mutex
	events []store.Event
	states int
}

func (f *fakeUI) NotifyEvent(ev store.Event) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}
func (f *fakeUI) NotifyState() { f.mu.Lock(); f.states++; f.mu.Unlock() }
func (f *fakeUI) lastKindAction(t *testing.T) (string, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		t.Fatal("no events notified")
	}
	ev := f.events[len(f.events)-1]
	return ev.Kind, ev.Payload
}

// ---- rig ----

type rig struct {
	st     *store.Store
	key    []byte
	agents *fakeAgents
	ui     *fakeUI
	e      *Engine
	pA, pB string
}

func testConfig() Config {
	return Config{
		DebounceWindow: 20 * time.Millisecond, VetoWindow: 30,
		FailoverMinGap: 200 * time.Millisecond,
		ProbeBase:      10 * time.Millisecond, ProbeMax: 100 * time.Millisecond,
		ProbeTimeoutS: 1, ProbeScan: 10 * time.Millisecond,
		SpeedtestExpiry: time.Minute,
	}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, key: key, agents: &fakeAgents{online: []string{"dev1"}}, ui: &fakeUI{}}
	r.e = New(st, key, r.agents, r.ui, testConfig())

	mk := func(id, name string) string {
		enc, _ := cryptoutil.Seal(key, []byte("sk-"+id))
		if err := st.CreateProvider(store.Provider{
			ID: id, Name: name, Protocol: "anthropic",
			BaseURL: "https://" + id + ".example", APIKeyEnc: enc, CostCoefficient: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	r.pA, r.pB = mk("prov-a", "站点A"), mk("prov-b", "站点B")
	if err := st.SetFallbackOrder("claude-code", []string{r.pA, r.pB}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppState("claude-code", r.pA, "admin"); err != nil {
		t.Fatal(err)
	}
	return r
}

func hardReport(app, provider string) wire.HealthReport {
	return wire.HealthReport{App: app, ProviderID: provider, Kind: wire.HealthKindHard, Count: 3}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

// ---- tests ----

func TestFailoverSwitchesToNextCandidate(t *testing.T) {
	r := newRig(t)
	r.e.HandleHealthReport("dev1", hardReport("claude-code", r.pA))

	waitCond(t, "switch to B", func() bool {
		st, err := r.st.GetAppState("claude-code")
		return err == nil && st.ActiveProviderID == r.pB
	})
	// SetAppState 先于 Broadcast，等待式断言避免时序毛刺。
	waitCond(t, "agent broadcast", func() bool { return r.agents.broadcastCount() >= 1 })
	kind, payload := r.ui.lastKindAction(t)
	if kind != "failover" || !contains(payload, `"action":"switched"`) {
		t.Fatalf("event = %s %s", kind, payload)
	}
	// 失败者进入冷却。
	h, _ := r.st.GetProviderHealth(r.pA)
	if !h.InCooldown(time.Now().Unix()) || h.DemoteCount != 1 {
		t.Fatalf("health = %+v", h)
	}
}

func TestNegativeEvidenceVeto(t *testing.T) {
	r := newRig(t)
	// 另一设备 30s 内在 A 上有成功请求 → 否决。
	if _, err := r.st.InsertUsageRecords("other-dev", []wire.UsageRecord{{
		RequestID: "req-ok", TS: time.Now().Unix(), App: "claude-code",
		ProviderID: r.pA, Model: "m", Status: 200, UsageSource: "wire",
	}}); err != nil {
		t.Fatal(err)
	}
	r.e.HandleHealthReport("dev1", hardReport("claude-code", r.pA))

	waitCond(t, "veto event", func() bool {
		r.ui.mu.Lock()
		defer r.ui.mu.Unlock()
		return len(r.ui.events) > 0 && contains(r.ui.events[len(r.ui.events)-1].Payload, `"action":"vetoed"`)
	})
	st, _ := r.st.GetAppState("claude-code")
	if st.ActiveProviderID != r.pA {
		t.Fatal("vetoed report still switched")
	}
}

func TestNoHealthyCandidateNotifiesOnly(t *testing.T) {
	r := newRig(t)
	// B 也在冷却中 → 无健康候选。
	if _, err := r.st.DemoteProvider(r.pB); err != nil {
		t.Fatal(err)
	}
	r.e.HandleHealthReport("dev1", hardReport("claude-code", r.pA))

	waitCond(t, "no_candidate event", func() bool {
		r.ui.mu.Lock()
		defer r.ui.mu.Unlock()
		for _, ev := range r.ui.events {
			if contains(ev.Payload, `"action":"no_candidate"`) {
				return true
			}
		}
		return false
	})
	st, _ := r.st.GetAppState("claude-code")
	if st.ActiveProviderID != r.pA {
		t.Fatal("switched despite no healthy candidate")
	}
}

func TestConfigKindMarksNeedsAttention(t *testing.T) {
	r := newRig(t)
	rep := hardReport("claude-code", r.pA)
	rep.Kind = wire.HealthKindConfig
	r.e.HandleHealthReport("dev1", rep)

	waitCond(t, "needs_attention set", func() bool {
		h, _ := r.st.GetProviderHealth(r.pA)
		return h.NeedsAttention
	})
	// needs_attention 的 A 不会被选为候选（B 是故障者也被排除 → 无候选）。
	if p, ok := r.e.pickCandidate("claude-code", r.pB); ok {
		t.Fatalf("needs_attention provider selected as candidate: %+v", p)
	}
}

func TestFailoverRateLimitPerApp(t *testing.T) {
	r := newRig(t)
	r.e.HandleHealthReport("dev1", hardReport("claude-code", r.pA))
	waitCond(t, "first switch", func() bool {
		st, _ := r.st.GetAppState("claude-code")
		return st.ActiveProviderID == r.pB
	})
	// 立即再报 B 故障：限速窗口内不得再切。
	r.e.HandleHealthReport("dev1", hardReport("claude-code", r.pB))
	time.Sleep(60 * time.Millisecond) // 防抖 20ms + 裁决余量，仍在 200ms 限速窗内
	st, _ := r.st.GetAppState("claude-code")
	if st.ActiveProviderID != r.pB {
		t.Fatal("rate limit did not hold")
	}
}

func TestCooldownExponentAndProbeRecovery(t *testing.T) {
	r := newRig(t)
	// 连续降级 3 次：300 → 600 → 1200（秒）。
	now := time.Now().Unix()
	h1, _ := r.st.DemoteProvider(r.pA)
	h2, _ := r.st.DemoteProvider(r.pA)
	h3, _ := r.st.DemoteProvider(r.pA)
	d1, d2, d3 := h1.CooldownUntil-now, h2.CooldownUntil-now, h3.CooldownUntil-now
	if d1 < 295 || d1 > 305 || d2 < 595 || d2 > 605 || d3 < 1195 || d3 > 1205 {
		t.Fatalf("cooldowns = %d %d %d", d1, d2, d3)
	}

	// 恢复探测：连续 2 次成功 → 清冷却 + recovered 事件；1 次失败重置连击。
	if rec, _ := r.st.RecordProbe(r.pA, true); rec {
		t.Fatal("recovered after single OK")
	}
	if _, err := r.st.RecordProbe(r.pA, false); err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.st.RecordProbe(r.pA, true); rec {
		t.Fatal("failure did not reset the streak")
	}
	rec, _ := r.st.RecordProbe(r.pA, true)
	if !rec {
		t.Fatal("2 consecutive OKs should recover")
	}
	h, _ := r.st.GetProviderHealth(r.pA)
	if h.InCooldown(time.Now().Unix()) || h.NeedsAttention {
		t.Fatalf("recovered health = %+v", h)
	}
}

func TestProbeDispatchAndRecoveryEvent(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.DemoteProvider(r.pA); err != nil {
		t.Fatal(err)
	}
	r.e.scheduleProbe(r.pA, 0)

	// 手动驱动调度器节拍。
	waitCond(t, "probe_cmd dispatched", func() bool {
		r.e.dispatchDueProbes()
		r.agents.mu.Lock()
		defer r.agents.mu.Unlock()
		return len(r.agents.sent) > 0
	})
	var cmd wire.ProbeCmd
	r.agents.mu.Lock()
	env := r.agents.sent[0]
	r.agents.mu.Unlock()
	if env.Type != wire.TypeProbeCmd || env.Decode(&cmd) != nil || cmd.Target.ProviderID != r.pA {
		t.Fatalf("cmd = %+v", env)
	}
	if cmd.Target.APIKey != "sk-prov-a" {
		t.Fatalf("probe target key not decrypted: %q", cmd.Target.APIKey)
	}

	// 第一针成功 → 立即安排第二针；第二针成功 → recovered 事件。
	r.e.HandleProbeResult("dev1", wire.ProbeResult{ProbeID: cmd.ProbeID, ProviderID: r.pA, OK: true, LatencyMS: 5})
	waitCond(t, "second probe dispatched", func() bool {
		r.e.dispatchDueProbes()
		r.agents.mu.Lock()
		defer r.agents.mu.Unlock()
		return len(r.agents.sent) >= 2
	})
	r.agents.mu.Lock()
	env2 := r.agents.sent[len(r.agents.sent)-1]
	r.agents.mu.Unlock()
	var cmd2 wire.ProbeCmd
	env2.Decode(&cmd2)
	r.e.HandleProbeResult("dev1", wire.ProbeResult{ProbeID: cmd2.ProbeID, ProviderID: r.pA, OK: true, LatencyMS: 6})

	waitCond(t, "recovered event", func() bool {
		r.ui.mu.Lock()
		defer r.ui.mu.Unlock()
		for _, ev := range r.ui.events {
			if ev.Kind == "probe" && contains(ev.Payload, `"action":"recovered"`) {
				return true
			}
		}
		return false
	})
	// 不自动切回：app_state 不变。
	st, _ := r.st.GetAppState("claude-code")
	if st.ActiveProviderID != r.pA {
		t.Fatal("app state changed by probe recovery")
	}
}

func TestSpeedtestAggregation(t *testing.T) {
	r := newRig(t)
	r.agents.online = []string{"dev1", "dev2"}
	id, err := r.e.StartSpeedtest()
	if err != nil {
		t.Fatal(err)
	}
	r.e.HandleSpeedtestResult("dev1", wire.SpeedtestResult{TestID: id, Results: []wire.ProbeResult{
		{ProviderID: r.pA, OK: true, LatencyMS: 12}, {ProviderID: r.pB, OK: false, Error: "503"},
	}})
	run := r.e.SpeedtestLatest()
	if run == nil || run.ID != id || len(run.Results["dev1"]) != 2 {
		t.Fatalf("latest = %+v", run)
	}
	if len(run.Expected) != 2 {
		t.Fatalf("expected devices = %v", run.Expected)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
