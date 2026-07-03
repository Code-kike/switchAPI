package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func testOpts(t *testing.T, env map[string]string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		ListenAddr:         "127.0.0.1:9527",
		LocalToken:         "tok-0123456789abcdef",
		ClaudeSettingsPath: filepath.Join(dir, "claude", "settings.json"),
		CodexConfigPath:    filepath.Join(dir, "codex", "config.toml"),
		CodexAuthPath:      filepath.Join(dir, "codex", "auth.json"),
		Env:                func(k string) string { return env[k] },
	}
}

const fixtureSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "claude-fable-5",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "echo hi"}]}]},
  "env": {
    "ANTHROPIC_BASE_URL": "https://old-relay.example",
    "ANTHROPIC_AUTH_TOKEN": "sk-old-token",
    "DISABLE_TELEMETRY": "1"
  }
}`

func TestClaudeSurgicalMerge(t *testing.T) {
	opts := testOpts(t, nil)
	os.MkdirAll(filepath.Dir(opts.ClaudeSettingsPath), 0o755)
	os.WriteFile(opts.ClaudeSettingsPath, []byte(fixtureSettings), 0o644)

	changes, _, err := Apply(opts, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("no changes planned")
	}

	raw, _ := os.ReadFile(opts.ClaudeSettingsPath)
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	env := root["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9527/anthropic" ||
		env["ANTHROPIC_AUTH_TOKEN"] != opts.LocalToken {
		t.Fatalf("env not taken over: %v", env)
	}
	// 无关键全部保留（手术式合并，研究/01 C6）。
	if env["DISABLE_TELEMETRY"] != "1" {
		t.Fatal("unrelated env key lost")
	}
	for _, k := range []string{"$schema", "model", "permissions", "hooks"} {
		if _, ok := root[k]; !ok {
			t.Fatalf("top-level key %q lost", k)
		}
	}
	// 备份存在且内容 = 原文件。
	bak, err := newestBackup(opts.ClaudeSettingsPath)
	if err != nil || bak == "" {
		t.Fatalf("no backup: %v", err)
	}
	if b, _ := os.ReadFile(bak); string(b) != fixtureSettings {
		t.Fatal("backup differs from original")
	}

	// 幂等：再 apply 无变更、不再新增备份。
	changes2, _, err := Apply(opts, false)
	if err != nil || len(changes2) != 0 {
		t.Fatalf("second apply not idempotent: %v %v", changes2, err)
	}
}

func TestClaudeMissingFileCreated(t *testing.T) {
	opts := testOpts(t, nil)
	if _, _, err := Apply(opts, false); err != nil {
		t.Fatalf("apply on missing file: %v", err)
	}
	raw, err := os.ReadFile(opts.ClaudeSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://127.0.0.1:9527/anthropic") {
		t.Fatalf("minimal settings not created: %s", raw)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	opts := testOpts(t, nil)
	os.MkdirAll(filepath.Dir(opts.ClaudeSettingsPath), 0o755)
	os.WriteFile(opts.ClaudeSettingsPath, []byte(fixtureSettings), 0o644)

	changes, _, err := Apply(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("dry-run planned nothing")
	}
	// diff 不得泄露完整 token。
	for _, c := range changes {
		for _, l := range c.Lines {
			if strings.Contains(l, opts.LocalToken) {
				t.Fatalf("diff leaks full token: %s", l)
			}
		}
	}
	raw, _ := os.ReadFile(opts.ClaudeSettingsPath)
	if string(raw) != fixtureSettings {
		t.Fatal("dry-run modified the file")
	}
	if bak, _ := newestBackup(opts.ClaudeSettingsPath); bak != "" {
		t.Fatal("dry-run created a backup")
	}
	if _, err := os.Stat(opts.CodexConfigPath); !os.IsNotExist(err) {
		t.Fatal("dry-run touched codex config")
	}
}

func TestCodexTakeoverPreservesOtherTables(t *testing.T) {
	opts := testOpts(t, nil)
	os.MkdirAll(filepath.Dir(opts.CodexConfigPath), 0o755)
	os.WriteFile(opts.CodexConfigPath, []byte(`
