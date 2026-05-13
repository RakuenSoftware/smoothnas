package db_test

import (
	"errors"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestNonRaidArrayCRUD(t *testing.T) {
	store := openTestDB(t)

	created, err := store.CreateNonRaidArray(&db.NonRaidArrayRow{
		Name:           "media",
		State:          "configured",
		Filesystem:     "xfs",
		MountPath:      "/mnt/media",
		ParityCount:    2,
		MinParityBytes: 10 << 40,
		CapacityBytes:  18 << 40,
	}, []db.NonRaidDeviceRow{
		{Role: "data", Slot: 1, DevicePath: "/dev/sdb", Serial: "data1", SizeBytes: 8 << 40, UsableBytes: 8 << 40, MountPath: "/mnt/.nonraid/media/disk1", State: "configured"},
		{Role: "data", Slot: 2, DevicePath: "/dev/sdc", Serial: "data2", SizeBytes: 10 << 40, UsableBytes: 10 << 40, MountPath: "/mnt/.nonraid/media/disk2", State: "configured"},
		{Role: "parity", Slot: 1, DevicePath: "/dev/sdd", Serial: "parity1", SizeBytes: 10 << 40, UsableBytes: 10 << 40, State: "configured"},
		{Role: "parity", Slot: 2, DevicePath: "/dev/sde", Serial: "parity2", SizeBytes: 12 << 40, UsableBytes: 10 << 40, State: "configured"},
	})
	if err != nil {
		t.Fatalf("CreateNonRaidArray: %v", err)
	}
	if created.ID == 0 || len(created.Devices) != 4 {
		t.Fatalf("created row = %+v", created)
	}

	listed, err := store.ListNonRaidArrays()
	if err != nil {
		t.Fatalf("ListNonRaidArrays: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "media" || len(listed[0].Devices) != 4 {
		t.Fatalf("listed = %+v", listed)
	}

	if err := store.DeleteNonRaidArray("media"); err != nil {
		t.Fatalf("DeleteNonRaidArray: %v", err)
	}
	if _, err := store.GetNonRaidArray("media"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
}

func TestNonRaidDevicePathUnique(t *testing.T) {
	store := openTestDB(t)
	_, err := store.CreateNonRaidArray(&db.NonRaidArrayRow{
		Name:           "media",
		State:          "configured",
		Filesystem:     "xfs",
		MountPath:      "/mnt/media",
		ParityCount:    1,
		MinParityBytes: 10 << 40,
		CapacityBytes:  8 << 40,
	}, []db.NonRaidDeviceRow{
		{Role: "data", Slot: 1, DevicePath: "/dev/sdb", SizeBytes: 8 << 40, UsableBytes: 8 << 40, State: "configured"},
		{Role: "parity", Slot: 1, DevicePath: "/dev/sdb", SizeBytes: 10 << 40, UsableBytes: 10 << 40, State: "configured"},
	})
	if err == nil {
		t.Fatal("CreateNonRaidArray succeeded with duplicate device path")
	}
}
