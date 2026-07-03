// Package appconfig performs the one-time takeover of CC/Codex configs so
// their traffic enters the local Agent (research/01 + research/02 are the
// field spec). Dry-run is first-class; every real write is preceded by a
// timestamped backup and lands via temp-file + rename.
package appconfig

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options selects targets and carries the values to write. Zero-value paths
// resolve to the real user files; tests inject temp paths and a fake Env.
type Options struct {
	ListenAddr string // 转发监听地址，默认 127.0.0.1:9527
	LocalToken string // Agent 本地 token（写入 CC/Codex 作为鉴权凭据）

	ClaudeSettingsPath string // 默认 ~/.claude/settings.json
	CodexConfigPath    string // 默认 ~/.codex/config.toml
	CodexAuthPath      string // 默认 ~/.codex/auth.json

	Env func(string) string // 默认 os.Getenv（测试可注入）
}

// Change describes the planned edits for one file.
type Change struct {
	File  string
	Lines []string // 人类可读 diff（"键: 旧 → 新"）
}

const backupInfix = ".switchapi-bak-"

func (o *Options) normalize() error {
	if o.ListenAddr == "" {
		o.ListenAddr = "127.0.0.1:9527"
	}
	if o.LocalToken == "" {
		return fmt.Errorf("LocalToken 为空：请先 agent pair 生成本地 token")
	}
	if o.Env == nil {
		o.Env = os.Getenv
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if o.ClaudeSettingsPath == "" {
		o.ClaudeSettingsPath = filepath.Join(home, ".claude", "settings.json")
	}
	if o.CodexConfigPath == "" {
		o.CodexConfigPath = filepath.Join(home, ".codex", "config.toml")
	}
	if o.CodexAuthPath == "" {
		o.CodexAuthPath = filepath.Join(home, ".codex", "auth.json")
	}
	return nil
}

func (o *Options) anthropicBase() string { return "http://" + o.ListenAddr + "/anthropic" }
func (o *Options) openaiBase() string    { return "http://" + o.ListenAddr + "/openai/v1" }

// Plan computes the takeover changes plus conflict warnings without touching
// anything. A returned error means takeover must NOT proceed (research/01 C8
// refusal class).
func Plan(opts Options) ([]Change, []string, error) {
	if err := opts.normalize(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkConflicts(opts)
	if err != nil {
		return nil, warnings, err
	}

	var changes []Change
	ccChange, ccWarn, err := planClaude(opts)
	if err != nil {
		return nil, warnings, fmt.Errorf("CC 配置解析失败: %w", err)
	}
	warnings = append(warnings, ccWarn...)
	if ccChange != nil {
		changes = append(changes, *ccChange)
	}
	codexChanges, err := planCodex(opts)
	if err != nil {
		return nil, warnings, fmt.Errorf("Codex 配置解析失败: %w", err)
	}
	changes = append(changes, codexChanges...)
	return changes, warnings, nil
}

// Apply executes the plan. dryRun=true prints nothing to disk — callers show
// the returned changes/warnings.
func Apply(opts Options, dryRun bool) ([]Change, []string, error) {
	changes, warnings, err := Plan(opts)
	if err != nil || dryRun {
		return changes, warnings, err
	}
	if err := opts.normalize(); err != nil {
		return changes, warnings, err
	}
	if err := applyClaude(opts); err != nil {
		return changes, warnings, fmt.Errorf("写入 CC 配置失败: %w", err)
	}
	if err := applyCodex(opts); err != nil {
		return changes, warnings, fmt.Errorf("写入 Codex 配置失败: %w", err)
	}
	return changes, warnings, nil
}

// Rollback restores the newest backup of every managed file.
func Rollback(opts Options) ([]string, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	var restored []string
	for _, path := range []string{opts.ClaudeSettingsPath, opts.CodexConfigPath, opts.CodexAuthPath} {
		bak, err := newestBackup(path)
		if err != nil {
			return restored, err
		}
		if bak == "" {
			continue
		}
		if err := copyFile(bak, path); err != nil {
			return restored, fmt.Errorf("恢复 %s 失败: %w", path, err)
		}
		restored = append(restored, path+" ← "+filepath.Base(bak))
	}
	return restored, nil
}

// checkConflicts implements the research/01 C8 checklist. Returned error =
// refusal; warnings are advisory.
func checkConflicts(opts Options) ([]string, error) {
	var warnings []string
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		if v := opts.Env(k); v != "" && v != "0" && !strings.EqualFold(v, "false") {
			return warnings, fmt.Errorf("检测到 %s=%s：Bedrock/Vertex 模式的流量不走 ANTHROPIC_BASE_URL，拒绝接管", k, v)
		}
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if opts.Env(k) != "" {
			warnings = append(warnings,
				"shell 环境导出了 "+k+"：其与 settings.json env 块的优先级官方未明示，建议从 shell 配置移除（研究#1 遗留#1）")
		}
	}
	if opts.Env("HTTP_PROXY") != "" || opts.Env("HTTPS_PROXY") != "" ||
		opts.Env("http_proxy") != "" || opts.Env("https_proxy") != "" {
		noProxy := opts.Env("NO_PROXY") + opts.Env("no_proxy")
		if !strings.Contains(noProxy, "127.0.0.1") && !strings.Contains(noProxy, "localhost") {
			warnings = append(warnings,
				"检测到 HTTP(S)_PROXY 且 NO_PROXY 未包含 127.0.0.1：回环请求可能被送进代理，请把 127.0.0.1 加入 NO_PROXY")
		}
	}
	return warnings, nil
}

// ---- shared file helpers ----

// backupThenWrite backs up path (when it exists) and writes data atomically,
// preserving the original mode (fallback mode for new files).
func backupThenWrite(path string, data []byte, fallbackMode os.FileMode) error {
	mode := fallbackMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
		bak := path + backupInfix + fmt.Sprint(time.Now().Unix())
		if err := copyFile(path, bak); err != nil {
			return fmt.Errorf("备份失败: %w", err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".switchapi-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func newestBackup(path string) (string, error) {
	matches, err := filepath.Glob(path + backupInfix + "*")
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches) // 时间戳后缀，字典序即时间序
	return matches[len(matches)-1], nil
}
