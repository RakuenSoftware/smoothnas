package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openBackupRouteStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedSmoothfsBackupRoutePool(t *testing.T, store *db.Store) {
	t.Helper()
	if err := store.CreateTierPool("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 3},
	}); err != nil {
		t.Fatalf("CreateTierPool: %v", err)
	}
	if err := store.SetTierSlotFill("media", "NVME", 60, 90); err != nil {
		t.Fatalf("SetTierSlotFill: %v", err)
	}
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000001",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	}); err != nil {
		t.Fatalf("CreateSmoothfsPool: %v", err)
	}
}

func TestSmoothfsBulkIngestPathRoutesToBulkTier(t *testing.T) {
	store := openBackupRouteStore(t)
	seedSmoothfsBackupRoutePool(t, store)
	h := NewBackupHandler(store)

	got, routed, err := h.smoothfsBulkIngestPath(&db.BackupConfig{
		Method:    "rsync",
		Direction: "pull",
		LocalPath: "/mnt/media/storage/backup",
	})
	if err != nil {
		t.Fatalf("smoothfsBulkIngestPath: %v", err)
	}
	if !routed {
		t.Fatal("expected SmoothFS destination to route to bulk backing")
	}
	want := "/mnt/.tierd-backing/media/HDD/storage/backup"
	if got != want {
		t.Fatalf("route = %q, want %q", got, want)
	}
}

func TestSmoothfsBulkIngestPathSkipsSingleTierPool(t *testing.T) {
	store := openBackupRouteStore(t)
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000001",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	}); err != nil {
		t.Fatalf("CreateSmoothfsPool: %v", err)
	}
	h := NewBackupHandler(store)

	if got, routed, err := h.smoothfsBulkIngestPath(&db.BackupConfig{
		Method:    "rsync",
		Direction: "pull",
		LocalPath: "/mnt/media/storage/backup",
	}); err != nil || routed || got != "" {
		t.Fatalf("route = %q routed=%v err=%v, want no route", got, routed, err)
	}
}

func TestSmoothfsBulkIngestPathSkipsNonEmptyDestination(t *testing.T) {
	store := openBackupRouteStore(t)
	root := t.TempDir()
	mountpoint := filepath.Join(root, "media")
	dst := filepath.Join(mountpoint, "storage")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "existing.txt"), []byte("exists"), 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000001",
		Name:       "media",
		Tiers:      []string{filepath.Join(root, "fast"), filepath.Join(root, "bulk")},
		Mountpoint: mountpoint,
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	}); err != nil {
		t.Fatalf("CreateSmoothfsPool: %v", err)
	}
	h := NewBackupHandler(store)

	if got, routed, err := h.smoothfsBulkIngestPath(&db.BackupConfig{
		Method:    "rsync",
		Direction: "pull",
		LocalPath: dst,
	}); err != nil || routed || got != "" {
		t.Fatalf("route = %q routed=%v err=%v, want no route", got, routed, err)
	}
}

func TestSmoothfsBulkIngestPathSkipsDeleteMode(t *testing.T) {
	store := openBackupRouteStore(t)
	seedSmoothfsBackupRoutePool(t, store)
	h := NewBackupHandler(store)

	if got, routed, err := h.smoothfsBulkIngestPath(&db.BackupConfig{
		Method:     "rsync",
		Direction:  "pull",
		DeleteMode: true,
		LocalPath:  "/mnt/media/storage/backup",
	}); err != nil || routed || got != "" {
		t.Fatalf("route = %q routed=%v err=%v, want no route", got, routed, err)
	}
}
