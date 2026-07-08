package api

// reliability.go — M4 可靠性只读/触发面：供应商健康视图、手动测速。
// 仲裁引擎在 internal/hub/failover；api 只持引用（测试可为 nil）。

import (
	"net/http"

	"github.com/Code-kike/switchAPI/internal/hub/failover"
	"github.com/Code-kike/switchAPI/internal/hub/store"
)

// SetReliability wires the failover engine (call before serving; nil in tests
// that don't exercise these endpoints).
func (s *Server) SetReliability(e *failover.Engine) { s.reliability = e }

// GET /api/v1/health — provider_health 全量视图（冷却/needs_attention/探测状态）。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	list, err := s.st.ListProviderHealth()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.ProviderHealth{}
	}
	writeJSON(w, http.StatusOK, list)
}

// POST /api/v1/speedtest — 广播测速指令。
func (s *Server) handleSpeedtest(w http.ResponseWriter, _ *http.Request) {
	if s.reliability == nil {
		httpError(w, http.StatusServiceUnavailable, "测速引擎未启用")
		return
	}
	id, err := s.reliability.StartSpeedtest()
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"test_id": id})
}

// GET /api/v1/speedtest/latest — 最近一轮结果（按设备聚合；无则 null）。
func (s *Server) handleSpeedtestLatest(w http.ResponseWriter, _ *http.Request) {
	if s.reliability == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, s.reliability.SpeedtestLatest())
}
