package api

// export_test.go — 口令加密导出→全新 Hub 导入 roundtrip、明文确认门、
// 错误口令拒绝、CSV 输出、cc-switch 上传导入（fixture 复用 importer 包思路）。

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
)

func TestExportImportRoundtrip(t *testing.T) {
	src := newTestRig(t)
	pA := src.createProvider(t, "站点A", "anthropic", "sk-live-aaaa")
	pB := src.createProvider(t, "站点B", "openai", "sk-live-bbbb")
	src.do(src.auth, "POST", "/api/v1/switch", `{"app":"claude-code","provider_id":"`+pA+`"}`)
	src.do(src.auth, "PUT", "/api/v1/fallback-order/claude-code", `{"provider_ids":["`+pA+`"]}`)

	// 无口令且未确认 → 400（明文导出必须二次确认）。
	if code, _ := src.do(src.auth, "POST", "/api/v1/export", `{}`); code != 400 {
		t.Fatalf("unconfirmed plaintext export = %d, want 400", code)
	}

	code, exported := src.do(src.auth, "POST", "/api/v1/export", `{"passphrase":"口令-pass"}`)
	if code != 200 {
		t.Fatalf("export = %d: %s", code, exported)
	}
	if bytes.Contains(exported, []byte("sk-live-aaaa")) {
		t.Fatal("encrypted export leaks plaintext key")
	}

	// 导入请求体 = 导出 JSON + passphrase 字段。
	withPass := func(pass string) string {
		var m map[string]any
		if err := json.Unmarshal(exported, &m); err != nil {
			t.Fatal(err)
		}
		m["passphrase"] = pass
		b, _ := json.Marshal(m)
		return string(b)
	}

	// 错误口令 → 401。
	dst := newTestRig(t)
	if code, _ := dst.do(dst.auth, "POST", "/api/v1/import", withPass("wrong")); code != 401 {
		t.Fatalf("wrong passphrase import = %d, want 401", code)
	}

	code, body := dst.do(dst.auth, "POST", "/api/v1/import", withPass("口令-pass"))
	if code != 200 {
		t.Fatalf("import = %d: %s", code, body)
	}

	// 还原校验：key 解密一致、app_state、备选序列。
	got, err := dst.st.GetProvider(pA)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := cryptoutil.Open(dst.key, got.APIKeyEnc); err != nil || string(plain) != "sk-live-aaaa" {
		t.Fatalf("restored key mismatch: %v %q", err, plain)
	}
	if _, err := dst.st.GetProvider(pB); err != nil {
		t.Fatal("provider B not restored")
	}
	st, err := dst.st.GetAppState("claude-code")
	if err != nil || st.ActiveProviderID != pA {
		t.Fatalf("app state not restored: %+v %v", st, err)
	}
	order, _ := dst.st.GetFallbackOrder("claude-code")
	if len(order) != 1 || order[0] != pA {
		t.Fatalf("fallback order not restored: %v", order)
	}

	// 明文导出（已确认）路径可用且能导回。
	code, plainExp := src.do(src.auth, "POST", "/api/v1/export", `{"plaintext_confirmed":true}`)
	if code != 200 || !bytes.Contains(plainExp, []byte("sk-live-aaaa")) {
		t.Fatalf("confirmed plaintext export = %d", code)
	}
	dst2 := newTestRig(t)
	if code, _ := dst2.do(dst2.auth, "POST", "/api/v1/import", string(plainExp)); code != 200 {
		t.Fatal("plaintext import failed")
	}
}

func TestUsageCSVExport(t *testing.T) {
	r := newTestRig(t)
	req, _ := http.NewRequest("GET", r.srv.URL+"/api/v1/usage/export.csv", nil)
	resp, err := r.auth.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("csv = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if !strings.HasPrefix(buf.String(), "ts,time,device_id,app,provider_id,model") {
		t.Fatalf("csv header = %.80s", buf.String())
	}
}

func TestImportCCSwitchUpload(t *testing.T) {
	r := newTestRig(t)

	// 构造最小 cc-switch.db fixture（1 可导入 + 1 E1 跳过）。
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT,
		settings_config TEXT, meta TEXT DEFAULT '{}', sort_index INTEGER,
		is_current BOOLEAN DEFAULT 0, in_failover_queue BOOLEAN DEFAULT 0,
		cost_multiplier TEXT DEFAULT '1.0', notes TEXT, provider_type TEXT,
		PRIMARY KEY (id, app_type))`)
	cfg := `{"env":{"ANTHROPIC_BASE_URL":"https://relay.example","ANTHROPIC_AUTH_TOKEN":"sk-cc-1234"}}`
	db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, meta, sort_index, is_current, in_failover_queue)
		VALUES ('a','claude','导入站A',?, '{"costMultiplier":"0.3"}', 1, 1, 1)`, cfg)
	db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, meta, sort_index)
		VALUES ('b','claude','转换站',?, '{"apiFormat":"openai_chat"}', 2)`, cfg)
	db.Close()
	raw, _ := os.ReadFile(dbPath)

	req, _ := http.NewRequest("POST", r.srv.URL+"/api/v1/import/cc-switch", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := r.auth.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Imported []struct{ App, Name string }    `json:"imported"`
		Skipped  []struct{ Name, Reason string } `json:"skipped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("import cc-switch = %d %v", resp.StatusCode, err)
	}
	if len(out.Imported) != 1 || out.Imported[0].Name != "导入站A" {
		t.Fatalf("imported = %+v", out.Imported)
	}
	if len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0].Reason, "E1") {
		t.Fatalf("skipped = %+v", out.Skipped)
	}

	// 落库校验：key 加密、折扣系数、is_current→app_state、队列→备选序列。
	list, _ := r.st.ListProviders()
	if len(list) != 1 || list[0].CostCoefficient != 0.3 {
		t.Fatalf("providers = %+v", list)
	}
	if plain, err := cryptoutil.Open(r.key, list[0].APIKeyEnc); err != nil || string(plain) != "sk-cc-1234" {
		t.Fatal("imported key not sealed correctly")
	}
	st, err := r.st.GetAppState("claude-code")
	if err != nil || st.ActiveProviderID != list[0].ID {
		t.Fatalf("app state = %+v %v", st, err)
	}
	order, _ := r.st.GetFallbackOrder("claude-code")
	if len(order) != 1 || order[0] != list[0].ID {
		t.Fatalf("fallback = %v", order)
	}
}
