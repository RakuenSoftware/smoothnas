package mdadm

import (
	"path/filepath"

	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func openAdapterStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedTestManagedNamespace(t *testing.T, a *Adapter, namespaceID, poolName string) {
	t.Helper()
	if err := a.upsertManagedNamespace(seedManagedNamespaceRow(t, namespaceID, poolName)); err != nil {
		t.Fatalf("upsert namespace: %v", err)
	}
}

func seedManagedNamespaceRow(t *testing.T, namespaceID, poolName string) *db.MdadmManagedNamespaceRow {
	t.Helper()
	return &db.MdadmManagedNamespaceRow{
		NamespaceID: namespaceID,
		PoolName:    poolName,
		MountPath:   filepath.Join(t.TempDir(), poolName),
	}
}

func seedManagedNamespaceParent(t *testing.T, store *db.Store, namespaceID, poolName string) {
	t.Helper()
	if err := store.CreateManagedNamespace(&db.ManagedNamespaceRow{
		ID:              namespaceID,
		Name:            poolName,
		PlacementDomain: poolName,
		BackendKind:     BackendKind,
		NamespaceKind:   "filespace",
		ExposedPath:     filepath.Join("/mnt", poolName),
		PinState:        "none",
		Health:          "healthy",
		PlacementState:  "placed",
		BackendRef:      backingRefManagedNamespace(poolName),
	}); err != nil {
		t.Fatalf("create managed namespace parent: %v", err)
	}
}

func seedTestManagedTarget(t *testing.T, a *Adapter, poolName, tierName, mountPath string, rank int) {
	t.Helper()
	row := &db.TierTargetRow{
		Name:            tierName,
		PlacementDomain: poolName,
		BackendKind:     BackendKind,
		Rank:            rank,
		Health:          "healthy",
		BackingRef:      backingRefTarget(poolName, tierName),
	}
	if err := a.createTierTarget(row); err != nil {
		t.Fatalf("create tier target %s: %v", tierName, err)
	}
	if err := a.upsertManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: row.ID,
		PoolName:     poolName,
		TierName:     tierName,
		MountPath:    mountPath,
	}); err != nil {
		t.Fatalf("upsert managed target %s: %v", tierName, err)
	}
}
