// Package importer parses cc-switch data（研究/05 映射表为规格）：SQLite
// `cc-switch.db`（v3.8.0+ 现行唯一格式，按列名读、容忍未知列）为主，
// v2 config.json（≤3.7.1）兜底，v1 拒绝。只提取映射表字段——settings_config
// 里的其余内容（hooks/其他 env/第三方机密）绝不搬运（研究/05 C3/Drop-list）。
package importer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	_ "modernc.org/sqlite" // 只读打开上传的 db 副本
)

// Candidate is one importable provider row (already mapped to our model).
type Candidate struct {
	App             string  `json:"app"` // claude-code | codex
	Name            string  `json:"name"`
	BaseURL         string  `json:"base_url"`
	APIKey          string  `json:"-"` // 明文，仅内部流转；响应里绝不回显
	CostCoefficient float64 `json:"cost_coefficient"`
	Note            string  `json:"note,omitempty"`
	SortIndex       int     `json:"sort_index"`
	IsCurrent       bool    `json:"is_current"`
	InFailoverQueue bool    `json:"in_failover_queue"`
}

// Skipped is one row we refuse to import, with its E1-E9 reason.
type Skipped struct {
	App    string `json:"app"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Parsed is the full parse outcome.
type Parsed struct {
	Candidates []Candidate `json:"candidates"`
	Skipped    []Skipped   `json:"skipped"`
}

var appMap = map[string]string{"claude": "claude-code", "codex": "codex"}

// Parse sniffs the payload: SQLite db vs JSON（v2 接受、v1 拒绝）。
func Parse(data []byte) (*Parsed, error) {
	if len(data) >= 16 && string(data[:15]) == "SQLite format 3" {
		return parseDB(data)
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		return parseJSONV2([]byte(trimmed))
	}
	return nil, fmt.Errorf("无法识别的文件：既不是 cc-switch.db（SQLite）也不是 config.json")
}

// ---- SQLite 分支（现行格式）----

func parseDB(data []byte) (*Parsed, error) {
	// 上传的是文件副本 → 落临时文件后 mode=ro 打开（天然规避 E7 锁问题）。
	dir, err := os.MkdirTemp("", "ccswitch-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cc-switch.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT * FROM providers WHERE app_type IN ('claude','codex')`)
	if err != nil {
		return nil, fmt.Errorf("读取 providers 表失败（确定是 cc-switch.db？）: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := &Parsed{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		// 按列名取值（E6：容忍未知列、列序变化）。
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		mapRow(out, rawRow{
			appType:        asString(row["app_type"]),
			name:           asString(row["name"]),
			settingsConfig: asString(row["settings_config"]),
			meta:           asString(row["meta"]),
			notes:          asString(row["notes"]),
			costMultCol:    asString(row["cost_multiplier"]),
			providerType:   asString(row["provider_type"]),
			sortIndex:      asInt(row["sort_index"]),
			isCurrent:      asBool(row["is_current"]),
			inQueue:        asBool(row["in_failover_queue"]),
		})
	}
	sortCandidates(out)
	return out, rows.Err()
}

// ---- v2 config.json 分支（≤3.7.1 legacy）----

