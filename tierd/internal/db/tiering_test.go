package db_test

import (
	"errors"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// helpers -------------------------------------------------------------------

func makeTarget(t *testing.T, store *db.Store, name, domain string, rank int) *db.TierTargetRow {
	t.Helper()
	row := &db.TierTargetRow{
		Name:             name,
		PlacementDomain:  domain,
		BackendKind:      "stub",
		Rank:             rank,
		TargetFillPct:    50,
		FullThresholdPct: 95,
		Health:           "healthy",
	}
	if err := store.CreateTierTarget(row); err != nil {
		t.Fatalf("CreateTierTarget(%q): %v", name, err)
	}
	return row
}

func makeNamespace(t *testing.T, store *db.Store, name, domain string) *db.ManagedNamespaceRow {
	t.Helper()
	row := &db.ManagedNamespaceRow{
		Name:            name,
		PlacementDomain: domain,
		BackendKind:     "stub",
		NamespaceKind:   "volume",
		Health:          "healthy",
		PlacementState:  "placed",
		PinState:        "none",
	}
	if err := store.CreateManagedNamespace(row); err != nil {
		t.Fatalf("CreateManagedNamespace(%q): %v", name, err)
	}
	return row
}

// placement_domain auto-create / auto-remove ---------------------------------

func TestPlacementDomainAutoCreate(t *testing.T) {
	store := openTierStore(t)

	// Before any target exists, domain list is empty.
	domains, err := store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 0 {
		t.Fatalf("expected 0 domains before any target, got %d", len(domains))
	}

	// Creating a target auto-creates the domain.
	makeTarget(t, store, "nvme0", "fast", 1)

	domains, err = store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("expected 1 domain after target create, got %d", len(domains))
	}
	if domains[0].ID != "fast" {
		t.Fatalf("domain id = %q, want %q", domains[0].ID, "fast")
	}

	// A second target in the same domain does not create a duplicate.
	makeTarget(t, store, "nvme1", "fast", 2)

	domains, err = store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("expected still 1 domain, got %d", len(domains))
	}

	// A target in a different domain creates a second domain.
	makeTarget(t, store, "hdd0", "slow", 1)

	domains, err = store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(domains))
	}
}

func TestPlacementDomainAutoRemove(t *testing.T) {
	store := openTierStore(t)

	t1 := makeTarget(t, store, "nvme0", "fast", 1)
	t2 := makeTarget(t, store, "nvme1", "fast", 2)

	// Removing the first target: domain still has one member, must not be pruned.
	if err := store.DeleteTierTarget(t1.ID); err != nil {
		t.Fatalf("DeleteTierTarget(t1): %v", err)
	}
	domains, err := store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("expected 1 domain after first delete, got %d", len(domains))
	}

	// Removing the last target: domain must be pruned.
	if err := store.DeleteTierTarget(t2.ID); err != nil {
		t.Fatalf("DeleteTierTarget(t2): %v", err)
	}
	domains, err = store.ListPlacementDomains()
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 0 {
		t.Fatalf("expected 0 domains after last target removed, got %d", len(domains))
	}
}

// DeleteManagedNamespacesByPlacementDomain ------------------------------------

func TestDeleteManagedNamespacesByPlacementDomain(t *testing.T) {
	store := openTierStore(t)

	makeNamespace(t, store, "vol1", "pool-a")
	makeNamespace(t, store, "vol2", "pool-a")
	makeNamespace(t, store, "vol3", "pool-b")

	if err := store.DeleteManagedNamespacesByPlacementDomain("pool-a"); err != nil {
		t.Fatalf("DeleteManagedNamespacesByPlacementDomain: %v", err)
	}

	remaining, err := store.ListManagedNamespaces()
	if err != nil {
		t.Fatalf("ListManagedNamespaces: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 namespace after delete, got %d", len(remaining))
	}
	if remaining[0].PlacementDomain != "pool-b" {
		t.Fatalf("remaining namespace domain = %q, want %q", remaining[0].PlacementDomain, "pool-b")
	}
}

// policy_revision invalidation -----------------------------------------------

