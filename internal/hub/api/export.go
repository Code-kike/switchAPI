package api

// export.go — 备份/导出/导入/CSV（父 design.md §7）。
// 导出 payload 含明文 key：给口令 → scrypt+AES-256-GCM 整体加密；
// 不给口令必须显式 plaintext_confirmed（UI 二次确认），否则 400。

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/backup"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/google/uuid"
)

const (
	exportFormatSealed = "switchapi-export-v1"
	exportFormatPlain  = "switchapi-export-plain-v1"
)

// SetBackup wires the snapshot manager (nil in tests → 503).
func (s *Server) SetBackup(m *backup.Manager) { s.backups = m }

// markDirty schedules a debounced backup after structural writes.
func (s *Server) markDirty() {
	if s.backups != nil {
		s.backups.MarkDirty()
	}
}

func (s *Server) handleBackupRun(w http.ResponseWriter, _ *http.Request) {
	if s.backups == nil {
		httpError(w, http.StatusServiceUnavailable, "备份未启用")
		return
	}
	info, err := s.backups.RunNow()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("backup", `{"action":"manual","name":"`+info.Name+`"}`)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleBackupList(w http.ResponseWriter, _ *http.Request) {
	if s.backups == nil {
		writeJSON(w, http.StatusOK, []backup.Info{})
		return
	}
	list, err := s.backups.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---- 导出 / 导入 ----

type exportProvider struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	APIKey          string            `json:"api_key"` // 明文——整体加密或明文确认后才出门
	ModelRedirects  map[string]string `json:"model_redirects,omitempty"`
	CostCoefficient float64           `json:"cost_coefficient"`
	PresetID        string            `json:"preset_id,omitempty"`
	Sort            int               `json:"sort,omitempty"`
	Note            string            `json:"note,omitempty"`
}

type exportPayload struct {
	Schema           int                     `json:"schema"`
	ExportedAt       int64                   `json:"exported_at"`
	Providers        []exportProvider        `json:"providers"`
	AppState         map[string]string       `json:"app_state"`
	FallbackOrders   map[string][]string     `json:"fallback_orders"`
	PricingOverrides []store.PricingOverride `json:"pricing_overrides,omitempty"`
}

func (s *Server) buildExport() (*exportPayload, error) {
	list, err := s.st.ListProviders()
	if err != nil {
		return nil, err
	}
	out := &exportPayload{
		Schema: 1, ExportedAt: time.Now().Unix(),
		AppState: map[string]string{}, FallbackOrders: map[string][]string{},
	}
	for _, p := range list {
		plain, err := cryptoutil.Open(s.masterKey, p.APIKeyEnc)
		if err != nil {
			return nil, fmt.Errorf("解密供应商 %s 的 key 失败: %w", p.Name, err)
		}
		redirects := map[string]string{}
		json.Unmarshal([]byte(p.ModelRedirects), &redirects)
		out.Providers = append(out.Providers, exportProvider{
			ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL,
			APIKey: string(plain), ModelRedirects: redirects,
			CostCoefficient: p.CostCoefficient, PresetID: p.PresetID, Sort: p.Sort, Note: p.Note,
		})
	}
	for app := range appProtocol {
		if st, err := s.st.GetAppState(app); err == nil {
			out.AppState[app] = st.ActiveProviderID
		}
		if order, err := s.st.GetFallbackOrder(app); err == nil && len(order) > 0 {
			out.FallbackOrders[app] = order
		}
	}
	if ovr, err := s.st.GetPricingOverrides(); err == nil {
		out.PricingOverrides = ovr
	}
	return out, nil
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase         string `json:"passphrase"`
		PlaintextConfirmed bool   `json:"plaintext_confirmed"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	payload, err := s.buildExport()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.Passphrase == "" {
		if !req.PlaintextConfirmed {
			httpError(w, http.StatusBadRequest,
				"导出内容含明文 API key：必须提供 passphrase 加密，或显式确认明文导出")
			return
		}
		s.event("backup", `{"action":"export","mode":"plaintext"}`)
		writeJSON(w, http.StatusOK, map[string]any{
			"format": exportFormatPlain, "payload": json.RawMessage(raw),
		})
		return
	}

	salt, sealed, err := cryptoutil.SealWithPassphrase(req.Passphrase, raw)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("backup", `{"action":"export","mode":"encrypted"}`)
	writeJSON(w, http.StatusOK, map[string]any{
		"format": exportFormatSealed,
		"kdf": map[string]any{"algo": "scrypt", "n": cryptoutil.ScryptN, "r": cryptoutil.ScryptR,
			"p": cryptoutil.ScryptP, "salt": base64.StdEncoding.EncodeToString(salt)},
		"data": base64.StdEncoding.EncodeToString(sealed),
	})
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string          `json:"passphrase"`
		Format     string          `json:"format"`
		Payload    json.RawMessage `json:"payload"`
		KDF        struct {
			Salt string `json:"salt"`
		} `json:"kdf"`
		Data string `json:"data"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "导入文件不是合法 JSON："+err.Error())
		return
	}

	var raw []byte
	switch req.Format {
	case exportFormatPlain:
		raw = req.Payload
	case exportFormatSealed:
		if req.Passphrase == "" {
			httpError(w, http.StatusBadRequest, "该导出文件已加密，请提供口令")
			return
		}
		salt, err := base64.StdEncoding.DecodeString(req.KDF.Salt)
		if err != nil {
			httpError(w, http.StatusBadRequest, "kdf.salt 非法")
			return
		}
		sealed, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			httpError(w, http.StatusBadRequest, "data 非法")
			return
		}
		raw, err = cryptoutil.OpenWithPassphrase(req.Passphrase, salt, sealed)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "口令错误或文件已损坏")
			return
		}
	default:
		httpError(w, http.StatusBadRequest, "未知导出格式："+req.Format)
		return
	}

	var payload exportPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Schema != 1 {
		httpError(w, http.StatusBadRequest, "导出内容损坏或 schema 不支持")
		return
	}

	imported := 0
	for _, ep := range payload.Providers {
		enc, err := cryptoutil.Seal(s.masterKey, []byte(ep.APIKey))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		id := ep.ID
		if id == "" {
			id = uuid.NewString()
		}
		p := store.Provider{
			ID: id, Name: ep.Name, Protocol: ep.Protocol, BaseURL: ep.BaseURL,
			APIKeyEnc: enc, ModelRedirects: marshalRedirects(ep.ModelRedirects),
			CostCoefficient: ep.CostCoefficient, PresetID: ep.PresetID, Sort: ep.Sort, Note: ep.Note,
		}
		if _, err := s.st.GetProvider(id); err == nil {
			if err := s.st.UpdateProvider(p); err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
		} else if err := s.st.CreateProvider(p); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		imported++
	}
	for app, order := range payload.FallbackOrders {
		if validApp(app) {
			s.st.SetFallbackOrder(app, order)
		}
	}
	for app, pid := range payload.AppState {
		if validApp(app) && pid != "" {
			if _, err := s.st.GetProvider(pid); err == nil {
				s.st.SetAppState(app, pid, "import")
			}
		}
	}
	s.st.UpsertPricingOverrides(payload.PricingOverrides)

	s.event("backup", `{"action":"import","providers":"`+strconv.Itoa(imported)+`"}`)
	s.broadcast()
	s.NotifyState()
	s.markDirty()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": imported})
}

