package main

// upstream.go — fake provider emitting Anthropic-flavored SSE at a fixed
// interval, recording what it received so tests can assert on the proxied
// request, and recording exactly what it wrote so tests can assert verbatim
// passthrough.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type upstreamServer struct {
	interval time.Duration
	events   int // number of content_block_delta events

	mu       sync.Mutex
	lastBody []byte   // raw request body as received
	lastCL   int64    // r.ContentLength as received
	lastTE   []string // r.TransferEncoding as received
	lastAuth string   // X-Api-Key as received
	written  []byte   // exact bytes written to the response

	cancelOnce     sync.Once
	CancelObserved chan struct{} // closed when the (proxied) client ctx cancels mid-stream
}

func newUpstream(interval time.Duration, events int) *upstreamServer {
	return &upstreamServer{interval: interval, events: events, CancelObserved: make(chan struct{})}
}

func (u *upstreamServer) snapshot() (body []byte, cl int64, te []string, auth string, written []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.lastBody...), u.lastCL, append([]string(nil), u.lastTE...),
		u.lastAuth, append([]byte(nil), u.written...)
}

func (u *upstreamServer) markCanceled() { u.cancelOnce.Do(func() { close(u.CancelObserved) }) }

func (u *upstreamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)

	u.mu.Lock()
	u.lastBody = body
	u.lastCL = r.ContentLength
	u.lastTE = r.TransferEncoding
	u.lastAuth = r.Header.Get("X-Api-Key")
	u.written = nil
	u.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	send := func(event, data string) bool {
		payload := fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)
		if _, err := io.WriteString(w, payload); err != nil {
			u.markCanceled() // write error after client vanished
			return false
		}
		u.mu.Lock()
		u.written = append(u.written, payload...)
		u.mu.Unlock()
		rc.Flush()
		return true
	}
	pace := func() bool { // sleep one interval, watching for cancellation
		select {
		case <-time.After(u.interval):
			return true
		case <-r.Context().Done():
			u.markCanceled()
			return false
		}
	}

	if !send("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_proto","model":%q,"usage":{"input_tokens":123,"cache_creation_input_tokens":7,"cache_read_input_tokens":11,"output_tokens":1}}}`,
		req.Model)) {
		return
	}
	for i := 0; i < u.events; i++ {
		if !pace() {
			return
		}
		if !send("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk-%d"}}`, i)) {
			return
		}
	}
	if !pace() {
		return
	}
	if !send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`) {
		return
	}
	send("message_stop", `{"type":"message_stop"}`)
}
