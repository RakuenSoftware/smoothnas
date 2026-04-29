package api

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"
)

func TestRenderManagedPoolUnitRequiresLowerMountUnits(t *testing.T) {
	root := t.TempDir()
	fast := filepath.Join(root, "fast")
	slow := filepath.Join(root, "slow")
	if err := os.Mkdir(fast, 0755); err != nil {
		t.Fatalf("mkdir fast: %v", err)
	}
	if err := os.Mkdir(slow, 0755); err != nil {
		t.Fatalf("mkdir slow: %v", err)
	}
	body, err := renderManagedPoolUnit(smoothfsclient.ManagedPool{
		Name:       "media",
		UUID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Tiers:      []string{fast, slow},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("renderManagedPoolUnit: %v", err)
	}
	if !strings.Contains(body, "Requires="+smoothfsclient.UnitFilenameFor(fast)) ||
		!strings.Contains(body, "Requires="+smoothfsclient.UnitFilenameFor(slow)) {
		t.Fatalf("unit must require lower mount units:\n%s", body)
	}
	if !strings.Contains(body, "After=local-fs-pre.target") {
		t.Fatalf("unit missing local-fs-pre ordering:\n%s", body)
	}
	if !strings.Contains(body, "Options=pool=media,uuid=00000000-0000-0000-0000-000000000001,tiers="+fast+":"+slow) {
		t.Fatalf("unit missing smoothfs options:\n%s", body)
	}
}

func TestEnsureManagedSmoothfsPoolCreatesMountAtTierPath(t *testing.T) {
	store := openSmoothfsUnitTestStore(t)
	if err := store.CreateTierPoolWithOptions("media", "xfs", nil, false); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	arrayID, err := store.EnsureMDADMArray("/dev/md0")
	if err != nil {
		t.Fatalf("ensure array: %v", err)
	}
	if err := store.AssignArrayToTier("media", "HDD", arrayID, "/dev/md0"); err != nil {
		t.Fatalf("assign tier: %v", err)
	}

	origCreate := createManagedPoolForSystem
	origUsable := managedSmoothfsTierUsable
	t.Cleanup(func() {
		createManagedPoolForSystem = origCreate
		managedSmoothfsTierUsable = origUsable
	})
	managedSmoothfsTierUsable = func(string) bool { return true }

	var gotReq smoothfsclient.CreateManagedPoolRequest
	createManagedPoolForSystem = func(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
		gotReq = req
		return &smoothfsclient.ManagedPool{
			Name:       req.Name,
			UUID:       uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			Tiers:      req.Tiers,
			Mountpoint: "/mnt/" + req.Name,
			UnitPath:   "/etc/systemd/system/mnt-" + req.Name + ".mount",
		}, nil
	}

	if err := ensureManagedSmoothfsPool(store, "media"); err != nil {
		t.Fatalf("ensure managed smoothfs pool: %v", err)
	}
	if gotReq.Name != "media" {
		t.Fatalf("request name = %q, want media", gotReq.Name)
	}
	if gotReq.MountBase != "/mnt" {
		t.Fatalf("request mount base = %q, want /mnt", gotReq.MountBase)
	}
	wantTiers := []string{"/mnt/.tierd-backing/media/HDD"}
	if !reflect.DeepEqual(gotReq.Tiers, wantTiers) {
		t.Fatalf("request tiers = %#v, want %#v", gotReq.Tiers, wantTiers)
	}
	pool, err := store.GetSmoothfsPool("media")
	if err != nil {
		t.Fatalf("get smoothfs pool: %v", err)
	}
	if pool.Mountpoint != "/mnt/media" {
		t.Fatalf("mountpoint = %q, want /mnt/media", pool.Mountpoint)
	}
}

func TestEnsureManagedSmoothfsPoolReplacesStaleTierList(t *testing.T) {
	store := openSmoothfsUnitTestStore(t)
	if err := store.CreateTierPoolWithOptions("media", "xfs", nil, false); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	arrayID, err := store.EnsureMDADMArray("/dev/md0")
	if err != nil {
		t.Fatalf("ensure array: %v", err)
	}
	if err := store.AssignArrayToTier("media", "NVME", arrayID, "/dev/md0"); err != nil {
		t.Fatalf("assign tier: %v", err)
	}
	_, err = store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000004",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("create stale smoothfs pool: %v", err)
	}

	origCreate := createManagedPoolForSystem
	origDestroy := destroyManagedPoolForSystem
	origUsable := managedSmoothfsTierUsable
	t.Cleanup(func() {
		createManagedPoolForSystem = origCreate
		destroyManagedPoolForSystem = origDestroy
		managedSmoothfsTierUsable = origUsable
	})
	managedSmoothfsTierUsable = func(path string) bool {
		return path == "/mnt/.tierd-backing/media/NVME"
	}

	var destroyed smoothfsclient.ManagedPool
	destroyManagedPoolForSystem = func(pool smoothfsclient.ManagedPool) error {
		destroyed = pool
		return nil
	}
	var gotReq smoothfsclient.CreateManagedPoolRequest
	createManagedPoolForSystem = func(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
		gotReq = req
		return &smoothfsclient.ManagedPool{
			Name:       req.Name,
			UUID:       req.UUID,
			Tiers:      req.Tiers,
			Mountpoint: "/mnt/" + req.Name,
			UnitPath:   "/etc/systemd/system/mnt-" + req.Name + ".mount",
		}, nil
	}

	if err := ensureManagedSmoothfsPool(store, "media"); err != nil {
		t.Fatalf("ensure managed smoothfs pool: %v", err)
	}
	if destroyed.Tiers[0] != "/mnt/.tierd-backing/media/HDD" {
		t.Fatalf("destroyed old tiers = %#v", destroyed.Tiers)
	}
	wantTiers := []string{"/mnt/.tierd-backing/media/NVME"}
	if !reflect.DeepEqual(gotReq.Tiers, wantTiers) {
		t.Fatalf("request tiers = %#v, want %#v", gotReq.Tiers, wantTiers)
	}
	pool, err := store.GetSmoothfsPool("media")
	if err != nil {
		t.Fatalf("get smoothfs pool: %v", err)
	}
	if !reflect.DeepEqual(pool.Tiers, wantTiers) {
		t.Fatalf("stored tiers = %#v, want %#v", pool.Tiers, wantTiers)
	}
}

