package forward

// tee.go — O(1)-memory usage extraction from the response stream. The tee
// wraps the upstream body: the client keeps receiving byte-identical data
// while a bounded line parser watches for usage-bearing SSE events.
// M1 wires the anthropic parser only; the openai (Responses) parser and the
// Hub reporting pipeline land in M2 (research/03 is the field spec).

import (
	"bytes"
	"encoding/json"
	"io"
)

// Usage is what the Agent will ship to the Hub (M2 extends per research/03:
// usage_source, request_id, duration...).
type Usage struct {
	ProviderID   string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheWrite   int64
	CacheRead    int64
	Status       int
	Done         bool // saw message_stop
	HighWater    int  // parser line-buffer high-water mark (test instrumentation)
}

// UsageSink receives one Usage per parsed response. Called from Close — must
// not block.
type UsageSink func(Usage)

type usageTee struct {
	rc       io.ReadCloser
	parser   sseParser
	sink     UsageSink
	finished bool
}

func newUsageTee(rc io.ReadCloser, status int, providerID string, sink UsageSink) *usageTee {
	t := &usageTee{rc: rc, sink: sink}
	t.parser.usage.Status = status
	t.parser.usage.ProviderID = providerID
	return t
}

func (t *usageTee) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.parser.feed(p[:n]) // never blocks, never mutates p
	}
	if err == io.EOF {
		t.finish()
	}
	return n, err
}

// Close is called by ReverseProxy on normal completion AND on the abort path
// (client disconnect / upstream failure), so finalization here is exhaustive:
// partial streams still emit a record (Done=false — at-least-once accounting).
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
	t.parser.flushTail()
	t.parser.usage.HighWater = t.parser.maxLine
	if t.sink != nil {
		t.sink(t.parser.usage)
	}
}

// sseParser keeps at most one bounded line in memory regardless of stream
// length; oversized lines are discarded (anthropic usage events are tiny).
type sseParser struct {
	line     []byte
	overflow bool
	maxLine  int // high-water mark, exported via Usage for the O(1) test
	usage    Usage
}

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
	if len(s.line)+len(b) > maxSSELine {
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

type usageFields struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

type ssePayload struct {
	Type    string `json:"type"`
	Message *struct {
		Model string       `json:"model"`
		Usage *usageFields `json:"usage"`
	} `json:"message"`
	Usage *usageFields `json:"usage"` // message_delta carries usage at top level
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
	var p ssePayload
	if json.Unmarshal(rest, &p) != nil {
		return
	}
	switch p.Type {
	case "message_start":
		if p.Message != nil {
			s.usage.Model = p.Message.Model
			if u := p.Message.Usage; u != nil {
				s.applyUsage(u)
			}
		}
	case "message_delta":
		if p.Usage != nil {
			s.applyUsage(p.Usage) // cumulative values — overwrite, never add (research/03 C1)
		}
	case "message_stop":
		s.usage.Done = true
	}
}

func (s *sseParser) applyUsage(u *usageFields) {
	if u.InputTokens != nil {
		s.usage.InputTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		s.usage.OutputTokens = *u.OutputTokens
	}
	if u.CacheCreationInputTokens != nil {
		s.usage.CacheWrite = *u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens != nil {
		s.usage.CacheRead = *u.CacheReadInputTokens
	}
}