model = "gpt-5.2"
model_provider = "anyrouter"
disable_response_storage = true

[model_providers.anyrouter]
name = "AnyRouter"
base_url = "https://anyrouter.example/v1"
wire_api = "responses"

[profiles.fast]
model = "gpt-5.2-mini"
`), 0o644)
	os.WriteFile(opts.CodexAuthPath, []byte(`{"OPENAI_API_KEY":"sk-site-real","tokens":{"access_token":"at"}}`), 0o600)

	if _, _, err := Apply(opts, false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	raw, _ := os.ReadFile(opts.CodexConfigPath)
	var cfg map[string]any
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("output not TOML: %v", err)
	}
	if cfg["model_provider"] != "switchapi" {
		t.Fatalf("model_provider = %v", cfg["model_provider"])
	}
	mp := cfg["model_providers"].(map[string]any)
	sw := mp["switchapi"].(map[string]any)
	if sw["base_url"] != "http://127.0.0.1:9527/openai/v1" || sw["wire_api"] != "responses" {
		t.Fatalf("switchapi block wrong: %v", sw)
	}
	// 用户原有内容保留。
	if _, ok := mp["anyrouter"]; !ok {
		t.Fatal("existing provider table lost")
	}
	if cfg["model"] != "gpt-5.2" || cfg["disable_response_storage"] != true {
		t.Fatal("top-level keys lost")
	}
	if _, ok := cfg["profiles"]; !ok {
		t.Fatal("profiles table lost")
	}

	// auth.json：token 写入、其余键保留、0600、有备份。
	rawAuth, _ := os.ReadFile(opts.CodexAuthPath)
	var auth map[string]any
	json.Unmarshal(rawAuth, &auth)
	if auth["OPENAI_API_KEY"] != opts.LocalToken {
		t.Fatalf("auth token not written: %v", auth)
	}
	if _, ok := auth["tokens"]; !ok {
		t.Fatal("OAuth tokens block lost")
	}
	if info, _ := os.Stat(opts.CodexAuthPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode = %v", info.Mode().Perm())
	}
	if bak, _ := newestBackup(opts.CodexAuthPath); bak == "" {
		t.Fatal("auth.json not backed up")
	}
}

func TestConflictChecklist(t *testing.T) {
	// Bedrock → 拒绝接管。
	opts := testOpts(t, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"})
	if _, _, err := Plan(opts); err == nil {
		t.Fatal("bedrock mode not refused")
	}
	// shell 导出 + 代理缺 NO_PROXY → 仅警告。
	opts = testOpts(t, map[string]string{
		"ANTHROPIC_BASE_URL": "https://x", "HTTPS_PROXY": "http://proxy:7890"})
	_, warnings, err := Plan(opts)
	if err != nil {
		t.Fatalf("warn-class treated as refusal: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL") || !strings.Contains(joined, "NO_PROXY") {
		t.Fatalf("warnings incomplete: %v", warnings)
	}
	// apiKeyHelper → 警告。
	opts = testOpts(t, nil)
	os.MkdirAll(filepath.Dir(opts.ClaudeSettingsPath), 0o755)
	os.WriteFile(opts.ClaudeSettingsPath, []byte(`{"apiKeyHelper":"/bin/helper.sh","env":{}}`), 0o644)
	_, warnings, err = Plan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "apiKeyHelper") {
		t.Fatalf("apiKeyHelper warning missing: %v", warnings)
	}
}

func TestRollbackRestoresOriginals(t *testing.T) {
	opts := testOpts(t, nil)
	os.MkdirAll(filepath.Dir(opts.ClaudeSettingsPath), 0o755)
	os.WriteFile(opts.ClaudeSettingsPath, []byte(fixtureSettings), 0o644)

	if _, _, err := Apply(opts, false); err != nil {
		t.Fatal(err)
	}
	restored, err := Rollback(opts)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(restored) == 0 {
		t.Fatal("nothing restored")
	}
	raw, _ := os.ReadFile(opts.ClaudeSettingsPath)
	if string(raw) != fixtureSettings {
		t.Fatal("rollback did not restore original content")
	}
}
