package store

import (
	"database/sql"
	"errors"
	"time"
)

// Device mirrors the devices table (CONTEXT.md: 设备/配对).
type Device struct {
	ID        string
	Name      string
	Platform  string
	TokenHash string
	PairedAt  int64
	LastSeen  int64
	Revoked   bool
}

const deviceCols = `id, name, platform, token_hash, paired_at, last_seen, revoked`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var revoked int
	err := row.Scan(&d.ID, &d.Name, &d.Platform, &d.TokenHash, &d.PairedAt, &d.LastSeen, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.Revoked = revoked != 0
	return d, err
}

// CreateDevice registers a paired device (token already hashed by the caller).
func (s *Store) CreateDevice(d Device) error {
	_, err := s.db.Exec(`INSERT INTO devices (id, name, platform, token_hash, paired_at)
		VALUES (?,?,?,?,?)`, d.ID, d.Name, d.Platform, d.TokenHash, time.Now().Unix())
	return err
}

// ListDevices returns every device, newest pairing first.
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT ` + deviceCols + ` FROM devices ORDER BY paired_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FindDeviceByTokenHash resolves a live (non-revoked) device from a token
// hash — the WS handshake auth path.
func (s *Store) FindDeviceByTokenHash(hash string) (Device, error) {
	return scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices
		WHERE token_hash=? AND revoked=0`, hash))
}

// RevokeDevice marks the device revoked; its token stops working immediately.
func (s *Store) RevokeDevice(id string) error {
	res, err := s.db.Exec(`UPDATE devices SET revoked=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchDevice updates last_seen (heartbeat path).
func (s *Store) TouchDevice(id string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, time.Now().Unix(), id)
	return err
}
