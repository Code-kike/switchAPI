package forward

// usage_test.go — M2 usage-parsing coverage: the openai Responses stream
// parser, non-streaming JSON parsing (both protocols), the interruption matrix
// rows that matter (research/03 C7), and the billing-path predicate (C8).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// jsonUpstream returns a fixed JSON body with a chosen content-type/status.
func jsonUpstream(t *testing.T, contentType string, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sseUpstream streams the given raw SSE payload (already framed) then closes.
func sseUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, payload)
		http.NewResponseController(w).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fwdTo wires a forwarder pointed at one upstream on the given protocol.
func fwdTo(t *testing.T, srv *httptest.Server, protocol string) (*Server, *httptest.Server, chan Usage) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan Usage, 4)
	fwd := New(localToken, func(us Usage) { ch <- us })
	up := &Upstream{ProviderID: "p-" + protocol, Protocol: protocol, BaseURL: u, APIKey: upKey}
	if protocol == "anthropic" {
		fwd.Swap(&RoutingTable{Rev: 1, Anthropic: up})
	} else {
		fwd.Swap(&RoutingTable{Rev: 1, OpenAI: up})
	}
	fwdSrv := httptest.NewServer(fwd.Handler())
	t.Cleanup(fwdSrv.Close)
	return fwd, fwdSrv, ch
}

func doPOST(t *testing.T, fwdSrv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", fwdSrv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func drain(resp *http.Response) { io.Copy(io.Discard, resp.Body); resp.Body.Close() }

func waitUsage(t *testing.T, ch chan Usage) Usage {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("usage sink never fired")
		return Usage{}
	}
}