func parseJSONV2(data []byte) (*Parsed, error) {
	var top struct {
		Version   int             `json:"version"`
		Providers json.RawMessage `json:"providers"` // v1 特征：顶层直挂
		Claude    *v2App          `json:"claude"`
		Codex     *v2App          `json:"codex"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("config.json 解析失败: %w", err)
	}
	if top.Version != 2 {
		if len(top.Providers) > 0 {
			return nil, fmt.Errorf("检测到 cc-switch v1 格式（过旧）：请先升级 cc-switch 到 3.2+ 迁移后再导出")
		}
		return nil, fmt.Errorf("不支持的 config.json 版本 %d（仅支持 v2）", top.Version)
	}

	out := &Parsed{}
	for appType, appData := range map[string]*v2App{"claude": top.Claude, "codex": top.Codex} {
		if appData == nil {
			continue
		}
		for id, p := range appData.Providers {
			mapRow(out, rawRow{
				appType:        appType,
				name:           p.Name,
				settingsConfig: string(p.SettingsConfig),
				meta:           string(p.Meta),
				notes:          p.Notes,
				sortIndex:      p.SortIndex,
				isCurrent:      id == appData.Current,
				inQueue:        p.InFailoverQueue,
			})
		}
	}
	sortCandidates(out)
	return out, nil
}

type v2App struct {
	Providers map[string]v2Provider `json:"providers"`
	Current   string                `json:"current"`
}

type v2Provider struct {
	Name            string          `json:"name"`
	SettingsConfig  json.RawMessage `json:"settingsConfig"`
	Meta            json.RawMessage `json:"meta"`
	Notes           string          `json:"notes"`
	SortIndex       int             `json:"sortIndex"`
	InFailoverQueue bool            `json:"inFailoverQueue"`
}

// ---- 共享映射（研究/05 字段映射表 + E1-E4 跳过判定）----

type rawRow struct {
	appType, name, settingsConfig, meta, notes string
	costMultCol, providerType                  string
	sortIndex                                  int
	isCurrent, inQueue                         bool
}

type metaFields struct {
	CostMultiplier string `json:"costMultiplier"`
	APIFormat      string `json:"apiFormat"`
	ProviderType   string `json:"providerType"`
	AuthBinding    *struct {
		Source string `json:"source"`
	} `json:"authBinding"`
}

func mapRow(out *Parsed, r rawRow) {
	app, ok := appMap[r.appType]
	if !ok {
		return // gemini/hermes/… 明确不做的工具，静默忽略（Drop-list #9）
	}
	skip := func(reason string) {
		out.Skipped = append(out.Skipped, Skipped{App: app, Name: r.name, Reason: reason})
	}

	var meta metaFields
	json.Unmarshal([]byte(r.meta), &meta)

	// E2：OAuth 托管类无静态 key 可迁。
	pt := meta.ProviderType
	if pt == "" {
		pt = r.providerType
	}
	if pt == "codex_oauth" || pt == "github_copilot" ||
		(meta.AuthBinding != nil && meta.AuthBinding.Source == "managed_account") {
		skip("E2 OAuth 托管凭据（" + pt + "），无静态 key 可迁移")
		return
	}
	// E1：依赖 cc-switch 本地代理做跨协议转换（我方 ADR-0002 不做转换）。
	if r.appType == "claude" && (meta.APIFormat == "openai_chat" || meta.APIFormat == "openai_responses") {
		skip("E1 该供应商依赖 cc-switch 的 " + meta.APIFormat + " 协议转换，switchAPI 不做跨协议转换")
		return
	}

	var baseURL, apiKey, pinNote string
	var err error
	if r.appType == "claude" {
		baseURL, apiKey, pinNote, err = extractClaude(r.settingsConfig)
	} else {
		baseURL, apiKey, err = extractCodex(r.settingsConfig)
	}
	if err != nil {
		skip("settings_config 解析失败：" + err.Error())
		return
	}
	if apiKey == "" {
		skip("E4 API key 为空（预设占位未填）")
		return
	}
	if baseURL == "" {
		skip("缺少 base_url")
		return
	}
	// E3：回环/本机代理——导入后会形成双层代理甚至回环。
	if u, err := url.Parse(baseURL); err == nil {
		host := u.Hostname()
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			skip("E3 base_url 指向本机（" + host + "），导入会造成双层代理")
			return
		}
	}

	coeff := 1.0
	if v, err := strconv.ParseFloat(meta.CostMultiplier, 64); err == nil && v > 0 {
		coeff = v // meta 优先（研究/05 遗留不确定性 #3）
	} else if v, err := strconv.ParseFloat(r.costMultCol, 64); err == nil && v > 0 {
		coeff = v
	}

	note := r.notes
	if pinNote != "" {
		if note != "" {
			note += "；"
		}
		note += pinNote
	}
	if note != "" {
		note += " "
	}
	note += "[imported from cc-switch]"

	out.Candidates = append(out.Candidates, Candidate{
		App: app, Name: r.name, BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey,
		CostCoefficient: coeff, Note: note, SortIndex: r.sortIndex,
		IsCurrent: r.isCurrent, InFailoverQueue: r.inQueue,
	})
}

// extractClaude follows the upstream credential chain（研究/05 C2）：
// ANTHROPIC_AUTH_TOKEN → ANTHROPIC_API_KEY → OPENROUTER_API_KEY →
// GOOGLE_API_KEY，取首个非空。模型钉扎写入备注（Drop-list #2）。
func extractClaude(cfg string) (baseURL, apiKey, pinNote string, err error) {
	var sc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(cfg), &sc); err != nil {
		return "", "", "", err
	}
	baseURL = sc.Env["ANTHROPIC_BASE_URL"]
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "GOOGLE_API_KEY"} {
		if v := sc.Env[k]; v != "" {
			apiKey = v
			break
		}
	}
	var pins []string
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		if v := sc.Env[k]; v != "" {
			pins = append(pins, k+"="+v)
		}
	}
	if len(pins) > 0 {
		pinNote = "原模型钉扎: " + strings.Join(pins, ", ")
	}
	return baseURL, apiKey, pinNote, nil
}

// extractCodex mirrors upstream extract_codex_base_url（研究/05 C2）：解析
// config TOML，取顶层 model_provider 指向的 [model_providers.<id>].base_url，
// 回退顶层 base_url；key 取 auth.OPENAI_API_KEY 回退 experimental_bearer_token。
func extractCodex(cfg string) (baseURL, apiKey string, err error) {
	var sc struct {
		Auth   map[string]string `json:"auth"`
		Config string            `json:"config"`
	}
	if err := json.Unmarshal([]byte(cfg), &sc); err != nil {
		return "", "", err
	}
	apiKey = sc.Auth["OPENAI_API_KEY"]

	var conf map[string]any
	if err := toml.Unmarshal([]byte(sc.Config), &conf); err != nil {
		return "", "", fmt.Errorf("config TOML: %w", err)
	}
	if mp, _ := conf["model_provider"].(string); mp != "" {
		if mps, _ := conf["model_providers"].(map[string]any); mps != nil {
			if entry, _ := mps[mp].(map[string]any); entry != nil {
				baseURL, _ = entry["base_url"].(string)
			}
		}
	}
	if baseURL == "" {
		baseURL, _ = conf["base_url"].(string)
	}
	if apiKey == "" {
		apiKey, _ = conf["experimental_bearer_token"].(string)
	}
	return baseURL, apiKey, nil
}

func sortCandidates(p *Parsed) {
	sort.SliceStable(p.Candidates, func(i, j int) bool {
		if p.Candidates[i].App != p.Candidates[j].App {
			return p.Candidates[i].App < p.Candidates[j].App
		}
		return p.Candidates[i].SortIndex < p.Candidates[j].SortIndex
	})
}

// ---- 弱类型列值辅助（SQLite 动态类型）----

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

func asInt(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func asBool(v any) bool {
	switch x := v.(type) {
	case int64:
		return x != 0
	case bool:
		return x
	case string:
		return x == "1" || x == "true"
	}
	return false
}
