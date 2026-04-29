package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreateDatasetArgsIncludesManagedProperties(t *testing.T) {
	got := createDatasetArgs("tank/tierd/media/HDD")
	want := []string{
		"create", "-p",
		"-o", "mountpoint=legacy",
		"-o", "compression=lz4",
		"-o", "atime=off",
		"-o", "recordsize=1M",
		"-o", "logbias=throughput",
		"-o", "xattr=sa",
		"tank/tierd/media/HDD",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("createDatasetArgs = %#v, want %#v", got, want)
	}
}

func TestEnsureLegacyFSTabEntryIdempotent(t *testing.T) {
	orig := zfsFSTabPath
	zfsFSTabPath = filepath.Join(t.TempDir(), "fstab")
	defer func() { zfsFSTabPath = orig }()

	if err := ensureLegacyFSTabEntry("tank/tierd/media/HDD", "/mnt/.tierd-backing/media/HDD"); err != nil {
		t.Fatalf("ensureLegacyFSTabEntry: %v", err)
	}
	if err := ensureLegacyFSTabEntry("tank/tierd/media/HDD", "/mnt/.tierd-backing/media/HDD"); err != nil {
		t.Fatalf("ensureLegacyFSTabEntry second call: %v", err)
	}

	data, err := os.ReadFile(zfsFSTabPath)
	if err != nil {
		t.Fatalf("read fstab: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 fstab line, got %d: %q", len(lines), string(data))
	}
	want := "tank/tierd/media/HDD /mnt/.tierd-backing/media/HDD zfs defaults,nofail 0 0"
	if lines[0] != want {
		t.Fatalf("fstab line = %q, want %q", lines[0], want)
	}
}

func TestEnsureLegacyFSTabEntryUpgradesExistingEntry(t *testing.T) {
	orig := zfsFSTabPath
	zfsFSTabPath = filepath.Join(t.TempDir(), "fstab")
	defer func() { zfsFSTabPath = orig }()

	initial := "tank/tierd/media/HDD /mnt/.tierd-backing/media/HDD zfs defaults 0 0\n"
	if err := os.WriteFile(zfsFSTabPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("seed fstab: %v", err)
	}

	if err := ensureLegacyFSTabEntry("tank/tierd/media/HDD", "/mnt/.tierd-backing/media/HDD"); err != nil {
		t.Fatalf("ensureLegacyFSTabEntry: %v", err)
	}

	data, err := os.ReadFile(zfsFSTabPath)
	if err != nil {
		t.Fatalf("read fstab: %v", err)
	}
	want := "tank/tierd/media/HDD /mnt/.tierd-backing/media/HDD zfs defaults,nofail 0 0"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("fstab line = %q, want %q", got, want)
	}
}

func TestRemoveLegacyFSTabEntryRemovesMatchingDatasetAndMount(t *testing.T) {
	orig := zfsFSTabPath
	zfsFSTabPath = filepath.Join(t.TempDir(), "fstab")
	defer func() { zfsFSTabPath = orig }()

	initial := "" +
		"tank/tierd/media/HDD /mnt/.tierd-backing/media/HDD zfs defaults 0 0\n" +
		"tank/other /mnt/other zfs defaults 0 0\n"
	if err := os.WriteFile(zfsFSTabPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("seed fstab: %v", err)
	}

	if err := removeLegacyFSTabEntry("tank/tierd/media/HDD", "/mnt/.tierd-backing/media/HDD"); err != nil {
		t.Fatalf("removeLegacyFSTabEntry: %v", err)
	}

	data, err := os.ReadFile(zfsFSTabPath)
	if err != nil {
		t.Fatalf("read fstab: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "tank/other /mnt/other zfs defaults 0 0"
	if got != want {
		t.Fatalf("remaining fstab = %q, want %q", got, want)
	}
}
