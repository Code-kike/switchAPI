package appconfig

// cc.go — Claude Code takeover: surgical merge of exactly two keys into the
// user-level settings.json env block (research/01 C1/C6). Officially
// hot-reloads — running CC sessions pick the change up without restart.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const (
	envBaseURL   = "ANTHROPIC_BASE_URL"
	envAuthToken = "ANTHROPIC_AUTH_TOKEN"
)

// loadClaudeSettings reads settings.json into a generic map ({} when absent).
func loadClaudeSettings(path string) (map[string]any, error) {
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

// claudeTargets returns the desired env values.
func claudeTargets(opts Options) map[string]string {
	return map[string]string{
		envBaseURL:   opts.anthropicBase(),
		envAuthToken: opts.LocalToken,
	}
}

func planClaude(opts Options) (*Change, []string, error) {
	root, err := loadClaudeSettings(opts.ClaudeSettingsPath)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if _, has := root["apiKeyHelper"]; has {
		warnings = append(warnings,
			"settings.json 配置了 apiKeyHelper：其值会以双头发送、可能与本地 token 冲突，建议移除（研究/01 C8）")
	}
	env, _ := root["env"].(map[string]any)
	change := Change{File: opts.ClaudeSettingsPath}
	for k, want := range claudeTargets(opts) {
		old := ""
		if env != nil {
			old, _ = env[k].(string)
		}
		if old != want {
			change.Lines = append(change.Lines, fmt.Sprintf("env.%s: %s → %s", k, displayVal(k, old), displayVal(k, want)))
		}
	}
	if len(change.Lines) == 0 {
		return nil, warnings, nil // 已是目标状态
	}
	return &change, warnings, nil
}

func applyClaude(opts Options) error {
	root, err := loadClaudeSettings(opts.ClaudeSettingsPath)
	if err != nil {
		return err
	}
	env, ok := root["env"].(map[string]any)
	if !ok {
		env = map[string]any{}
		root["env"] = env
	}
	dirty := false
	for k, want := range claudeTargets(opts) {
		if old, _ := env[k].(string); old != want {
			env[k] = want
			dirty = true
		}
	}
	if !dirty {
		return nil
	}
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return backupThenWrite(opts.ClaudeSettingsPath, raw, 0o644)
}

// displayVal redacts token-ish values to their last 4 chars in diffs.
func displayVal(key, v string) string {
	if v == "" {
		return "(未设置)"
	}
	if key == envAuthToken || len(v) > 48 {
		if len(v) > 4 {
			return "…" + v[len(v)-4:]
		}
		return "…"
	}
	return v
}
