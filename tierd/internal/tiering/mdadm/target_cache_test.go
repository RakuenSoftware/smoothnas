package mdadm

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// seedManagedNamespace inserts a managed_namespaces row so an
// mdadm_managed_namespaces row can be inserted (FK). Returns the
// generated namespace_id.
func seedManagedNamespace(t *testing.T, store *db.Store, name, poolName string) string {
	t.Helper()
	ns := &db.ManagedNamespaceRow{
		Name:            name,
		PlacementDomain: poolName,
		BackendKind:     BackendKind,
		NamespaceKind:   "filespace",
		ExposedPath:     "/mnt/" + poolName,
		PinState:        "none",
		Health:          "healthy",
		PlacementState:  "unknown",
		BackendRef:      "mdadm:ns:" + poolName,
	}
	if err := store.CreateManagedNamespace(ns); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}
	return ns.ID
}

func seedTierTarget(t *testing.T, store *db.Store, id, domain, backingRef string, rank int) *db.TierTargetRow {
	t.Helper()
	row := &db.TierTargetRow{
		ID:              id,
		Name:            id,
		PlacementDomain: domain,
		BackendKind:     BackendKind,
		Rank:            rank,
		Health:          "healthy",
		BackingRef:      backingRef,
	}
	if err := store.CreateTierTarget(row); err != nil {
		t.Fatalf("seed tier target %s: %v", id, err)
	}
	return row
}

// drainWithin waits up to d for the drain goroutine to catch up. It
// relies on the observable effect: the store should reflect whatever
// ops the test enqueued.
func drainWithin(t *testing.T, d time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("drain did not complete within %s", d)
}

func TestTargetCacheServesReadsFromMemory(t *testing.T) {
	store := openTestStore(t)
	// Seed directly through the store so the cache's initial reload
	// picks them up.
	seedTierTarget(t, store, "tt-1", "pool-a", "pool-a/HDD", 3)
	if err := store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: "tt-1",
		PoolName:     "pool-a",
		TierName:     "HDD",
		VGName:       "tier-pool-a-HDD",
		LVName:       "data",
		MountPath:    "/mnt/.tierd-backing/pool-a/HDD",
	}); err != nil {
		t.Fatalf("seed mdadm target: %v", err)
	}

	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	got := c.listMdadmTargets()
	if len(got) != 1 || got[0].TierTargetID != "tt-1" {
		t.Fatalf("listMdadmTargets = %+v", got)
	}
	if mt, ok := c.getMdadmByPoolTier("pool-a", "HDD"); !ok || mt.MountPath != "/mnt/.tierd-backing/pool-a/HDD" {
		t.Fatalf("getMdadmByPoolTier missing or wrong: %+v ok=%v", mt, ok)
	}
	if tt, ok := c.getTierByBackingRef("pool-a/HDD", BackendKind); !ok || tt.Rank != 3 {
		t.Fatalf("getTierByBackingRef = %+v ok=%v", tt, ok)
	}
}

func TestTargetCacheWriteThroughIsVisibleImmediately(t *testing.T) {
	store := openTestStore(t)
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	// Seed a placement domain row the tier_target references.
	seedTierTarget(t, store, "tt-seed", "pool-b", "pool-b/seed", 1)
	if err := c.createTierTarget(&db.TierTargetRow{
		ID:              "tt-new",
		Name:            "tt-new",
		PlacementDomain: "pool-b",
		BackendKind:     BackendKind,
		Rank:            2,
		Health:          "healthy",
		BackingRef:      "pool-b/NVME",
	}); err != nil {
		t.Fatalf("createTierTarget: %v", err)
	}
	// The write is visible in memory before the drain touches SQLite.
	if _, ok := c.getTierByBackingRef("pool-b/NVME", BackendKind); !ok {
		t.Fatal("memory read should return the just-written tier target")
	}
	c.upsertMdadmTarget(&db.MdadmManagedTargetRow{
		TierTargetID: "tt-new",
		PoolName:     "pool-b",
		TierName:     "NVME",
		VGName:       "tier-pool-b-NVME",
		LVName:       "data",
		MountPath:    "/mnt/.tierd-backing/pool-b/NVME",
	})
	if _, ok := c.getMdadmByPoolTier("pool-b", "NVME"); !ok {
		t.Fatal("memory read should return the just-written managed target")
	}

	// Drain lag is at most drainTick; give it generous headroom.
	drainWithin(t, 500*time.Millisecond, func() bool {
		got, err := store.GetTierTargetByBackingRef("pool-b/NVME", BackendKind)
		return err == nil && got != nil && got.ID == "tt-new"
	})
	drainWithin(t, 500*time.Millisecond, func() bool {
		got, err := store.GetMdadmManagedTargetByPoolTier("pool-b", "NVME")
		return err == nil && got != nil && got.MountPath == "/mnt/.tierd-backing/pool-b/NVME"
	})
}

