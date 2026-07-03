package main

// proxy.go — the actual passthrough forwarder under test. Mirrors the Agent
// design in design.md §4: httputil.ReverseProxy + Rewrite hook, immediate
// flush, auth swap, optional model redirect, and an O(1)-memory usage tee.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxRewriteBody = 32 << 20 // cap request buffering for the rewrite path
	maxSSELine     = 1 << 20  // bound for a single SSE line kept by the usage parser
)

// bearerAuth switches upstream auth injection from X-Api-Key to
// Authorization: Bearer — real relays configured via ANTHROPIC_AUTH_TOKEN
// expect the latter. Default false keeps the original test behavior.
var bearerAuth bool

// Usage is what the Agent would ship to the Hub (research #3 refines fields).
type Usage struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheWrite   int64
	CacheRead    int64
	Status       int
	Done         bool // saw message_stop
	HighWater    int  // parser line-buffer high-water mark (prototype instrumentation)
}

type UsageSink func(Usage)

// newProxy builds the forwarder. upstream is the provider base URL; apiKey is
// injected after stripping the local token; redirects maps model names.
func newProxy(upstream *url.URL, apiKey string, redirects map[string]string, sink UsageSink) *httputil.ReverseProxy {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Keep upstream bytes verbatim: never let the Transport inject
	// "Accept-Encoding: gzip" + transparently gunzip. The client's own
	// Accept-Encoding still passes through untouched.
	tr.DisableCompression = true
	// Clone() keeps ForceAttemptHTTP2=true from DefaultTransport → h2 to
	// https upstreams, HTTP/1.1 otherwise (h2c not attempted by stdlib).
	tr.MaxIdleConnsPerHost = 32 // one active provider; allow parallel keep-alive reuse
	tr.IdleConnTimeout = 90 * time.Second
	// Connection-phase timeouts are safe; DO NOT set an overall deadline.
	// ResponseHeaderTimeout deliberately 0: a NON-streaming generation sends
	// headers only after full completion (can take minutes).
	tr.ResponseHeaderTimeout = 0
	tr.TLSHandshakeTimeout = 10 * time.Second

	return &httputil.ReverseProxy{
		Transport: tr,
		// Belt and braces: stdlib already forces immediate flush for
		// text/event-stream and ContentLength==-1 responses; -1 extends
		// that to every response (harmless for small JSON errors).
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream) // scheme/host + path join; rewrites outbound Host
			// Auth swap: strip whatever the App sent, inject provider key.
			pr.Out.Header.Del("Authorization")
			if bearerAuth {
				pr.Out.Header.Set("Authorization", "Bearer "+apiKey)
			} else {
				pr.Out.Header.Set("X-Api-Key", apiKey)
			}
			// No SetXForwarded(): loopback-only hop, upstream needs no client IP.
			rewriteModel(pr, redirects)
		},
		ModifyResponse: func(res *http.Response) error {
			if sink != nil && shouldParseSSE(res) {
				res.Body = newUsageTee(res.Body, res.StatusCode, sink)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Transport-level failure (dial/TLS/context). Client disconnects
			// mid-stream do NOT land here; they abort via http.ErrAbortHandler.
			log.Printf("proxy error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

func shouldParseSSE(res *http.Response) bool {
	ct := res.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

// rewriteModel buffers the request body ONLY when a redirect table exists and
// the body is a plain JSON candidate; otherwise the body streams through
// untouched (zero copy).
func rewriteModel(pr *httputil.ProxyRequest, redirects map[string]string) {
	if len(redirects) == 0 || pr.Out.Body == nil {
		return
	}
	// Never touch compressed or non-JSON bodies.
	if pr.In.Header.Get("Content-Encoding") != "" {
		return
	}
	if ct := pr.In.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return
	}

	head, err := io.ReadAll(io.LimitReader(pr.Out.Body, maxRewriteBody+1))
	if err != nil {
		// Can't trust a half-read body; fail the request instead of
		// forwarding garbage. (Transport will surface the read error.)
		pr.Out.Body = io.NopCloser(errReader{err})
		return
	}
	if int64(len(head)) > maxRewriteBody {
		// Oversized: forward buffered head + the unread remainder verbatim.
		rest := pr.Out.Body
		pr.Out.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), rest), rest}
		return
	}
	pr.Out.Body.Close()

	patched, _, _, ok, perr := patchModel(head, redirects)
	if perr != nil || !ok {
		patched = head // not JSON / no match → forward original bytes
	}
	pr.Out.Body = io.NopCloser(bytes.NewReader(patched))
	pr.Out.ContentLength = int64(len(patched))                      // Transport writes CL from this field
	pr.Out.Header.Set("Content-Length", strconv.Itoa(len(patched))) // cosmetic/consistency
	pr.Out.TransferEncoding = nil                                   // normalize inbound chunked → fixed length
	pr.Out.GetBody = func() (io.ReadCloser, error) {                // enables safe replay when permitted
		return io.NopCloser(bytes.NewReader(patched)), nil
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// ---- usage tee: incremental SSE parse with O(1) memory ----

type usageTee struct {
	rc       io.ReadCloser
	parser   sseParser
	sink     UsageSink
	finished bool
}

func newUsageTee(rc io.ReadCloser, status int, sink UsageSink) *usageTee {
	t := &usageTee{rc: rc, sink: sink}
	t.parser.usage.Status = status
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
// (client disconnect / upstream failure), so finalization here is exhaustive.
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
// length; oversized lines are discarded (usage events are tiny).
type sseParser struct {
	line     []byte
	overflow bool
	maxLine  int // high-water mark, exported for the O(1) test
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
			s.applyUsage(p.Usage) // output_tokens here is cumulative
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
