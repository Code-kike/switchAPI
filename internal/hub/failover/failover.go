// Package failover is the Hub-side arbitration engine (research/08 参数表
// #9-#18 + 父 design.md §5)：health_report 防抖汇集 → 负证据否决 → 沿备选
// 序列全局切换（冷却指数退避）→ 事件 + Agent 推送 + UI 通知；随后对冷却中
// 的供应商轮转下发恢复探测（连续 2 次成功 = 恢复，不自动切回）。
// 手动测速（speedtest）复用同一条上行通道，在此聚合按设备展示。
package failover

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/google/uuid"
)

// Agents is what the engine needs from realtime.Hub.
type Agents interface {
	Broadcast() // 配置变化 → 全量 config_push
	SendTo(deviceID string, env wire.Envelope) error
	BroadcastEnvelope(env wire.Envelope)
	OnlineDevices() []string
}

// Notifier is what the engine needs from the ws/ui layer (api.Server).
type Notifier interface {
	NotifyEvent(ev store.Event)
	NotifyState()
}

// Config carries the arbitration knobs (research/08 defaults; tests shrink).
type Config struct {
	DebounceWindow  time.Duration // 汇集并发上报
	VetoWindow      int64         // 负证据回看窗口（秒）
	FailoverMinGap  time.Duration // 每 App 两次自动切换最小间隔
	ProbeBase       time.Duration // 恢复探测起始间隔
	ProbeMax        time.Duration // 恢复探测间隔上限
	ProbeTimeoutS   int           // 单次探测超时（秒）
	ProbeScan       time.Duration // 冷却扫描节拍
	SpeedtestExpiry time.Duration // 测速结果保留
}

// DefaultConfig returns the research/08 values.
func DefaultConfig() Config {
	return Config{
		DebounceWindow: 5 * time.Second, VetoWindow: 30,
		FailoverMinGap: 10 * time.Second,
		ProbeBase:      60 * time.Second, ProbeMax: 900 * time.Second,
		ProbeTimeoutS: 10, ProbeScan: 5 * time.Second,
		SpeedtestExpiry: 10 * time.Minute,
	}
}

// Engine implements realtime.ReportHandler.
type Engine struct {
	st        *store.Store
	masterKey []byte
	agents    Agents
	ui        Notifier
	cfg       Config

	mu           sync.Mutex
	pending      map[string]*episode  // provider_id → 防抖中的一轮上报
	lastFailover map[string]time.Time // app → 上次自动切换
	probes       map[string]*probeState
	rotate       int // 探测执行设备的轮转游标

	stMu      sync.Mutex
	speedtest *SpeedtestRun
}

type episode struct {
	timer   *time.Timer
	reports []deviceReport
}

type deviceReport struct {
	deviceID string
	r        wire.HealthReport
}

type probeState struct {
	backoff    int // 指数指数（0 → base）
	nextAt     time.Time
	inFlightID string
}

type SpeedtestRun struct {
	ID        string                        `json:"id"`
	StartedAt int64                         `json:"started_at"`
	Expected  []string                      `json:"expected_devices"`
	Results   map[string][]wire.ProbeResult `json:"results"` // device_id → results
}

// New builds the engine; call Run to start the probe loop.
func New(st *store.Store, masterKey []byte, agents Agents, ui Notifier, cfg Config) *Engine {
	return &Engine{
		st: st, masterKey: masterKey, agents: agents, ui: ui, cfg: cfg,
		pending:      map[string]*episode{},
		lastFailover: map[string]time.Time{},
		probes:       map[string]*probeState{},
	}
}

// event appends to the timeline and fans out to UI clients.
func (e *Engine) event(kind string, payload map[string]string) {
	raw, _ := json.Marshal(payload)
	ev, err := e.st.AppendEvent(kind, string(raw))
	if err != nil {
		log.Printf("failover: append event: %v", err)
		return
	}
	if e.ui != nil {
		e.ui.NotifyEvent(ev)
	}
}

// ---- health_report 汇集与仲裁 ----