// Regression: if createTierTarget is called with an empty ID, the cache
// must still return a row whose ID is valid so that a follow-up
// upsertMdadmTarget (which keys off tier_target_id via a FK) survives
// drain to SQLite. The bug surfaced when provisioning a tier with two
// fresh arrays: reconcile loop #1 enqueued the tier_target with no ID,
// loop #2 read it from the cache and enqueued a managed target with
// TierTargetID="", and drain then failed the FK constraint.
func TestTargetCacheCreateTierTargetWithoutIDSurvivesFKAtDrain(t *testing.T) {
	store := openTestStore(t)
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	row := &db.TierTargetRow{
		Name:            "NVME",
		PlacementDomain: "pool-fk",
		BackendKind:     BackendKind,
		Rank:            1,
		Health:          "healthy",
		BackingRef:      "mdadm:pool-fk:NVME",
	}
	if err := c.createTierTarget(row); err != nil {
		t.Fatalf("createTierTarget: %v", err)
	}
	if row.ID == "" {
		t.Fatal("createTierTarget must populate row.ID so callers can key off it")
	}
	tt, ok := c.getTierByBackingRef("mdadm:pool-fk:NVME", BackendKind)
	if !ok || tt.ID != row.ID {
		t.Fatalf("cached tier target has wrong ID: got %q want %q (ok=%v)", tt.ID, row.ID, ok)
	}

	c.upsertMdadmTarget(&db.MdadmManagedTargetRow{
		TierTargetID: tt.ID,
		PoolName:     "pool-fk",
		TierName:     "NVME",
		VGName:       "tier-pool-fk-NVME",
		LVName:       "data",
		MountPath:    "/mnt/.tierd-backing/pool-fk/NVME",
	})

	drainWithin(t, 500*time.Millisecond, func() bool {
		got, err := store.GetMdadmManagedTargetByPoolTier("pool-fk", "NVME")
		return err == nil && got != nil && got.TierTargetID == row.ID
	})
}

func TestTargetCacheDeleteRemovesFromBoth(t *testing.T) {
	store := openTestStore(t)
	seedTierTarget(t, store, "tt-del", "pool-c", "pool-c/HDD", 3)
	if err := store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: "tt-del",
		PoolName:     "pool-c",
		TierName:     "HDD",
		VGName:       "tier-pool-c-HDD",
		LVName:       "data",
		MountPath:    "/mnt/.tierd-backing/pool-c/HDD",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	c.deleteMdadmTarget("tt-del")
	c.deleteTierTarget("tt-del")
	if _, ok := c.getMdadmByID("tt-del"); ok {
		t.Fatal("deleted mdadm target still present in cache")
	}
	if _, ok := c.getTier("tt-del"); ok {
		t.Fatal("deleted tier target still present in cache")
	}

	drainWithin(t, 500*time.Millisecond, func() bool {
		_, err := store.GetMdadmManagedTarget("tt-del")
		return err != nil // not-found after drain
	})
}

func TestTargetCacheUpdateActivityReflectedInMemoryThenSQL(t *testing.T) {
	store := openTestStore(t)
	seedTierTarget(t, store, "tt-act", "pool-d", "pool-d/HDD", 3)

	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	c.updateTierTargetActivity("tt-act", "degraded", "active", "")
	tt, ok := c.getTier("tt-act")
	if !ok || tt.Health != "degraded" || tt.ActivityBand != "active" {
		t.Fatalf("in-memory update not visible: %+v ok=%v", tt, ok)
	}

	drainWithin(t, 500*time.Millisecond, func() bool {
		got, err := store.GetTierTarget("tt-act")
		return err == nil && got != nil && got.Health == "degraded" && got.ActivityBand == "active"
	})
}

