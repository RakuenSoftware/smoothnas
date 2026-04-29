package db_test

import (
	"reflect"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestSmoothfsPoolCRUD(t *testing.T) {
	store := openTestDB(t)
	defer store.Close()

	created, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Name:       "tank",
		Tiers:      []string{"/mnt/fast", "/mnt/slow"},
		Mountpoint: "/mnt/smoothfs/tank",
		UnitPath:   "/etc/systemd/system/mnt-smoothfs-tank.mount",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.CreatedAt == "" {
		t.Fatal("CreatedAt not populated")
	}

	got, err := store.GetSmoothfsPool("tank")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UUID != created.UUID {
		t.Errorf("uuid = %q, want %q", got.UUID, created.UUID)
	}
	wantTiers := []string{"/mnt/fast", "/mnt/slow"}
	if !reflect.DeepEqual(got.Tiers, wantTiers) {
		t.Errorf("tiers = %v, want %v", got.Tiers, wantTiers)
	}

	got.Tiers = []string{"/mnt/fast", "/mnt/slow", "/mnt/archive"}
	got.Mountpoint = "/mnt/tank"
	got.UnitPath = "/etc/systemd/system/mnt-tank.mount"
	if err := store.UpdateSmoothfsPool(*got); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := store.GetSmoothfsPool("tank")
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	wantUpdatedTiers := []string{"/mnt/fast", "/mnt/slow", "/mnt/archive"}
	if updated.Mountpoint != "/mnt/tank" || updated.UnitPath != "/etc/systemd/system/mnt-tank.mount" || !reflect.DeepEqual(updated.Tiers, wantUpdatedTiers) {
		t.Fatalf("updated = %+v, want tiers %v", updated, wantUpdatedTiers)
	}

	// Duplicate name must be rejected.
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Name:       "tank",
		Tiers:      []string{"/mnt/other"},
		Mountpoint: "/mnt/smoothfs/tank",
		UnitPath:   "/etc/systemd/system/mnt-smoothfs-tank.mount",
	}); err != db.ErrDuplicate {
		t.Fatalf("expected db.ErrDuplicate, got %v", err)
	}

	// List returns the one we wrote.
	pools, err := store.ListSmoothfsPools()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "tank" {
		t.Fatalf("list = %+v, want one pool named tank", pools)
	}

	if err := store.DeleteSmoothfsPool("tank"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetSmoothfsPool("tank"); err != db.ErrNotFound {
		t.Errorf("get after delete: expected db.ErrNotFound, got %v", err)
	}
	if err := store.DeleteSmoothfsPool("tank"); err != db.ErrNotFound {
		t.Errorf("delete again: expected db.ErrNotFound, got %v", err)
	}
}