// HandleHealthReport implements realtime.ReportHandler: start/extend a
// debounce episode for the provider, arbitrating once the window closes.
func (e *Engine) HandleHealthReport(deviceID string, r wire.HealthReport) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep := e.pending[r.ProviderID]
	if ep == nil {
		ep = &episode{}
		e.pending[r.ProviderID] = ep
		ep.timer = time.AfterFunc(e.cfg.DebounceWindow, func() { e.arbitrate(r.ProviderID) })
	}
	ep.reports = append(ep.reports, deviceReport{deviceID, r})
}

// arbitrate runs after the debounce window: veto check → rate limit →
// candidate selection → global switch.
func (e *Engine) arbitrate(providerID string) {
	e.mu.Lock()
	ep := e.pending[providerID]
	delete(e.pending, providerID)
	e.mu.Unlock()
	if ep == nil || len(ep.reports) == 0 {
		return
	}
	first := ep.reports[0]
	app, kind := first.r.App, first.r.Kind

	prov, err := e.st.GetProvider(providerID)
	if err != nil {
		return // provider deleted mid-episode
	}

	// 配置类（401/403）：标记 needs_attention（不自愈，UI 提示查 key），仍尝试切换。
	if kind == wire.HealthKindConfig {
		e.st.MarkNeedsAttention(providerID, true)
	}

	// 负证据否决：其他设备 30s 内在同供应商有成功记录 → 判设备本地网络问题。
	if ok, err := e.st.HasRecentSuccess(providerID, first.deviceID, e.cfg.VetoWindow); err == nil && ok {
		e.event("failover", map[string]string{
			"action": "vetoed", "app": app, "provider_id": providerID, "provider": prov.Name,
			"reason": "其他设备在 30 秒内经该供应商成功请求，疑似上报设备本地网络问题",
		})
		return
	}

	// 该供应商必须仍是该 App 的当前生效者（可能已被手动切走）。
	stt, err := e.st.GetAppState(app)
	if err != nil || stt.ActiveProviderID != providerID {
		return
	}

	// 每 App 限速。
	e.mu.Lock()
	if time.Since(e.lastFailover[app]) < e.cfg.FailoverMinGap {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	next, ok := e.pickCandidate(app, providerID)
	// 降级失败者进入冷却（指数退避），恢复探测随之启动。
	if _, err := e.st.DemoteProvider(providerID); err != nil {
		log.Printf("failover: demote %s: %v", providerID, err)
	}
	e.scheduleProbe(providerID, 0)

	if !ok {
		// 无健康候选：不切换，只通知（research/08 #13）。
		e.event("failover", map[string]string{
			"action": "no_candidate", "app": app, "provider_id": providerID, "provider": prov.Name,
			"reason": "备选序列中没有健康候选，保持当前供应商",
		})
		return
	}

	if err := e.st.SetAppState(app, next.ID, "failover"); err != nil {
		log.Printf("failover: set app state: %v", err)
		return
	}
	e.mu.Lock()
	e.lastFailover[app] = time.Now()
	e.mu.Unlock()

	e.event("failover", map[string]string{
		"action": "switched", "app": app,
		"from": providerID, "from_name": prov.Name,
		"to": next.ID, "to_name": next.Name, "kind": kind,
	})
	e.agents.Broadcast()
	if e.ui != nil {
		e.ui.NotifyState()
	}
	log.Printf("failover: %s %s(%s) → %s（%s）", app, prov.Name, providerID, next.Name, kind)
}

// pickCandidate walks the app's fallback order and returns the first healthy
// provider that is not the failed one（跳过冷却中与 needs_attention）.
func (e *Engine) pickCandidate(app, failedID string) (store.Provider, bool) {
	order, err := e.st.GetFallbackOrder(app)
	if err != nil {
		return store.Provider{}, false
	}
	now := time.Now().Unix()
	for _, pid := range order {
		if pid == failedID {
			continue
		}
		h, err := e.st.GetProviderHealth(pid)
		if err != nil || h.InCooldown(now) || h.NeedsAttention {
			continue
		}
		p, err := e.st.GetProvider(pid)
		if err != nil {
			continue
		}
		return p, true
	}
	return store.Provider{}, false
}

// ---- 恢复探测 ----

// Run starts the probe scheduler loop; blocks until ctx is done.
func (e *Engine) Run(ctx interface{ Done() <-chan struct{} }) {
	tick := time.NewTicker(e.cfg.ProbeScan)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.dispatchDueProbes()
		}
	}
}

