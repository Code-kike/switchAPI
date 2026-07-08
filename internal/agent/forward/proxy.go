package forward

// proxy.go — the shared httputil.ReverseProxy. Per-request routing decisions
// arrive via the request context (set in Server.ServeHTTP); everything here
// mirrors the settings validated by research/06 against a real relay.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"
)

const (
	maxRewriteBody = 32 << 20 // cap request buffering for the rewrite path
	maxSSELine     = 1 << 20  // bound for a single anthropic SSE line
	maxOpenAILine  = 8 << 20  // openai response.completed embeds the whole output on one line
	maxJSONBody    = 8 << 20  // bound for buffering a non-streaming JSON response
)

func buildProxy(sink UsageSink) *httputil.ReverseProxy {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Keep upstream bytes verbatim: never let the Transport inject
	// "Accept-Encoding: gzip" + transparently gunzip.
	tr.DisableCompression = true
	// Clone() keeps ForceAttemptHTTP2=true → h2 to https upstreams via ALPN.
	tr.MaxIdleConnsPerHost = 32
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// ResponseHeaderTimeout deliberately 0 at the Transport level: a
	// NON-streaming generation sends headers only after full completion.
	// 条件化超时（研究/08）在 Rewrite 里按请求布防（见 prepareBody 之后）。
	tr.ResponseHeaderTimeout = 0

	return &httputil.ReverseProxy{
		Transport: tr,
		// Belt and braces: stdlib already force-flushes text/event-stream and
		// unknown-length responses; -1 extends that to every response.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			d := decisionFrom(pr.In.Context())
			if d == nil || d.up == nil {
				return // unreachable via Server.ServeHTTP; nothing sane to do
			}
			pr.SetURL(d.up.BaseURL) // scheme/host + base-path join; rewrites outbound Host

			// Auth swap: strip the local token (either header), inject the
			// provider key. anthropic gets BOTH headers — the pattern CC
			// itself uses for apiKeyHelper values, maximizing relay compat;
			// openai wire is Bearer-only. (research/01 C2/C8, research/02)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			switch d.up.Protocol {
			case "anthropic":
				pr.Out.Header.Set("X-Api-Key", d.up.APIKey)
				pr.Out.Header.Set("Authorization", "Bearer "+d.up.APIKey)
			default:
				pr.Out.Header.Set("Authorization", "Bearer "+d.up.APIKey)
			}
			// The usage tee must never see compressed bytes (research/06).
			pr.Out.Header.Set("Accept-Encoding", "identity")
			// No SetXForwarded(): loopback-only hop.
			parsed := prepareBody(pr, d)

			// 超时布防（研究/08 #4/#6，M4）：仅在请求体成功解析、流式与否
			// 明确时才布防——未知形态保持 M1 的"不限时"语义，不误杀慢请求。
			// 流中静默（#5）由 stream tee 的看门狗接管。
			if d.billing && parsed {
				if d.stream {
					d.armHeaderTimer(firstByteTimeout, "timeout_first_byte")
				} else {
					d.armHeaderTimer(nonStreamTotalTimeout, "timeout_total")
				}
			}
		},
		ModifyResponse: func(res *http.Response) error {
			d := decisionFrom(res.Request.Context())
			if d == nil || d.up == nil {
				return nil
			}
			if d.stream {
				// 流式：响应头已到，TTFB 看门狗解除（流中静默由 tee 接管）。
				d.stopHeaderTimer()
			}
			if sink == nil || !d.billing {
				return nil // non-billing path: pass through, no record (research/03 C8)
			}
			if shouldParseSSE(res) {
				res.Body = newStreamTee(res.Body, res.StatusCode, d, sink)
			} else {
				// Non-streaming: buffer + parse only genuine JSON bodies; other
				// content (error pages) still yields a record (usage_source=none).
				res.Body = newJSONTee(res.Body, res.StatusCode, d, sink, isJSONResponse(res))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Transport-level failure (dial/TLS/watchdog cancel). Client
			// disconnects mid-stream do NOT land here; they abort via
			// http.ErrAbortHandler. 健康判定最重要的信号在这里产生（M4）：
			// 计费请求即使没拿到响应头，也必须留下一条 usage 记录。
			d := decisionFrom(r.Context())
			kind := classifyTransportErr(err)
			status := http.StatusBadGateway
			if d != nil {
				if tk := d.getTimeoutKind(); tk != "" {
					kind = tk
					status = http.StatusGatewayTimeout
				}
				d.stopTimers()
				if d.billing && sink != nil && kind != "client_abort" && d.markRecorded() {
					sink(Usage{
						RequestID: d.requestID, App: d.app, ProviderID: d.up.ProviderID,
						TS: d.start.Unix(), Model: d.reqModel, ModelRedirected: d.redirModel,
						DurationMS: time.Since(d.start).Milliseconds(),
						Status:     status, ErrorKind: kind, UsageSource: "none",
					})
				}
			}
			log.Printf("forward: upstream error (%s): %v", kind, err)
			writeErr(w, status, "上游请求失败："+err.Error())
		},
	}
}

