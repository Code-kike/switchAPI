package api

// devices.go — pairing (one-time code → revocable device token, CONTEXT.md:
// 配对) plus device listing/revocation and the event timeline.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/Code-kike/switchAPI/internal/hub/store"
	"github.com/Code-kike/switchAPI/internal/shared/cryptoutil"
	"github.com/google/uuid"
)

func (s *Server) handlePairingCode(w http.ResponseWriter, _ *http.Request) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	expiry := time.Now().Add(pairingTTL)
	s.mu.Lock()
	s.pairings[code] = expiry
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": expiry.Unix()})
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	exp, ok := s.pairings[req.Code]
	if ok {
		delete(s.pairings, req.Code) // 一次性：无论成败都作废
	}
	s.mu.Unlock()
	if !ok || time.Now().After(exp) {
		httpError(w, http.StatusBadRequest, "配对码无效或已过期")
		return
	}
	if req.Name == "" {
		req.Name = "未命名设备"
	}

	token, err := cryptoutil.NewToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := store.Device{
		ID: uuid.NewString(), Name: req.Name, Platform: req.Platform,
		TokenHash: cryptoutil.HashToken(token),
	}
	if err := s.st.CreateDevice(d); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.event("pairing", eventJSON(map[string]string{
		"action": "paired", "device_id": d.ID, "name": d.Name}))
	// token 仅此一次下发；库中只存哈希（ADR-0005）。
	writeJSON(w, http.StatusOK, map[string]any{"device_id": d.ID, "token": token})
}

func (s *Server) handleDeviceList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.st.ListDevices()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type deviceView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
		PairedAt int64  `json:"paired_at"`
		LastSeen int64  `json:"last_seen"`
		Revoked  bool   `json:"revoked"`
	}
	out := make([]deviceView, 0, len(list))
	for _, d := range list {
		out = append(out, deviceView{d.ID, d.Name, d.Platform, d.PairedAt, d.LastSeen, d.Revoked})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.st.RevokeDevice(id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "设备不存在")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.agents != nil {
		s.agents.Kick(id) // 掐断已建立的 WS 连接，令吊销即时生效
	}
	s.event("pairing", eventJSON(map[string]string{"action": "revoked", "device_id": id}))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	evs, err := s.st.RecentEvents(limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type eventView struct {
		ID      int64           `json:"id"`
		TS      int64           `json:"ts"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	out := make([]eventView, 0, len(evs))
	for _, e := range evs {
		out = append(out, eventView{e.ID, e.TS, e.Kind, json.RawMessage(e.Payload)})
	}
	writeJSON(w, http.StatusOK, out)
}