func TestPolicyRevisionInvalidatesMovementJobs(t *testing.T) {
	t.Skip("movement_jobs dropped in migration 52")
	store := openTierStore(t)

	src := makeTarget(t, store, "nvme0", "fast", 1)
	dst := makeTarget(t, store, "hdd0", "fast", 2)

	// Create a pending movement job recording the current policy_revision.
	job := &db.MovementJobRow{
		BackendKind:    "stub",
		NamespaceID:    "ns1",
		MovementUnit:   "region",
		SourceTargetID: src.ID,
		DestTargetID:   dst.ID,
		PolicyRevision: 1, // snapshot at creation time
		IntentRevision: 1,
		TriggeredBy:    "test",
	}
	if err := store.CreateMovementJob(job); err != nil {
		t.Fatalf("CreateMovementJob: %v", err)
	}

	// Verify job is pending.
	got, err := store.GetMovementJob(job.ID)
	if err != nil {
		t.Fatalf("GetMovementJob: %v", err)
	}
	if got.State != db.MovementJobStatePending {
		t.Fatalf("state = %q, want %q", got.State, db.MovementJobStatePending)
	}

	// Update the target's policy: policy_revision becomes 2.
	if err := store.UpdateTierTargetPolicy(src.ID, 60, 90); err != nil {
		t.Fatalf("UpdateTierTargetPolicy: %v", err)
	}

	// The movement job must now be stale.
	got, err = store.GetMovementJob(job.ID)
	if err != nil {
		t.Fatalf("GetMovementJob after policy update: %v", err)
	}
	if got.State != db.MovementJobStateStale {
		t.Fatalf("state = %q, want %q after policy change", got.State, db.MovementJobStateStale)
	}
}

// intent_revision invalidation -----------------------------------------------

func TestIntentRevisionInvalidatesMovementJobs(t *testing.T) {
	t.Skip("movement_jobs dropped in migration 52")
	store := openTierStore(t)

	src := makeTarget(t, store, "nvme0", "fast", 1)
	dst := makeTarget(t, store, "hdd0", "fast", 2)
	ns := makeNamespace(t, store, "vol1", "fast")

	job := &db.MovementJobRow{
		BackendKind:    "stub",
		NamespaceID:    ns.ID,
		MovementUnit:   "region",
		SourceTargetID: src.ID,
		DestTargetID:   dst.ID,
		PolicyRevision: 1,
		IntentRevision: 1,
		TriggeredBy:    "test",
	}
	if err := store.CreateMovementJob(job); err != nil {
		t.Fatalf("CreateMovementJob: %v", err)
	}

	// Pin the namespace: intent_revision becomes 2.
	if err := store.SetNamespacePinState(ns.ID, "pinned-hot"); err != nil {
		t.Fatalf("SetNamespacePinState: %v", err)
	}

	got, err := store.GetMovementJob(job.ID)
	if err != nil {
		t.Fatalf("GetMovementJob after pin: %v", err)
	}
	if got.State != db.MovementJobStateStale {
		t.Fatalf("state = %q, want %q after intent change", got.State, db.MovementJobStateStale)
	}
}

// cross-domain movement rejection --------------------------------------------

func TestCrossDomainMovementRejected(t *testing.T) {
	t.Skip("movement_jobs dropped in migration 52")
	store := openTierStore(t)

	// Two targets in different domains.
	src := makeTarget(t, store, "nvme0", "fast", 1)
	dst := makeTarget(t, store, "hdd0", "slow", 1)

	job := &db.MovementJobRow{
		BackendKind:    "stub",
		NamespaceID:    "ns1",
		MovementUnit:   "region",
		SourceTargetID: src.ID,
		DestTargetID:   dst.ID,
		PolicyRevision: 1,
		IntentRevision: 1,
		TriggeredBy:    "test",
	}
	err := store.CreateMovementJob(job)
	if !errors.Is(err, db.ErrCrossDomainMovement) {
		t.Fatalf("CreateMovementJob across domains: got %v, want ErrCrossDomainMovement", err)
	}
}

// policy_revision increment --------------------------------------------------

func TestPolicyRevisionIncrement(t *testing.T) {
	store := openTierStore(t)
	tgt := makeTarget(t, store, "nvme0", "fast", 1)

	if tgt.PolicyRevision != 1 {
		t.Fatalf("initial policy_revision = %d, want 1", tgt.PolicyRevision)
	}

	if err := store.UpdateTierTargetPolicy(tgt.ID, 70, 85); err != nil {
		t.Fatalf("UpdateTierTargetPolicy: %v", err)
	}

	got, err := store.GetTierTarget(tgt.ID)
	if err != nil {
		t.Fatalf("GetTierTarget: %v", err)
	}
	if got.PolicyRevision != 2 {
		t.Fatalf("policy_revision = %d, want 2", got.PolicyRevision)
	}
	if got.TargetFillPct != 70 {
		t.Fatalf("target_fill_pct = %d, want 70", got.TargetFillPct)
	}
	if got.FullThresholdPct != 85 {
		t.Fatalf("full_threshold_pct = %d, want 85", got.FullThresholdPct)
	}
}

