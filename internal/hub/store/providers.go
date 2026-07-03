package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned by lookups that match nothing.
var ErrNotFound = errors.New("store: not found")

// Provider mirrors the providers table. APIKeyEnc is opaque ciphertext —
// encryption/decryption lives in the API layer (cryptoutil + master key).
type Provider struct {
	ID              string
	Name            string
	Protocol        string // "anthropic" | "openai"
	BaseURL         string
	APIKeyEnc       []byte
	ModelRedirects  string // JSON object text
	CostCoefficient float64
	PresetID        string
	Sort            int
	Note            string
	CreatedAt       int64
	UpdatedAt       int64
}

const providerCols = `id, name, protocol, base_url, api_key_enc, model_redirects,
	cost_coefficient, preset_id, sort, note, created_at, updated_at`

func scanProvider(row interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	err := row.Scan(&p.ID, &p.Name, &p.Protocol, &p.BaseURL, &p.APIKeyEnc, &p.ModelRedirects,
		&p.CostCoefficient, &p.PresetID, &p.Sort, &p.Note, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CreateProvider inserts p (ID must be set by the caller).
func (s *Store) CreateProvider(p Provider) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO providers (`+providerCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Protocol, p.BaseURL, p.APIKeyEnc, p.ModelRedirects,
		p.CostCoefficient, p.PresetID, p.Sort, p.Note, now, now)
	return err
}

// UpdateProvider rewrites every mutable column of p. An empty APIKeyEnc keeps
// the stored ciphertext (so the UI can edit a provider without re-entering
// the key).
func (s *Store) UpdateProvider(p Provider) error {
	now := time.Now().Unix()
	var res sql.Result
	var err error
	if len(p.APIKeyEnc) == 0 {
		res, err = s.db.Exec(`UPDATE providers SET name=?, protocol=?, base_url=?,
			model_redirects=?, cost_coefficient=?, preset_id=?, sort=?, note=?, updated_at=?
			WHERE id=?`,
			p.Name, p.Protocol, p.BaseURL, p.ModelRedirects, p.CostCoefficient,
			p.PresetID, p.Sort, p.Note, now, p.ID)
	} else {
		res, err = s.db.Exec(`UPDATE providers SET name=?, protocol=?, base_url=?, api_key_enc=?,
			model_redirects=?, cost_coefficient=?, preset_id=?, sort=?, note=?, updated_at=?
			WHERE id=?`,
			p.Name, p.Protocol, p.BaseURL, p.APIKeyEnc, p.ModelRedirects, p.CostCoefficient,
			p.PresetID, p.Sort, p.Note, now, p.ID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProvider removes the provider. It fails (foreign key) while the
// provider is referenced by app_state — callers must switch away first.
func (s *Store) DeleteProvider(id string) error {
	res, err := s.db.Exec(`DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetProvider fetches one provider by id.
func (s *Store) GetProvider(id string) (Provider, error) {
	return scanProvider(s.db.QueryRow(`SELECT `+providerCols+` FROM providers WHERE id=?`, id))
}

// ListProviders returns all providers ordered by sort, then name.
func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT ` + providerCols + ` FROM providers ORDER BY sort, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