// Namespaces are the hottest read on the metadata hot path. Cache reads
// must come from memory and never touch SQLite; writes update the cache
// immediately and drain asynchronously.
func TestTargetCacheServesNamespaceReadsFromMemory(t *testing.T) {
	store := openTestStore(t)
	nsID := seedManagedNamespace(t, store, "ns-a", "pool-ns-a")
	if err := store.UpsertMdadmManagedNamespace(&db.MdadmManagedNamespaceRow{
		NamespaceID: nsID,
		PoolName:    "pool-ns-a",
		MountPath:   "/mnt/pool-ns-a",
	}); err != nil {
		t.Fatalf("seed ns: %v", err)
	}

	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	got, ok := c.getMdadmNs(nsID)
	if !ok || got.PoolName != "pool-ns-a" {
		t.Fatalf("getMdadmNs: got %+v ok=%v", got, ok)
	}
	byPool, ok := c.getMdadmNsByPool("pool-ns-a")
	if !ok || byPool.NamespaceID != nsID {
		t.Fatalf("getMdadmNsByPool: got %+v ok=%v", byPool, ok)
	}
	list := c.listMdadmNs()
	if len(list) != 1 || list[0].NamespaceID != nsID {
		t.Fatalf("listMdadmNs: %+v", list)
	}
}

func TestTargetCacheNamespaceWritesAreImmediatelyVisibleThenDrain(t *testing.T) {
	store := openTestStore(t)
	nsID := seedManagedNamespace(t, store, "ns-b", "pool-ns-b")
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	c.upsertMdadmNs(&db.MdadmManagedNamespaceRow{
		NamespaceID: nsID,
		PoolName:    "pool-ns-b",
		MountPath:   "/mnt/pool-ns-b",
	})
	if r, ok := c.getMdadmNs(nsID); !ok || r.NamespaceID != nsID {
		t.Fatalf("memory read after upsert: %+v ok=%v", r, ok)
	}

	drainWithin(t, 500*time.Millisecond, func() bool {
		got, err := store.GetMdadmManagedNamespace(nsID)
		return err == nil && got != nil && got.PoolName == "pool-ns-b"
	})
}

func TestTargetCacheNamespaceDeleteRemovesFromBoth(t *testing.T) {
	store := openTestStore(t)
	nsID := seedManagedNamespace(t, store, "ns-c", "pool-ns-c")
	if err := store.UpsertMdadmManagedNamespace(&db.MdadmManagedNamespaceRow{
		NamespaceID: nsID,
		PoolName:    "pool-ns-c",
		MountPath:   "/mnt/pool-ns-c",
	}); err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)

	c.deleteMdadmNs(nsID)
	if _, ok := c.getMdadmNs(nsID); ok {
		t.Fatal("cache still has deleted ns")
	}
	if _, ok := c.getMdadmNsByPool("pool-ns-c"); ok {
		t.Fatal("cache byPool still has deleted ns")
	}
	drainWithin(t, 500*time.Millisecond, func() bool {
		_, err := store.GetMdadmManagedNamespace(nsID)
		return err != nil // not found
	})
}

// Write-through mode persists synchronously. By the time the cache
// write returns, SQLite already has the row — drainWithin is not
// needed.
func TestTargetCacheWriteThroughModePersistsSynchronously(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetControlPlaneConfig(ControlPlaneKeyCacheWriteThrough, "true"); err != nil {
		t.Fatalf("set config: %v", err)
	}
	nsID := seedManagedNamespace(t, store, "ns-wt", "pool-wt")
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)
	if c.writeMode != writeThrough {
		t.Fatalf("expected writeThrough mode, got %v", c.writeMode)
	}

	c.upsertMdadmNs(&db.MdadmManagedNamespaceRow{
		NamespaceID: nsID,
		PoolName:    "pool-wt",
		MountPath:   "/mnt/pool-wt",
	})
	got, err := store.GetMdadmManagedNamespace(nsID)
	if err != nil {
		t.Fatalf("store read immediately after write-through: %v", err)
	}
	if got.PoolName != "pool-wt" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestTargetCacheDefaultsToWriteBack(t *testing.T) {
	store := openTestStore(t)
	c, err := newTargetCache(store)
	if err != nil {
		t.Fatalf("newTargetCache: %v", err)
	}
	t.Cleanup(c.Close)
	if c.writeMode != writeBack {
		t.Fatalf("expected writeBack by default, got %v", c.writeMode)
	}
}

func TestTargetCacheWriteThroughFalsyValuesStayWriteBack(t *testing.T) {
	for _, v := range []string{"", "false", "no", "0", "off", "nonsense"} {
		t.Run("v="+v, func(t *testing.T) {
			store := openTestStore(t)
			if err := store.SetControlPlaneConfig(ControlPlaneKeyCacheWriteThrough, v); err != nil {
				t.Fatalf("set config: %v", err)
			}
			c, err := newTargetCache(store)
			if err != nil {
				t.Fatalf("newTargetCache: %v", err)
			}
			t.Cleanup(c.Close)
			if c.writeMode != writeBack {
				t.Fatalf("value %q should default to writeBack, got %v", v, c.writeMode)
			}
		})
	}
}
