// Package realtime is the Hub side of the /api/v1/ws/agent channel: device
// token auth, a live-connection registry, the full-snapshot ConfigPush on
// connect, and Broadcast() after every routing-affecting change.
package realtime

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	writeTimeout  = 10 * time.Second
	readDeadline  = 90 * time.Second // 心跳 30s，3 次未见即判静默
	revSettingKey = "config_rev"
)

// Hub manages Agent connections. Implements api.AgentChannel.
type Hub struct {
	st        *store.Store
	masterKey []byte

	mu    sync.Mutex
	conns map[string]*agentConn // device_id → live conn（同设备重连时旧连接被替换）
}

type agentConn struct {
	c      *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds the channel hub.
func New(st *store.Store, masterKey []byte) *Hub {
	return &Hub{st: st, masterKey: masterKey, conns: map[string]*agentConn{}}
}

// Handler serves GET /api/v1/ws/agent.
func (h *Hub) Handler() http.Handler { return http.HandlerFunc(h.serveWS) }

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		http.Error(w, "missing device token", http.StatusUnauthorized)
		return
	}
	dev, err := h.st.FindDeviceByTokenHash(cryptoutil.HashToken(token))
	if err != nil {
		http.Error(w, "invalid or revoked device token", http.StatusUnauthorized)
		return
	}

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &agentConn{c: c, ctx: ctx, cancel: cancel}

	h.mu.Lock()
	if old, ok := h.conns[dev.ID]; ok {
		old.cancel()
		old.c.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	h.conns[dev.ID] = conn
	h.mu.Unlock()

	defer func() {
		cancel()
		c.Close(websocket.StatusNormalClosure, "bye")
		h.mu.Lock()
		if h.conns[dev.ID] == conn {
			delete(h.conns, dev.ID)
		}
		h.mu.Unlock()
	}()

	h.st.TouchDevice(dev.ID)

	// 连接即全量推送当前快照（design.md §5 手动切换流程的接入端）。
	push, err := h.buildPush()
	if err != nil {
		log.Printf("realtime: build push: %v", err)
		return
	}
	if err := send(ctx, c, push); err != nil {
		return
	}

	// 读循环：任何入站消息都刷新 last_seen；静默超时断开。
	for {
		rctx, rcancel := context.WithTimeout(ctx, readDeadline)
		var env wire.Envelope
		err := wsjson.Read(rctx, c, &env)
		rcancel()
		if err != nil {
			return
		}
		h.st.TouchDevice(dev.ID)
		switch env.Type {
		case wire.TypeHello:
			var hello wire.Hello
			if env.Decode(&hello) == nil {
				log.Printf("realtime: device %s hello: %s/%s %s", dev.ID, hello.Name, hello.Platform, hello.Version)
			}
		case wire.TypeHeartbeat:
			// TouchDevice 已完成全部工作
		}
	}
}

// Broadcast bumps the config revision and pushes a fresh snapshot to every
// live Agent. Failed writes drop the connection (the Agent reconnects).
func (h *Hub) Broadcast() {
	if err := h.bumpRev(); err != nil {
		log.Printf("realtime: bump rev: %v", err)
		return
	}
	push, err := h.buildPush()
	if err != nil {
		log.Printf("realtime: build push: %v", err)
		return
	}

	h.mu.Lock()
	targets := make(map[string]*agentConn, len(h.conns))
	for id, c := range h.conns {
		targets[id] = c
	}
	h.mu.Unlock()

	for id, conn := range targets {
		if err := send(conn.ctx, conn.c, push); err != nil {
			log.Printf("realtime: push to %s failed: %v", id, err)
			conn.cancel()
			conn.c.Close(websocket.StatusAbnormalClosure, "write failed")
			h.mu.Lock()
			if h.conns[id] == conn {
				delete(h.conns, id)
			}
			h.mu.Unlock()
		}
	}
}

// Kick drops a device's live connection (called on revocation).
func (h *Hub) Kick(deviceID string) {
	h.mu.Lock()
	conn, ok := h.conns[deviceID]
	if ok {
		delete(h.conns, deviceID)
	}
	h.mu.Unlock()
	if ok {
		conn.cancel()
		conn.c.Close(websocket.StatusPolicyViolation, "device revoked")
	}
}

// CloseAll drops every live connection — the graceful-shutdown path. WS
// conns are hijacked from net/http, so http.Server.Shutdown/Close never
// closes them; the Hub must do it itself (Agents reconnect with backoff).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	conns := h.conns
	h.conns = map[string]*agentConn{}
	h.mu.Unlock()
	for _, conn := range conns {
		conn.cancel()
		conn.c.Close(websocket.StatusGoingAway, "hub shutting down")
	}
}

func send(ctx context.Context, c *websocket.Conn, push wire.ConfigPush) error {
	env, err := wire.NewEnvelope(wire.TypeConfigPush, push)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(wctx, c, env)
}

// buildPush assembles the full snapshot: active provider per App (api key
// decrypted — LAN trust, ADR-0005) + fallback orders + current rev.
func (h *Hub) buildPush() (wire.ConfigPush, error) {
	push := wire.ConfigPush{
		Apps:           map[string]wire.AppRoute{},
		FallbackOrders: map[string][]string{},
	}
	rev, err := h.currentRev()
	if err != nil {
		return push, err
	}
	push.Rev = rev

	for _, app := range []string{"claude-code", "codex"} {
		st, err := h.st.GetAppState(app)
		if err != nil {
			continue // 尚未切换过该 App
		}
		p, err := h.st.GetProvider(st.ActiveProviderID)
		if err != nil {
			log.Printf("realtime: app %s points at missing provider %s", app, st.ActiveProviderID)
			continue
		}
		plain, err := cryptoutil.Open(h.masterKey, p.APIKeyEnc)
		if err != nil {
			log.Printf("realtime: decrypt key for provider %s: %v", p.ID, err)
			continue
		}
		redirects := map[string]string{}
		json.Unmarshal([]byte(p.ModelRedirects), &redirects)
		push.Apps[app] = wire.AppRoute{
			ProviderID: p.ID, Name: p.Name, Protocol: p.Protocol,
			BaseURL: p.BaseURL, APIKey: string(plain), ModelRedirects: redirects,
		}
		if order, err := h.st.GetFallbackOrder(app); err == nil && len(order) > 0 {
			push.FallbackOrders[app] = order
		}
	}
	return push, nil
}

func (h *Hub) currentRev() (int64, error) {
	v, ok, err := h.st.GetSetting(revSettingKey)
	if err != nil {
		return 0, err
	}
	if !ok {
		if err := h.st.SetSetting(revSettingKey, "1"); err != nil {
			return 0, err
		}
		return 1, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (h *Hub) bumpRev() error {
	rev, err := h.currentRev()
	if err != nil {
		return err
	}
	return h.st.SetSetting(revSettingKey, strconv.FormatInt(rev+1, 10))
}
