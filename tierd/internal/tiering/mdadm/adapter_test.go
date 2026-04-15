package mdadm_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	mdadmadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/mdadm"
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

// seedPool creates a tier pool with one assigned slot.
func seedPool(t *testing.T, store *db.Store, poolName, slotName string, rank int) {
	t.Helper()
	if err := store.CreateTierPool(poolName, "xfs", []db.TierDefinition{{Name: slotName, Rank: rank}}); err != nil {
		t.Fatalf("CreateTierPool(%q): %v", poolName, err)
	}
	if err := store.TransitionTierInstanceState(poolName, db.TierPoolStateHealthy); err != nil {
		t.Fatalf("TransitionTierInstanceState healthy: %v", err)
	}
	arrayID, err := store.EnsureMDADMArray("/dev/md0")
	if err != nil {
		t.Fatalf("EnsureMDADMArray: %v", err)
	}
	if err := store.AssignArrayToTier(poolName, slotName, arrayID, "/dev/md0"); err != nil {
		t.Fatalf("AssignArrayToTier: %v", err)
	}
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
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	if got := a.Kind(); got != "mdadm" {
		t.Errorf("Kind() = %q, want %q", got, "mdadm")
	}
}

// ---- CreateTarget / DestroyTarget capability violations ---------------------

func TestCreateTargetMissingFields(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	// CreateTarget requires both PlacementDomain and Name.
	_, err := a.CreateTarget(tiering.TargetSpec{Name: "foo"})
	adapterErr(t, err, tiering.ErrPermanent)
}

func TestDestroyTargetCapabilityViolation(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	if err := a.DestroyTarget("anything"); err == nil {
		t.Fatal("expected error from DestroyTarget")
	}
}

// ---- ListTargets ------------------------------------------------------------

func TestListTargetsEmpty(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	targets, err := a.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
}

func TestListTargetsAssignedSlot(t *testing.T) {
	store := openStore(t)
	seedPool(t, store, "fastpool", "NVME", 1)
	a := mdadmadapter.NewAdapter(store, t.TempDir())

	targets, err := a.ListTargets()
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	tgt := targets[0]
	if tgt.ID != "mdadm:fastpool:NVME" {
		t.Errorf("ID = %q, want mdadm:fastpool:NVME", tgt.ID)
	}
	if tgt.Name != "NVME" {
		t.Errorf("Name = %q, want NVME", tgt.Name)
	}
	if tgt.Health != "healthy" {
		t.Errorf("Health = %q, want healthy", tgt.Health)
	}
	if tgt.Capabilities.MovementGranularity != "region" {
		t.Errorf("MovementGranularity = %q, want region", tgt.Capabilities.MovementGranularity)
	}
	if !tgt.Capabilities.SupportsOnlineMove {
		t.Error("SupportsOnlineMove should be true for mdadm")
	}
	if tgt.Capabilities.SupportsRecall {
		t.Error("SupportsRecall should be false for mdadm")
	}
	if tgt.Capabilities.PinScope != "volume" {
		t.Errorf("PinScope = %q, want volume", tgt.Capabilities.PinScope)
	}
}

// ---- GetCapabilities --------------------------------------------------------

func TestGetCapabilitiesValidTarget(t *testing.T) {
	store := openStore(t)
	seedPool(t, store, "pool1", "SSD", 2)
	a := mdadmadapter.NewAdapter(store, t.TempDir())

	caps, err := a.GetCapabilities("mdadm:pool1:SSD")
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.MovementGranularity != "region" {
		t.Errorf("MovementGranularity = %q", caps.MovementGranularity)
	}
	if caps.FUSEMode != "n/a" {
		t.Errorf("FUSEMode = %q, want n/a", caps.FUSEMode)
	}
}

func TestGetCapabilitiesInvalidTargetID(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	_, err := a.GetCapabilities("bad-id")
	adapterErr(t, err, tiering.ErrPermanent)
}

func TestGetCapabilitiesNotFound(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	_, err := a.GetCapabilities("mdadm:nopool:NVME")
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

// ---- GetPolicy / SetPolicy --------------------------------------------------

func TestGetSetPolicy(t *testing.T) {
	store := openStore(t)
	seedPool(t, store, "pool1", "HDD", 3)
	a := mdadmadapter.NewAdapter(store, t.TempDir())

	// Seed fill policy.
	if err := store.SetTierSlotFill("pool1", "HDD", 70, 90); err != nil {
		t.Fatalf("SetTierSlotFill: %v", err)
	}

	pol, err := a.GetPolicy("mdadm:pool1:HDD")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol.TargetFillPct != 70 {
		t.Errorf("TargetFillPct = %d, want 70", pol.TargetFillPct)
	}
	if pol.FullThresholdPct != 90 {
		t.Errorf("FullThresholdPct = %d, want 90", pol.FullThresholdPct)
	}

	// Update via adapter.
	if err := a.SetPolicy("mdadm:pool1:HDD", tiering.TargetPolicy{TargetFillPct: 60, FullThresholdPct: 85}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	pol2, err := a.GetPolicy("mdadm:pool1:HDD")
	if err != nil {
		t.Fatalf("GetPolicy after set: %v", err)
	}
	if pol2.TargetFillPct != 60 {
		t.Errorf("TargetFillPct after set = %d, want 60", pol2.TargetFillPct)
	}
}

func TestPinUnsupportedScope(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	err := a.Pin(tiering.PinScopeObject, "mdadm:vg/lv", "obj1")
	adapterErr(t, err, tiering.ErrCapabilityViolation)
}

// ---- GetDegradedState -------------------------------------------------------

func TestGetDegradedStateEmpty(t *testing.T) {
	store := openStore(t)
	a := mdadmadapter.NewAdapter(store, t.TempDir())
	states, err := a.GetDegradedState()
	if err != nil {
		t.Fatalf("GetDegradedState: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 degraded states, got %d", len(states))
	}
}

func TestGetDegradedStateAfterReconcileWithMissingSlot(t *testing.T) {
	store := openStore(t)
	seedPool(t, store, "dpool", "HDD", 3)

	// Force the slot to missing state via direct transition.
	if err := store.TransitionTierSlotState("dpool", "HDD", db.TierSlotStateMissing); err != nil {
		t.Fatalf("TransitionTierSlotState missing: %v", err)
	}

	a := mdadmadapter.NewAdapter(store, t.TempDir())
	if err := a.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	states, err := a.GetDegradedState()
	if err != nil {
		t.Fatalf("GetDegradedState: %v", err)
	}
	if len(states) == 0 {
		t.Fatal("expected at least one degraded state after missing slot")
	}
	found := false
	for _, s := range states {
		if s.Code == "reconciliation_required" && s.Severity == "critical" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected critical reconciliation_required state, got: %+v", states)
	}
}

