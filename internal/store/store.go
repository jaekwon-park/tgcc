// Package store provides SQLite database access layer.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// migrate applies all SQL migration files in alphabetical order from the
// migrations directory located next to the executable.
func (s *Store) migrate() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	migrationsDir := filepath.Join(filepath.Dir(exe), "migrations")

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := s.execMigration(name, string(data)); err != nil {
			return err
		}
	}
	return nil
}

// execMigration runs a single migration file's SQL, skipping ALTER TABLE ADD COLUMN
// statements where the column already exists (safe for re-runs).
func (s *Store) execMigration(name, sql string) error {
	statements := splitStatements(sql)
	for _, stmt := range statements {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "ALTER TABLE") && strings.Contains(upper, "ADD COLUMN") {
			table, column, ok := parseAlterAddColumn(trimmed)
			if ok {
				exists, err := columnExists(s.DB, table, column)
				if err != nil {
					return fmt.Errorf("%s: check column %s.%s: %w", name, table, column, err)
				}
				if exists {
					continue
				}
			}
		}
		if _, err := s.DB.Exec(trimmed); err != nil {
			return fmt.Errorf("exec %s: %w", name, err)
		}
	}
	return nil
}

// splitStatements splits SQL text into individual statements by semicolon.
func splitStatements(sql string) []string {
	var parts []string
	for _, s := range strings.Split(sql, ";") {
		if strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return parts
}

// parseAlterAddColumn extracts table and column names from an ALTER TABLE ... ADD COLUMN statement.
func parseAlterAddColumn(stmt string) (table, column string, ok bool) {
	upper := strings.ToUpper(stmt)
	addIdx := strings.Index(upper, "ADD COLUMN")
	if addIdx < 0 {
		return "", "", false
	}
	// tokens: ALTER TABLE <table> ADD COLUMN <column> ...
	fields := strings.Fields(stmt)
	if len(fields) < 5 {
		return "", "", false
	}
	return fields[2], fields[4], true
}

// columnExists reports whether the given column exists in the given table.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var colName, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &colName, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(colName, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}
