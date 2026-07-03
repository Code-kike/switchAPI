package forward

// forward_test.go — the four assertions carried over from the validated
// research/06 prototype (timing, byte-exact rewrite, O(1) tee, disconnect
// propagation) plus the M1 additions: local-token dual-header auth, prefix
// routing/strip, per-protocol auth injection, and atomic table swap.

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	haiku      = "claude-3-5-haiku-20241022"
	redirTo    = "claude-haiku-4-5"
	upKey      = "sk-upstream-real-key"
	localToken = "local-agent-token-0123456789abcdef"
)

// rig wires fake upstream(s) + a forwarder Server on ephemeral ports.
type rig struct {
	up      *upstreamServer
	upSrv   *httptest.Server
	fwd     *Server
	fwdSrv  *httptest.Server
	usageCh chan Usage
}

func mustUpstream(t *testing.T, srv *httptest.Server, protocol string, redirects map[string]string) *Upstream {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Upstream{ProviderID: "p-" + protocol, Protocol: protocol, BaseURL: u,
		APIKey: upKey, ModelRedirects: redirects}
}

func newRig(t *testing.T, interval time.Duration, events int) *rig {
	t.Helper()
	up := newUpstream(interval, events)
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	usageCh := make(chan Usage, 4)
	fwd := New(localToken, func(u Usage) { usageCh <- u })
	fwd.Swap(&RoutingTable{Rev: 1, Anthropic: mustUpstream(t, upSrv, "anthropic", map[string]string{haiku: redirTo})})
	fwdSrv := httptest.NewServer(fwd.Handler())
	t.Cleanup(fwdSrv.Close)
	return &rig{up: up, upSrv: upSrv, fwd: fwd, fwdSrv: fwdSrv, usageCh: usageCh}
}

func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}}
	// Deliberately NO Client.Timeout: it would interrupt Response.Body reads.
}

func postJSON(t *testing.T, cli *http.Client, base, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/anthropic/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Test 1 (ported): per-event arrival with gaps preserved through the proxy.
func TestChunkTimingPreservedNoBatching(t *testing.T) {
	const interval = 50 * time.Millisecond
	const deltas = 8
	r := newRig(t, interval, deltas)

	start := time.Now()
	resp := postJSON(t, rawClient(), r.fwdSrv.URL, `{"model": "`+haiku+`", "stream": true}`)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	var all bytes.Buffer
	var arrivals []time.Time
	for {
		line, err := br.ReadString('\n')
		all.WriteString(line)
		if line == "\n" {
			arrivals = append(arrivals, time.Now())
		}
		if err != nil {
			if err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			break
		}
	}

	wantEvents := deltas + 3 // message_start + deltas + message_delta + message_stop
	if len(arrivals) != wantEvents {
		t.Fatalf("events = %d, want %d", len(arrivals), wantEvents)
	}
	if d := arrivals[0].Sub(start); d > 200*time.Millisecond {
		t.Errorf("first event after %v, want <200ms (buffered?)", d)
	}
	var spaced int
	for i := 1; i < len(arrivals); i++ {
		if arrivals[i].Sub(arrivals[i-1]) >= 30*time.Millisecond {
			spaced++
		}
	}
	if minSpaced := wantEvents - 3; spaced < minSpaced { // tolerate scheduler jitter
		t.Errorf("only %d/%d inter-event gaps >= 30ms — stream was batched", spaced, wantEvents-1)
	}

	// Verbatim passthrough: client bytes == exactly what upstream wrote.
	if snap := r.up.snapshot(); !bytes.Equal(all.Bytes(), snap.written) {
		t.Errorf("client stream differs from upstream bytes")
	}

	u := <-r.usageCh
	if u.InputTokens != 123 || u.OutputTokens != 42 || u.CacheWrite != 7 || u.CacheRead != 11 || !u.Done {
		t.Errorf("usage = %+v", u)
	}
	if u.ProviderID != "p-anthropic" {
		t.Errorf("usage provider = %q", u.ProviderID)
	}
}

// Test 2 (ported): byte-exact model rewrite + Content-Length fix + chunked
// normalization + auth injection.
func TestModelRewriteByteExact(t *testing.T) {
	in := "{\n  \"metadata\": {\"model\": \"nested-untouched\"},\n" +
		"  \"model\":   \"" + haiku + "\"  ,\n" +
		"  \"system\": \"say \\\"hi\\\" 中文 \\u00e9\",\n  \"max_tokens\": 64\n}"
	want := strings.Replace(in, `"`+haiku+`"`, `"`+redirTo+`"`, 1)

	t.Run("fixed-length request", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		resp := postJSON(t, rawClient(), r.fwdSrv.URL, in)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		snap := r.up.snapshot()
		if string(snap.body) != want {
			t.Errorf("upstream body:\n got %q\nwant %q", snap.body, want)
		}
		if snap.cl != int64(len(want)) {
			t.Errorf("upstream Content-Length = %d, want %d", snap.cl, len(want))
		}
		if len(snap.te) != 0 {
			t.Errorf("upstream TransferEncoding = %v, want none", snap.te)
		}
		// anthropic gets BOTH auth headers with the upstream key; the local
		// token must be gone.
		if snap.xAPIKey != upKey || snap.authz != "Bearer "+upKey {
			t.Errorf("upstream auth: x-api-key=%q authz=%q", snap.xAPIKey, snap.authz)
		}
	})

	t.Run("chunked request normalized", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		// Opaque reader → net/http cannot know the length → chunked upload.
		req, _ := http.NewRequest("POST", r.fwdSrv.URL+"/anthropic/v1/messages",
			struct{ io.Reader }{strings.NewReader(in)})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", localToken) // auth via the other header
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		snap := r.up.snapshot()
		if string(snap.body) != want {
			t.Errorf("upstream body:\n got %q\nwant %q", snap.body, want)
		}
		if snap.cl != int64(len(want)) {
			t.Errorf("upstream Content-Length = %d, want %d (chunked not normalized)", snap.cl, len(want))
		}
	})

	t.Run("no redirect match passes through byte-identical", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		orig := "{\"model\":\"untouched-model\" , \"x\":[1,2,3]}"
		resp := postJSON(t, rawClient(), r.fwdSrv.URL, orig)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		snap := r.up.snapshot()
		if string(snap.body) != orig {
			t.Errorf("body mutated without redirect:\n got %q\nwant %q", snap.body, orig)
		}
	})
}

