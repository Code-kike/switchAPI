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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/agent/forward"
	"github.com/Code-kike/switchAPI/internal/agent/health"
	"github.com/Code-kike/switchAPI/internal/agent/hubclient"
	"github.com/Code-kike/switchAPI/internal/agent/usagebuf"
	"github.com/Code-kike/switchAPI/internal/hub/api"
	"github.com/Code-kike/switchAPI/internal/hub/failover"
	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/realtime"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeUpstream is a minimal Anthropic-flavored SSE provider whose message id
// carries its name, so responses reveal which upstream served them. Flip
// failing to simulate an outage (503 on every request, incl. probes).
type fakeUpstream struct {
	name    string
	failing atomic.Bool
	mu      sync.Mutex
	hits    int
	auth    string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &req)
	f.mu.Lock()
	f.hits++
	f.auth = r.Header.Get("X-Api-Key")
	f.mu.Unlock()

	if f.failing.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"type":"overloaded","message":"synthetic outage"}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	for _, ev := range []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%s","model":%q,"usage":{"input_tokens":1,"output_tokens":1}}}`, f.name, req.Model),
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
	srv        *http.Server
	ln         net.Listener
	rt         *realtime.Hub
	api        *api.Server
	stopEngine context.CancelFunc
}

func startHub(t *testing.T, st *store.Store, key []byte, addr string) *hubProc {
	t.Helper()
	if err := pricing.EnsureLoaded(st); err != nil { // 首次灌入快照，二次为幂等空操作
		t.Fatal(err)
	}
	resolver, err := pricing.NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	rt := realtime.New(st, key)
	apiSrv := api.New(st, key, rt, resolver)
	rt.SetUsageNotifier(apiSrv) // 用量入库 → ws/ui usage_tick（与 cmd/hub 相同接线）

	// M4：故障切换引擎（测试用快节奏参数）。
	engine := failover.New(st, key, rt, apiSrv, failover.Config{
		DebounceWindow: 50 * time.Millisecond, VetoWindow: 30,
		FailoverMinGap: 100 * time.Millisecond,
		ProbeBase:      50 * time.Millisecond, ProbeMax: 200 * time.Millisecond,
		ProbeTimeoutS: 5, ProbeScan: 20 * time.Millisecond,
		SpeedtestExpiry: time.Minute,
	})
	rt.SetReportHandler(engine)
	apiSrv.SetReliability(engine)
	ectx, ecancel := context.WithCancel(context.Background())
	go engine.Run(ectx)

	root := http.NewServeMux()
	root.Handle("GET /api/v1/ws/agent", rt.Handler())
	root.Handle("/", apiSrv.Handler())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	srv := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	return &hubProc{srv: srv, ln: ln, rt: rt, api: apiSrv, stopEngine: ecancel}
}

