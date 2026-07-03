package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()
	s2, err := Open(path) // second run must be a no-op, not a re-apply
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
		t.Fatalf("user_version = %d, err=%v, want 1", v, err)
	}
}

func TestProviderCRUD(t *testing.T) {
	s := openTest(t)
	p := Provider{
		ID: "p1", Name: "AnyRouter", Protocol: "anthropic",
		BaseURL: "https://anyrouter.top", APIKeyEnc: []byte{1, 2, 3},
		ModelRedirects: `{"a":"b"}`, CostCoefficient: 0.1, Sort: 2, Note: "测试",
	}
	if err := s.CreateProvider(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetProvider("p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != p.Name || got.Protocol != p.Protocol || string(got.APIKeyEnc) != string(p.APIKeyEnc) ||
		got.ModelRedirects != p.ModelRedirects || got.CostCoefficient != p.CostCoefficient {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Update without key keeps ciphertext.
	got.Name = "改名"
	got.APIKeyEnc = nil
	if err := s.UpdateProvider(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := s.GetProvider("p1")
	if again.Name != "改名" || string(again.APIKeyEnc) != string(p.APIKeyEnc) {
		t.Fatalf("update lost key or name: %+v", again)
	}

	// Update with key replaces ciphertext.
	again.APIKeyEnc = []byte{9}
	if err := s.UpdateProvider(again); err != nil {
		t.Fatalf("update2: %v", err)
	}
	final, _ := s.GetProvider("p1")
	if string(final.APIKeyEnc) != string([]byte{9}) {
		t.Fatalf("key not replaced")
	}

	list, err := s.ListProviders()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if err := s.DeleteProvider("p1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetProvider("p1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.DeleteProvider("p1"); err != ErrNotFound {
		t.Fatalf("double delete want ErrNotFound, got %v", err)
	}
}

func TestAppStateAndFallback(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"p1", "p2"} {
		if err := s.CreateProvider(Provider{ID: id, Name: id, Protocol: "anthropic",
			BaseURL: "https://x", APIKeyEnc: []byte{1}, ModelRedirects: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetAppState("claude-code"); err != ErrNotFound {
		t.Fatalf("empty state want ErrNotFound, got %v", err)
	}
	if err := s.SetAppState("claude-code", "p1", "test"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetAppState("claude-code", "p2", "test"); err != nil {
		t.Fatalf("set2 (upsert): %v", err)
	}
	st, err := s.GetAppState("claude-code")
	if err != nil || st.ActiveProviderID != "p2" {
		t.Fatalf("state = %+v err=%v", st, err)
	}

	// Foreign key: switching to a ghost provider must fail.
	if err := s.SetAppState("codex", "ghost", "test"); err == nil {
		t.Fatal("ghost provider accepted — foreign_keys pragma off?")
	}

	if err := s.SetFallbackOrder("claude-code", []string{"p2", "p1"}); err != nil {
		t.Fatalf("fallback set: %v", err)
	}
	order, err := s.GetFallbackOrder("claude-code")
	if err != nil || len(order) != 2 || order[0] != "p2" || order[1] != "p1" {
		t.Fatalf("order = %v err=%v", order, err)
	}
	// Replace wholesale.
	if err := s.SetFallbackOrder("claude-code", []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	order, _ = s.GetFallbackOrder("claude-code")
	if len(order) != 1 || order[0] != "p1" {
		t.Fatalf("order after replace = %v", order)
	}
}

func TestDevices(t *testing.T) {
	s := openTest(t)
	d := Device{ID: "d1", Name: "dev-machine", Platform: "linux", TokenHash: "abc"}
	if err := s.CreateDevice(d); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.FindDeviceByTokenHash("abc")
	if err != nil || got.ID != "d1" {
		t.Fatalf("find: %+v err=%v", got, err)
	}
	if err := s.TouchDevice("d1"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListDevices()
	if len(list) != 1 || list[0].LastSeen == 0 {
		t.Fatalf("touch not visible: %+v", list)
	}
	if err := s.RevokeDevice("d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindDeviceByTokenHash("abc"); err != ErrNotFound {
		t.Fatalf("revoked token still resolves: %v", err)
	}
}

func TestEventsAndSettings(t *testing.T) {
	s := openTest(t)
	if err := s.AppendEvent("switch", `{"app":"claude-code"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent("pairing", ""); err != nil {
		t.Fatal(err)
	}
	evs, err := s.RecentEvents(10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %v len=%d", err, len(evs))
	}
	if evs[0].Kind != "pairing" { // newest first
		t.Fatalf("order wrong: %+v", evs)
	}
	if evs[0].Payload != "{}" {
		t.Fatalf("empty payload not defaulted: %q", evs[0].Payload)
	}

	if _, ok, _ := s.GetSetting("nope"); ok {
		t.Fatal("unset key reported found")
	}
	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetSetting("k")
	if err != nil || !ok || v != "v2" {
		t.Fatalf("setting = %q ok=%v err=%v", v, ok, err)
	}
}
