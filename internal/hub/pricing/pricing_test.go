package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Code-kike/switchAPI/internal/hub/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEnsureLoadedIdempotentAndSnapshotContents(t *testing.T) {
	st := openStore(t)
	if err := EnsureLoaded(st); err != nil {
		t.Fatalf("EnsureLoaded: %v", err)
	}
	n, _ := st.CountPricingBase()
	if n < 100 {
		t.Fatalf("snapshot import loaded only %d models", n)
	}

	// Second call is a no-op (rows already present) — count unchanged.
	if err := EnsureLoaded(st); err != nil {
		t.Fatal(err)
	}
	n2, _ := st.CountPricingBase()
	if n2 != n {
		t.Fatalf("EnsureLoaded not idempotent: %d → %d", n, n2)
	}

	// Mainstream claude/gpt prices present with sane values.
	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	haiku, ok := r.Resolve("claude-haiku-4-5-20251001")
	if !ok || haiku.Input != 1e-6 || haiku.Output != 5e-6 || haiku.CacheRead != 1e-7 {
		t.Fatalf("haiku exact-hit wrong: %+v ok=%v", haiku, ok)
	}
	if _, ok := r.Resolve("gpt-5.1-codex"); !ok {
		t.Fatal("gpt-5.1-codex should be in snapshot")
	}
}

func TestFourStepMatching(t *testing.T) {
	st := openStore(t)
	i, o, cr := 3e-6, 1.5e-5, 3e-7
	if err := st.UpsertPricingBase([]store.PricingBaseEntry{
		{Model: "claude-sonnet-4-5", InputCost: &i, OutputCost: &o, CacheReadCost: &cr,
			LitellmProvider: "anthropic", Mode: "chat", Source: "snapshot"},
		{Model: "glm-5.1", InputCost: &i, OutputCost: &o, LitellmProvider: "zai", Mode: "chat"},
	}); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: exact.
	if _, ok := r.Resolve("claude-sonnet-4-5"); !ok {
		t.Fatal("exact match failed")
	}
	// Step 2: strip -YYYYMMDD.
	if p, ok := r.Resolve("claude-sonnet-4-5-20250929"); !ok || p.Input != i {
		t.Fatalf("date-strip match failed: %+v ok=%v", p, ok)
	}
	// Step 3: lowercase last path segment.
	if _, ok := r.Resolve("vendor/GLM-5.1"); !ok {
		t.Fatal("path-segment match failed")
	}
	// Step 4: real-world unknown (中转站 GLM-5.2, no table key).
	if _, ok := r.Resolve("ZhipuAI/GLM-5.2"); ok {
		t.Fatal("ZhipuAI/GLM-5.2 must fall through to unknown")
	}
}

func TestCostThreeLayerPrecedence(t *testing.T) {
	st := openStore(t)
	i, o, cr := 3e-6, 1.5e-5, 3e-7
	if err := st.UpsertPricingBase([]store.PricingBaseEntry{
		{Model: "m1", InputCost: &i, OutputCost: &o, CacheReadCost: &cr,
			LitellmProvider: "anthropic", Mode: "chat"},
	}); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	u := Usage{Model: "m1", InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200}

	// Layer 2: base × coefficient=1.
	usd, known := r.Cost(u, 1.0, nil)
	want := 1000*i + 500*o + 200*cr
	if !known || usd != want {
		t.Fatalf("base cost = %v (want %v) known=%v", usd, want, known)
	}
	// Coefficient scales the base.
	usd, _ = r.Cost(u, 0.5, nil)
	if usd != want*0.5 {
		t.Fatalf("coeff cost = %v, want %v", usd, want*0.5)
	}
	// Layer 1: override used verbatim — bypasses the coefficient entirely.
	ovr := &Prices{Input: 1e-3, Output: 2e-3, CacheRead: 5e-4}
	usd, known = r.Cost(u, 0.5, ovr)
	wantOvr := 1000*1e-3 + 500*2e-3 + 200*5e-4
	if !known || usd != wantOvr {
		t.Fatalf("override cost = %v (want %v, coeff ignored)", usd, wantOvr)
	}
	// Unknown model → (0, false).
	if usd, known := r.Cost(Usage{Model: "nope", InputTokens: 100}, 1.0, nil); known || usd != 0 {
		t.Fatalf("unknown cost = %v known=%v, want 0/false", usd, known)
	}
}

