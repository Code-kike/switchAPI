package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func TestStaticAndSPAFallback(t *testing.T) {
	h := Handler()

	// 根路径与显式 index.html 都是占位/构建产物首页。
	resp, body := get(t, h, "/")
	if resp.StatusCode != 200 || !strings.Contains(body, "switchAPI") {
		t.Fatalf("GET / = %d: %.80s", resp.StatusCode, body)
	}

	// 未知路径 = SPA 客户端路由 → 返回 index 内容且不缓存。
	resp, fallback := get(t, h, "/providers")
	if resp.StatusCode != 200 || fallback != body {
		t.Fatalf("SPA fallback broken: %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("fallback Cache-Control = %q", cc)
	}

	// 深层未知路径同样回退。
	if resp, _ := get(t, h, "/a/b/c?x=1"); resp.StatusCode != 200 {
		t.Fatalf("deep fallback = %d", resp.StatusCode)
	}
}

// TestMountDoesNotShadowAPI mirrors the cmd/hub root mux layout: the "/"
// catch-all must lose to the more specific /api/ and /healthz patterns.
func TestMountDoesNotShadowAPI(t *testing.T) {
	root := http.NewServeMux()
	root.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("api"))
	})
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	root.Handle("/", Handler())

	if _, body := get(t, root, "/api/v1/providers"); body != "api" {
		t.Fatalf("/api shadowed by webui: %q", body)
	}
	if _, body := get(t, root, "/healthz"); body != "ok" {
		t.Fatalf("/healthz shadowed by webui: %q", body)
	}
	if _, body := get(t, root, "/devices"); !strings.Contains(body, "switchAPI") {
		t.Fatalf("SPA route not served under root mux: %.80s", body)
	}
}
