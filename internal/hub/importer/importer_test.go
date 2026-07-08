package importer

// fixture 形态取自研究/05 C5 的本机 ground truth：多账号同站、openai_chat
// 转换类、localhost 回环、空 key 占位、OAuth 托管、TOML 混杂无关段。

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func claudeCfg(baseURL, key string, extraEnv map[string]string) string {
	env := map[string]string{"ANTHROPIC_BASE_URL": baseURL, "ANTHROPIC_AUTH_TOKEN": key}
	for k, v := range extraEnv {
		env[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"env": env, "hooks": map[string]any{"x": 1}, "permissions": map[string]any{},
	})
	return string(b)
}

func codexCfg(mp, baseURL, key string) string {
	tomlText := "model = \"gpt-5\"\nmodel_provider = \"" + mp + "\"\n" +
		"[model_providers." + mp + "]\nbase_url = \"" + baseURL + "\"\nwire_api = \"responses\"\n" +
		"[mcp_servers.foo]\ncommand = \"npx\"\nenv = { TOKEN = \"third-party-secret\" }\n" +
		"[projects.\"/home/x\"]\ntrust_level = \"trusted\"\n"
	b, _ := json.Marshal(map[string]any{
		"auth": map[string]string{"OPENAI_API_KEY": key}, "config": tomlText,
	})
	return string(b)
}

func buildFixtureDB(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 列集 = 上游 v3.16.5 schema（研究/05 C2）；importer 必须按列名取值。
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, website_url TEXT, category TEXT,
		created_at INTEGER, sort_index INTEGER, notes TEXT, icon TEXT, icon_color TEXT,
		meta TEXT NOT NULL DEFAULT '{}', is_current BOOLEAN NOT NULL DEFAULT 0,
		in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
		cost_multiplier TEXT NOT NULL DEFAULT '1.0',
		limit_daily_usd TEXT, limit_monthly_usd TEXT, provider_type TEXT,
		PRIMARY KEY (id, app_type))`); err != nil {
		t.Fatal(err)
	}
	ins := func(id, app, name, cfg, meta string, sortIdx, cur, queue int, costCol string) {
		if _, err := db.Exec(`INSERT INTO providers
			(id, app_type, name, settings_config, meta, sort_index, is_current, in_failover_queue, cost_multiplier, notes)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, app, name, cfg, meta, sortIdx, cur, queue, costCol, "旧备注"); err != nil {
			t.Fatal(err)
		}
	}
	// 多账号同站（合法，逐条导入；#2 是 current + 在队列）。
	ins("c1", "claude", "anyrouter 主号", claudeCfg("https://anyrouter.top", "sk-a-1", nil),
		`{"costMultiplier":"0.1"}`, 1, 0, 1, "1.0")
	ins("c2", "claude", "anyrouter 备号",
		claudeCfg("https://anyrouter.top/", "sk-a-2", map[string]string{"ANTHROPIC_MODEL": "GLM-5"}),
		`{}`, 2, 1, 1, "0.5")
	// E1：依赖协议转换。
	ins("c3", "claude", "Nvidia", claudeCfg("https://integrate.api.nvidia.com", "sk-nv", nil),
		`{"apiFormat":"openai_chat"}`, 3, 0, 0, "1.0")
	// E4：空 key 占位。
	ins("c4", "claude", "预设占位", claudeCfg("https://empty.example", "", nil), `{}`, 4, 0, 0, "1.0")
	// codex 正常（TOML 混杂 mcp/projects 段）。
	ins("x1", "codex", "relay-openai", codexCfg("anyrouter", "https://relay.example/v1", "sk-o-1"),
		`{}`, 1, 1, 1, "1.0")
	// E3：回环。
	ins("x2", "codex", "local any", codexCfg("local", "http://127.0.0.1:23000/v1", "sk-l"),
		`{}`, 2, 0, 1, "1.0")
	// E2：OAuth 托管。
	ins("x3", "codex", "chatgpt 官方", `{"auth":{},"config":""}`,
		`{"providerType":"codex_oauth"}`, 3, 0, 0, "1.0")

	db.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseDBFixture(t *testing.T) {
	p, err := Parse(buildFixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 3 {
		t.Fatalf("candidates = %d: %+v", len(p.Candidates), p.Candidates)
	}
	if len(p.Skipped) != 4 {
		t.Fatalf("skipped = %d: %+v", len(p.Skipped), p.Skipped)
	}

	byName := map[string]Candidate{}
	for _, c := range p.Candidates {
		byName[c.Name] = c
	}
	a1 := byName["anyrouter 主号"]
	if a1.App != "claude-code" || a1.BaseURL != "https://anyrouter.top" ||
		a1.APIKey != "sk-a-1" || a1.CostCoefficient != 0.1 || !a1.InFailoverQueue {
		t.Fatalf("a1 = %+v", a1)
	}
	a2 := byName["anyrouter 备号"]
	// meta 无 costMultiplier → 列兜底 0.5；模型钉扎进备注不进重定向；尾斜杠剥除。
	if a2.CostCoefficient != 0.5 || !a2.IsCurrent || a2.BaseURL != "https://anyrouter.top" {
		t.Fatalf("a2 = %+v", a2)
	}
	if !contains(a2.Note, "ANTHROPIC_MODEL=GLM-5") || !contains(a2.Note, "imported from cc-switch") {
		t.Fatalf("a2 note = %q", a2.Note)
	}
	x1 := byName["relay-openai"]
	if x1.App != "codex" || x1.BaseURL != "https://relay.example/v1" || x1.APIKey != "sk-o-1" {
		t.Fatalf("x1 = %+v", x1)
	}

	reasons := map[string]string{}
	for _, s := range p.Skipped {
		reasons[s.Name] = s.Reason
	}
	if !contains(reasons["Nvidia"], "E1") || !contains(reasons["预设占位"], "E4") ||
		!contains(reasons["local any"], "E3") || !contains(reasons["chatgpt 官方"], "E2") {
		t.Fatalf("skip reasons = %v", reasons)
	}
}

func TestParseJSONV2AndV1Reject(t *testing.T) {
	v2 := map[string]any{
		"version": 2,
		"claude": map[string]any{
			"current": "p1",
			"providers": map[string]any{
				"p1": map[string]any{
					"name":            "legacy 站",
					"settingsConfig":  json.RawMessage(claudeCfg("https://legacy.example", "sk-legacy", nil)),
					"meta":            json.RawMessage(`{"costMultiplier":"0.2"}`),
					"sortIndex":       1,
					"inFailoverQueue": true,
				},
			},
		},
	}
	raw, _ := json.Marshal(v2)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Name != "legacy 站" ||
		p.Candidates[0].CostCoefficient != 0.2 || !p.Candidates[0].IsCurrent {
		t.Fatalf("v2 parse = %+v", p.Candidates)
	}

	// v1（顶层 providers + current，无 version）→ 明确拒绝。
	v1 := `{"providers":{"a":{"name":"x"}},"current":"a"}`
	if _, err := Parse([]byte(v1)); err == nil || !contains(err.Error(), "v1") {
		t.Fatalf("v1 not rejected: %v", err)
	}

	// 完全无法识别的内容。
	if _, err := Parse([]byte("hello")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