// intent_revision increment --------------------------------------------------

func TestIntentRevisionIncrement(t *testing.T) {
	store := openTierStore(t)
	ns := makeNamespace(t, store, "vol1", "fast")

	if ns.IntentRevision != 1 {
		t.Fatalf("initial intent_revision = %d, want 1", ns.IntentRevision)
	}

	if err := store.SetNamespacePinState(ns.ID, "pinned-hot"); err != nil {
		t.Fatalf("SetNamespacePinState: %v", err)
	}

	got, err := store.GetManagedNamespace(ns.ID)
	if err != nil {
		t.Fatalf("GetManagedNamespace: %v", err)
	}
	if got.IntentRevision != 2 {
		t.Fatalf("intent_revision = %d, want 2", got.IntentRevision)
	}
	if got.PinState != "pinned-hot" {
		t.Fatalf("pin_state = %q, want %q", got.PinState, "pinned-hot")
	}
}

// managed objects ------------------------------------------------------------

func TestManagedObjectCRUD(t *testing.T) {
	t.Skip("managed_objects dropped in migration 52")
	store := openTierStore(t)
	ns := makeNamespace(t, store, "vol1", "fast")

	obj := &db.ManagedObjectRow{
		NamespaceID:          ns.ID,
		ObjectKind:           "volume",
		ObjectKey:            "data-lv",
		PinState:             "none",
		PlacementSummaryJSON: `{"current_target_id":"","state":"unknown"}`,
	}
	if err := store.CreateManagedObject(obj); err != nil {
		t.Fatalf("CreateManagedObject: %v", err)
	}
	if obj.ID == "" {
		t.Fatal("expected non-empty ID after CreateManagedObject")
	}

	list, err := store.ListManagedObjects(ns.ID)
	if err != nil {
		t.Fatalf("ListManagedObjects: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 object, got %d", len(list))
	}
	if list[0].ObjectKey != "data-lv" {
		t.Fatalf("object_key = %q, want %q", list[0].ObjectKey, "data-lv")
	}
}

// degraded states ------------------------------------------------------------

func TestDegradedStateUpsert(t *testing.T) {
	store := openTierStore(t)

	d := &db.DegradedStateRow{
		BackendKind: "stub",
		ScopeKind:   "target",
		ScopeID:     "tgt-1",
		Severity:    db.DegradedSeverityCritical,
		Code:        "array_degraded",
		Message:     "RAID array is in degraded state",
	}
	if err := store.UpsertDegradedState(d); err != nil {
		t.Fatalf("UpsertDegradedState: %v", err)
	}

	list, err := store.ListDegradedStates()
	if err != nil {
		t.Fatalf("ListDegradedStates: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 degraded state, got %d", len(list))
	}
	if list[0].Severity != db.DegradedSeverityCritical {
		t.Fatalf("severity = %q, want critical", list[0].Severity)
	}
}

// health monitoring ----------------------------------------------------------

func TestCheckTieringHealthEmptyDB(t *testing.T) {
	store := openTierStore(t)
	alerts, err := store.CheckTieringHealth(10, 30, 60, 60, 5)
	if err != nil {
		t.Fatalf("CheckTieringHealth: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts on empty DB, got %d: %v", len(alerts), alerts)
	}
}

func TestCheckTieringHealthCriticalDegradedState(t *testing.T) {
	store := openTierStore(t)

	d := &db.DegradedStateRow{
		BackendKind: "stub",
		ScopeKind:   "target",
		ScopeID:     "tgt-1",
		Severity:    db.DegradedSeverityCritical,
		Code:        "array_failed",
		Message:     "array gone",
	}
	if err := store.UpsertDegradedState(d); err != nil {
		t.Fatalf("UpsertDegradedState: %v", err)
	}

	alerts, err := store.CheckTieringHealth(10, 30, 60, 60, 5)
	if err != nil {
		t.Fatalf("CheckTieringHealth: %v", err)
	}
	found := false
	for _, a := range alerts {
		if a.Check == "degraded_state_critical" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected degraded_state_critical alert, got %v", alerts)
	}
}
