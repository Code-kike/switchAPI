package main

// main_test.go — assertions for research item #06:
//  1. chunks traverse the proxy individually with timing gaps preserved (no batching)
//  2. model rewrite is byte-exact (only the top-level "model" value changes),
//     Content-Length is fixed up, chunked requests are normalized
//  3. usage tee parses correctly with O(1) line-buffer memory on long streams,
//     while the client still receives a byte-identical stream
//  4. client disconnect propagates to the upstream via context cancellation

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
	haiku    = "claude-3-5-haiku-20241022"
	redirTo  = "claude-haiku-4-5"
	proxyKey = "sk-prototype-upstream-key"
)

// rig wires fake upstream + proxy on ephemeral ports.
type rig struct {
	up      *upstreamServer
	upSrv   *httptest.Server
	pxSrv   *httptest.Server
	usageCh chan Usage
}

func newRig(t *testing.T, interval time.Duration, events int) *rig {
	t.Helper()
	up := newUpstream(interval, events)
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	target, err := url.Parse(upSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	usageCh := make(chan Usage, 4)
	proxy := newProxy(target, proxyKey, map[string]string{haiku: redirTo}, func(u Usage) { usageCh <- u })
	pxSrv := httptest.NewServer(proxy)
	t.Cleanup(pxSrv.Close)
	return &rig{up: up, upSrv: upSrv, pxSrv: pxSrv, usageCh: usageCh}
}

func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}}
	// Deliberately NO Client.Timeout: it would interrupt Response.Body reads.
}

func postJSON(t *testing.T, cli *http.Client, target, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", target+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local-agent-token")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Test 1: per-event arrival with gaps preserved through the proxy.
func TestChunkTimingPreservedNoBatching(t *testing.T) {
	const interval = 50 * time.Millisecond
	const deltas = 8
	r := newRig(t, interval, deltas)

	start := time.Now()
	resp := postJSON(t, rawClient(), r.pxSrv.URL, `{"model": "`+haiku+`", "stream": true}`)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// Read raw stream; timestamp each complete SSE event (terminated by blank line).
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

	// First event must arrive long before the stream completes (headers +
	// message_start not held back by any buffering).
	if d := arrivals[0].Sub(start); d > 200*time.Millisecond {
		t.Errorf("first event after %v, want <200ms (buffered?)", d)
	}

	// Gap preservation: upstream paces every event except message_stop.
	// If anything batched the stream, gaps collapse to ~0.
	var spaced int
	for i := 1; i < len(arrivals); i++ {
		if arrivals[i].Sub(arrivals[i-1]) >= 30*time.Millisecond {
			spaced++
		}
	}
	if minSpaced := wantEvents - 3; spaced < minSpaced { // tolerate scheduler jitter
		t.Errorf("only %d/%d inter-event gaps >= 30ms — stream was batched", spaced, wantEvents-1)
	}
	if total := arrivals[len(arrivals)-1].Sub(start); total < time.Duration(deltas)*interval {
		t.Errorf("stream finished in %v, faster than upstream emitted it", total)
	}

	// Verbatim passthrough: client bytes == exactly what upstream wrote.
	_, _, _, _, written := r.up.snapshot()
	if !bytes.Equal(all.Bytes(), written) {
		t.Errorf("client stream differs from upstream bytes:\nclient : %q\nwrote  : %q", all.Bytes(), written)
	}

	u := <-r.usageCh
	if u.InputTokens != 123 || u.OutputTokens != 42 || u.CacheWrite != 7 || u.CacheRead != 11 || !u.Done {
		t.Errorf("usage = %+v", u)
	}
}

