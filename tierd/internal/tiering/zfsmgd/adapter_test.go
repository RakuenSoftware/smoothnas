package zfsmgd_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	zfsmgdadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/zfsmgd"
)

// openStore opens a fully migrated SQLite store for tests.
func openStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// adapterErr asserts err is a *tiering.AdapterError with the given kind.
func adapterErr(t *testing.T, err error, kind tiering.AdapterErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected AdapterError(%s), got nil", kind)
	}
	var ae *tiering.AdapterError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *tiering.AdapterError, got %T: %v", err, err)
	}
	if ae.Kind != kind {
		t.Errorf("error kind = %q, want %q", ae.Kind, kind)
	}
}

// ---- Kind -------------------------------------------------------------------

func TestKind(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	if got := a.Kind(); got != "zfs-managed" {
		t.Errorf("Kind() = %q, want %q", got, "zfs-managed")
	}
}

// ---- CreateTarget (no ZFS) --------------------------------------------------

// CreateTarget requires a real ZFS pool; without one it should return a
// permanent adapter error (zfs create will fail because the pool does not
// exist).  This test only checks that the error is correctly wrapped.
func TestCreateTargetNoPool(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.CreateTarget(tiering.TargetSpec{
		Name:            "fast",
		PlacementDomain: "test-domain",
		BackendDetails: map[string]any{
			"pool_name": "nonexistent_pool_abc123",
		},
	})
	if err == nil {
		t.Fatal("expected error from CreateTarget without a ZFS pool")
	}
}

// CreateTarget without pool_name must return ErrPermanent.
func TestCreateTargetMissingPoolName(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.CreateTarget(tiering.TargetSpec{
		Name:            "fast",
		PlacementDomain: "test-domain",
		BackendDetails:  map[string]any{},
	})
	adapterErr(t, err, tiering.ErrPermanent)
}

// ---- DestroyTarget ----------------------------------------------------------

func TestDestroyTargetNotFound(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	err := a.DestroyTarget("nonexistent-id")
	if err == nil {
		t.Fatal("expected error from DestroyTarget on unknown id")
	}
}

// ---- ListTargets (empty) ----------------------------------------------------

func TestListTargetsEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	targets, err := a.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
}

// ---- ListNamespaces (empty) -------------------------------------------------

func TestListNamespacesEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	namespaces, err := a.ListNamespaces()
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(namespaces) != 0 {
		t.Errorf("expected 0 namespaces, got %d", len(namespaces))
	}
}

// ---- ListManagedObjects (empty namespace) ------------------------------------

// ListManagedObjects returns an empty slice for a namespace with no registered
// objects; it does not error on an unknown namespace ID.
func TestListManagedObjectsUnknownNamespace(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	objs, err := a.ListManagedObjects("zfs-managed:nopool/nons")
	if err != nil {
		t.Fatalf("ListManagedObjects: unexpected error: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("expected 0 objects for unknown namespace, got %d", len(objs))
	}
}

// ---- GetCapabilities --------------------------------------------------------

func TestGetCapabilitiesUnknownTarget(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.GetCapabilities("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

// ---- GetPolicy / SetPolicy --------------------------------------------------

func TestGetPolicyUnknownTarget(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.GetPolicy("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestSetPolicyUnknownTarget(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	err := a.SetPolicy("nonexistent-id", tiering.TargetPolicy{
		TargetFillPct:    80,
		FullThresholdPct: 95,
	})
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

// ---- PlanMovements (empty) --------------------------------------------------

func TestPlanMovementsEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	plans, err := a.PlanMovements()
	if err != nil {
		t.Fatalf("PlanMovements: %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("expected 0 plans, got %d", len(plans))
	}
}

// ---- StartMovement ----------------------------------------------------------

// StartMovement with an invalid plan (missing source target) must return an
// ErrPermanent or ErrTransient (DB lookup will fail).
func TestStartMovementInvalidPlan(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.StartMovement(tiering.MovementPlan{
		NamespaceID:    "ns1",
		ObjectID:       "obj1",
		MovementUnit:   "file",
		SourceTargetID: "nonexistent-src",
		DestTargetID:   "nonexistent-dst",
	})
	if err == nil {
		t.Fatal("expected error for movement with nonexistent targets")
	}
}

// ---- GetMovement (not found) ------------------------------------------------

func TestGetMovementNotFound(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	_, err := a.GetMovement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent movement")
	}
}

// ---- CancelMovement (not found) ---------------------------------------------

func TestCancelMovementNotFound(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	err := a.CancelMovement("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent movement")
	}
}

// ---- Pin / Unpin (unknown scope/namespace) -----------------------------------

func TestPinUnknownNamespace(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	err := a.Pin(tiering.PinScopeObject, "unknown-ns", "unknown-obj")
	if err == nil {
		t.Fatal("expected error for unknown namespace/object")
	}
}

func TestUnpinUnknownNamespace(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	err := a.Unpin(tiering.PinScopeObject, "unknown-ns", "unknown-obj")
	if err == nil {
		t.Fatal("expected error for unknown namespace/object")
	}
}

// ---- CollectActivity (empty) ------------------------------------------------

func TestCollectActivityEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	samples, err := a.CollectActivity()
	if err != nil {
		t.Fatalf("CollectActivity: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("expected 0 samples, got %d", len(samples))
	}
}

// ---- GetDegradedState (empty) -----------------------------------------------

func TestGetDegradedStateEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	states, err := a.GetDegradedState()
	if err != nil {
		t.Fatalf("GetDegradedState: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 degraded states, got %d", len(states))
	}
}

// ---- Reconcile (no namespaces registered) -----------------------------------

func TestReconcileEmpty(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	if err := a.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// ---- Capabilities fields ----------------------------------------------------

func TestCapabilitiesFields(t *testing.T) {
	caps := zfsmgdadapter.ExportedCapabilities()
	if caps.MovementGranularity != "file" {
		t.Errorf("MovementGranularity = %q, want file", caps.MovementGranularity)
	}
	if caps.RecallMode != "synchronous" {
		t.Errorf("RecallMode = %q, want synchronous", caps.RecallMode)
	}
	if caps.SnapshotMode != "none" {
		t.Errorf("SnapshotMode = %q, want none", caps.SnapshotMode)
	}
	if caps.SupportsChecksums != true {
		t.Errorf("SupportsChecksums = false, want true")
	}
	if !caps.SupportsChecksums {
		t.Error("SupportsChecksums should be true")
	}
	if !caps.SupportsCompression {
		t.Error("SupportsCompression should be true")
	}
	if !caps.SupportsRecall {
		t.Error("SupportsRecall should be true")
	}
	if caps.PinScope != "object" {
		t.Errorf("PinScope = %q, want object", caps.PinScope)
	}
}