// Test 3 (ported): long stream — usage parsed, O(1) line buffer, byte-identical delivery.
func TestUsageTeeO1MemoryOnLongStream(t *testing.T) {
	const deltas = 5000
	r := newRig(t, 0, deltas)

	resp := postJSON(t, rawClient(), r.fwdSrv.URL, `{"model":"`+haiku+`"}`)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if snap := r.up.snapshot(); !bytes.Equal(got, snap.written) {
		t.Fatalf("client stream (%d bytes) != upstream bytes (%d bytes)", len(got), len(snap.written))
	}

	u := <-r.usageCh
	if u.Model != redirTo {
		t.Errorf("usage model = %q, want %q (redirect must be visible in echo)", u.Model, redirTo)
	}
	if u.InputTokens != 123 || u.OutputTokens != 42 || !u.Done {
		t.Errorf("usage = %+v", u)
	}
	if u.HighWater > 4096 {
		t.Errorf("parser high-water = %d bytes on a %d-byte stream — not O(1)", u.HighWater, len(got))
	}
}

// Test 4 (ported): client disconnect must cancel the upstream request context.
func TestClientDisconnectPropagatesUpstream(t *testing.T) {
	r := newRig(t, 50*time.Millisecond, 200) // would run ~10s if not canceled

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", r.fwdSrv.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"`+haiku+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 1024)
	resp.Body.Read(buf) // consume a first chunk, then walk away
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-r.up.CancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never observed cancellation after client disconnect")
	}

	select {
	case u := <-r.usageCh:
		if u.Done {
			t.Errorf("partial stream reported Done=true: %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage sink never fired after abort")
	}
}

// M1: local-token auth — both carrier headers accepted, everything else 401.
func TestLocalTokenAuth(t *testing.T) {
	r := newRig(t, time.Millisecond, 1)
	cli := rawClient()

	cases := []struct {
		name   string
		header func(*http.Request)
		want   int
	}{
		{"no auth", func(*http.Request) {}, 401},
		{"wrong bearer", func(q *http.Request) { q.Header.Set("Authorization", "Bearer nope") }, 401},
		{"wrong x-api-key", func(q *http.Request) { q.Header.Set("X-Api-Key", "nope") }, 401},
		{"bearer ok", func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+localToken) }, 200},
		{"x-api-key ok", func(q *http.Request) { q.Header.Set("X-Api-Key", localToken) }, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", r.fwdSrv.URL+"/anthropic/v1/messages",
				strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Content-Type", "application/json")
			tc.header(req)
			resp, err := cli.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 200 {
				<-r.usageCh
			}
		})
	}
}

// M1: prefix routing — strip rules, unknown prefix 404, unconfigured protocol 503.
func TestPrefixRoutingAndStrip(t *testing.T) {
	r := newRig(t, time.Millisecond, 1)
	cli := rawClient()

	// /anthropic/v1/messages → upstream sees /v1/messages.
	resp := postJSON(t, cli, r.fwdSrv.URL, `{"model":"m"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	<-r.usageCh
	if snap := r.up.snapshot(); snap.path != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", snap.path)
	}

	get := func(path string, hdr map[string]string) int {
		req, _ := http.NewRequest("GET", r.fwdSrv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	auth := map[string]string{"Authorization": "Bearer " + localToken}

	if code := get("/unknown/v1/messages", auth); code != 404 {
		t.Fatalf("unknown prefix status = %d, want 404", code)
	}
	// "/anthropicfoo" must NOT match the /anthropic prefix.
	if code := get("/anthropicfoo/v1/messages", auth); code != 404 {
		t.Fatalf("prefix over-match: status = %d, want 404", code)
	}
	// openai protocol has no upstream in this rig.
	if code := get("/openai/v1/models", auth); code != 503 {
		t.Fatalf("unconfigured protocol status = %d, want 503", code)
	}
}

// M1: openai wire gets Bearer-only injection and the /openai/v1 strip rule.
func TestOpenAIInjection(t *testing.T) {
	up := newUpstream(time.Millisecond, 1)
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	fwd := New(localToken, nil)
	fwd.Swap(&RoutingTable{Rev: 1, OpenAI: mustUpstream(t, upSrv, "openai", nil)})
	fwdSrv := httptest.NewServer(fwd.Handler())
	defer fwdSrv.Close()

	req, _ := http.NewRequest("POST", fwdSrv.URL+"/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5-codex","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+localToken)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	snap := up.snapshot()
	if snap.path != "/responses" {
		t.Fatalf("upstream path = %q, want /responses", snap.path)
	}
	if snap.authz != "Bearer "+upKey {
		t.Fatalf("upstream authz = %q", snap.authz)
	}
	if snap.xAPIKey != "" {
		t.Fatalf("openai wire must not get x-api-key, got %q", snap.xAPIKey)
	}
}

// M1: swapping the table must not disturb an in-flight stream, and the next
// request must hit the new upstream.
func TestSwapMidStreamAtomic(t *testing.T) {
	upA := newUpstream(20*time.Millisecond, 20)
	srvA := httptest.NewServer(upA)
	defer srvA.Close()
	upB := newUpstream(time.Millisecond, 1)
	srvB := httptest.NewServer(upB)
	defer srvB.Close()

	fwd := New(localToken, nil)
	fwd.Swap(&RoutingTable{Rev: 1, Anthropic: mustUpstream(t, srvA, "anthropic", nil)})
	fwdSrv := httptest.NewServer(fwd.Handler())
	defer fwdSrv.Close()

	// Start a long stream against A.
	respA := postJSON(t, rawClient(), fwdSrv.URL, `{"model":"m","stream":true}`)
	defer respA.Body.Close()
	first := make([]byte, 64)
	if _, err := respA.Body.Read(first); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Swap to B mid-stream.
	fwd.Swap(&RoutingTable{Rev: 2, Anthropic: mustUpstream(t, srvB, "anthropic", nil)})

	// New request goes to B...
	respB := postJSON(t, rawClient(), fwdSrv.URL, `{"model":"from-b"}`)
	io.Copy(io.Discard, respB.Body)
	respB.Body.Close()
	if snap := upB.snapshot(); !strings.Contains(string(snap.body), "from-b") {
		t.Fatalf("post-swap request did not reach upstream B: %q", snap.body)
	}

	// ...while the in-flight stream from A still completes fully.
	rest, err := io.ReadAll(respA.Body)
	if err != nil {
		t.Fatalf("in-flight stream broken by swap: %v", err)
	}
	full := string(first) + string(rest)
	if !strings.Contains(full, "message_stop") {
		t.Fatalf("in-flight stream did not run to completion after swap")
	}
}
