package api

// providers.go — provider CRUD, presets, the global switch, and fallback
// orders. API keys arrive as plaintext in requests, are sealed with the
// master key before hitting the store, and only ever leave as last-4 hints.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/google/uuid"
)

type providerPayload struct {
	Name            string            `json:"name"`
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	APIKey          string            `json:"api_key"` // create: required; update: empty = keep
	ModelRedirects  map[string]string `json:"model_redirects"`
	CostCoefficient *float64          `json:"cost_coefficient"`
	PresetID        string            `json:"preset_id"`
	Sort            int               `json:"sort"`
	Note            string            `json:"note"`
}

type providerView struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	KeyLast4        string            `json:"key_last4"`
	ModelRedirects  map[string]string `json:"model_redirects"`
	CostCoefficient float64           `json:"cost_coefficient"`
	PresetID        string            `json:"preset_id"`
	Sort            int               `json:"sort"`
	Note            string            `json:"note"`
	CreatedAt       int64             `json:"created_at"`
	UpdatedAt       int64             `json:"updated_at"`
}

func (s *Server) view(p store.Provider) providerView {
	v := providerView{
		ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL,
		CostCoefficient: p.CostCoefficient, PresetID: p.PresetID, Sort: p.Sort,
		Note: p.Note, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		ModelRedirects: map[string]string{},
	}
	json.Unmarshal([]byte(p.ModelRedirects), &v.ModelRedirects)
	if plain, err := cryptoutil.Open(s.masterKey, p.APIKeyEnc); err == nil && len(plain) >= 4 {
		v.KeyLast4 = string(plain[len(plain)-4:])
	}
	return v
}

func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListProviders()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]providerView, 0, len(list))
	for _, p := range list {
		out = append(out, s.view(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	var req providerPayload
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.BaseURL == "" || req.APIKey == "" {
		httpError(w, http.StatusBadRequest, "name、base_url、api_key 均为必填")
		return
	}
	if _, ok := map[string]bool{"anthropic": true, "openai": true}[req.Protocol]; !ok {
		httpError(w, http.StatusBadRequest, "protocol 必须是 anthropic 或 openai")
		return
	}
	enc, err := cryptoutil.Seal(s.masterKey, []byte(req.APIKey))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := store.Provider{
		ID: uuid.NewString(), Name: req.Name, Protocol: req.Protocol,
		BaseURL: strings.TrimRight(req.BaseURL, "/"), APIKeyEnc: enc,
		ModelRedirects:  marshalRedirects(req.ModelRedirects),
		CostCoefficient: 1.0, PresetID: req.PresetID, Sort: req.Sort, Note: req.Note,
	}
	if req.CostCoefficient != nil {
		p.CostCoefficient = *req.CostCoefficient
	}
	if err := s.st.CreateProvider(p); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("provider", eventJSON(map[string]string{"action": "created", "id": p.ID, "name": p.Name}))
	created, _ := s.st.GetProvider(p.ID)
	writeJSON(w, http.StatusCreated, s.view(created))
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.st.GetProvider(id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "供应商不存在")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req providerPayload
	if !readJSON(w, r, &req) {
		return
	}
	if req.Protocol != "" && req.Protocol != existing.Protocol {
		// 协议决定 App 归属（design.md §2），改协议等于换一条 Provider。
		httpError(w, http.StatusBadRequest, "protocol 不可修改，请新建供应商")
		return
	}
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.BaseURL != "" {
		existing.BaseURL = strings.TrimRight(req.BaseURL, "/")
	}
	if req.ModelRedirects != nil {
		existing.ModelRedirects = marshalRedirects(req.ModelRedirects)
	}
	if req.CostCoefficient != nil {
		existing.CostCoefficient = *req.CostCoefficient
	}
	existing.PresetID = orKeep(req.PresetID, existing.PresetID)
	if req.Sort != 0 {
		existing.Sort = req.Sort
	}
	if req.Note != "" {
		existing.Note = req.Note
	}
	existing.APIKeyEnc = nil // default: keep stored ciphertext
	if req.APIKey != "" {
		enc, err := cryptoutil.Seal(s.masterKey, []byte(req.APIKey))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		existing.APIKeyEnc = enc
	}
	if err := s.st.UpdateProvider(existing); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 用户编辑（可能修了 key/地址）→ 健康状态既往不咎（研究/08：needs_attention
	// 由用户操作解除；冷却一并清除，下次失败重新计）。
	s.st.ClearProviderHealth(id)
	// 若更新的是某 App 的当前生效供应商，路由内容变了 → 推送新快照。
	if s.isActive(id) {
		s.broadcast()
	}
	updated, _ := s.st.GetProvider(id)
	writeJSON(w, http.StatusOK, s.view(updated))
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.isActive(id) {
		httpError(w, http.StatusConflict, "该供应商是某个 App 的当前生效供应商，请先切换到其他供应商再删除")
		return
	}
	err := s.st.DeleteProvider(id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "供应商不存在")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("provider", eventJSON(map[string]string{"action": "deleted", "id": id}))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) isActive(providerID string) bool {
	for app := range appProtocol {
		if st, err := s.st.GetAppState(app); err == nil && st.ActiveProviderID == providerID {
			return true
		}
	}
	return false
}

// ---- presets ----

type preset struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Protocol        string  `json:"protocol"`
	BaseURLHint     string  `json:"base_url_hint"`
	CostCoefficient float64 `json:"cost_coefficient"`
	Note            string  `json:"note"`
}

// 预设模板（CONTEXT.md: 预设模板）。base_url 惯例：anthropic 不含 /v1、
// openai 含 /v1（design.md §2，与 cc-switch 导入零摩擦）。
var presets = []preset{
	{ID: "anthropic-relay", Name: "Anthropic 兼容中转站", Protocol: "anthropic",
		BaseURLHint: "https://your-relay.example", CostCoefficient: 1.0,
		Note: "base_url 不含 /v1；填入站点 API Key 即可"},
	{ID: "openai-relay", Name: "OpenAI 兼容中转站（Codex）", Protocol: "openai",
		BaseURLHint: "https://your-relay.example/v1", CostCoefficient: 1.0,
		Note: "base_url 含 /v1；Codex 走 Responses 通道"},
	{ID: "anthropic-official", Name: "Anthropic 官方 API", Protocol: "anthropic",
		BaseURLHint: "https://api.anthropic.com", CostCoefficient: 1.0,
		Note: "官方计费，折扣系数保持 1.0"},
}

func (s *Server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, presets)
}

