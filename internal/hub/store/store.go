// Package store is the Hub's authoritative persistence layer: a single
// SQLite file (ADR-0003, modernc driver — no CGO) with embedded sequential
// migrations tracked via PRAGMA user_version.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the SQLite handle; all DAOs hang off it.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// pending migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite is a single-writer database; a second writing conn just queues
	// behind busy_timeout. Keep the pool tiny and deterministic.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for e2e assertions and the M2 aggregation layer.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names) // 0001_..., 0002_... — lexicographic == numeric by convention

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i, name := range names {
		v := i + 1
		if v <= version {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		// PRAGMA cannot be parameterized; v is derived from the loop index.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: set user_version: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
