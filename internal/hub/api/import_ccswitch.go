package api

// import_ccswitch.go — cc-switch 一键导入（研究/05）：SPA 上传 cc-switch.db
// （或 legacy v2 config.json）原始字节 → importer 解析映射 → 逐条建供应商 +
// is_current→app_state + failover 队列→备选序列；E1-E9 跳过项逐条带原因返回。

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Code-kike/switchAPI/internal/hub/importer"
	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/google/uuid"
)

func (s *Server) handleImportCCSwitch(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "读取上传内容失败（上限 64MB）："+err.Error())
		return
	}
	parsed, err := importer.Parse(raw)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 同名冲突加后缀（研究/05 映射表）；对照现有与本批已导入的名字。
	existing, err := s.st.ListProviders()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	taken := map[string]bool{}
	for _, p := range existing {
		taken[p.Name] = true
	}

	type importedView struct {
		App  string `json:"app"`
		Name string `json:"name"`
	}
	var imported []importedView
	queue := map[string][]string{} // app → provider ids（按 SortIndex 已排序）
	current := map[string]string{} // app → provider id

	for _, c := range parsed.Candidates {
		name := c.Name
		for i := 2; taken[name]; i++ {
			name = c.Name + "（" + strconv.Itoa(i) + "）"
		}
		taken[name] = true

		enc, err := cryptoutil.Seal(s.masterKey, []byte(c.APIKey))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p := store.Provider{
			ID: uuid.NewString(), Name: name, Protocol: appProtocol[c.App],
			BaseURL: c.BaseURL, APIKeyEnc: enc, ModelRedirects: "{}",
			CostCoefficient: c.CostCoefficient, Sort: c.SortIndex, Note: strings.TrimSpace(c.Note),
		}
		if err := s.st.CreateProvider(p); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		imported = append(imported, importedView{App: c.App, Name: name})
		if c.InFailoverQueue {
			queue[c.App] = append(queue[c.App], p.ID)
		}
		if c.IsCurrent {
			current[c.App] = p.ID
		}
	}
	for app, ids := range queue {
		s.st.SetFallbackOrder(app, ids)
	}
	for app, pid := range current {
		// 仅当该 App 尚未设置过当前供应商时才接管切换状态——已有生效配置的
		// Hub 不应被一次导入悄悄改路由。
		if _, err := s.st.GetAppState(app); err != nil {
			s.st.SetAppState(app, pid, "cc-switch-import")
		}
	}

	payload, _ := json.Marshal(map[string]int{"imported": len(imported), "skipped": len(parsed.Skipped)})
	s.event("backup", `{"action":"cc-switch-import","summary":`+strconv.Quote(string(payload))+`}`)
	s.broadcast()
	s.NotifyState()
	s.markDirty()

	if imported == nil {
		imported = []importedView{}
	}
	if parsed.Skipped == nil {
		parsed.Skipped = []importer.Skipped{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"skipped":  parsed.Skipped,
	})
}
