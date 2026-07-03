package forward

// proxy.go — the shared httputil.ReverseProxy. Per-request routing decisions
// arrive via the request context (set in Server.ServeHTTP); everything here
// mirrors the settings validated by research/06 against a real relay.

import (
	"bytes"
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
	maxSSELine     = 1 << 20  // bound for a single SSE line kept by the usage parser
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
	// ResponseHeaderTimeout deliberately 0: a NON-streaming generation sends
	// headers only after full completion (can take minutes).
	// TODO(M4): make this conditional — 60s for requests detected as
	// streaming (headers arrive immediately on SSE), per research/08.
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
			rewriteModel(pr, d.up.ModelRedirects)
		},
		ModifyResponse: func(res *http.Response) error {
			if sink == nil || !shouldParseSSE(res) {
				return nil
			}
			d := decisionFrom(res.Request.Context())
			if d == nil || d.up == nil || d.up.Protocol != "anthropic" {
				return nil // openai (Responses) parsing lands in M2
			}
			res.Body = newUsageTee(res.Body, res.StatusCode, d.up.ProviderID, sink)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Transport-level failure (dial/TLS/context). Client disconnects
			// mid-stream do NOT land here; they abort via http.ErrAbortHandler.
			log.Printf("forward: upstream error: %v", err)
			writeErr(w, http.StatusBadGateway, "上游请求失败："+err.Error())
		},
	}
}

func shouldParseSSE(res *http.Response) bool {
	return strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream")
}

// rewriteModel buffers the request body ONLY when a redirect table exists and
// the body is a plain JSON candidate; otherwise the body streams through
// untouched (zero copy).
func rewriteModel(pr *httputil.ProxyRequest, redirects map[string]string) {
	if len(redirects) == 0 || pr.Out.Body == nil {
		return
	}
	// Never touch compressed or non-JSON bodies.
	if pr.In.Header.Get("Content-Encoding") != "" {
		return
	}
	if ct := pr.In.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return
	}

	head, err := io.ReadAll(io.LimitReader(pr.Out.Body, maxRewriteBody+1))
	if err != nil {
		// Can't trust a half-read body; fail the request instead of
		// forwarding garbage. (Transport will surface the read error.)
		pr.Out.Body = io.NopCloser(errReader{err})
		return
	}
	if int64(len(head)) > maxRewriteBody {
		// Oversized: forward buffered head + the unread remainder verbatim.
		rest := pr.Out.Body
		pr.Out.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), rest), rest}
		return
	}
	pr.Out.Body.Close()

	patched, _, _, ok, perr := patchModel(head, redirects)
	if perr != nil || !ok {
		patched = head // not JSON / no match → forward original bytes
	}
	pr.Out.Body = io.NopCloser(bytes.NewReader(patched))
	pr.Out.ContentLength = int64(len(patched))                      // Transport writes CL from this field
	pr.Out.Header.Set("Content-Length", strconv.Itoa(len(patched))) // cosmetic/consistency
	pr.Out.TransferEncoding = nil                                   // normalize inbound chunked → fixed length
	pr.Out.GetBody = func() (io.ReadCloser, error) {                // enables safe replay when permitted
		return io.NopCloser(bytes.NewReader(patched)), nil
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