func TestEnsureManagedSmoothfsPoolRefusesPartialAssignedTierSet(t *testing.T) {
	store := openSmoothfsUnitTestStore(t)
	if err := store.CreateTierPool("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 3},
	}); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	arrayID, err := store.EnsureMDADMArray("/dev/md0")
	if err != nil {
		t.Fatalf("ensure array: %v", err)
	}
	if err := store.AssignArrayToTier("media", "NVME", arrayID, "/dev/md0"); err != nil {
		t.Fatalf("assign nvme tier: %v", err)
	}
	if err := store.AssignBackingToTier("media", "HDD", "zfs", "tank"); err != nil {
		t.Fatalf("assign hdd tier: %v", err)
	}
	_, err = store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000005",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("create smoothfs pool: %v", err)
	}

	origCreate := createManagedPoolForSystem
	origDestroy := destroyManagedPoolForSystem
	origUsable := managedSmoothfsTierUsable
	t.Cleanup(func() {
		createManagedPoolForSystem = origCreate
		destroyManagedPoolForSystem = origDestroy
		managedSmoothfsTierUsable = origUsable
	})
	managedSmoothfsTierUsable = func(path string) bool {
		return path == "/mnt/.tierd-backing/media/NVME"
	}
	createManagedPoolForSystem = func(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
		t.Fatalf("createManagedPoolForSystem should not be called for a partial assigned tier set")
		return nil, nil
	}
	destroyManagedPoolForSystem = func(pool smoothfsclient.ManagedPool) error {
		t.Fatalf("destroyManagedPoolForSystem should not be called for a partial assigned tier set")
		return nil
	}

	if err := ensureManagedSmoothfsPool(store, "media"); err == nil || !strings.Contains(err.Error(), "HDD") {
		t.Fatalf("expected missing HDD backing error, got %v", err)
	}
	pool, err := store.GetSmoothfsPool("media")
	if err != nil {
		t.Fatalf("get smoothfs pool: %v", err)
	}
	wantTiers := []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"}
	if !reflect.DeepEqual(pool.Tiers, wantTiers) {
		t.Fatalf("stored tiers changed to %#v, want %#v", pool.Tiers, wantTiers)
	}
}

func TestDestroyManagedSmoothfsPoolDeletesUnitAndRow(t *testing.T) {
	store := openSmoothfsUnitTestStore(t)
	_, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000003",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("create smoothfs pool: %v", err)
	}

	origDestroy := destroyManagedPoolForSystem
	t.Cleanup(func() { destroyManagedPoolForSystem = origDestroy })

	var destroyed smoothfsclient.ManagedPool
	destroyManagedPoolForSystem = func(pool smoothfsclient.ManagedPool) error {
		destroyed = pool
		return nil
	}

	if err := destroyManagedSmoothfsPool(store, "media"); err != nil {
		t.Fatalf("destroy managed smoothfs pool: %v", err)
	}
	if destroyed.Mountpoint != "/mnt/media" {
		t.Fatalf("destroyed mountpoint = %q, want /mnt/media", destroyed.Mountpoint)
	}
	if _, err := store.GetSmoothfsPool("media"); err != db.ErrNotFound {
		t.Fatalf("get after destroy = %v, want ErrNotFound", err)
	}
}

func openSmoothfsUnitTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "smoothfs-units.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}