// ---- state / switch / fallback ----

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{}
	for app := range appProtocol {
		if st, err := s.st.GetAppState(app); err == nil {
			out[app] = map[string]any{
				"active_provider_id": st.ActiveProviderID,
				"updated_at":         st.UpdatedAt,
				"updated_by":         st.UpdatedBy,
			}
		} else {
			out[app] = nil
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App        string `json:"app"`
		ProviderID string `json:"provider_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validApp(req.App) {
		httpError(w, http.StatusBadRequest, "app 必须是 claude-code 或 codex")
		return
	}
	p, err := s.st.GetProvider(req.ProviderID)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "供应商不存在")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p.Protocol != appProtocol[req.App] {
		httpError(w, http.StatusBadRequest,
			"协议不匹配："+req.App+" 需要 "+appProtocol[req.App]+" 协议的供应商")
		return
	}

	from := ""
	if st, err := s.st.GetAppState(req.App); err == nil {
		from = st.ActiveProviderID
	}
	if err := s.st.SetAppState(req.App, p.ID, "admin"); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("switch", eventJSON(map[string]string{
		"app": req.App, "from": from, "to": p.ID, "to_name": p.Name}))
	s.markDirty()
	s.broadcast()   // Agent 侧 config_push（内部先 bump config_rev）
	s.NotifyState() // UI 侧 state_changed（在 broadcast 后取到新 rev）
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "app": req.App, "active": p.ID})
}

func (s *Server) handleFallbackGet(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !validApp(app) {
		httpError(w, http.StatusBadRequest, "未知 App")
		return
	}
	order, err := s.st.GetFallbackOrder(app)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app, "provider_ids": order})
}

func (s *Server) handleFallbackPut(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !validApp(app) {
		httpError(w, http.StatusBadRequest, "未知 App")
		return
	}
	var req struct {
		ProviderIDs []string `json:"provider_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	for _, id := range req.ProviderIDs {
		p, err := s.st.GetProvider(id)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusBadRequest, "备选序列包含不存在的供应商："+id)
			return
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p.Protocol != appProtocol[app] {
			httpError(w, http.StatusBadRequest, "备选序列包含协议不匹配的供应商："+p.Name)
			return
		}
	}
	if err := s.st.SetFallbackOrder(app, req.ProviderIDs); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.markDirty()
	s.broadcast() // Agent 缓存备选序列供 M4 本地临时降级使用
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- small helpers ----

func marshalRedirects(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func eventJSON(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func orKeep(newVal, oldVal string) string {
	if newVal != "" {
		return newVal
	}
	return oldVal
}