// Timeout knobs (research/08 参数表 #4/#5/#6).
const (
	firstByteTimeout      = 60 * time.Second
	streamIdleTimeout     = 120 * time.Second
	nonStreamTotalTimeout = 300 * time.Second
)

// classifyTransportErr maps a RoundTrip error onto the health taxonomy
// (research/08 失败分类). Message sniffing is inherently best-effort; anything
// unrecognized lands in the generic hard bucket "transport".
func classifyTransportErr(err error) string {
	if errors.Is(err, context.Canceled) {
		return "client_abort"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "tls:"), strings.Contains(s, "x509"):
		return "tls"
	case strings.Contains(s, "no such host"), strings.Contains(s, "connection refused"),
		strings.Contains(s, "network is unreachable"), strings.Contains(s, "i/o timeout"):
		return "connect"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout_total"
	default:
		return "transport"
	}
}

func shouldParseSSE(res *http.Response) bool {
	return strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream")
}

func isJSONResponse(res *http.Response) bool {
	return strings.Contains(res.Header.Get("Content-Type"), "json")
}

// prepareBody buffers relevant JSON request bodies once, to (a) capture the
// requested model + stream flag（记账与超时布防的输入）and (b) apply the
// top-level model redirect. It runs for billing requests OR when a redirect
// table exists; compressed / non-JSON / oversized bodies stream through
// untouched (zero copy) and return false — no timers get armed for them.
func prepareBody(pr *httputil.ProxyRequest, d *decision) (parsed bool) {
	redirects := d.up.ModelRedirects
	if pr.Out.Body == nil || (!d.billing && len(redirects) == 0) {
		return false
	}
	// Never touch compressed or non-JSON bodies.
	if pr.In.Header.Get("Content-Encoding") != "" {
		return false
	}
	if ct := pr.In.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return false
	}

	head, err := io.ReadAll(io.LimitReader(pr.Out.Body, maxRewriteBody+1))
	if err != nil {
		// Can't trust a half-read body; fail the request instead of
		// forwarding garbage. (Transport will surface the read error.)
		pr.Out.Body = io.NopCloser(errReader{err})
		return false
	}
	if int64(len(head)) > maxRewriteBody {
		// Oversized: forward buffered head + the unread remainder verbatim.
		rest := pr.Out.Body
		pr.Out.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), rest), rest}
		return false
	}
	pr.Out.Body.Close()

	// Top-level probe: model（记账）+ stream（超时布防）。解析失败按原样转发。
	var top struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(head, &top); err == nil {
		parsed = true
		d.stream = top.Stream
		d.reqModel = top.Model
	}

	patched := head
	if len(redirects) > 0 {
		if p, from, to, ok, perr := patchModel(head, redirects); perr == nil && ok {
			patched = p
			d.reqModel, d.redirModel = from, to
		}
	}
	pr.Out.Body = io.NopCloser(bytes.NewReader(patched))
	pr.Out.ContentLength = int64(len(patched))                      // Transport writes CL from this field
	pr.Out.Header.Set("Content-Length", strconv.Itoa(len(patched))) // cosmetic/consistency
	pr.Out.TransferEncoding = nil                                   // normalize inbound chunked → fixed length
	pr.Out.GetBody = func() (io.ReadCloser, error) {                // enables safe replay when permitted
		return io.NopCloser(bytes.NewReader(patched)), nil
	}
	return parsed
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
