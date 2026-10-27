package fsarray

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildPlanBtrfsDefaults(t *testing.T) {
	plan, err := BuildPlan("fast", KindBtrfs, "", "", "", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.DataProfile != "single" || plan.MetadataProfile != "raid1" {
		t.Fatalf("profiles = %s/%s, want single/raid1", plan.DataProfile, plan.MetadataProfile)
	}
	if plan.Label != "smoothnas-btrfs-fast" || plan.MountPath != "/mnt/fast" || plan.SizeBytes != 300 {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	plan, err = BuildPlan("single", KindBtrfs, "", "", "", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
	})
	if err != nil {
		t.Fatalf("single-disk BuildPlan: %v", err)
	}
	if plan.DataProfile != "single" || plan.MetadataProfile != "dup" {
		t.Fatalf("single-disk profiles = %s/%s, want single/dup", plan.DataProfile, plan.MetadataProfile)
	}
}

func TestBuildPlanBcachefsReplicas(t *testing.T) {
	if _, err := BuildPlan("archive", KindBcachefs, "", "", "", 3, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 100},
	}); err == nil {
		t.Fatal("expected replicas > device count to fail")
	}
	plan, err := BuildPlan("archive", KindBcachefs, "", "", "", 2, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 100},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Replicas != 2 || plan.DataProfile != "" || plan.MetadataProfile != "" {
		t.Fatalf("unexpected bcachefs plan: %#v", plan)
	}
}

func TestBuildCommandArgs(t *testing.T) {
	plan, err := BuildPlan("fast", KindBtrfs, "", "raid1", "raid1", 0, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantBtrfs := []string{"mkfs.btrfs", "-f", "-L", "smoothnas-btrfs-fast", "-d", "raid1", "-m", "raid1", "/dev/sdb", "/dev/sdc"}
	if got := BuildMkfsBtrfsArgs(plan); !reflect.DeepEqual(got, wantBtrfs) {
		t.Fatalf("btrfs args = %#v, want %#v", got, wantBtrfs)
	}

	plan, err = BuildPlan("cache", KindBcachefs, "", "", "", 2, []Device{
		{Path: "/dev/sdb", Size: 100},
		{Path: "/dev/sdc", Size: 200},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantBcachefs := []string{"bcachefs", "format", "--replicas=2", "/dev/sdb", "/dev/sdc"}
	if got := BuildBcachefsFormatArgs(plan); !reflect.DeepEqual(got, wantBcachefs) {
		t.Fatalf("bcachefs args = %#v, want %#v", got, wantBcachefs)
	}
	if got := BcachefsMountSource(plan); got != "/dev/sdb:/dev/sdc" {
		t.Fatalf("mount source = %q", got)
	}
}

func TestBcachefsFSTabUsesFilesystemIdentity(t *testing.T) {
	oldPath, oldUUID := fstabPath, bcachefsUUIDForDevice
	t.Cleanup(func() { fstabPath, bcachefsUUIDForDevice = oldPath, oldUUID })
	fstabPath = filepath.Join(t.TempDir(), "fstab")
	bcachefsUUIDForDevice = func(device string) (string, error) {
		if device != "/dev/sdb" {
			t.Fatalf("UUID device = %s", device)
		}
		return "4a40986a-67d5-46d7-ae1a-04ed1078941b", nil
	}
	if err := ensurePersistentFSTabEntry(KindBcachefs, "/dev/sdb:/dev/sdc", "/mnt/archive"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fstabPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "UUID=4a40986a-67d5-46d7-ae1a-04ed1078941b /mnt/archive bcachefs defaults,nofail 0 0\n"
	if string(data) != want {
		t.Fatalf("fstab = %q", data)
	}
}

func TestStableMemberIdentitySurvivesDiskNameChange(t *testing.T) {
	oldID, oldPath := deviceByIDDir, deviceByPathDir
	t.Cleanup(func() { deviceByIDDir, deviceByPathDir = oldID, oldPath })
	root := t.TempDir()
	deviceByIDDir = filepath.Join(root, "by-id")
	deviceByPathDir = filepath.Join(root, "by-path")
	if err := os.MkdirAll(deviceByIDDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldDevice, newDevice := filepath.Join(root, "sdb"), filepath.Join(root, "sdf")
	if err := os.WriteFile(oldDevice, []byte("disk"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(deviceByIDDir, "wwn-unique")
	if err := os.Symlink(oldDevice, alias); err != nil {
		t.Fatal(err)
	}
	recorded := StableDevicePath(oldDevice)
	if recorded != alias {
		t.Fatalf("recorded member = %s", recorded)
	}
	if err := os.Rename(oldDevice, newDevice); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newDevice, alias); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(recorded)
	if err != nil || resolved != newDevice {
		t.Fatalf("member after enumeration change: %s, %v", resolved, err)
	}
}
