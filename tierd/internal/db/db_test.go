package db_test

import (
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openTestDB(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func openRawTestDB(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMigrations(t *testing.T) {
	store := openTestDB(t)

	// Running migrate again should be a no-op.
	if err := store.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}


