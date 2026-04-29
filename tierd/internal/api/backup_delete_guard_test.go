package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestSmoothfsPoolForPathChoosesLongestMountPrefix(t *testing.T) {
	pools := []db.SmoothfsPool{
		{Name: "top", Mountpoint: "/mnt/smoothfs"},
		{Name: "tank", Mountpoint: "/mnt/smoothfs/tank"},
	}

	got := smoothfsPoolForPath("/mnt/smoothfs/tank/backups/host1", pools)
	if got == nil || got.Name != "tank" {
		t.Fatalf("smoothfsPoolForPath chose %+v, want tank", got)
	}
}

func TestSmoothfsPoolForPathMiss(t *testing.T) {
	pools := []db.SmoothfsPool{{Name: "tank", Mountpoint: "/mnt/smoothfs/tank"}}
	if got := smoothfsPoolForPath("/srv/data", pools); got != nil {
		t.Fatalf("smoothfsPoolForPath returned %+v, want nil", got)
	}
}

func TestSmoothfsPoolHasAnySpill(t *testing.T) {
	root := t.TempDir()
	orig := spillFlagPathForUUID
	spillFlagPathForUUID = func(uuid string) string {
		return filepath.Join(root, uuid, "any_spill_since_mount")
	}
	t.Cleanup(func() { spillFlagPathForUUID = orig })

	uuid := "11111111-2222-4333-8444-555555555555"
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "any_spill_since_mount"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := smoothfsPoolHasAnySpill(uuid)
	if err != nil {
		t.Fatalf("smoothfsPoolHasAnySpill: %v", err)
	}
	if !got {
		t.Fatal("smoothfsPoolHasAnySpill = false, want true")
	}
}

func TestEffectiveDeleteMode(t *testing.T) {
	if effectiveDeleteMode("cp", true) {
		t.Fatal("effectiveDeleteMode(cp, true) = true, want false")
	}
	if !effectiveDeleteMode("rsync", true) {
		t.Fatal("effectiveDeleteMode(rsync, true) = false, want true")
	}
	if effectiveDeleteMode("rsync", false) {
		t.Fatal("effectiveDeleteMode(rsync, false) = true, want false")
	}
}