// Test 2: byte-exact model rewrite + Content-Length fix + chunked normalization.
func TestModelRewriteByteExact(t *testing.T) {
	in := "{\n  \"metadata\": {\"model\": \"nested-untouched\"},\n" +
		"  \"model\":   \"" + haiku + "\"  ,\n" +
		"  \"system\": \"say \\\"hi\\\" 中文 \\u00e9\",\n  \"max_tokens\": 64\n}"
	want := strings.Replace(in, `"`+haiku+`"`, `"`+redirTo+`"`, 1)

	t.Run("fixed-length request", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		resp := postJSON(t, rawClient(), r.pxSrv.URL, in)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		body, cl, te, auth, _ := r.up.snapshot()
		if string(body) != want {
			t.Errorf("upstream body:\n got %q\nwant %q", body, want)
		}
		if cl != int64(len(want)) {
			t.Errorf("upstream Content-Length = %d, want %d", cl, len(want))
		}
		if len(te) != 0 {
			t.Errorf("upstream TransferEncoding = %v, want none", te)
		}
		if auth != proxyKey {
			t.Errorf("upstream X-Api-Key = %q", auth)
		}
	})

	t.Run("chunked request normalized", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		// Opaque reader → net/http cannot know the length → chunked upload.
		req, _ := http.NewRequest("POST", r.pxSrv.URL+"/v1/messages", struct{ io.Reader }{strings.NewReader(in)})
		req.Header.Set("Content-Type", "application/json")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		body, cl, _, _, _ := r.up.snapshot()
		if string(body) != want {
			t.Errorf("upstream body:\n got %q\nwant %q", body, want)
		}
		if cl != int64(len(want)) {
			t.Errorf("upstream Content-Length = %d, want %d (chunked not normalized)", cl, len(want))
		}
	})

	t.Run("no redirect match passes through byte-identical", func(t *testing.T) {
		r := newRig(t, time.Millisecond, 1)
		orig := "{\"model\":\"untouched-model\" , \"x\":[1,2,3]}"
		resp := postJSON(t, rawClient(), r.pxSrv.URL, orig)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-r.usageCh

		body, cl, _, _, _ := r.up.snapshot()
		if string(body) != orig {
			t.Errorf("body mutated without redirect:\n got %q\nwant %q", body, orig)
		}
		if cl != int64(len(orig)) {
			t.Errorf("Content-Length = %d, want %d", cl, len(orig))
		}
	})
}

// Test 3: long stream — usage parsed, O(1) line buffer, byte-identical delivery.
func TestUsageTeeO1MemoryOnLongStream(t *testing.T) {
	const deltas = 5000
	r := newRig(t, 0, deltas)

	resp := postJSON(t, rawClient(), r.pxSrv.URL, `{"model":"`+haiku+`"}`)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, _, written := r.up.snapshot()
	if !bytes.Equal(got, written) {
		t.Fatalf("client stream (%d bytes) != upstream bytes (%d bytes)", len(got), len(written))
	}

	u := <-r.usageCh
	if u.Model != redirTo {
		t.Errorf("usage model = %q, want %q (redirect must be visible in echo)", u.Model, redirTo)
	}
	if u.InputTokens != 123 || u.OutputTokens != 42 || !u.Done {
		t.Errorf("usage = %+v", u)
	}
	// O(1) evidence: buffer high-water mark stays at one SSE line, regardless
	// of the ~%dKB stream length.
	if u.HighWater > 4096 {
		t.Errorf("parser high-water = %d bytes on a %d-byte stream — not O(1)", u.HighWater, len(got))
	}
	t.Logf("stream=%d bytes, events=%d, parser high-water=%d bytes", len(got), deltas+3, u.HighWater)
}

// Test 4: client disconnect must cancel the upstream request context.
func TestClientDisconnectPropagatesUpstream(t *testing.T) {
	r := newRig(t, 50*time.Millisecond, 200) // would run ~10s if not canceled

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", r.pxSrv.URL+"/v1/messages",
		strings.NewReader(`{"model":"`+haiku+`"}`))
	req.Header.Set("Content-Type", "application/json")
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
		// upstream saw ctx cancellation propagated through the proxy
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never observed cancellation after client disconnect")
	}

	// The tee must still finalize (partial usage, at-least-once accounting).
	select {
	case u := <-r.usageCh:
		if u.Done {
			t.Errorf("partial stream reported Done=true: %+v", u)
		}
		t.Logf("partial usage on disconnect: %+v", u)
	case <-time.After(2 * time.Second):
		t.Fatal("usage sink never fired after abort")
	}
}
