package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestRuntimeRootFromSmoothfsPoolsPrefersMediaFirstTier(t *testing.T) {
	got, ok := runtimeRootFromSmoothfsPools([]db.SmoothfsPool{
		{Name: "backup", Tiers: []string{"/mnt/.tierd-backing/backup/NVME"}},
		{Name: "media", Tiers: []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/SSD"}},
	})
	if !ok {
		t.Fatal("expected runtime root")
	}
	want := "/mnt/.tierd-backing/media/NVME/.smoothnas/runtime"
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestRuntimeRootFromSmoothfsPoolsFallsBackToFirstPool(t *testing.T) {
	got, ok := runtimeRootFromSmoothfsPools([]db.SmoothfsPool{
		{Name: "backup", Tiers: []string{"", "/mnt/.tierd-backing/backup/SSD"}},
	})
	if !ok {
		t.Fatal("expected runtime root")
	}
	want := "/mnt/.tierd-backing/backup/SSD/.smoothnas/runtime"
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestRuntimeRootFromSmoothfsPoolsNoTier(t *testing.T) {
	if got, ok := runtimeRootFromSmoothfsPools([]db.SmoothfsPool{{Name: "media"}}); ok {
		t.Fatalf("root = %q, want no root", got)
	}
}

func TestRuntimePathsFromRoot(t *testing.T) {
	lxcPath, statePath, err := runtimePathsFromRoot("/tier/.smoothnas/runtime")
	if err != nil {
		t.Fatalf("runtimePathsFromRoot: %v", err)
	}
	if lxcPath != "/tier/.smoothnas/runtime/lxc" {
		t.Fatalf("lxcPath = %q", lxcPath)
	}
	if statePath != "/tier/.smoothnas/runtime/state" {
		t.Fatalf("statePath = %q", statePath)
	}
	if _, _, err := runtimePathsFromRoot(" "); err == nil {
		t.Fatal("empty runtime root should fail")
	}
}

func TestDirHasEntries(t *testing.T) {
	if hasEntries, err := dirHasEntries(filepath.Join(t.TempDir(), "missing")); err != nil || hasEntries {
		t.Fatalf("missing dir: hasEntries=%v err=%v", hasEntries, err)
	}
	dir := t.TempDir()
	if hasEntries, err := dirHasEntries(dir); err != nil || hasEntries {
		t.Fatalf("empty dir: hasEntries=%v err=%v", hasEntries, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasEntries, err := dirHasEntries(dir); err != nil || !hasEntries {
		t.Fatalf("non-empty dir: hasEntries=%v err=%v", hasEntries, err)
	}
}

func TestMigrateRuntimeStorageMarkerRemovesOldPath(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newPath, ".migrated-from-var-lib"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := migrateRuntimeStorage(oldPath, newPath); err != nil {
		t.Fatalf("migrateRuntimeStorage: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists or unexpected stat error: %v", err)
	}
}

func TestMigrateRuntimeStorageRefusesUnmarkedNonEmptyTarget(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newPath, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := migrateRuntimeStorage(oldPath, newPath)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("migrateRuntimeStorage err = %v, want non-empty target error", err)
	}
}

func TestRewriteLXCConfigPaths(t *testing.T) {
	root := t.TempDir()
	newPath := filepath.Join(root, "new")
	containerPath := filepath.Join(newPath, "container")
	if err := os.MkdirAll(containerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(containerPath, "config")
	config := "lxc.rootfs.path = dir:/old/lxc/container/rootfs\nlxc.console.logfile = /old/lxc/log\n"
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteLXCConfigPaths("/old/lxc", newPath); err != nil {
		t.Fatalf("rewriteLXCConfigPaths: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "/old/lxc") {
		t.Fatalf("config still contains old path: %s", got)
	}
	if !strings.Contains(got, newPath+"/container/rootfs") {
		t.Fatalf("config missing new rootfs path: %s", got)
	}
}
