// Package e2e wires real Hub components and a real Agent stack in-process
// and walks the M1 acceptance path (task prd.md 验收 #1/#5)：
// 配对 → config_push → 经 Agent 流式请求命中 A → POST /switch → 命中 B →
// Hub 宕机仍可转发 → Hub 重启后重连、再次切换生效。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/agent/hubclient"
	"github.com/Code-kike/switchAPI/internal/hub/api"
	"github.com/Code-kike/switchAPI/internal/hub/realtime"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
)

// fakeUpstream is a minimal Anthropic-flavored SSE provider whose message id
// carries its name, so responses reveal which upstream served them.
type fakeUpstream struct {
	name string
	mu   sync.Mutex
	hits int
	auth string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.hits++
	f.auth = r.Header.Get("X-Api-Key")
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	for _, ev := range []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%s","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`, f.name),
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	} {
		io.WriteString(w, "data: "+ev+"\n\n")
		rc.Flush()
	}
}

func (f *fakeUpstream) snapshot() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.auth
}

// hubProc is one Hub "process": store-backed api+realtime on a fixed address.
type hubProc struct {
	srv *http.Server
	ln  net.Listener
	rt  *realtime.Hub
}

func startHub(t *testing.T, st *store.Store, key []byte, addr string) *hubProc {
	t.Helper()
	rt := realtime.New(st, key)
	root := http.NewServeMux()
	root.Handle("GET /api/v1/ws/agent", rt.Handler())
	root.Handle("/", api.New(st, key, rt).Handler())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	srv := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	return &hubProc{srv: srv, ln: ln, rt: rt}
}

func (h *hubProc) stop() {
	h.rt.CloseAll() // 模拟进程死亡：hijacked WS 必须显式断开
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.srv.Shutdown(ctx)
	h.srv.Close()
}

type adminClient struct {
	base string
	cli  *http.Client
	t    *testing.T
}

func newAdmin(t *testing.T, base string) *adminClient {
	jar, _ := cookiejar.New(nil)
	a := &adminClient{base: base, cli: &http.Client{Jar: jar, Timeout: 10 * time.Second}, t: t}
	a.post("/api/v1/auth/login", `{"password":"e2e-pw"}`, 200)
	return a
}

func (a *adminClient) post(path, body string, want int) []byte {
	a.t.Helper()
	req, _ := http.NewRequest("POST", a.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.cli.Do(req)
	if err != nil {
		a.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		a.t.Fatalf("POST %s = %d (want %d): %s", path, resp.StatusCode, want, raw)
	}
	return raw
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

func TestM1EndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	// Fake providers A and B.
	upA, upB := &fakeUpstream{name: "A"}, &fakeUpstream{name: "B"}
	srvA, srvB := httptest.NewServer(upA), httptest.NewServer(upB)
	defer srvA.Close()
	defer srvB.Close()

	// Hub round 1.
	hub := startHub(t, st, key, "127.0.0.1:0")
	hubURL := "http://" + hub.ln.Addr().String()
	admin := newAdmin(t, hubURL)

	const keyA, keyB = "sk-e2e-aaaa-1111", "sk-e2e-bbbb-2222"
	var pA, pB struct {
		ID string `json:"id"`
	}
	json.Unmarshal(admin.post("/api/v1/providers", fmt.Sprintf(
		`{"name":"A","protocol":"anthropic","base_url":%q,"api_key":%q}`, srvA.URL, keyA), 201), &pA)
	json.Unmarshal(admin.post("/api/v1/providers", fmt.Sprintf(
		`{"name":"B","protocol":"anthropic","base_url":%q,"api_key":%q}`, srvB.URL, keyB), 201), &pB)
	admin.post("/api/v1/switch", `{"app":"claude-code","provider_id":"`+pA.ID+`"}`, 200)

	// Pair a device with the real pairing path.
	var pc struct {
		Code string `json:"code"`
	}
	json.Unmarshal(admin.post("/api/v1/devices/pairing-code", `{}`, 200), &pc)
	statePath := filepath.Join(dir, "agent-state.json")
	state, err := hubclient.Pair(hubURL, pc.Code, "e2e-dev", statePath)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	// Agent stack: forwarder + hub client.
	fwd := forward.New(state.LocalToken, nil)
	fwdSrv := httptest.NewServer(fwd.Handler())
	defer fwdSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := hubclient.New(statePath, state, fwd)
	go client.Run(ctx)

	waitFor(t, "initial config push (A active)", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pA.ID
	})

	// Streaming request through the Agent must hit A with the swapped key.
	askVia := func() string {
		req, _ := http.NewRequest("POST", fwdSrv.URL+"/anthropic/v1/messages",
			strings.NewReader(`{"model":"m","stream":true,"max_tokens":8,"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+state.LocalToken)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("request via agent: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("via agent = %d: %s", resp.StatusCode, raw)
		}
		return string(raw)
	}

	if out := askVia(); !strings.Contains(out, "msg_A") {
		t.Fatalf("first request did not hit A: %s", out)
	}
	if hits, auth := upA.snapshot(); hits != 1 || auth != keyA {
		t.Fatalf("upstream A saw hits=%d auth=%q (auth swap broken)", hits, auth)
	}

	// Global switch → next request hits B, no restarts anywhere.
	admin.post("/api/v1/switch", `{"app":"claude-code","provider_id":"`+pB.ID+`"}`, 200)
	waitFor(t, "push after switch (B active)", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pB.ID
	})
	if out := askVia(); !strings.Contains(out, "msg_B") {
		t.Fatalf("post-switch request did not hit B: %s", out)
	}

	// Hub down → cached routing keeps forwarding (验收 #5 前半).
	hubAddr := hub.ln.Addr().String()
	hub.stop()
	time.Sleep(200 * time.Millisecond)
	if out := askVia(); !strings.Contains(out, "msg_B") {
		t.Fatalf("request failed while hub down: %s", out)
	}

	// Hub back on the SAME address → agent reconnects; a fresh switch lands.
	hub2 := startHub(t, st, key, hubAddr)
	defer hub2.stop()
	admin2 := newAdmin(t, hubURL) // 新 Hub 进程内存会话已失效，重新登录
	admin2.post("/api/v1/switch", `{"app":"claude-code","provider_id":"`+pA.ID+`"}`, 200)
	waitFor(t, "reconnect + post-restart switch (A active)", 30*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pA.ID
	})
	if out := askVia(); !strings.Contains(out, "msg_A") {
		t.Fatalf("post-restart request did not hit A: %s", out)
	}

	// At rest: provider keys never in plaintext inside the SQLite file(s).
	for _, f := range []string{dbPath, dbPath + "-wal"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue // -wal 可能不存在
		}
		if bytes.Contains(raw, []byte(keyA)) || bytes.Contains(raw, []byte(keyB)) {
			t.Fatalf("plaintext api key found in %s", f)
		}
	}

	// Agent state file: 0600 and carries the last push (含明文 key，靠权限保护).
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("agent state mode = %v", info.Mode().Perm())
	}
}
