// Package forward is the Agent's passthrough forwarder (ADR-0002): CC/Codex
// requests enter on 127.0.0.1, get authenticated with the local token,
// prefix-routed to the current provider of their protocol, and forwarded
// byte-verbatim (auth-header swap and optional top-level model redirect
// excepted). Evolved from the validated research/06-sse-prototype.
package forward

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Upstream is one provider endpoint as pushed by the Hub.
type Upstream struct {
	ProviderID     string
	Protocol       string // "anthropic" | "openai"
	BaseURL        *url.URL
	APIKey         string
	ModelRedirects map[string]string
}

// RoutingTable is an immutable snapshot: Swap replaces it wholesale and
// in-flight requests keep the upstream they started with.
type RoutingTable struct {
	Rev       int64
	Anthropic *Upstream
	OpenAI    *Upstream
}

// Server is the forwarder. Construct with New, install routing via Swap,
// serve Handler() on 127.0.0.1 only (ADR-0005).
type Server struct {
	localToken string
	table      atomic.Pointer[RoutingTable]
	proxy      http.Handler
}

// New builds a forwarder that authenticates every request against
// localToken. sink receives parsed usage (pass nil for M1's no-op).
func New(localToken string, sink UsageSink) *Server {
	s := &Server{localToken: localToken}
	s.table.Store(&RoutingTable{})
	s.proxy = buildProxy(sink)
	return s
}

// Swap atomically installs a new routing table.
func (s *Server) Swap(t *RoutingTable) {
	if t == nil {
		t = &RoutingTable{}
	}
	s.table.Store(t)
}

// Table returns the current snapshot (for status displays and tests).
func (s *Server) Table() *RoutingTable { return s.table.Load() }

// App-side fixed prefixes (parent design.md §4 path-join table):
//
//	/anthropic/<rest>  → anthropic provider base_url + /<rest>
//	/openai/v1/<rest>  → openai   provider base_url + /<rest>
const (
	prefixAnthropic = "/anthropic"
	prefixOpenAI    = "/openai/v1"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "本地 token 校验失败：请通过 switchAPI Agent 接管的 CC/Codex 访问")
		return
	}

	table := s.table.Load()
	var up *Upstream
	var rest string
	var app string
	switch {
	case hasPrefix(r.URL.Path, prefixAnthropic):
		up, rest, app = table.Anthropic, strings.TrimPrefix(r.URL.Path, prefixAnthropic), "claude-code"
	case hasPrefix(r.URL.Path, prefixOpenAI):
		up, rest, app = table.OpenAI, strings.TrimPrefix(r.URL.Path, prefixOpenAI), "codex"
	default:
		writeErr(w, http.StatusNotFound, "未知路由前缀（应为 /anthropic/* 或 /openai/v1/*）")
		return
	}
	if up == nil {
		writeErr(w, http.StatusServiceUnavailable, "该协议当前没有生效供应商（等待 Hub 配置推送）")
		return
	}

	// Billing predicate (research/03 C8): only POST to the inference endpoint of
	// each protocol produces a usage record. count_tokens / GET models / OPTIONS
	// pass through untee'd. Evaluated on the stripped path before the "/" fixup.
	d := &decision{
		up:        up,
		requestID: uuid.NewString(),
		app:       app,
		start:     time.Now(),
		billing:   r.Method == http.MethodPost && isBillingPath(up.Protocol, rest),
	}
	if rest == "" {
		rest = "/"
	}

	// The proxy's Rewrite reads the decision from the context; the prefix is
	// stripped here so SetURL's path join sees only the provider-relative path.
	r2 := r.Clone(context.WithValue(r.Context(), decisionKey{}, d))
	r2.URL.Path = rest
	r2.URL.RawPath = ""
	s.proxy.ServeHTTP(w, r2)
}

// isBillingPath reports whether a stripped request path is the metered
// inference endpoint for its protocol: anthropic .../messages, openai
// .../responses. count_tokens (.../messages/count_tokens) is excluded.
func isBillingPath(protocol, rest string) bool {
	switch protocol {
	case "anthropic":
		return strings.HasSuffix(rest, "/messages")
	case "openai":
		return strings.HasSuffix(rest, "/responses")
	}
	return false
}

// Handler returns the http.Handler to mount on the local listener.
func (s *Server) Handler() http.Handler { return s }

// authorized accepts the local token via either auth header: CC sends
// Authorization: Bearer (ANTHROPIC_AUTH_TOKEN) or x-api-key
// (ANTHROPIC_API_KEY/apiKeyHelper), Codex sends Bearer (research/01 C2).
func (s *Server) authorized(r *http.Request) bool {
	candidate := r.Header.Get("X-Api-Key")
	if h := r.Header.Get("Authorization"); candidate == "" && strings.HasPrefix(h, "Bearer ") {
		candidate = strings.TrimPrefix(h, "Bearer ")
	}
	return candidate != "" &&
		subtle.ConstantTimeCompare([]byte(candidate), []byte(s.localToken)) == 1
}

func hasPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

type decisionKey struct{}

// decision is the per-request routing + accounting context, set in ServeHTTP
// and read by the proxy's Rewrite (model capture) and the usage tee.
type decision struct {
	up        *Upstream
	requestID string
	app       string
	start     time.Time
	billing   bool

	// Model provenance, filled by rewriteModel when the request body is parsed:
	// reqModel is the model the client asked for; redirModel is the redirect
	// target ("to"), set only when a redirect actually applied.
	reqModel   string
	redirModel string
}

// recordModel picks the model to record: the client-requested model when known,
// otherwise the model observed on the wire (which, absent a redirect, is the
// same thing).
func (d *decision) recordModel(wireModel string) string {
	if d.reqModel != "" {
		return d.reqModel
	}
	return wireModel
}

func decisionFrom(ctx context.Context) *decision {
	d, _ := ctx.Value(decisionKey{}).(*decision)
	return d
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": "switchapi_agent", "message": msg},
	})
}
