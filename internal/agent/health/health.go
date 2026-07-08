// Package health is the Agent-side failure classifier and edge-triggered
// reporter (research/08 参数表 #1/#2/#7/#8)：按 provider 维护连续硬失败
// （阈值 3、新鲜度 300s）、429 独立升级（6 次跨 ≥60s）、401/403 配置类
// （3 连）三条通道；成功请求清空一切。达到阈值时经 Notifier 吐一条
// wire.HealthReport（边沿触发——同一轮故障只报一次，成功复位后才可再报）。
package health

import (
	"sync"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// Config carries the thresholds (research/08 defaults).
type Config struct {
	HardThreshold   int           // 连续硬失败上报阈值
	Freshness       time.Duration // 距上次失败超过则计数清零
	RateThreshold   int           // 连续 429 次数
	RateSpan        time.Duration // 首末 429 须跨越的最小时长
	ConfigThreshold int           // 连续 401/403
}

// DefaultConfig returns the research/08 parameter table values.
func DefaultConfig() Config {
	return Config{
		HardThreshold: 3, Freshness: 300 * time.Second,
		RateThreshold: 6, RateSpan: 60 * time.Second,
		ConfigThreshold: 3,
	}
}

// Notifier receives edge-triggered reports (hubclient sends them upstream).
type Notifier func(wire.HealthReport)

// Tracker consumes forward.Usage records and fires reports.
type Tracker struct {
	cfg    Config
	notify Notifier

	mu        sync.Mutex
	providers map[string]*counters
	now       func() time.Time // 可注入时钟（测试）
}

type counters struct {
	hard         int
	lastHardAt   time.Time
	hardReported bool

	rate         int
	rateFirstAt  time.Time
	rateReported bool

	config         int
	configReported bool

	samples []wire.ErrorSample // 最近 ≤5 条证据（环形）
	app     string             // 最近一次观测到的 app（报告归属）
}

// New builds a tracker.
func New(cfg Config, notify Notifier) *Tracker {
	return &Tracker{cfg: cfg, notify: notify,
		providers: map[string]*counters{}, now: time.Now}
}

// class is the research/08 failure taxonomy.
type class int

const (
	classSuccess class = iota
	classHard
	classRate
	classConfig
	classIgnore
)

func classify(u forward.Usage) class {
	switch u.ErrorKind {
	case "":
		if u.Status >= 200 && u.Status < 300 {
			return classSuccess
		}
		return classIgnore // 4xx 业务错等：只进明细
	case "client_abort":
		return classIgnore
	case "http_429":
		return classRate
	case "http_auth":
		return classConfig
	case "connect", "tls", "transport", "upstream_5xx", "stream_aborted", "fake_200",
		"timeout_first_byte", "timeout_idle", "timeout_total":
		return classHard
	}
	return classIgnore
}

// Observe feeds one completed usage record through the classifier. Reports
// fire synchronously on the caller's goroutine — Notifier must not block
// （hubclient 侧是带缓冲 channel）.
func (t *Tracker) Observe(u forward.Usage) {
	if u.ProviderID == "" {
		return
	}
	cl := classify(u)
	if cl == classIgnore {
		return
	}

	t.mu.Lock()
	c := t.providers[u.ProviderID]
	if c == nil {
		c = &counters{}
		t.providers[u.ProviderID] = c
	}
	c.app = u.App
	now := t.now()

	var report *wire.HealthReport
	switch cl {
	case classSuccess:
		*c = counters{app: c.app} // 成功清零全部通道与已报标记
	case classHard:
		if !c.lastHardAt.IsZero() && now.Sub(c.lastHardAt) > t.cfg.Freshness {
			c.hard = 0 // 陈旧计数不累积（研究/08 #2）
			c.hardReported = false
		}
		c.hard++
		c.lastHardAt = now
		c.push(u, now)
		if c.hard >= t.cfg.HardThreshold && !c.hardReported {
			c.hardReported = true
			report = c.report(u.ProviderID, wire.HealthKindHard, c.hard)
		}
	case classRate:
		if c.rate == 0 || now.Sub(c.rateFirstAt) > 2*t.cfg.RateSpan {
			c.rate = 0
			c.rateFirstAt = now
			c.rateReported = false
		}
		c.rate++
		c.push(u, now)
		if c.rate >= t.cfg.RateThreshold && now.Sub(c.rateFirstAt) >= t.cfg.RateSpan && !c.rateReported {
			c.rateReported = true
			report = c.report(u.ProviderID, wire.HealthKindRateLimit, c.rate)
		}
	case classConfig:
		c.config++
		c.push(u, now)
		if c.config >= t.cfg.ConfigThreshold && !c.configReported {
			c.configReported = true
			report = c.report(u.ProviderID, wire.HealthKindConfig, c.config)
		}
	}
	t.mu.Unlock()

	if report != nil && t.notify != nil {
		t.notify(*report)
	}
}

func (c *counters) push(u forward.Usage, now time.Time) {
	s := wire.ErrorSample{Kind: u.ErrorKind, TS: now.Unix(), Status: u.Status, LatencyMS: u.DurationMS}
	c.samples = append(c.samples, s)
	if len(c.samples) > 5 {
		c.samples = c.samples[len(c.samples)-5:]
	}
}

func (c *counters) report(providerID, kind string, count int) *wire.HealthReport {
	samples := make([]wire.ErrorSample, len(c.samples))
	copy(samples, c.samples)
	return &wire.HealthReport{
		App: c.app, ProviderID: providerID, Kind: kind, Count: count, Samples: samples,
	}
}