func (h *hubProc) stop() {
	h.stopEngine()
	h.rt.CloseAll() // 模拟进程死亡：hijacked WS 必须显式断开
	h.api.CloseUI() // ws/ui 同理
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

func (a *adminClient) get(path string) (int, []byte) {
	a.t.Helper()
	req, _ := http.NewRequest("GET", a.base+path, nil)
	resp, err := a.cli.Do(req)
	if err != nil {
		a.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
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

	// Agent stack: forwarder + usage queue + hub client (M2 全管线).
	buf, err := usagebuf.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Close()
	fwd := forward.New(state.LocalToken, func(u forward.Usage) { buf.Enqueue(u.ToRecord()) })
	fwdSrv := httptest.NewServer(fwd.Handler())
	defer fwdSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := hubclient.New(statePath, state, fwd)
	client.UseQueue(buf)
	go client.Run(ctx)

	waitFor(t, "initial config push (A active)", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pA.ID
	})

	// Streaming request through the Agent must hit A with the swapped key.
	askVia := func() string {
		req, _ := http.NewRequest("POST", fwdSrv.URL+"/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"max_tokens":8,"messages":[]}`))
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

	// ---- M2 用量管线（验收：断连补报无缺口、request_id 无重复） ----
	// 全程共 4 次计费请求：A、B、B（Hub 宕机期间，走本地缓冲）、A（重启后）。
	// 宕机期间那条必须经重连补报落库；total==4 同时证明"无缺口"与"无重复"。
	type usageResp struct {
		Total int `json:"total"`
		Rows  []struct {
			RequestID  string   `json:"request_id"`
			DeviceID   string   `json:"device_id"`
			App        string   `json:"app"`
			ProviderID string   `json:"provider_id"`
			Model      string   `json:"model"`
			Input      int64    `json:"input_tokens"`
			Output     int64    `json:"output_tokens"`
			Source     string   `json:"usage_source"`
			Cost       *float64 `json:"cost"`
		} `json:"rows"`
	}
	var ur usageResp
	waitFor(t, "all 4 usage records land (incl. hub-down backfill)", 20*time.Second, func() bool {
		code, raw := admin2.get("/api/v1/usage?limit=50")
		if code != 200 {
			return false
		}
		ur = usageResp{}
		return json.Unmarshal(raw, &ur) == nil && ur.Total == 4
	})

	seen := map[string]bool{}
	byProvider := map[string]int{}
	for _, row := range ur.Rows {
		if seen[row.RequestID] {
			t.Fatalf("duplicate request_id in usage rows: %s", row.RequestID)
		}
		seen[row.RequestID] = true
		byProvider[row.ProviderID]++
		if row.DeviceID != state.DeviceID {
			t.Fatalf("usage row device = %q, want %q", row.DeviceID, state.DeviceID)
		}
		if row.App != "claude-code" || row.Model != "claude-haiku-4-5" || row.Source != "wire" {
			t.Fatalf("usage row attribution wrong: %+v", row)
		}
		if row.Input != 1 || row.Output != 2 {
			t.Fatalf("usage tokens = %d/%d, want 1/2 (fake upstream)", row.Input, row.Output)
		}
		if row.Cost == nil || *row.Cost <= 0 {
			t.Fatalf("cost not settled for known model claude-haiku-4-5: %+v", row.Cost)
		}
	}
	if byProvider[pA.ID] != 2 || byProvider[pB.ID] != 2 {
		t.Fatalf("provider attribution = %v, want A:2 B:2", byProvider)
	}

	// 聚合口径抽查：summary 的请求数与费用已知性。
	code, raw := admin2.get("/api/v1/stats/summary?from=1")
	if code != 200 {
		t.Fatalf("summary = %d", code)
	}
	var sum struct {
		Requests            int64    `json:"requests"`
		Cost                *float64 `json:"cost"`
		CostUnknownRequests int64    `json:"cost_unknown_requests"`
	}
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 4 || sum.Cost == nil || sum.CostUnknownRequests != 0 {
		t.Fatalf("summary = %s", raw)
	}

	// ---- M3 ws/ui 实时通道（验收：任一端操作，其他端 1 秒内可见） ----
	// 两个"端"（模拟手机浏览器与桌面壳）同时在线；切换与新用量都要双端推达。
	dialUI := func() *websocket.Conn {
		u, _ := url.Parse(hubURL)
		h := http.Header{}
		for _, c := range admin2.cli.Jar.Cookies(u) {
			h.Add("Cookie", c.Name+"="+c.Value)
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		c, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(hubURL, "http")+"/api/v1/ws/ui",
			&websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatalf("dial ws/ui: %v", err)
		}
		return c
	}
	readUntil := func(c *websocket.Conn, typ string, timeout time.Duration) wire.Envelope {
		deadline := time.Now().Add(timeout)
		for {
			rctx, rcancel := context.WithDeadline(context.Background(), deadline)
			var env wire.Envelope
			err := wsjson.Read(rctx, c, &env)
			rcancel()
			if err != nil {
				t.Fatalf("ws/ui waiting for %s: %v", typ, err)
			}
			if env.Type == typ {
				return env
			}
		}
	}
	ui1, ui2 := dialUI(), dialUI()
	defer ui1.CloseNow()
	defer ui2.CloseNow()

	// 切换 → 双端 1s 内收到 state_changed（PRD 验收上限）。
	admin2.post("/api/v1/switch", `{"app":"claude-code","provider_id":"`+pB.ID+`"}`, 200)
	for i, c := range []*websocket.Conn{ui1, ui2} {
		var st wire.UIStateChanged
		if err := readUntil(c, wire.TypeUIStateChanged, time.Second).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st.Apps["claude-code"] != pB.ID {
			t.Fatalf("ui client %d state_changed = %v, want claude-code=%s", i+1, st.Apps, pB.ID)
		}
		if st.Rev <= 0 {
			t.Fatalf("ui client %d rev = %d, want >0", i+1, st.Rev)
		}
	}

	// 新用量（第 5 次计费请求）→ 批量上报入库 → 双端收到 usage_tick。
	waitFor(t, "agent routes to B before the 5th request", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pB.ID
	})
	askVia()
	for i, c := range []*websocket.Conn{ui1, ui2} {
		var tk wire.UIUsageTick
		if err := readUntil(c, wire.TypeUIUsageTick, 15*time.Second).Decode(&tk); err != nil {
			t.Fatal(err)
		}
		if tk.Inserted != 1 || tk.LastTS == 0 {
			t.Fatalf("ui client %d usage_tick = %+v", i+1, tk)
		}
	}
}

// TestM4FailoverEndToEnd walks the reliability chain (M4 prd 验收 #1/#3)：
// 打断上游 A → Agent 连续 3 次硬失败 → health_report → Hub 仲裁沿备选切到 B
// （Agent 新路由 + ws/ui failover 事件 + state_changed）→ A 复活 → 恢复探测
// 连续 2 次成功 → recovered 事件且不自动切回 → 手动测速按设备回报。
func TestM4FailoverEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	upA, upB := &fakeUpstream{name: "A"}, &fakeUpstream{name: "B"}
	srvA, srvB := httptest.NewServer(upA), httptest.NewServer(upB)
	defer srvA.Close()
	defer srvB.Close()

	hub := startHub(t, st, key, "127.0.0.1:0")
	defer hub.stop()
	hubURL := "http://" + hub.ln.Addr().String()
	admin := newAdmin(t, hubURL)

	var pA, pB struct {
		ID string `json:"id"`
	}
	json.Unmarshal(admin.post("/api/v1/providers", fmt.Sprintf(
		`{"name":"A","protocol":"anthropic","base_url":%q,"api_key":"sk-m4-aaaa"}`, srvA.URL), 201), &pA)
	json.Unmarshal(admin.post("/api/v1/providers", fmt.Sprintf(
		`{"name":"B","protocol":"anthropic","base_url":%q,"api_key":"sk-m4-bbbb"}`, srvB.URL), 201), &pB)
	admin.post("/api/v1/fallback-order/claude-code", "", 405) // sanity: PUT only
	req, _ := http.NewRequest("PUT", hubURL+"/api/v1/fallback-order/claude-code",
		strings.NewReader(`{"provider_ids":["`+pA.ID+`","`+pB.ID+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := admin.cli.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("set fallback order: %v", err)
	}
	admin.post("/api/v1/switch", `{"app":"claude-code","provider_id":"`+pA.ID+`"}`, 200)

	var pc struct {
		Code string `json:"code"`
	}
	json.Unmarshal(admin.post("/api/v1/devices/pairing-code", `{}`, 200), &pc)
	statePath := filepath.Join(dir, "agent-state.json")
	state, err := hubclient.Pair(hubURL, pc.Code, "m4-dev", statePath)
	if err != nil {
		t.Fatal(err)
	}

	// Agent 栈：cli.go 同构接线——sink 同时喂用量队列与健康计数器。
	buf, err := usagebuf.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Close()
	var client *hubclient.Client
	tracker := health.New(health.DefaultConfig(), func(r wire.HealthReport) {
		client.ReportHealth(r)
	})
	fwd := forward.New(state.LocalToken, func(u forward.Usage) {
		buf.Enqueue(u.ToRecord())
		tracker.Observe(u)
	})
	fwdSrv := httptest.NewServer(fwd.Handler())
	defer fwdSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client = hubclient.New(statePath, state, fwd)
	client.UseQueue(buf)
	go client.Run(ctx)

	waitFor(t, "initial push (A active)", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pA.ID
	})

	ask := func() (int, string) {
		req, _ := http.NewRequest("POST", fwdSrv.URL+"/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"max_tokens":8,"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+state.LocalToken)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("request via agent: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// ws/ui 观察端。
	dialUI := func() *websocket.Conn {
		u, _ := url.Parse(hubURL)
		h := http.Header{}
		for _, c := range admin.cli.Jar.Cookies(u) {
			h.Add("Cookie", c.Name+"="+c.Value)
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		c, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(hubURL, "http")+"/api/v1/ws/ui",
			&websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatalf("dial ws/ui: %v", err)
		}
		return c
	}
	ui := dialUI()
	defer ui.CloseNow()
	readEventUntil := func(match func(kind string, payload string) bool, timeout time.Duration, what string) {
		deadline := time.Now().Add(timeout)
		for {
			rctx, rcancel := context.WithDeadline(context.Background(), deadline)
			var env wire.Envelope
			err := wsjson.Read(rctx, ui, &env)
			rcancel()
			if err != nil {
				t.Fatalf("ws/ui waiting for %s: %v", what, err)
			}
			if env.Type != wire.TypeUIEvent {
				continue
			}
			var ev wire.UIEvent
			if env.Decode(&ev) != nil {
				continue
			}
			if match(ev.Kind, string(ev.Payload)) {
				return
			}
		}
	}

	// 健康路径先行确认。
	if code, out := ask(); code != 200 || !strings.Contains(out, "msg_A") {
		t.Fatalf("healthy request = %d %s", code, out)
	}

	// ---- 打断 A：连续硬失败 → failover 到 B ----
	upA.failing.Store(true)
	for i := 0; i < 3; i++ {
		if code, _ := ask(); code != 503 {
			t.Fatalf("outage request #%d = %d, want 503", i+1, code)
		}
	}
	waitFor(t, "failover switch lands on agent (B active)", 10*time.Second, func() bool {
		tb := fwd.Table()
		return tb.Anthropic != nil && tb.Anthropic.ProviderID == pB.ID
	})
	readEventUntil(func(kind, payload string) bool {
		return kind == "failover" && strings.Contains(payload, `"action":"switched"`) &&
			strings.Contains(payload, `"to":"`+pB.ID+`"`)
	}, 5*time.Second, "failover switched event")
	if code, out := ask(); code != 200 || !strings.Contains(out, "msg_B") {
		t.Fatalf("post-failover request = %d %s", code, out)
	}

	// ---- A 复活：恢复探测（Agent 执行）→ recovered 事件；不自动切回 ----
	upA.failing.Store(false)
	readEventUntil(func(kind, payload string) bool {
		return kind == "probe" && strings.Contains(payload, `"action":"recovered"`)
	}, 15*time.Second, "probe recovered event")
	code, raw := admin.get("/api/v1/state")
	if code != 200 || !strings.Contains(string(raw), pB.ID) {
		t.Fatalf("auto failback happened? state = %s", raw)
	}

	// ---- 手动测速：按设备回报全部供应商 ----
	admin.post("/api/v1/speedtest", `{}`, 200)
	waitFor(t, "speedtest results from our device", 15*time.Second, func() bool {
		code, raw := admin.get("/api/v1/speedtest/latest")
		if code != 200 {
			return false
		}
		var run struct {
			Results map[string][]wire.ProbeResult `json:"results"`
		}
		if json.Unmarshal(raw, &run) != nil {
			return false
		}
		rs, ok := run.Results[state.DeviceID]
		return ok && len(rs) == 2
	})
}
