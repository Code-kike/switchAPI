package store

import (
	"database/sql"
	"errors"
	"time"
)

// AppState is the global switch state for one App (CONTEXT.md: 切换 —
// 全局作用域，per-App)。device_id 列为二期设备覆盖预留，M1 恒 NULL。
type AppState struct {
	App              string
	ActiveProviderID string
	UpdatedAt        int64
	UpdatedBy        string
}

// GetAppState returns the active provider for app (ErrNotFound before the
// first switch).
func (s *Store) GetAppState(app string) (AppState, error) {
	var st AppState
	err := s.db.QueryRow(`SELECT app, active_provider_id, updated_at, updated_by
		FROM app_state WHERE app=?`, app).
		Scan(&st.App, &st.ActiveProviderID, &st.UpdatedAt, &st.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// SetAppState upserts the active provider for app.
func (s *Store) SetAppState(app, providerID, updatedBy string) error {
	_, err := s.db.Exec(`INSERT INTO app_state (app, active_provider_id, updated_at, updated_by)
		VALUES (?,?,?,?)
		ON CONFLICT(app) DO UPDATE SET
			active_provider_id=excluded.active_provider_id,
			updated_at=excluded.updated_at,
			updated_by=excluded.updated_by`,
		app, providerID, time.Now().Unix(), updatedBy)
	return err
}

// GetFallbackOrder returns provider ids for app in priority order.
func (s *Store) GetFallbackOrder(app string) ([]string, error) {
	rows, err := s.db.Query(`SELECT provider_id FROM fallback_orders
		WHERE app=? ORDER BY position`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetFallbackOrder atomically replaces the fallback order for app.
func (s *Store) SetFallbackOrder(app string, providerIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM fallback_orders WHERE app=?`, app); err != nil {
		return err
	}
	for i, id := range providerIDs {
		if _, err := tx.Exec(`INSERT INTO fallback_orders (app, provider_id, position)
			VALUES (?,?,?)`, app, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
