// Package api is the Hub's REST surface (/api/v1, parent design.md §3):
// single-admin session auth, provider management, the global switch, pairing
// and the event timeline. WebSocket channels live in internal/hub/realtime.
package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/backup"
	"github.com/Code-kike/switchAPI/internal/hub/failover"
	"github.com/Code-kike/switchAPI/internal/hub/pricing"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
)

// AgentChannel is what the API needs from the realtime layer (implemented by
// realtime.Hub; interface here avoids an import cycle).
type AgentChannel interface {
	Broadcast()           // config changed → push a fresh snapshot to all Agents
	Kick(deviceID string) // revoked device → drop its live connection
}

const (
	sessionCookie = "switchapi_session"
	sessionTTL    = 30 * 24 * time.Hour
	pairingTTL    = 10 * time.Minute
)

// Server carries the handler state. Construct with New, mount Handler().
type Server struct {
	st        *store.Store
	masterKey []byte
	agents    AgentChannel
	pricer    *pricing.Resolver
	mux       *http.ServeMux

	mu       sync.Mutex
	sessions map[string]time.Time // token → expiry
	pairings map[string]time.Time // one-time code → expiry

	ui uiHub // ws/ui 实时下行通道（uiws.go）

	reliability *failover.Engine // 可选：测速/健康（reliability.go，测试可 nil）
	backups     *backup.Manager  // 可选：快照（export.go，测试可 nil）
}

// New wires all routes. agents and pricer may be nil in tests.
func New(st *store.Store, masterKey []byte, agents AgentChannel, pricer *pricing.Resolver) *Server {
	s := &Server{
		st: st, masterKey: masterKey, agents: agents, pricer: pricer,
		mux:      http.NewServeMux(),
		sessions: map[string]time.Time{},
		pairings: map[string]time.Time{},
	}
	m := s.mux
	m.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	m.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	m.HandleFunc("GET /api/v1/providers", s.handleProviderList)
	m.HandleFunc("POST /api/v1/providers", s.handleProviderCreate)
	m.HandleFunc("PUT /api/v1/providers/{id}", s.handleProviderUpdate)
	m.HandleFunc("DELETE /api/v1/providers/{id}", s.handleProviderDelete)
	m.HandleFunc("GET /api/v1/presets", s.handlePresets)
	m.HandleFunc("GET /api/v1/state", s.handleState)
	m.HandleFunc("POST /api/v1/switch", s.handleSwitch)
	m.HandleFunc("GET /api/v1/fallback-order/{app}", s.handleFallbackGet)
	m.HandleFunc("PUT /api/v1/fallback-order/{app}", s.handleFallbackPut)
	m.HandleFunc("POST /api/v1/devices/pairing-code", s.handlePairingCode)
	m.HandleFunc("POST /api/v1/devices/pair", s.handlePair)
	m.HandleFunc("GET /api/v1/devices", s.handleDeviceList)
	m.HandleFunc("DELETE /api/v1/devices/{id}", s.handleDeviceRevoke)
	m.HandleFunc("GET /api/v1/events", s.handleEvents)
	m.HandleFunc("GET /api/v1/usage", s.handleUsage)
	m.HandleFunc("GET /api/v1/stats/summary", s.handleStatsSummary)
	m.HandleFunc("GET /api/v1/stats/trend", s.handleStatsTrend)
	m.HandleFunc("GET /api/v1/stats/breakdown", s.handleStatsBreakdown)
	m.HandleFunc("GET /api/v1/ws/ui", s.handleUIWS) // 受 session 中间件保护
	m.HandleFunc("GET /api/v1/health", s.handleHealth)
	m.HandleFunc("POST /api/v1/speedtest", s.handleSpeedtest)
	m.HandleFunc("GET /api/v1/speedtest/latest", s.handleSpeedtestLatest)
	m.HandleFunc("POST /api/v1/backup/run", s.handleBackupRun)
	m.HandleFunc("GET /api/v1/backups", s.handleBackupList)
	m.HandleFunc("POST /api/v1/export", s.handleExport)
	m.HandleFunc("POST /api/v1/import", s.handleImport)
	m.HandleFunc("POST /api/v1/import/cc-switch", s.handleImportCCSwitch)
	m.HandleFunc("GET /api/v1/usage/export.csv", s.handleUsageCSV)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return s
}

// Handler returns the mux wrapped with the session middleware.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPublic(r) && !s.validSession(r) {
			httpError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// isPublic lists the routes reachable without a session: login, the
// code-gated pairing endpoint, and liveness.
func isPublic(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/login",
		r.Method == http.MethodPost && r.URL.Path == "/api/v1/devices/pair",
		r.Method == http.MethodGet && r.URL.Path == "/healthz":
		return true
	}
	return false
}

// ---- auth ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		httpError(w, http.StatusBadRequest, "密码不能为空")
		return
	}
	hash, ok, err := s.st.GetSetting("admin_password_hash")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		// 单用户引导：首次登录即设定管理员密码（design.md 任务版 §2）。
		h, err := cryptoutil.Argon2idHash(req.Password)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.st.SetSetting("admin_password_hash", h); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.event("auth", `{"action":"bootstrap"}`)
	} else if !cryptoutil.Argon2idVerify(req.Password, hash) {
		httpError(w, http.StatusUnauthorized, "密码错误")
		return
	}

	token, err := cryptoutil.NewToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()

	// Max-Age 必设（Tauri WebView 会话 cookie 不跨重启）；Secure 必不设
	// （WebKit 拒绝 http 下的 Secure cookie）——研究#7。
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

// ---- shared helpers ----

func (s *Server) broadcast() {
	if s.agents != nil {
		s.agents.Broadcast()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": msg}})
}

// appProtocol maps an App to its wire protocol (CONTEXT.md: App/协议边界).
var appProtocol = map[string]string{
	"claude-code": "anthropic",
	"codex":       "openai",
}

func validApp(app string) bool { _, ok := appProtocol[app]; return ok }