// scheduleProbe (re)arms the probe plan for a provider. backoff k → 间隔
// base×2^k 封顶 max，±20% 抖动（research/08 #15）。
func (e *Engine) scheduleProbe(providerID string, backoff int) {
	interval := e.cfg.ProbeBase << min(backoff, 10)
	if interval > e.cfg.ProbeMax {
		interval = e.cfg.ProbeMax
	}
	jittered := time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64()))
	e.mu.Lock()
	e.probes[providerID] = &probeState{backoff: backoff, nextAt: time.Now().Add(jittered)}
	e.mu.Unlock()
}

// dispatchDueProbes sends probe_cmd for every due provider to a rotating
// online agent（单台执行，避免 N 台并发烧钱；research/08 #14）.
func (e *Engine) dispatchDueProbes() {
	now := time.Now()
	e.mu.Lock()
	due := make(map[string]*probeState)
	for pid, ps := range e.probes {
		if ps.inFlightID == "" && now.After(ps.nextAt) {
			due[pid] = ps
		}
	}
	e.mu.Unlock()
	if len(due) == 0 {
		return
	}
	devices := e.agents.OnlineDevices()
	if len(devices) == 0 {
		return // 无在线 Agent，下个节拍再试
	}

	for pid, ps := range due {
		h, err := e.st.GetProviderHealth(pid)
		if err != nil {
			continue
		}
		if !h.InCooldown(now.Unix()) && !h.NeedsAttention {
			// 已不需要探测（手动编辑清除/已恢复）。
			e.mu.Lock()
			delete(e.probes, pid)
			e.mu.Unlock()
			continue
		}
		target, err := e.probeTarget(pid)
		if err != nil {
			log.Printf("failover: probe target %s: %v", pid, err)
			continue
		}
		probeID := uuid.NewString()
		e.mu.Lock()
		dev := devices[e.rotate%len(devices)]
		e.rotate++
		ps.inFlightID = probeID
		e.mu.Unlock()

		env, err := wire.NewEnvelope(wire.TypeProbeCmd, wire.ProbeCmd{
			ProbeID: probeID, Target: target, TimeoutS: e.cfg.ProbeTimeoutS,
		})
		if err != nil {
			continue
		}
		if err := e.agents.SendTo(dev, env); err != nil {
			// 设备刚下线：解除 in-flight，下个节拍换设备重试。
			e.mu.Lock()
			ps.inFlightID = ""
			e.mu.Unlock()
		}
	}
}

// HandleProbeResult implements realtime.ReportHandler.
func (e *Engine) HandleProbeResult(deviceID string, r wire.ProbeResult) {
	e.mu.Lock()
	ps := e.probes[r.ProviderID]
	if ps == nil || ps.inFlightID != r.ProbeID {
		e.mu.Unlock()
		return // 过期/未知探测
	}
	ps.inFlightID = ""
	backoff := ps.backoff
	e.mu.Unlock()

	recovered, err := e.st.RecordProbe(r.ProviderID, r.OK)
	if err != nil {
		log.Printf("failover: record probe: %v", err)
		return
	}
	prov, _ := e.st.GetProvider(r.ProviderID)
	switch {
	case recovered:
		e.mu.Lock()
		delete(e.probes, r.ProviderID)
		e.mu.Unlock()
		// 恢复：通知 + 一键切回由 UI 承担；不自动切回（research/08 #18）。
		e.event("probe", map[string]string{
			"action": "recovered", "provider_id": r.ProviderID, "provider": prov.Name,
			"latency_ms": fmt.Sprint(r.LatencyMS),
		})
	case r.OK:
		// 1/2 连击：尽快补第二针（不退避）。
		e.scheduleProbe(r.ProviderID, 0)
	default:
		e.scheduleProbe(r.ProviderID, backoff+1)
	}
}

