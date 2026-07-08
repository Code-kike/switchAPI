package forward

// tee.go — usage extraction from the response body. The tee wraps the upstream
// body: the client keeps receiving byte-identical data while a parser watches
// for usage. Two parsers share one tee shell:
//   - sseParser  streams event lines (O(1) memory), anthropic + openai wire,
//   - jsonParser buffers a non-streaming JSON body (≤8MB) and reads top-level
//     usage at the end.
// Field spec + interruption matrix: research/03. The completed Usage maps 1:1
// to wire.UsageRecord (see ToRecord).

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

// Usage is what the Agent ships to the Hub — pure metadata (never content).
// Identity/context fields come from the request decision; token fields come
// from the wire parser.
type Usage struct {
	RequestID       string
	App             string // claude-code | codex
	ProviderID      string
	TS              int64 // request start, unix seconds
	Model           string
	ModelRedirected string // redirect target ("to"); empty when no redirect applied
	InputTokens     int64
	OutputTokens    int64
	CacheWrite      int64
	CacheRead       int64
	DurationMS      int64
	Status          int
	ErrorKind       string
	UsageSource     string // wire | none
	Done            bool   // saw a terminal event (message_stop / response.completed|incomplete|failed)
	HighWater       int    // parser line-buffer high-water mark (test instrumentation)
}

// ToRecord converts to the wire form reported to the Hub.
func (u Usage) ToRecord() wire.UsageRecord {
	return wire.UsageRecord{
		RequestID:        u.RequestID,
		TS:               u.TS,
		App:              u.App,
		ProviderID:       u.ProviderID,
		Model:            u.Model,
		ModelRedirected:  u.ModelRedirected,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheWriteTokens: u.CacheWrite,
		CacheReadTokens:  u.CacheRead,
		DurationMS:       u.DurationMS,
		Status:           u.Status,
		ErrorKind:        u.ErrorKind,
		UsageSource:      u.UsageSource,
	}
}

// UsageSink receives one Usage per parsed billing response. Called from the
// tee's finish path (EOF or Close) — must not block the forwarding goroutine.
type UsageSink func(Usage)

// parsedUsage is what a parser extracts from the wire, before it is combined
// with the request decision into a completed Usage.
type parsedUsage struct {
	Model      string
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
	Done       bool // reached a terminal wire event
	SawTokens  bool // at least one token field was present → usage_source=wire
	SawError   bool // provider signalled failure inside a 2xx stream (fake-200, research/08)
	HighWater  int
}

// usageParser is the strategy plugged into the tee.
type usageParser interface {
	feed(b []byte) // fed every read chunk; never blocks, never mutates input
	done()         // end-of-body: flush the tail / parse the buffered JSON
	result() parsedUsage
}

type usageTee struct {
	rc       io.ReadCloser
	parser   usageParser
	d        *decision
	status   int
	stream   bool // SSE (vs buffered JSON) — gates the stream_aborted classification
	sink     UsageSink
	finished bool
	idle     *time.Timer // 流中静默看门狗（研究/08 #5，仅流式计费响应）
}

func newStreamTee(rc io.ReadCloser, status int, d *decision, sink UsageSink) *usageTee {
	cap := maxSSELine
	if d.up.Protocol == "openai" {
		cap = maxOpenAILine // response.completed embeds the whole output on one line
	}
	t := &usageTee{rc: rc, parser: &sseParser{proto: d.up.Protocol, lineCap: cap},
		d: d, status: status, stream: true, sink: sink}
	t.idle = d.armIdleTimer(streamIdleTimeout) // nil for non-billing/unarmed decisions
	return t
}

func newJSONTee(rc io.ReadCloser, status int, d *decision, sink UsageSink, parse bool) *usageTee {
	return &usageTee{rc: rc, parser: &jsonParser{proto: d.up.Protocol, parse: parse, cap: maxJSONBody},
		d: d, status: status, stream: false, sink: sink}
}

func (t *usageTee) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.parser.feed(p[:n]) // never blocks, never mutates p
		if t.idle != nil {
			t.idle.Reset(streamIdleTimeout)
		}
	}
	if err == io.EOF {
		t.finish()
	}
	return n, err
}

// Close runs on normal completion AND on the abort path (client disconnect /
// upstream failure), so finalization here is exhaustive: partial streams still
// emit a record (Done=false — at-least-once accounting, research/03 C7).
func (t *usageTee) Close() error {
	err := t.rc.Close()
	t.finish()
	return err
}

