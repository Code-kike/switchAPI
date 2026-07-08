package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

func TestAnthropicProbeShapeAndAuth(t *testing.T) {
	var gotPath, gotKey, gotBearer string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		gotBearer = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	res := Run(context.Background(), wire.ProbeTarget{
		ProviderID: "p1", Protocol: "anthropic", BaseURL: srv.URL, APIKey: "sk-x", Model: "m1",
	}, time.Second)
	if !res.OK || res.Status != 200 || res.ProviderID != "p1" {
		t.Fatalf("res = %+v", res)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-x" || gotBearer != "Bearer sk-x" {
		t.Fatalf("request shape: path=%s key=%s bearer=%s", gotPath, gotKey, gotBearer)
	}
	if gotBody["model"] != "m1" || gotBody["max_tokens"] != float64(1) {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestOpenAIProbeAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(503)
	}))
	t.Cleanup(srv.Close)

	res := Run(context.Background(), wire.ProbeTarget{
		ProviderID: "p2", Protocol: "openai", BaseURL: srv.URL + "/v1", APIKey: "sk", Model: "m",
	}, time.Second)
	if res.OK || res.Status != 503 || res.Error == "" {
		t.Fatalf("res = %+v", res)
	}
}

func TestProbeTimeoutAndConnectError(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)
	res := Run(context.Background(), wire.ProbeTarget{
		ProviderID: "p", Protocol: "anthropic", BaseURL: hang.URL, Model: "m",
	}, 100*time.Millisecond)
	if res.OK || res.Error == "" {
		t.Fatalf("timeout not reported: %+v", res)
	}

	res = Run(context.Background(), wire.ProbeTarget{
		ProviderID: "p", Protocol: "anthropic", BaseURL: "http://127.0.0.1:1", Model: "m",
	}, time.Second)
	if res.OK || res.Error == "" {
		t.Fatalf("connect error not reported: %+v", res)
	}
}