// probeTarget builds the decrypted probe target; model = last used through
// this provider, else a per-protocol default.
func (e *Engine) probeTarget(providerID string) (wire.ProbeTarget, error) {
	p, err := e.st.GetProvider(providerID)
	if err != nil {
		return wire.ProbeTarget{}, err
	}
	plain, err := cryptoutil.Open(e.masterKey, p.APIKeyEnc)
	if err != nil {
		return wire.ProbeTarget{}, err
	}
	model, found, _ := e.st.LastModelForProvider(providerID)
	if !found {
		if p.Protocol == "anthropic" {
			model = "claude-haiku-4-5"
		} else {
			model = "gpt-5"
		}
	}
	return wire.ProbeTarget{
		ProviderID: p.ID, Protocol: p.Protocol, BaseURL: p.BaseURL,
		APIKey: string(plain), Model: model,
	}, nil
}

// ---- 手动测速 ----

// StartSpeedtest broadcasts a speedtest_cmd covering every provider to all
// online agents; results aggregate per device (design.md §5).
func (e *Engine) StartSpeedtest() (string, error) {
	providers, err := e.st.ListProviders()
	if err != nil {
		return "", err
	}
	if len(providers) == 0 {
		return "", fmt.Errorf("没有可测的供应商")
	}
	devices := e.agents.OnlineDevices()
	if len(devices) == 0 {
		return "", fmt.Errorf("没有在线设备可执行测速")
	}
	targets := make([]wire.ProbeTarget, 0, len(providers))
	for _, p := range providers {
		t, err := e.probeTarget(p.ID)
		if err != nil {
			continue
		}
		targets = append(targets, t)
	}
	id := uuid.NewString()
	env, err := wire.NewEnvelope(wire.TypeSpeedtestCmd, wire.SpeedtestCmd{TestID: id, Targets: targets})
	if err != nil {
		return "", err
	}

	e.stMu.Lock()
	e.speedtest = &SpeedtestRun{
		ID: id, StartedAt: time.Now().Unix(), Expected: devices,
		Results: map[string][]wire.ProbeResult{},
	}
	e.stMu.Unlock()

	e.agents.BroadcastEnvelope(env)
	e.event("speedtest", map[string]string{"action": "started", "test_id": id,
		"devices": fmt.Sprint(len(devices)), "providers": fmt.Sprint(len(targets))})
	return id, nil
}

// HandleSpeedtestResult implements realtime.ReportHandler.
func (e *Engine) HandleSpeedtestResult(deviceID string, r wire.SpeedtestResult) {
	e.stMu.Lock()
	defer e.stMu.Unlock()
	if e.speedtest == nil || e.speedtest.ID != r.TestID {
		return
	}
	e.speedtest.Results[deviceID] = r.Results
	done := len(e.speedtest.Results) >= len(e.speedtest.Expected)
	if done && e.ui != nil {
		// 借 events 通道让 UI 刷新测速面板（内容走 GET /speedtest/latest）。
		go e.event("speedtest", map[string]string{"action": "completed", "test_id": r.TestID})
	}
}

// SpeedtestLatest returns the latest run (nil when none/expired). 深拷贝：
// 返回值会在锁外被 JSON 序列化，浅拷贝的 map 会与 HandleSpeedtestResult 竞争。
func (e *Engine) SpeedtestLatest() *SpeedtestRun {
	e.stMu.Lock()
	defer e.stMu.Unlock()
	if e.speedtest == nil ||
		time.Since(time.Unix(e.speedtest.StartedAt, 0)) > e.cfg.SpeedtestExpiry {
		return nil
	}
	cp := SpeedtestRun{
		ID: e.speedtest.ID, StartedAt: e.speedtest.StartedAt,
		Expected: append([]string(nil), e.speedtest.Expected...),
		Results:  make(map[string][]wire.ProbeResult, len(e.speedtest.Results)),
	}
	for dev, rs := range e.speedtest.Results {
		cp.Results[dev] = append([]wire.ProbeResult(nil), rs...)
	}
	return &cp
}