func (t *usageTee) finish() {
	if t.finished {
		return
	}
	t.finished = true
	t.d.stopTimers()
	t.parser.done()
	pu := t.parser.result()

	u := Usage{
		RequestID:       t.d.requestID,
		App:             t.d.app,
		ProviderID:      t.d.up.ProviderID,
		TS:              t.d.start.Unix(),
		Model:           t.d.recordModel(pu.Model),
		ModelRedirected: t.d.redirModel,
		InputTokens:     pu.Input,
		OutputTokens:    pu.Output,
		CacheWrite:      pu.CacheWrite,
		CacheRead:       pu.CacheRead,
		DurationMS:      time.Since(t.d.start).Milliseconds(),
		Status:          t.status,
		Done:            pu.Done,
		HighWater:       pu.HighWater,
	}
	if pu.SawTokens {
		u.UsageSource = "wire"
	} else {
		u.UsageSource = "none"
	}
	// 错误细分（研究/08 失败分类的观测输入；健康计数在 health 包做）。
	switch {
	case t.status == 429:
		u.ErrorKind = "http_429"
	case t.status == 401 || t.status == 403:
		u.ErrorKind = "http_auth"
	case t.status >= 500:
		u.ErrorKind = "upstream_5xx"
	case pu.SawError:
		u.ErrorKind = "fake_200" // 2xx 外壳、流内以 error/failed 终止
	case t.stream && !pu.Done:
		switch {
		case t.d.getTimeoutKind() != "":
			u.ErrorKind = t.d.getTimeoutKind() // timeout_idle（看门狗掐断）
		case t.d.clientGone():
			u.ErrorKind = "client_abort" // 客户端主动断开：记录但不计入健康
		default:
			u.ErrorKind = "stream_aborted" // 上游在终止事件前断流
		}
	}
	if !t.d.markRecorded() {
		return // ErrorHandler 已为该请求记账（理论不可达，防御双记）
	}
	if t.sink != nil {
		t.sink(u)
	}
}

// ---- SSE parser (anthropic + openai) ----

// sseParser keeps at most one bounded line in memory regardless of stream
// length; oversized lines are discarded (usage-bearing lines fit lineCap:
// anthropic events are tiny, openai response.completed is capped at 8MB).
type sseParser struct {
	proto    string
	lineCap  int
	line     []byte
	overflow bool
	maxLine  int // high-water mark, surfaced via parsedUsage for the O(1) test
	acc      parsedUsage
}

func (s *sseParser) result() parsedUsage {
	s.acc.HighWater = s.maxLine
	return s.acc
}

func (s *sseParser) done() { s.flushTail() }

func (s *sseParser) feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.buffer(b)
			return
		}
		s.buffer(b[:i])
		if !s.overflow {
			s.handleLine(s.line)
		}
		s.line = s.line[:0]
		s.overflow = false
		b = b[i+1:]
	}
}

func (s *sseParser) buffer(b []byte) {
	if s.overflow {
		return
	}
	if len(s.line)+len(b) > s.lineCap {
		s.overflow = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, b...)
	if len(s.line) > s.maxLine {
		s.maxLine = len(s.line)
	}
}

func (s *sseParser) flushTail() {
	if !s.overflow && len(s.line) > 0 {
		s.handleLine(s.line)
	}
	s.line = nil
}

func (s *sseParser) handleLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return // "event:"/comment/blank lines are irrelevant for usage
	}
	rest = bytes.TrimSpace(rest)
	if len(rest) == 0 || bytes.Equal(rest, []byte("[DONE]")) {
		return
	}
	if s.proto == "openai" {
		s.handleOpenAI(rest)
		return
	}
	s.handleAnthropic(rest)
}

// anthropic: message_start seeds input-side + model; message_delta carries the
// cumulative output (overwrite, never add — research/03 C1); message_stop marks
// clean completion.
func (s *sseParser) handleAnthropic(data []byte) {
	var p struct {
		Type    string `json:"type"`
		Message *struct {
			Model string           `json:"model"`
			Usage *wireUsageFields `json:"usage"`
		} `json:"message"`
		Usage *wireUsageFields `json:"usage"`
	}
	if json.Unmarshal(data, &p) != nil {
		return
	}
	switch p.Type {
	case "message_start":
		if p.Message != nil {
			if p.Message.Model != "" {
				s.acc.Model = p.Message.Model
			}
			if p.Message.Usage != nil {
				applyUsage(&s.acc, "anthropic", p.Message.Usage)
			}
		}
	case "message_delta":
		if p.Usage != nil {
			applyUsage(&s.acc, "anthropic", p.Usage)
		}
	case "message_stop":
		s.acc.Done = true
	case "error":
		s.acc.SawError = true // anthropic fake-200：2xx 流内 error 事件
	}
}

