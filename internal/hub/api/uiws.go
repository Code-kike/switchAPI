// uiws.go — /api/v1/ws/ui：浏览器/桌面壳的实时下行通道（M3 design.md §1）。
// Session 中间件先行拦截（未登录 401，不进 WS 握手）。只有下行三类消息：
// state_changed / event / usage_tick，语义是"失效通知"——客户端收到后 refetch，
// 通道本身不携带全量数据，避免与 REST 出现两套口径。
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/wire"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	uiWriteTimeout = 5 * time.Second
	uiSendBuffer   = 16 // 每连接待发队列；塞满视为慢客户端，直接摘除
)

// uiConn is one live browser/desktop-shell connection. A dedicated writer
// goroutine drains ch so a slow client never blocks broadcasts to the others.
type uiConn struct {
	c      *websocket.Conn
	ch     chan wire.Envelope
	ctx    context.Context
	cancel context.CancelFunc
}

// uiHub is the connection registry. Zero value is ready to use.
type uiHub struct {
	mu    sync.Mutex
	conns map[*uiConn]struct{}
}

func (h *uiHub) add(c *uiConn) {
	h.mu.Lock()
	if h.conns == nil {
		h.conns = map[*uiConn]struct{}{}
	}
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *uiHub) remove(c *uiConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// broadcast fans an envelope out to every live conn. Non-blocking: a full
// queue means the client stopped reading — drop it (it reconnects and
// refetches, which is exactly the channel's semantics).
func (h *uiHub) broadcast(env wire.Envelope) {
	h.mu.Lock()
	targets := make([]*uiConn, 0, len(h.conns))
	for c := range h.conns {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		select {
		case c.ch <- env:
		default:
			c.cancel()
		}
	}
}

// closeAll drops every live UI connection — graceful-shutdown path（WS 是
// hijacked 连接，http.Server.Shutdown 不会关它们）。
func (h *uiHub) closeAll() {
	h.mu.Lock()
	conns := h.conns
	h.conns = map[*uiConn]struct{}{}
	h.mu.Unlock()
	for c := range conns {
		c.cancel()
		c.c.Close(websocket.StatusGoingAway, "hub shutting down")
	}
}

// handleUIWS upgrades a logged-in session to the notification channel. The
// session middleware already rejected anonymous requests before we get here.
func (s *Server) handleUIWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &uiConn{c: c, ch: make(chan wire.Envelope, uiSendBuffer), ctx: ctx, cancel: cancel}
	s.ui.add(conn)

	// Writer: the only goroutine allowed to write this conn.
	go func() {
		defer func() {
			s.ui.remove(conn)
			cancel()
			c.Close(websocket.StatusNormalClosure, "bye")
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-conn.ch:
				wctx, wcancel := context.WithTimeout(ctx, uiWriteTimeout)
				err := wsjson.Write(wctx, c, env)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// Reader: clients never send meaningful frames; this only detects close
	// (and services control frames like pong).
	go func() {
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
}

// notifyUI marshals and fans out; marshal errors are programmer errors on our
// own payload types — log loudly instead of dropping silently.
func (s *Server) notifyUI(typ string, payload any) {
	env, err := wire.NewEnvelope(typ, payload)
	if err != nil {
		log.Printf("api: ws/ui marshal %s: %v", typ, err)
		return
	}
	s.ui.broadcast(env)
}

// NotifyState pushes the current app→provider map (same source as
// GET /api/v1/state) with the config revision.
func (s *Server) NotifyState() {
	apps := map[string]string{}
	for app := range appProtocol {
		if st, err := s.st.GetAppState(app); err == nil {
			apps[app] = st.ActiveProviderID
		}
	}
	rev := int64(0)
	if v, ok, err := s.st.GetSetting("config_rev"); err == nil && ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rev = n
		}
	}
	s.notifyUI(wire.TypeUIStateChanged, wire.UIStateChanged{Rev: rev, Apps: apps})
}

// NotifyEvent pushes one freshly written events row.
func (s *Server) NotifyEvent(ev store.Event) {
	s.notifyUI(wire.TypeUIEvent, wire.UIEvent{
		ID: ev.ID, TS: ev.TS, Kind: ev.Kind, Payload: json.RawMessage(ev.Payload),
	})
}

// UsageInserted implements realtime.UsageNotifier: new usage rows landed via
// ws/agent → tell UI clients to refetch stats.
func (s *Server) UsageInserted(inserted int, lastTS int64) {
	s.notifyUI(wire.TypeUIUsageTick, wire.UIUsageTick{Inserted: inserted, LastTS: lastTS})
}

// CloseUI drops all live UI connections (graceful shutdown).
func (s *Server) CloseUI() { s.ui.closeAll() }

// event appends to the timeline AND fans it out to live UI clients. All api
// call sites go through here so the timeline page stays live.
func (s *Server) event(kind, payload string) {
	ev, err := s.st.AppendEvent(kind, payload)
	if err != nil {
		log.Printf("api: append event %s: %v", kind, err)
		return
	}
	s.NotifyEvent(ev)
}
