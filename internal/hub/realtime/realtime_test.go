package realtime

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/api"
	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// rig assembles store + realtime + api exactly like cmd/hub/main.go does.
type rig struct {
	st    *store.Store
	key   []byte
	hub   *Hub
	srv   *httptest.Server
	admin *http.Client
	t     *testing.T
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := cryptoutil.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	hub := New(st, key)
	resolver, err := pricing.NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	root.Handle("GET /api/v1/ws/agent", hub.Handler())
	root.Handle("/", api.New(st, key, hub, resolver).Handler())
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar}
	r := &rig{st: st, key: key, hub: hub, srv: srv, admin: admin, t: t}
	r.post("/api/v1/auth/login", `{"password":"pw"}`, 200)
	return r
}

func (r *rig) post(path, body string, want int) []byte {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.admin.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != want {
		r.t.Fatalf("POST %s = %d (want %d): %s", path, resp.StatusCode, want, buf[:n])
	}
	return buf[:n]
}

// seedProvider creates a provider directly in the store with a sealed key.
func (r *rig) seedProvider(id, name, protocol, plainKey string) {
	r.t.Helper()
	enc, err := cryptoutil.Seal(r.key, []byte(plainKey))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.CreateProvider(store.Provider{
		ID: id, Name: name, Protocol: protocol,
		BaseURL: "https://relay.example", APIKeyEnc: enc, ModelRedirects: "{}",
		CostCoefficient: 1.0,
	}); err != nil {
		r.t.Fatal(err)
	}
}

// seedDevice registers a paired device and returns its token.
func (r *rig) seedDevice(id string) string {
	r.t.Helper()
	token, err := cryptoutil.NewToken()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.CreateDevice(store.Device{
		ID: id, Name: id, Platform: "linux", TokenHash: cryptoutil.HashToken(token),
	}); err != nil {
		r.t.Fatal(err)
	}
	return token
}

func (r *rig) dial(ctx context.Context, token string) (*websocket.Conn, *http.Response, error) {
	wsURL := strings.Replace(r.srv.URL, "http", "ws", 1) + "/api/v1/ws/agent"
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
}

func readPush(t *testing.T, ctx context.Context, c *websocket.Conn) wire.ConfigPush {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var env wire.Envelope
	if err := wsjson.Read(rctx, c, &env); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if env.Type != wire.TypeConfigPush {
		t.Fatalf("envelope type = %q", env.Type)
	}
	var push wire.ConfigPush
	if err := env.Decode(&push); err != nil {
		t.Fatal(err)
	}
	return push
}

func TestConnectPushAndBroadcastOnSwitch(t *testing.T) {
	r := newRig(t)
	r.seedProvider("p1", "站点1", "anthropic", "sk-plain-1111")
	r.seedProvider("p2", "站点2", "anthropic", "sk-plain-2222")
	if err := r.st.SetAppState("claude-code", "p1", "test"); err != nil {
		t.Fatal(err)
	}
	token := r.seedDevice("d1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := r.dial(ctx, token)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Full snapshot on connect, with the DECRYPTED key (LAN-trust push).
	push := readPush(t, ctx, c)
	route, ok := push.Apps["claude-code"]
	if !ok || route.ProviderID != "p1" || route.APIKey != "sk-plain-1111" {
		t.Fatalf("initial push wrong: %+v", push)
	}
	rev1 := push.Rev

	// Hello + heartbeat refresh last_seen.
	env, _ := wire.NewEnvelope(wire.TypeHello, wire.Hello{Name: "d1", Platform: "linux", Version: "test"})
	if err := wsjson.Write(ctx, c, env); err != nil {
		t.Fatal(err)
	}

	// A switch via the REST API must broadcast an updated push with a higher rev.
	r.post("/api/v1/switch", `{"app":"claude-code","provider_id":"p2"}`, 200)
	push2 := readPush(t, ctx, c)
	if push2.Apps["claude-code"].ProviderID != "p2" || push2.Apps["claude-code"].APIKey != "sk-plain-2222" {
		t.Fatalf("post-switch push wrong: %+v", push2)
	}
	if push2.Rev <= rev1 {
		t.Fatalf("rev did not increase: %d → %d", rev1, push2.Rev)
	}
}

func TestUsageBatchIngestAndAck(t *testing.T) {
	r := newRig(t)
	token := r.seedDevice("d1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := r.dial(ctx, token)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	_ = readPush(t, ctx, c) // initial snapshot

	batch := wire.UsageBatch{
		BatchID: "b1",
		Records: []wire.UsageRecord{
			{RequestID: "req1", TS: 1000, App: "claude-code", ProviderID: "p1",
				Model: "claude-haiku-4-5", InputTokens: 10, OutputTokens: 20,
				Status: 200, UsageSource: "wire"},
			{RequestID: "req2", TS: 1001, App: "codex", ProviderID: "p2",
				Model: "gpt-5.1-codex", InputTokens: 5, OutputTokens: 6,
				Status: 200, UsageSource: "wire"},
		},
	}

	// Send the batch, expect a usage_ack echoing batch_id.
	env, _ := wire.NewEnvelope(wire.TypeUsageBatch, batch)
	if err := wsjson.Write(ctx, c, env); err != nil {
		t.Fatal(err)
	}
	ack := readAck(t, ctx, c)
	if ack.BatchID != "b1" {
		t.Fatalf("ack batch_id = %q, want b1", ack.BatchID)
	}

	// Rows landed, attributed to the connection's device.
	rows, total, err := r.st.QueryUsage(store.UsageFilter{})
	if err != nil || total != 2 {
		t.Fatalf("query after batch: total=%d err=%v", total, err)
	}
	if rows[0].DeviceID != "d1" {
		t.Fatalf("device_id = %q, want d1", rows[0].DeviceID)
	}

	// Resending the same batch (at-least-once) dedups on request_id but still
	// acks so the Agent clears its queue.
	if err := wsjson.Write(ctx, c, env); err != nil {
		t.Fatal(err)
	}
	if ack := readAck(t, ctx, c); ack.BatchID != "b1" {
		t.Fatalf("re-ack batch_id = %q", ack.BatchID)
	}
	_, total, _ = r.st.QueryUsage(store.UsageFilter{})
	if total != 2 {
		t.Fatalf("duplicate batch inflated rows: total=%d", total)
	}
}

func readAck(t *testing.T, ctx context.Context, c *websocket.Conn) wire.UsageAck {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var env wire.Envelope
	if err := wsjson.Read(rctx, c, &env); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if env.Type != wire.TypeUsageAck {
		t.Fatalf("envelope type = %q, want usage_ack", env.Type)
	}
	var ack wire.UsageAck
	if err := env.Decode(&ack); err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestAuthRejectsBadAndRevokedTokens(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := r.dial(ctx, "bogus-token"); err == nil {
		t.Fatal("bogus token accepted")
	}

	token := r.seedDevice("d1")
	if err := r.st.RevokeDevice("d1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.dial(ctx, token); err == nil {
		t.Fatal("revoked token accepted")
	}
}

func TestKickOnRevokeClosesConnection(t *testing.T) {
	r := newRig(t)
	token := r.seedDevice("d1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := r.dial(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	_ = readPush(t, ctx, c) // initial snapshot

	// Revoke via REST → Kick → the read loop must terminate promptly.
	req, _ := http.NewRequest("DELETE", r.srv.URL+"/api/v1/devices/d1", nil)
	resp, err := r.admin.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	var env wire.Envelope
	if err := wsjson.Read(rctx, c, &env); err == nil {
		t.Fatal("connection survived revocation")
	}
}
