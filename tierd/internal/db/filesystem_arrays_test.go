package db

import (
	"path/filepath"
	"testing"
)

func TestFilesystemArrayCRUD(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fsarrays.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}

	row, err := store.CreateFilesystemArray(&FilesystemArrayRow{
		Name:            "fast",
		Kind:            "btrfs",
		Label:           "smoothnas-btrfs-fast",
		MountPath:       "/mnt/fast",
		DataProfile:     "single",
		MetadataProfile: "raid1",
		State:           "active",
		SizeBytes:       300,
	}, []FilesystemArrayDeviceRow{
		{DevicePath: "/dev/sdb", SizeBytes: 100, State: "active"},
		{DevicePath: "/dev/sdc", SizeBytes: 200, State: "active"},
	})
	if err != nil {
		t.Fatalf("CreateFilesystemArray: %v", err)
	}
	if row.ID == 0 || len(row.Devices) != 2 {
		t.Fatalf("unexpected row: %#v", row)
	}
	if err := store.SetFilesystemArrayState("fast", "error", "boom"); err != nil {
		t.Fatalf("SetFilesystemArrayState: %v", err)
	}
	got, err := store.GetFilesystemArray("fast")
	if err != nil {
		t.Fatalf("GetFilesystemArray: %v", err)
	}
	if got.State != "error" || got.ErrorReason != "boom" {
		t.Fatalf("state = %q reason = %q", got.State, got.ErrorReason)
	}
	all, err := store.ListFilesystemArrays()
	if err != nil {
		t.Fatalf("ListFilesystemArrays: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("array count = %d", len(all))
	}
	if err := store.DeleteFilesystemArray("fast"); err != nil {
		t.Fatalf("DeleteFilesystemArray: %v", err)
	}
	if _, err := store.GetFilesystemArray("fast"); err != ErrNotFound {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
}
