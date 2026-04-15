package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

// ErrTierSlotInUse is returned by DeleteTierSlot when the slot has a PV
// currently assigned and cannot be removed. The API layer translates this to
// HTTP 409 Conflict.
var ErrTierSlotInUse = errors.New("tier slot in use")

// Store wraps the SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open creates or opens the SQLite database at the given path.
// Creates parent directories if they don't exist.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying sql.DB for use by other packages.
func (s *Store) DB() *sql.DB {
	return s.db
}
