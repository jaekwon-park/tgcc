// Package store provides SQLite data access layer for tgcc.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the SQLite database connection.
type Store struct {
	DB *sql.DB
}

// Open opens (or creates) the SQLite database at the given path and runs migrations.
func Open(dbPath string) (*Store, error) {
	// Ensure parent directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}

	// Run migrations
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	slog.Info("store: opened", "path", dbPath)
	return &Store{DB: db}, nil
}

func runMigrations(db *sql.DB) error {
	// Read migration file
	data, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		return fmt.Errorf("reading migration: %w", err)
	}

	_, err = db.Exec(string(data))
	if err != nil {
		return fmt.Errorf("executing migration: %w", err)
	}

	slog.Info("store: migrations applied")
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.DB.Close()
}
