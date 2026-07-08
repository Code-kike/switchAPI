package store

import (
	"database/sql"
	"errors"
	"time"
)

// Event is one entry of the 事件时间线.
type Event struct {
	ID      int64
	TS      int64
	Kind    string // switch | failover | pairing | backup | speedtest | probe | auth ...
	Payload string // JSON object text
}

// AppendEvent records an event; payload must be valid JSON object text (the
// caller marshals). The inserted row is returned so callers can fan it out to
// live UI clients (M3 ws/ui) without a re-query.
func (s *Store) AppendEvent(kind, payload string) (Event, error) {
	if payload == "" {
		payload = "{}"
	}
	ts := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO events (ts, kind, payload) VALUES (?,?,?)`,
		ts, kind, payload)
	if err != nil {
		return Event{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, TS: ts, Kind: kind, Payload: payload}, nil
}

// RecentEvents returns up to n events, newest first.
func (s *Store) RecentEvents(n int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, ts, kind, payload FROM events
		ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetSetting returns the value for key ("" and found=false when unset).
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSetting upserts a settings key.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
