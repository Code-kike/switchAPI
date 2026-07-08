package api

// uiws_test.go — /api/v1/ws/ui 通道行为：鉴权拦截、switch → state_changed、
// usage 入库通知 → usage_tick、多客户端广播与慢/死客户端隔离。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// dialUI opens a ws/ui connection reusing cli's session cookies.
func dialUI(t *testing.T, r *testRig, cli *http.Client) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/api/v1/ws/ui"
	h := http.Header{}
	if cli.Jar != nil {
		u, _ := url.Parse(r.srv.URL)
		for _, c := range cli.Jar.Cookies(u) {
			h.Add("Cookie", c.Name+"="+c.Value)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial ws/ui: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

// readUntil reads envelopes until one of the wanted type arrives (skipping
// interleaved broadcasts) or the deadline passes.
func readUntil(t *testing.T, c *websocket.Conn, typ string, timeout time.Duration) wire.Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		var env wire.Envelope
		err := wsjson.Read(ctx, c, &env)
		cancel()
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if env.Type == typ {
			return env
		}
	}
}

func TestUIWSUnauthorized(t *testing.T) {
	r := newTestRig(t)
	wsURL := "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/api/v1/ws/ui"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		c.CloseNow()
		t.Fatal("anonymous ws/ui upgrade accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous upgrade status = %v, want 401", resp)
	}
}

func TestUIWSStateChangedOnSwitch(t *testing.T) {
	r := newTestRig(t)
	p1 := r.createProvider(t, "A1", "anthropic", "sk-a1-0001")
	c := dialUI(t, r, r.auth)

	if code, body := r.do(r.auth, "POST", "/api/v1/switch",
		`{"app":"claude-code","provider_id":"`+p1+`"}`); code != 200 {
		t.Fatalf("switch = %d: %s", code, body)
	}

	// switch 同时产生 event（时间线）与 state_changed（状态），两者都必须到达。
	env := readUntil(t, c, wire.TypeUIStateChanged, 3*time.Second)
	var st wire.UIStateChanged
	if err := env.Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Apps["claude-code"] != p1 {
		t.Fatalf("state_changed apps = %v, want claude-code=%s", st.Apps, p1)
	}
	// rev 由 realtime.Broadcast 递增；本 rig 用桩 channel，rev 语义在 e2e 验证。

	c2 := dialUI(t, r, r.auth)
	p2 := r.createProvider(t, "A2", "anthropic", "sk-a2-0002")
	envEv := readUntil(t, c2, wire.TypeUIEvent, 3*time.Second)
	var ev wire.UIEvent
	if err := envEv.Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "provider" || ev.ID == 0 || ev.TS == 0 {
		t.Fatalf("event = %+v", ev)
	}
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload["id"] != p2 {
		t.Fatalf("event payload = %s (err %v)", ev.Payload, err)
	}
}

func TestUIWSUsageTickBroadcastAndDeadClient(t *testing.T) {
	r := newTestRig(t)
	c1 := dialUI(t, r, r.auth)
	c2 := dialUI(t, r, r.auth)

	// 一个客户端直接死掉：广播不得因此失败或阻塞其他客户端。
	c1.CloseNow()

	r.api.UsageInserted(3, 42)
	env := readUntil(t, c2, wire.TypeUIUsageTick, 3*time.Second)
	var tick wire.UIUsageTick
	if err := env.Decode(&tick); err != nil {
		t.Fatal(err)
	}
	if tick.Inserted != 3 || tick.LastTS != 42 {
		t.Fatalf("usage_tick = %+v", tick)
	}

	// 死客户端被摘除后再广播，幸存者继续收到。
	r.api.UsageInserted(1, 43)
	env = readUntil(t, c2, wire.TypeUIUsageTick, 3*time.Second)
	if err := env.Decode(&tick); err != nil || tick.LastTS != 43 {
		t.Fatalf("second usage_tick = %+v (err %v)", tick, err)
	}
}

func TestUIWSCloseUI(t *testing.T) {
	r := newTestRig(t)
	c := dialUI(t, r, r.auth)
	r.api.CloseUI()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var env wire.Envelope
	if err := wsjson.Read(ctx, c, &env); err == nil {
		t.Fatal("connection survived CloseUI")
	}
}
