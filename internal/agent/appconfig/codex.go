package appconfig

// codex.go — Codex takeover per research/02: a custom provider block in
// config.toml (wire_api="responses" — the only surviving wire) plus the local
// token in auth.json.OPENAI_API_KEY (Codex sends it as Bearer). config.toml
// is re-marshaled through go-toml: comments are not preserved (stated
// limitation; the timestamped backup keeps the original). Codex reads config
// at process start — running sessions need a restart (research/02 C5).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

const codexProviderID = "switchapi"

func loadTOML(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := toml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s 不是合法 TOML: %w", path, err)
	}
	return root, nil
}

func loadAuthJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON: %w", path, err)
	}
	return root, nil
}

func codexProviderBlock(opts Options) map[string]any {
	return map[string]any{
		"name":     "switchAPI",
		"base_url": opts.openaiBase(),
		"wire_api": "responses",
	}
}

func planCodex(opts Options) ([]Change, error) {
	var changes []Change

	root, err := loadTOML(opts.CodexConfigPath)
	if err != nil {
		return nil, err
	}
	cfg := Change{File: opts.CodexConfigPath}
	if cur, _ := root["model_provider"].(string); cur != codexProviderID {
		cfg.Lines = append(cfg.Lines, fmt.Sprintf("model_provider: %s → %s", orUnset(cur), codexProviderID))
	}
	mp, _ := root["model_providers"].(map[string]any)
	want := codexProviderBlock(opts)
	var existing map[string]any
	if mp != nil {
		existing, _ = mp[codexProviderID].(map[string]any)
	}
	if !tomlBlockEqual(existing, want) {
		cfg.Lines = append(cfg.Lines, fmt.Sprintf(
			"model_providers.%s: → {name=%q, base_url=%q, wire_api=%q}",
			codexProviderID, want["name"], want["base_url"], want["wire_api"]))
	}
	if len(cfg.Lines) > 0 {
		cfg.Lines = append(cfg.Lines, "注意：config.toml 将经解析重写，注释不保留（已有时间戳备份）；运行中的 codex 会话需重启后生效")
		changes = append(changes, cfg)
	}

	auth, err := loadAuthJSON(opts.CodexAuthPath)
	if err != nil {
		return nil, err
	}
	authChange := Change{File: opts.CodexAuthPath}
	if cur, _ := auth["OPENAI_API_KEY"].(string); cur != opts.LocalToken {
		authChange.Lines = append(authChange.Lines,
			fmt.Sprintf("OPENAI_API_KEY: %s → %s", displayVal(envAuthToken, cur), displayVal(envAuthToken, opts.LocalToken)))
		if _, hasTokens := auth["tokens"]; hasTokens {
			authChange.Lines = append(authChange.Lines,
				"警告：auth.json 含 ChatGPT OAuth tokens，接管会顶掉登录态（备份可回滚）")
		}
		changes = append(changes, authChange)
	}
	return changes, nil
}

func applyCodex(opts Options) error {
	root, err := loadTOML(opts.CodexConfigPath)
	if err != nil {
		return err
	}
	dirty := false
	if cur, _ := root["model_provider"].(string); cur != codexProviderID {
		root["model_provider"] = codexProviderID
		dirty = true
	}
	mp, ok := root["model_providers"].(map[string]any)
	if !ok {
		mp = map[string]any{}
		root["model_providers"] = mp
	}
	want := codexProviderBlock(opts)
	if existing, _ := mp[codexProviderID].(map[string]any); !tomlBlockEqual(existing, want) {
		mp[codexProviderID] = want
		dirty = true
	}
	if dirty {
		raw, err := toml.Marshal(root)
		if err != nil {
			return err
		}
		if err := backupThenWrite(opts.CodexConfigPath, raw, 0o644); err != nil {
			return err
		}
	}

	auth, err := loadAuthJSON(opts.CodexAuthPath)
	if err != nil {
		return err
	}
	if cur, _ := auth["OPENAI_API_KEY"].(string); cur != opts.LocalToken {
		auth["OPENAI_API_KEY"] = opts.LocalToken
		raw, err := json.MarshalIndent(auth, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		// auth.json 承载凭据，无论新旧一律 0600。
		if err := backupThenWrite(opts.CodexAuthPath, raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func tomlBlockEqual(got, want map[string]any) bool {
	if got == nil {
		return false
	}
	for k, v := range want {
		if gv, _ := got[k].(string); gv != v.(string) {
			return false
		}
	}
	return true
}

func orUnset(s string) string {
	if s == "" {
		return "(未设置)"
	}
	return s
}
