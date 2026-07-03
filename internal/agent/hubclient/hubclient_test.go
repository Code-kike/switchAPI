package hubclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Code-kike/switchAPI/internal/shared/wire"
)

func TestStateSaveLoadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agent-state.json")
	st := &State{
		HubURL: "http://hub:8080", DeviceID: "d1",
		DeviceToken: "devtok", LocalToken: "loctok",
		LastPush: &wire.ConfigPush{Rev: 7, Apps: map[string]wire.AppRoute{
			"claude-code": {ProviderID: "p1", Protocol: "anthropic",
				BaseURL: "https://relay.example", APIKey: "sk-x"},
		}},
	}
	if err := SaveState(path, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600（含密钥，ADR-0005）", info.Mode().Perm())
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.DeviceID != "d1" || got.LocalToken != "loctok" ||
		got.LastPush == nil || got.LastPush.Rev != 7 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.SavedAt == 0 {
		t.Fatal("SavedAt not stamped")
	}
}

func TestBuildTable(t *testing.T) {
	push := &wire.ConfigPush{
		Rev: 3,
		Apps: map[string]wire.AppRoute{
			"claude-code": {ProviderID: "pa", Protocol: "anthropic",
				BaseURL: "https://a.example", APIKey: "ka",
				ModelRedirects: map[string]string{"x": "y"}},
			"codex": {ProviderID: "po", Protocol: "openai",
				BaseURL: "https://o.example/v1", APIKey: "ko"},
		},
	}
	tbl := BuildTable(push)
	if tbl.Rev != 3 {
		t.Fatalf("rev = %d", tbl.Rev)
	}
	if tbl.Anthropic == nil || tbl.Anthropic.ProviderID != "pa" ||
		tbl.Anthropic.BaseURL.Host != "a.example" || tbl.Anthropic.ModelRedirects["x"] != "y" {
		t.Fatalf("anthropic upstream: %+v", tbl.Anthropic)
	}
	if tbl.OpenAI == nil || tbl.OpenAI.BaseURL.Path != "/v1" {
		t.Fatalf("openai upstream: %+v", tbl.OpenAI)
	}

	// 非法 base_url 与未知协议：跳过而非崩溃。
	bad := &wire.ConfigPush{Apps: map[string]wire.AppRoute{
		"claude-code": {Protocol: "anthropic", BaseURL: "::::not-a-url"},
		"codex":       {Protocol: "grpc", BaseURL: "https://x"},
	}}
	tbl = BuildTable(bad)
	if tbl.Anthropic != nil || tbl.OpenAI != nil {
		t.Fatalf("invalid entries not skipped: %+v", tbl)
	}
	// nil push → 空表。
	if tbl := BuildTable(nil); tbl.Anthropic != nil || tbl.OpenAI != nil {
		t.Fatal("nil push not handled")
	}
}

func TestBackoffProgression(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 0; attempt <= 10; attempt++ {
		d := Backoff(attempt)
		if d < 500*time.Millisecond || d > 80*time.Second {
			t.Fatalf("attempt %d: %v out of sane range", attempt, d)
		}
		if attempt <= 5 && d < prev/4 {
			t.Fatalf("attempt %d: %v not roughly increasing (prev %v)", attempt, d, prev)
		}
		prev = d
	}
	// 封顶：大 attempt 不超过 60s×1.2。
	for i := 0; i < 20; i++ {
		if d := Backoff(50); d > 72*time.Second {
			t.Fatalf("cap exceeded: %v", d)
		}
	}
}
