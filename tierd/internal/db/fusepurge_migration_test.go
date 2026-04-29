package db

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFusePurgeMigrationDrops verifies migration 00007 has removed all
// legacy FUSE-daemon columns from the managed-namespace/target tables.
func TestFusePurgeMigrationDrops(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fusepurge.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	defer os.Remove(dbPath)

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Columns that must be gone after 00007.
	checks := []struct {
		table  string
		column string
	}{
		{"zfs_managed_targets", "fuse_mode"},
		{"zfs_managed_namespaces", "fuse_mode"},
		{"zfs_managed_namespaces", "socket_path"},
		{"zfs_managed_namespaces", "daemon_pid"},
		{"zfs_managed_namespaces", "daemon_state"},
		{"mdadm_managed_namespaces", "socket_path"},
		{"mdadm_managed_namespaces", "daemon_pid"},
		{"mdadm_managed_namespaces", "daemon_state"},
	}
	for _, c := range checks {
		q := "SELECT " + c.column + " FROM " + c.table + " LIMIT 1"
		if _, err := store.DB().Exec(q); err == nil {
			t.Errorf("%s.%s still present", c.table, c.column)
		}
	}
}
