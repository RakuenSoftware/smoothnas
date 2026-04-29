package db

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func init() {
	goose.SetBaseFS(migrationsFS)
}

// Migrate runs all pending migrations against the store's connection.
func (s *Store) Migrate() error {
	return MigrateDB(s.db)
}

// MigrateDB runs all pending migrations against an arbitrary *sql.DB.
// Exported so packages that hold their own connection (notably the
// smart subsystem in tests) can bring up the schema without going
// through Store.
func MigrateDB(db *sql.DB) error {
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