// openai Responses: usage rides the terminal event's embedded response object.
// completed is authoritative; incomplete/failed are read best-effort (usage may
// be null — Codex itself tolerates that). research/03 C3, table 2.
func (s *sseParser) handleOpenAI(data []byte) {
	var p struct {
		Type     string `json:"type"`
		Response *struct {
			Model string           `json:"model"`
			Usage *wireUsageFields `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &p) != nil {
		return
	}
	if p.Response != nil && p.Response.Model != "" {
		s.acc.Model = p.Response.Model
	}
	switch p.Type {
	case "response.completed", "response.incomplete", "response.failed":
		if p.Response != nil && p.Response.Usage != nil {
			applyUsage(&s.acc, "openai", p.Response.Usage)
		}
		s.acc.Done = true // reached a terminal event → not a mid-stream abort
		if p.Type == "response.failed" {
			s.acc.SawError = true // openai fake-200（incomplete 是截断非故障）
		}
	}
}

// ---- non-streaming JSON parser ----

// jsonParser buffers the whole body (≤cap) and parses top-level usage once at
// end-of-body. parse is false for non-JSON billing responses (error pages):
// nothing is buffered and the record is emitted with usage_source=none.
type jsonParser struct {
	proto    string
	parse    bool
	cap      int
	buf      []byte
	overflow bool
	acc      parsedUsage
}

func (p *jsonParser) feed(b []byte) {
	if !p.parse || p.overflow {
		return
	}
	if len(p.buf)+len(b) > p.cap {
		p.overflow = true // oversized body: give up parsing, keep passing bytes through
		p.buf = nil
		return
	}
	p.buf = append(p.buf, b...)
}

func (p *jsonParser) done() {
	if !p.parse || p.overflow || len(p.buf) == 0 {
		return
	}
	var tl struct {
		Model string           `json:"model"`
		Usage *wireUsageFields `json:"usage"`
	}
	if json.Unmarshal(p.buf, &tl) != nil {
		return // truncated / not the JSON we expected
	}
	if tl.Model != "" {
		p.acc.Model = tl.Model
	}
	if tl.Usage != nil {
		applyUsage(&p.acc, p.proto, tl.Usage)
	}
	p.acc.Done = true // a complete JSON body is a terminal result
}

func (p *jsonParser) result() parsedUsage { return p.acc }

// ---- shared usage mapping (research/03 tables 1 & 2) ----

// wireUsageFields is the superset of anthropic and openai usage shapes; applied
// per protocol. All fields optional — null/absent → 0 (relay defensiveness).
type wireUsageFields struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"` // anthropic cache write
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`     // anthropic cache read
	InputTokensDetails       *struct {
		CachedTokens *int64 `json:"cached_tokens"` // openai cache read (subset of input_tokens)
	} `json:"input_tokens_details"`
}

func applyUsage(acc *parsedUsage, proto string, u *wireUsageFields) {
	if proto == "openai" {
		var cached int64
		if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
			cached = *u.InputTokensDetails.CachedTokens
		}
		if u.InputTokens != nil {
			// OpenAI input_tokens INCLUDES cached — subtract so cache is not
			// double-billed at full price (research/03 table 2).
			in := *u.InputTokens - cached
			if in < 0 {
				in = 0
			}
			acc.Input = in
			acc.SawTokens = true
		}
		if cached != 0 {
			acc.CacheRead = cached
			acc.SawTokens = true
		}
		acc.CacheWrite = 0 // OpenAI has no cache-write price/field
		if u.OutputTokens != nil {
			acc.Output = *u.OutputTokens
			acc.SawTokens = true
		}
		return
	}
	// anthropic: input_tokens already excludes cache; each field is cumulative
	// (overwrite, never add).
	if u.InputTokens != nil {
		acc.Input = *u.InputTokens
		acc.SawTokens = true
	}
	if u.OutputTokens != nil {
		acc.Output = *u.OutputTokens
		acc.SawTokens = true
	}
	if u.CacheCreationInputTokens != nil {
		acc.CacheWrite = *u.CacheCreationInputTokens
		acc.SawTokens = true
	}
	if u.CacheReadInputTokens != nil {
		acc.CacheRead = *u.CacheReadInputTokens
		acc.SawTokens = true
	}
}