// OpenAI Responses streaming: usage rides response.completed, input_tokens
// includes cached and must be split (research/03 table 2).
func TestOpenAIResponsesStreamCompleted(t *testing.T) {
	payload := "event: response.created\n" +
		`data: {"type":"response.created","response":{"model":"gpt-5-codex"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":30},"output_tokens":50,"total_tokens":150}}}` + "\n\n"
	_, fwdSrv, ch := fwdTo(t, sseUpstream(t, payload), "openai")

	resp := doPOST(t, fwdSrv, "/openai/v1/responses", `{"model":"gpt-5-codex","stream":true}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.InputTokens != 70 || u.CacheRead != 30 || u.CacheWrite != 0 || u.OutputTokens != 50 {
		t.Errorf("tokens = %+v, want input=70 cache_read=30 cache_write=0 output=50", u)
	}
	if !u.Done || u.UsageSource != "wire" || u.ErrorKind != "" {
		t.Errorf("done=%v source=%q errkind=%q", u.Done, u.UsageSource, u.ErrorKind)
	}
	if u.App != "codex" || u.ProviderID != "p-openai" || u.RequestID == "" {
		t.Errorf("context = %+v", u)
	}
	if u.Model != "gpt-5-codex" {
		t.Errorf("model = %q (no redirect → wire model)", u.Model)
	}
}

// response.incomplete still carries usage — read it best-effort.
func TestOpenAIResponsesIncomplete(t *testing.T) {
	payload := "event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"model":"gpt-5-codex","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n"
	_, fwdSrv, ch := fwdTo(t, sseUpstream(t, payload), "openai")
	resp := doPOST(t, fwdSrv, "/openai/v1/responses", `{"model":"gpt-5-codex","stream":true}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.InputTokens != 10 || u.OutputTokens != 5 || !u.Done || u.UsageSource != "wire" {
		t.Errorf("usage = %+v", u)
	}
}

// usage null on the terminal event → no tokens, usage_source=none.
func TestOpenAIResponsesMissingUsage(t *testing.T) {
	payload := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","usage":null}}` + "\n\n"
	_, fwdSrv, ch := fwdTo(t, sseUpstream(t, payload), "openai")
	resp := doPOST(t, fwdSrv, "/openai/v1/responses", `{"model":"gpt-5-codex","stream":true}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.UsageSource != "none" || u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("usage = %+v, want none/zero", u)
	}
	if !u.Done {
		t.Error("terminal event seen → Done should be true")
	}
}

// A single response.completed line embedding a ~2MB output parses fine under the
// 8MB openai line cap (research/06 遗留#3).
func TestOpenAIResponsesBigLine(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	payload := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5-codex","output_text":"` + big +
		`","usage":{"input_tokens":9,"output_tokens":8}}}` + "\n\n"
	_, fwdSrv, ch := fwdTo(t, sseUpstream(t, payload), "openai")
	resp := doPOST(t, fwdSrv, "/openai/v1/responses", `{"model":"gpt-5-codex","stream":true}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.InputTokens != 9 || u.OutputTokens != 8 || u.UsageSource != "wire" {
		t.Errorf("usage = %+v (big line not parsed)", u)
	}
}

// Non-streaming openai JSON: top-level usage, cached split.
func TestOpenAINonStreamJSON(t *testing.T) {
	body := `{"model":"gpt-5-codex","usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":50},"output_tokens":25}}`
	_, fwdSrv, ch := fwdTo(t, jsonUpstream(t, "application/json", 200, body), "openai")
	resp := doPOST(t, fwdSrv, "/openai/v1/responses", `{"model":"gpt-5-codex"}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.InputTokens != 150 || u.CacheRead != 50 || u.OutputTokens != 25 || u.UsageSource != "wire" {
		t.Errorf("usage = %+v", u)
	}
	if !u.Done {
		t.Error("complete JSON body → Done true")
	}
}

// Non-streaming anthropic JSON: top-level usage, input excludes cache.
func TestAnthropicNonStreamJSON(t *testing.T) {
	body := `{"model":"claude-haiku-4-5","usage":{"input_tokens":80,"output_tokens":40,"cache_creation_input_tokens":6,"cache_read_input_tokens":9}}`
	_, fwdSrv, ch := fwdTo(t, jsonUpstream(t, "application/json", 200, body), "anthropic")
	resp := doPOST(t, fwdSrv, "/anthropic/v1/messages", `{"model":"claude-haiku-4-5"}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.InputTokens != 80 || u.OutputTokens != 40 || u.CacheWrite != 6 || u.CacheRead != 9 {
		t.Errorf("usage = %+v", u)
	}
	if u.UsageSource != "wire" || !u.Done || u.Model != "claude-haiku-4-5" {
		t.Errorf("usage = %+v", u)
	}
}

// anthropic message_delta may carry input-side fields; they OVERWRITE the
// message_start values, never add (research/03 C1).
func TestAnthropicDeltaOverwritesInputSide(t *testing.T) {
	payload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":100,"cache_read_input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":123,"cache_read_input_tokens":11,"output_tokens":42}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	_, fwdSrv, ch := fwdTo(t, sseUpstream(t, payload), "anthropic")
	resp := doPOST(t, fwdSrv, "/anthropic/v1/messages", `{"model":"claude-haiku-4-5","stream":true}`)
	drain(resp)

	u := waitUsage(t, ch)
	// overwrite, not add: 123 (not 223), 11 (not 21), 42 output.
	if u.InputTokens != 123 || u.CacheRead != 11 || u.OutputTokens != 42 {
		t.Errorf("usage = %+v, want input=123 cache_read=11 output=42 (overwrite)", u)
	}
}

// 5xx billing response → record with ErrorKind=upstream_5xx, no tokens.
func TestBillingUpstream5xx(t *testing.T) {
	_, fwdSrv, ch := fwdTo(t, jsonUpstream(t, "application/json", 503, `{"error":"overloaded"}`), "anthropic")
	resp := doPOST(t, fwdSrv, "/anthropic/v1/messages", `{"model":"m"}`)
	drain(resp)

	u := waitUsage(t, ch)
	if u.Status != 503 || u.ErrorKind != "upstream_5xx" || u.UsageSource != "none" {
		t.Errorf("usage = %+v, want status=503 errkind=upstream_5xx source=none", u)
	}
}

// count_tokens is NOT a billing path — passes through, produces NO record.
func TestCountTokensNoRecord(t *testing.T) {
	_, fwdSrv, ch := fwdTo(t, jsonUpstream(t, "application/json", 200, `{"input_tokens":5}`), "anthropic")
	resp := doPOST(t, fwdSrv, "/anthropic/v1/messages/count_tokens", `{"model":"m"}`)
	drain(resp)

	select {
	case u := <-ch:
		t.Fatalf("count_tokens produced a usage record: %+v", u)
	case <-time.After(300 * time.Millisecond):
		// expected: no record
	}
}

// GET /models is not a billing path either.
func TestModelsGetNoRecord(t *testing.T) {
	_, fwdSrv, ch := fwdTo(t, jsonUpstream(t, "application/json", 200, `{"data":[]}`), "openai")
	req, _ := http.NewRequest("GET", fwdSrv.URL+"/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)

	select {
	case u := <-ch:
		t.Fatalf("GET /models produced a usage record: %+v", u)
	case <-time.After(300 * time.Millisecond):
	}
}

// Client hangs up mid-stream: message_start seen (tokens on the wire) but no
// message_stop → partial record with ErrorKind=client_abort（M4：客户端主动
// 中断记录在案但不计入健康，research/08 失败分类）。
func TestAnthropicClientAbortMidStream(t *testing.T) {
	// Upstream sends message_start then blocks until the client goes away.
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":77,"output_tokens":1}}}`+"\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		close(gone)
	}))
	t.Cleanup(srv.Close)
	_, fwdSrv, ch := fwdTo(t, srv, "anthropic")

	req, _ := http.NewRequest("POST", fwdSrv.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	resp.Body.Read(buf) // consume message_start, then hang up
	resp.Body.Close()

	u := waitUsage(t, ch)
	if u.Done {
		t.Errorf("partial stream reported Done=true: %+v", u)
	}
	if u.ErrorKind != "client_abort" {
		t.Errorf("errkind = %q, want client_abort", u.ErrorKind)
	}
	if u.InputTokens != 77 || u.UsageSource != "wire" {
		t.Errorf("partial usage = %+v, want input=77 source=wire", u)
	}
	<-gone
}

// Upstream dies mid-stream while the client is still reading → the classic
// stream_aborted：终止事件未达而上游 EOF（research/03 C7 + research/08 #19）。
func TestAnthropicUpstreamDiedMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":9,"output_tokens":1}}}`+"\n\n")
		http.NewResponseController(w).Flush()
		// 直接返回：连接被服务器关闭，客户端在等下一事件时读到 EOF。
	}))
	t.Cleanup(srv.Close)
	_, fwdSrv, ch := fwdTo(t, srv, "anthropic")

	req, _ := http.NewRequest("POST", fwdSrv.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) // 读到上游 EOF
	resp.Body.Close()

	u := waitUsage(t, ch)
	if u.ErrorKind != "stream_aborted" || u.Done {
		t.Errorf("errkind = %q done=%v, want stream_aborted/false", u.ErrorKind, u.Done)
	}
	if u.InputTokens != 9 || u.UsageSource != "wire" {
		t.Errorf("partial usage = %+v, want input=9 source=wire", u)
	}
}
