// Package store provides SQLite database access layer.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store is the central database accessor.
type Store struct {
	DB *sql.DB
}

// New opens (or creates) the SQLite database at the given path and runs migrations.
func New(dbPath string) (*Store, error) {
	// Create parent directory if needed
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.DB.Close()
}

// migrate applies SQL migration files in order from the migrations directory
// located next to the executable.
func (s *Store) migrate() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	migrationsDir := filepath.Join(filepath.Dir(exe), "migrations")
	migrationFile := filepath.Join(migrationsDir, "0001_init.sql")
	data, err := os.ReadFile(migrationFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(migrationFile), err)
	}
	if _, err := s.DB.Exec(string(data)); err != nil {
		return fmt.Errorf("exec %s: %w", filepath.Base(migrationFile), err)
	}
	return nil
}
