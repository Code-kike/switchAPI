package store

// health.go — provider_health DAO（M4 故障切换的 Hub 侧仲裁状态，research/08
// 参数表 #12/#17）：冷却（指数退避）、needs_attention（配置类故障，不自愈）、
// 恢复探测连击。行按需创建（upsert）。

import (
	"database/sql"
	"errors"
	"time"
)

// ProviderHealth mirrors one provider_health row.
type ProviderHealth struct {
	ProviderID         string `json:"provider_id"`
	DemoteCount        int    `json:"demote_count"`
	CooldownUntil      int64  `json:"cooldown_until"`
	NeedsAttention     bool   `json:"needs_attention"`
	LastProbeAt        int64  `json:"last_probe_at"`
	ConsecutiveProbeOK int    `json:"consecutive_probe_ok"`
}

// InCooldown reports whether the provider is currently demoted.
func (h ProviderHealth) InCooldown(now int64) bool { return h.CooldownUntil > now }

// GetProviderHealth returns the row (zero-value health when absent).
func (s *Store) GetProviderHealth(providerID string) (ProviderHealth, error) {
	h := ProviderHealth{ProviderID: providerID}
	var attention int
	err := s.db.QueryRow(`SELECT demote_count, cooldown_until, needs_attention,
		last_probe_at, consecutive_probe_ok FROM provider_health WHERE provider_id=?`,
		providerID).Scan(&h.DemoteCount, &h.CooldownUntil, &attention, &h.LastProbeAt, &h.ConsecutiveProbeOK)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	h.NeedsAttention = attention != 0
	return h, nil
}

// ListProviderHealth returns all non-default rows.
func (s *Store) ListProviderHealth() ([]ProviderHealth, error) {
	rows, err := s.db.Query(`SELECT provider_id, demote_count, cooldown_until,
		needs_attention, last_probe_at, consecutive_probe_ok FROM provider_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderHealth
	for rows.Next() {
		var h ProviderHealth
		var attention int
		if err := rows.Scan(&h.ProviderID, &h.DemoteCount, &h.CooldownUntil,
			&attention, &h.LastProbeAt, &h.ConsecutiveProbeOK); err != nil {
			return nil, err
		}
		h.NeedsAttention = attention != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// DemoteProvider bumps the demote counter and applies the exponential
// cooldown 300s×2^(n-1) capped at 3600s (research/08 #12). Returns the row.
func (s *Store) DemoteProvider(providerID string) (ProviderHealth, error) {
	h, err := s.GetProviderHealth(providerID)
	if err != nil {
		return h, err
	}
	h.DemoteCount++
	cool := int64(300)
	for i := 1; i < h.DemoteCount && cool < 3600; i++ {
		cool *= 2
	}
	if cool > 3600 {
		cool = 3600
	}
	h.CooldownUntil = time.Now().Unix() + cool
	h.ConsecutiveProbeOK = 0
	return h, s.upsertHealth(h)
}

// MarkNeedsAttention flags a config-class failure (401/403); it never
// auto-recovers — probe success or a provider edit clears it.
func (s *Store) MarkNeedsAttention(providerID string, v bool) error {
	h, err := s.GetProviderHealth(providerID)
	if err != nil {
		return err
	}
	h.NeedsAttention = v
	return s.upsertHealth(h)
}

// RecordProbe updates the probe streak; two consecutive OKs recover the
// provider (clear cooldown + attention, research/08 #17). Returns recovered.
func (s *Store) RecordProbe(providerID string, ok bool) (bool, error) {
	h, err := s.GetProviderHealth(providerID)
	if err != nil {
		return false, err
	}
	h.LastProbeAt = time.Now().Unix()
	if !ok {
		h.ConsecutiveProbeOK = 0
		return false, s.upsertHealth(h)
	}
	h.ConsecutiveProbeOK++
	recovered := h.ConsecutiveProbeOK >= 2
	if recovered {
		h.CooldownUntil = 0
		h.ConsecutiveProbeOK = 0
		h.NeedsAttention = false
	}
	return recovered, s.upsertHealth(h)
}

// ClearProviderHealth resets everything (provider edited → benefit of doubt).
func (s *Store) ClearProviderHealth(providerID string) error {
	_, err := s.db.Exec(`DELETE FROM provider_health WHERE provider_id=?`, providerID)
	return err
}

func (s *Store) upsertHealth(h ProviderHealth) error {
	attention := 0
	if h.NeedsAttention {
		attention = 1
	}
	_, err := s.db.Exec(`INSERT INTO provider_health
		(provider_id, demote_count, cooldown_until, needs_attention, last_probe_at, consecutive_probe_ok)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(provider_id) DO UPDATE SET demote_count=excluded.demote_count,
		cooldown_until=excluded.cooldown_until, needs_attention=excluded.needs_attention,
		last_probe_at=excluded.last_probe_at, consecutive_probe_ok=excluded.consecutive_probe_ok`,
		h.ProviderID, h.DemoteCount, h.CooldownUntil, attention, h.LastProbeAt, h.ConsecutiveProbeOK)
	return err
}

// HasRecentSuccess reports whether ANY OTHER device saw a 2xx through the
// provider within windowSec — the negative-evidence veto (research/08 #10).
func (s *Store) HasRecentSuccess(providerID, excludeDeviceID string, windowSec int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_records
		WHERE provider_id=? AND device_id<>? AND ts>=? AND status>=200 AND status<300`,
		providerID, excludeDeviceID, time.Now().Unix()-windowSec).Scan(&n)
	return n > 0, err
}

// LastModelForProvider returns the most recently used effective model through
// a provider — the natural probe model (research/08 #16). found=false when the
// provider has no usage yet.
func (s *Store) LastModelForProvider(providerID string) (string, bool, error) {
	var m string
	err := s.db.QueryRow(`SELECT `+effModelExpr+` FROM usage_records
		WHERE provider_id=? AND `+effModelExpr+` <> '' ORDER BY ts DESC, id DESC LIMIT 1`,
		providerID).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return m, true, nil
}
