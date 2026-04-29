package iscsi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateBackingFilePath(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "lun.img")
	if err := os.WriteFile(regular, []byte("lun"), 0o644); err != nil {
		t.Fatalf("seed regular: %v", err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("seed subdir: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"regular file, absolute", regular, false},
		{"relative path rejected", "relative/lun.img", true},
		{"newline in path rejected", "/tmp/lun\n.img", true},
		{"null byte in path rejected", "/tmp/lun\x00.img", true},
		{"missing file rejected", filepath.Join(dir, "absent.img"), true},
		{"directory rejected", subdir, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBackingFilePath(tc.path)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestIsOnSmoothfsOnTmp(t *testing.T) {
	// /tmp is tmpfs on most hosts; in either case it's not smoothfs.
	// The function must report false and no error for any real
	// non-smoothfs mount.
	on, err := IsOnSmoothfs(t.TempDir())
	if err != nil {
		t.Fatalf("statfs failed on tmpdir: %v", err)
	}
	if on {
		t.Fatal("tmpdir reported as smoothfs — constant or detector is wrong")
	}
}

func TestPinLUN_NonSmoothfsIsNoOp(t *testing.T) {
	// Backing file on tmpfs — PinLUN/UnpinLUN must silently succeed
	// without touching any xattr. An accidental setxattr here would
	// either ENOTSUP or install junk on tmpfs; either way the test
	// would fail.
	dir := t.TempDir()
	path := filepath.Join(dir, "lun.img")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := PinLUN(path); err != nil {
		t.Fatalf("PinLUN on non-smoothfs returned error: %v", err)
	}
	if err := UnpinLUN(path); err != nil {
		t.Fatalf("UnpinLUN on non-smoothfs returned error: %v", err)
	}
}

func TestInspectLUNPinNonSmoothfs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lun.img")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	status := InspectLUNPin(path)
	if status.Path != path {
		t.Fatalf("Path = %q, want %q", status.Path, path)
	}
	if status.OnSmoothfs {
		t.Fatal("OnSmoothfs = true, want false")
	}
	if status.Pinned {
		t.Fatal("Pinned = true, want false")
	}
	if status.State != "not_applicable" {
		t.Fatalf("State = %q, want not_applicable", status.State)
	}
}

func TestInspectLUNPinMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.img")

	status := InspectLUNPin(path)
	if status.State != "missing" {
		t.Fatalf("State = %q, want missing", status.State)
	}
	if status.Pinned {
		t.Fatal("Pinned = true, want false")
	}
}

func TestSmoothfsMagicConstant(t *testing.T) {
	// Guard against a typo in the kernel constant. 'SMOF' LE = 0x46_4F_4D_53.
	if SmoothfsMagic != 0x534D4F46 {
		t.Fatalf("SmoothfsMagic = %#x, want 0x534D4F46 ('SMOF')", SmoothfsMagic)
	}
}
