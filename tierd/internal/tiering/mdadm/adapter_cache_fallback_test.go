package mdadm

import "testing"

// TestManagedNamespaceFallbacksUseStoreWhenCacheUnavailable exercises the
// cache-miss fallbacks in adapter.go so that a stack-overflow regression
// like the one the FUSE-purge commit fixed (getManagedNamespace and
// siblings recursing into themselves instead of a.store.*) cannot return.
// DaemonState / DaemonPID / setManagedNamespaceDaemonState were removed
// with the FUSE data plane, so this test only exercises the four
// surviving fallbacks: get, list, upsert, delete.
func TestManagedNamespaceFallbacksUseStoreWhenCacheUnavailable(t *testing.T) {
	store := openAdapterStore(t)
	a := NewAdapter(store, t.TempDir())
	a.cache = nil

	seedManagedNamespaceParent(t, store, "ns-cacheless", "media")
	row := seedManagedNamespaceRow(t, "ns-cacheless", "media")
	if err := a.upsertManagedNamespace(row); err != nil {
		t.Fatalf("upsertManagedNamespace: %v", err)
	}

	rows, err := a.listManagedNamespaces()
	if err != nil {
		t.Fatalf("listManagedNamespaces: %v", err)
	}
	if len(rows) != 1 || rows[0].NamespaceID != row.NamespaceID {
		t.Fatalf("unexpected managed namespaces: %+v", rows)
	}

	got, err := a.getManagedNamespace(row.NamespaceID)
	if err != nil {
		t.Fatalf("getManagedNamespace: %v", err)
	}
	if got.NamespaceID != row.NamespaceID || got.PoolName != row.PoolName {
		t.Fatalf("managed namespace round-trip mismatch: got %+v want %+v", got, row)
	}

	if err := a.deleteManagedNamespace(row.NamespaceID); err != nil {
		t.Fatalf("deleteManagedNamespace: %v", err)
	}
	rows, err = a.listManagedNamespaces()
	if err != nil {
		t.Fatalf("listManagedNamespaces after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected delete to remove namespace, got %+v", rows)
	}
}
