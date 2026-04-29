package mdadm

import (
	"os"
	"strings"
	"testing"
	"time"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestReconcileEnsuresNonMdadmBackingMount(t *testing.T) {
	store := openTestStore(t)
	if err := store.CreateTierPool("media", "xfs", []db.TierDefinition{
		{Name: "HDD", Rank: 2},
	}); err != nil {
		t.Fatalf("CreateTierPool: %v", err)
	}
	if err := store.AssignBackingToTier("media", "HDD", "zfs", "tank"); err != nil {
		t.Fatalf("AssignBackingToTier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("TransitionTierInstanceState: %v", err)
	}

	origEnsure := ensureTierBackingMount
	origMounted := managedTargetIsMounted
	origSmoothfsMounted := smoothfsMountIsMounted
	origCreateSmoothfs := createSmoothfsManagedPool
	origDestroySmoothfs := destroySmoothfsManagedPool
	origRenderUnit := renderSmoothfsUnit
	origReadUnit := readSmoothfsUnitFile
	t.Cleanup(func() {
		ensureTierBackingMount = origEnsure
		managedTargetIsMounted = origMounted
		smoothfsMountIsMounted = origSmoothfsMounted
		createSmoothfsManagedPool = origCreateSmoothfs
		destroySmoothfsManagedPool = origDestroySmoothfs
		renderSmoothfsUnit = origRenderUnit
		readSmoothfsUnitFile = origReadUnit
	})

	var ensured struct {
		pool, tier, kind, ref, filesystem string
		calls                             int
	}
	backingMounted := false
	ensureTierBackingMount = func(poolName, tierName, kind, ref, filesystem string) error {
		ensured.pool = poolName
		ensured.tier = tierName
		ensured.kind = kind
		ensured.ref = ref
		ensured.filesystem = filesystem
		ensured.calls++
		backingMounted = true
		return nil
	}
	managedTargetIsMounted = func(path string) bool {
		return path == "/mnt/.tierd-backing/media/HDD" && backingMounted
	}
	smoothMounted := false
	var currentUnit []byte
	smoothfsMountIsMounted = func(path string) bool { return smoothMounted }
	renderSmoothfsUnit = func(pool smoothfsclient.ManagedPool) (string, error) {
		return "tiers=" + strings.Join(pool.Tiers, ":"), nil
	}
	createSmoothfsManagedPool = func(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
		smoothMounted = true
		mp := &smoothfsclient.ManagedPool{
			Name:       req.Name,
			UUID:       uuid.New(),
			Tiers:      req.Tiers,
			Mountpoint: "/mnt/" + req.Name,
			UnitPath:   "/etc/systemd/system/mnt-" + req.Name + ".mount",
		}
		body, err := renderSmoothfsUnit(*mp)
		if err != nil {
			t.Fatalf("RenderMountUnit: %v", err)
		}
		currentUnit = []byte(body)
		return mp, nil
	}
	destroySmoothfsManagedPool = func(smoothfsclient.ManagedPool) error {
		t.Fatal("destroySmoothfsManagedPool should not be called")
		return nil
	}
	readSmoothfsUnitFile = func(string) ([]byte, error) {
		if currentUnit == nil {
			return nil, os.ErrNotExist
		}
		return currentUnit, nil
	}

	a := NewAdapter(store, t.TempDir())
	if err := a.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if ensured.calls == 0 {
		t.Fatal("ensureTierBackingMount was not called")
	}
	if ensured.pool != "media" || ensured.tier != "HDD" || ensured.kind != "zfs" || ensured.ref != "tank" || ensured.filesystem != "xfs" {
		t.Fatalf("unexpected ensureTierBackingMount args: %#v", ensured)
	}

	var mt *db.MdadmManagedTargetRow
	var err error
	drainWithin(t, 500*time.Millisecond, func() bool {
		mt, err = store.GetMdadmManagedTargetByPoolTier("media", "HDD")
		return err == nil && mt != nil && mt.MountPath == "/mnt/.tierd-backing/media/HDD"
	})

	states, err := store.ListDegradedStates()
	if err != nil {
		t.Fatalf("ListDegradedStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("expected no degraded states, got %#v", states)
	}
}

func TestReconcileRepairsStaleSmoothfsMountUnit(t *testing.T) {
	store := openTestStore(t)
	if err := store.CreateTierPool("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 3},
	}); err != nil {
		t.Fatalf("CreateTierPool: %v", err)
	}
	arrayID, err := store.EnsureMDADMArray("/dev/md0")
	if err != nil {
		t.Fatalf("EnsureMDADMArray: %v", err)
	}
	if err := store.AssignArrayToTier("media", "NVME", arrayID, "/dev/md0"); err != nil {
		t.Fatalf("AssignArrayToTier: %v", err)
	}
	if err := store.AssignBackingToTier("media", "HDD", "zfs", "tank"); err != nil {
		t.Fatalf("AssignBackingToTier: %v", err)
	}
	if err := store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("TransitionTierInstanceState: %v", err)
	}
	_, err = store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "00000000-0000-0000-0000-000000000001",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("CreateSmoothfsPool: %v", err)
	}

	origEnsure := ensureTierBackingMount
	origMounted := managedTargetIsMounted
	origSmoothfsMounted := smoothfsMountIsMounted
	origCreateSmoothfs := createSmoothfsManagedPool
	origDestroySmoothfs := destroySmoothfsManagedPool
	origRenderUnit := renderSmoothfsUnit
	origReadUnit := readSmoothfsUnitFile
	t.Cleanup(func() {
		ensureTierBackingMount = origEnsure
		managedTargetIsMounted = origMounted
		smoothfsMountIsMounted = origSmoothfsMounted
		createSmoothfsManagedPool = origCreateSmoothfs
		destroySmoothfsManagedPool = origDestroySmoothfs
		renderSmoothfsUnit = origRenderUnit
		readSmoothfsUnitFile = origReadUnit
	})

	ensureTierBackingMount = func(poolName, tierName, kind, ref, filesystem string) error {
		return nil
	}
	managedTargetIsMounted = func(path string) bool {
		return path == "/mnt/.tierd-backing/media/NVME" || path == "/mnt/.tierd-backing/media/HDD"
	}
	smoothMounted := true
	smoothfsMountIsMounted = func(path string) bool { return smoothMounted }
	renderSmoothfsUnit = func(pool smoothfsclient.ManagedPool) (string, error) {
		return "tiers=" + strings.Join(pool.Tiers, ":"), nil
	}
	currentUnit := []byte("Options=pool=media,uuid=00000000-0000-0000-0000-000000000001,tiers=/mnt/.tierd-backing/media/HDD\n")
	readSmoothfsUnitFile = func(path string) ([]byte, error) {
		if path != "/etc/systemd/system/mnt-media.mount" {
			t.Fatalf("unexpected unit path %q", path)
		}
		return currentUnit, nil
	}
	destroyed := 0
	destroySmoothfsManagedPool = func(pool smoothfsclient.ManagedPool) error {
		destroyed++
		smoothMounted = false
		return nil
	}
	var createdTiers []string
	createSmoothfsManagedPool = func(req smoothfsclient.CreateManagedPoolRequest) (*smoothfsclient.ManagedPool, error) {
		createdTiers = append([]string(nil), req.Tiers...)
		smoothMounted = true
		mp := &smoothfsclient.ManagedPool{
			Name:       req.Name,
			UUID:       req.UUID,
			Tiers:      req.Tiers,
			Mountpoint: "/mnt/" + req.Name,
			UnitPath:   "/etc/systemd/system/mnt-" + req.Name + ".mount",
		}
		body, err := renderSmoothfsUnit(*mp)
		if err != nil {
			t.Fatalf("renderSmoothfsUnit: %v", err)
		}
		currentUnit = []byte(body)
		return mp, nil
	}

	a := NewAdapter(store, t.TempDir())
	if err := a.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if destroyed != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyed)
	}
	want := []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"}
	if len(createdTiers) != len(want) {
		t.Fatalf("created tiers = %#v, want %#v", createdTiers, want)
	}
	for i := range want {
		if createdTiers[i] != want[i] {
			t.Fatalf("created tiers = %#v, want %#v", createdTiers, want)
		}
	}
	pool, err := store.GetSmoothfsPool("media")
	if err != nil {
		t.Fatalf("GetSmoothfsPool: %v", err)
	}
	if len(pool.Tiers) != len(want) {
		t.Fatalf("stored tiers = %#v, want %#v", pool.Tiers, want)
	}
	for i := range want {
		if pool.Tiers[i] != want[i] {
			t.Fatalf("stored tiers = %#v, want %#v", pool.Tiers, want)
		}
	}
}