func TestCostNullComponentsCountZero(t *testing.T) {
	st := openStore(t)
	// OpenAI-shaped entry: no cache_write price (NULL).
	i, o, cr := 1.25e-6, 1e-5, 1.25e-7
	if err := st.UpsertPricingBase([]store.PricingBaseEntry{
		{Model: "gpt-5.1-codex", InputCost: &i, OutputCost: &o, CacheReadCost: &cr,
			LitellmProvider: "openai", Mode: "responses"},
	}); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	// cache_write tokens present but priced at NULL→0: they add nothing.
	u := Usage{Model: "gpt-5.1-codex", InputTokens: 100, OutputTokens: 50,
		CacheWriteTokens: 999, CacheReadTokens: 30}
	usd, known := r.Cost(u, 1.0, nil)
	want := 100*i + 50*o + 30*cr // cache_write contributes 0
	if !known || usd != want {
		t.Fatalf("null-component cost = %v, want %v", usd, want)
	}
}

func TestResolverOverrideLookupAndReload(t *testing.T) {
	st := openStore(t)
	if err := st.CreateProvider(store.Provider{ID: "p1", Name: "p1", Protocol: "anthropic",
		BaseURL: "https://x", APIKeyEnc: []byte{1}, ModelRedirects: "{}"}); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Override("p1", "m1"); ok {
		t.Fatal("no override should exist yet")
	}
	// Add an override in the DB then Reload.
	if _, err := st.DB().Exec(`INSERT INTO pricing_overrides
		(provider_id, model, input_cost, output_cost) VALUES (?,?,?,?)`,
		"p1", "m1", 7e-6, 7e-5); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Override("p1", "m1"); ok {
		t.Fatal("override visible before reload")
	}
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	p, ok := r.Override("p1", "m1")
	if !ok || p.Input != 7e-6 {
		t.Fatalf("override after reload: %+v ok=%v", p, ok)
	}
}

func TestSyncDailyConditionalAndToggle(t *testing.T) {
	st := openStore(t)
	const etag = `"abc123"`
	table := `{"_meta":{"x":1},"sample_spec":{"mode":"chat"},
		"claude-x":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"litellm_provider":"anthropic","mode":"chat"},
		"embed-y":{"input_cost_per_token":5e-7,"litellm_provider":"openai","mode":"embedding"}}`

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits++
		if req.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(table))
	}))
	defer srv.Close()

	r, err := NewResolver(st)
	if err != nil {
		t.Fatal(err)
	}

	// First sync: 200 → upserts only the chat model (embedding filtered out).
	if err := syncOnce(context.Background(), st, srv.URL, r); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if n, _ := st.CountPricingBase(); n != 1 {
		t.Fatalf("after sync count = %d, want 1 (mode filter)", n)
	}
	if _, ok := r.Resolve("claude-x"); !ok {
		t.Fatal("synced model not visible after reload")
	}
	if v, _, _ := st.GetSetting(etagSettingKey); v != etag {
		t.Fatalf("etag not saved: %q", v)
	}

	// Second sync: stored ETag → server replies 304 → no change.
	if err := syncOnce(context.Background(), st, srv.URL, r); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n, _ := st.CountPricingBase(); n != 1 {
		t.Fatalf("304 sync changed rows: %d", n)
	}

	// Disable the toggle → no fetch at all.
	hitsBefore := hits
	st.SetSetting(enabledSettingKey, "0")
	if err := syncOnce(context.Background(), st, srv.URL, r); err != nil {
		t.Fatalf("disabled sync: %v", err)
	}
	if hits != hitsBefore {
		t.Fatalf("disabled sync still fetched: %d → %d", hitsBefore, hits)
	}
}