// ---- CSV 用量导出 ----

func (s *Server) handleUsageCSV(w http.ResponseWriter, r *http.Request) {
	f := parseUsageFilter(r)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="switchapi-usage-`+time.Now().Format("20060102")+`.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"ts", "time", "device_id", "app", "provider_id", "model",
		"model_redirected", "input_tokens", "output_tokens", "cache_write_tokens",
		"cache_read_tokens", "duration_ms", "status", "error_kind", "usage_source", "request_id"})

	const page = 500
	f.Limit = page
	for offset := 0; ; offset += page {
		f.Offset = offset
		rows, total, err := s.st.QueryUsage(f)
		if err != nil {
			return // 头已发出，只能截断
		}
		for _, u := range rows {
			cw.Write([]string{
				strconv.FormatInt(u.TS, 10), time.Unix(u.TS, 0).Format(time.RFC3339),
				u.DeviceID, u.App, u.ProviderID, u.Model, u.ModelRedirected,
				strconv.FormatInt(u.InputTokens, 10), strconv.FormatInt(u.OutputTokens, 10),
				strconv.FormatInt(u.CacheWriteTokens, 10), strconv.FormatInt(u.CacheReadTokens, 10),
				strconv.FormatInt(u.DurationMS, 10), strconv.Itoa(u.Status),
				u.ErrorKind, u.UsageSource, u.RequestID,
			})
		}
		if offset+page >= total || len(rows) == 0 {
			break
		}
	}
	cw.Flush()
}
