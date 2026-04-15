package zfsmgd_test

// Unit tests for P04B crash recovery: Reconcile() scans zfs_movement_log for
// non-terminal rows and applies the recovery procedure for each state.
//
// Tests use temp directories to simulate backing dataset paths, so no ZFS pool
// is required. The DB is a freshly migrated in-memory SQLite instance.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	zfsmgdadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/zfsmgd"
)

// ---- helpers ----------------------------------------------------------------

// insertMinimalTargetAndNamespace inserts the minimal DB rows needed to satisfy
// foreign key constraints for movement_log entries. Returns targetID and namespaceID.
func insertMinimalTargetAndNamespace(t *testing.T, store *db.Store, poolName, datasetPath string) (targetID, namespaceID string) {
	t.Helper()

	targetRow := &db.TierTargetRow{
		Name:             "fast",
		PlacementDomain:  "test-domain",
		BackendKind:      "zfs-managed",
		Rank:             1,
		TargetFillPct:    50,
		FullThresholdPct: 95,
		Health:           "healthy",
		ActivityBand:     tiering.ActivityBandCold,
		ActivityTrend:    tiering.ActivityTrendStable,
		CapabilitiesJSON: `{}`,
		BackingRef:       "zfs-managed:" + poolName + "/fast",
	}
	if err := store.CreateTierTarget(targetRow); err != nil {
		t.Fatalf("CreateTierTarget: %v", err)
	}

	zfsTargetRow := &db.ZFSManagedTargetRow{
		TierTargetID: targetRow.ID,
		PoolName:     poolName,
		DatasetName:  "fast",
		DatasetPath:  datasetPath,
		FUSEMode:     "passthrough",
	}
	if err := store.UpsertZFSManagedTarget(zfsTargetRow); err != nil {
		t.Fatalf("UpsertZFSManagedTarget: %v", err)
	}

	nsRow := &db.ManagedNamespaceRow{
		Name:            "testns",
		PlacementDomain: "test-domain",
		BackendKind:     "zfs-managed",
		NamespaceKind:   "filespace",
		ExposedPath:     "/mnt/tiering/testns",
		PinState:        "none",
		Health:          "healthy",
		PlacementState:  "unknown",
		BackendRef:      "zfs-managed:" + poolName + "/testns",
	}
	if err := store.CreateManagedNamespace(nsRow); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}

	return targetRow.ID, nsRow.ID
}

// insertObject inserts a minimal managed object row. Returns the object ID.
func insertObject(t *testing.T, store *db.Store, namespaceID, objectKey, currentTargetID string) string {
	t.Helper()
	objRow := &db.ManagedObjectRow{
		NamespaceID:          namespaceID,
		ObjectKind:           "file",
		ObjectKey:            objectKey,
		PinState:             "none",
		ActivityBand:         tiering.ActivityBandCold,
		PlacementSummaryJSON: `{"CurrentTargetID":"` + currentTargetID + `","IntendedTargetID":"` + currentTargetID + `","State":"placed"}`,
		BackendRef:           "zfs-managed:testns/" + objectKey,
	}
	if err := store.CreateManagedObject(objRow); err != nil {
		t.Fatalf("CreateManagedObject: %v", err)
	}
	return objRow.ID
}

// insertMovementLog inserts a zfs_movement_log row in the given state.
func insertMovementLog(t *testing.T, store *db.Store, objectID, namespaceID, srcTargetID, dstTargetID, objectKey, state string) string {
	t.Helper()
	now := time.Now().Unix()
	row := &db.ZFSMovementLogRow{
		ObjectID:       objectID,
		NamespaceID:    namespaceID,
		SourceTargetID: srcTargetID,
		DestTargetID:   dstTargetID,
		ObjectKey:      objectKey,
		State:          state,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.InsertZFSMovementLog(row); err != nil {
		t.Fatalf("InsertZFSMovementLog: %v", err)
	}
	return row.ID
}

// getMovementLogState returns the current state of a movement_log row.
func getMovementLogState(t *testing.T, store *db.Store, logID string) string {
	t.Helper()
	row, err := store.GetZFSMovementLog(logID)
	if err != nil {
		t.Fatalf("GetZFSMovementLog(%q): %v", logID, err)
	}
	return row.State
}

// ---- copy_in_progress -------------------------------------------------------

// TestReconcileCopyInProgressNoDestFile: interrupted before any bytes were
// written. No destination file exists. Source remains authoritative.
func TestReconcileCopyInProgressNoDestFile(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()
	src := t.TempDir()
	dst := t.TempDir()

	targetID, nsID := insertMinimalTargetAndNamespace(t, store, "pool", src)
	objID := insertObject(t, store, nsID, "data/file.bin", targetID)

	// No destination file exists (copy never started).
	logID := insertMovementLog(t, store, objID, nsID, targetID, targetID, "data/file.bin", db.ZFSMoveLogCopyInProgress)

	_ = dst // dest dir exists but no file in it

	a := zfsmgdadapter.NewAdapter(store, runDir)
	if err := a.ExportedRecoverMovementLog(); err != nil {
		t.Fatalf("RecoverMovementLog: %v", err)
	}

	if state := getMovementLogState(t, store, logID); state != db.ZFSMoveLogFailed {
		t.Errorf("movement_log state = %q, want %q", state, db.ZFSMoveLogFailed)
	}
	row, _ := store.GetZFSMovementLog(logID)
	if row.FailureReason != "interrupted_by_restart" {
		t.Errorf("failure_reason = %q, want %q", row.FailureReason, "interrupted_by_restart")
	}
}

// TestReconcileCopyInProgressPartialDestFile: interrupted mid-copy. Partial
// destination file must be deleted.
func TestReconcileCopyInProgressPartialDestFile(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Register two targets: src and dst.
	srcTargetRow := &db.TierTargetRow{
		Name: "fast", PlacementDomain: "d", BackendKind: "zfs-managed",
		Rank: 1, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p/fast",
	}
	if err := store.CreateTierTarget(srcTargetRow); err != nil {
		t.Fatalf("create src target: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: srcTargetRow.ID, PoolName: "pool", DatasetName: "fast",
		DatasetPath: srcDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert src zfs target: %v", err)
	}

	dstTargetRow := &db.TierTargetRow{
		Name: "warm", PlacementDomain: "d", BackendKind: "zfs-managed",
		Rank: 2, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p/warm",
	}
	if err := store.CreateTierTarget(dstTargetRow); err != nil {
		t.Fatalf("create dst target: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: dstTargetRow.ID, PoolName: "pool", DatasetName: "warm",
		DatasetPath: dstDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert dst zfs target: %v", err)
	}

	nsRow := &db.ManagedNamespaceRow{
		Name: "ns", PlacementDomain: "d", BackendKind: "zfs-managed",
		NamespaceKind: "filespace", ExposedPath: "/mnt/tiering/ns",
		PinState: "none", Health: "healthy", PlacementState: "unknown",
		BackendRef: "zfs-managed:pool/ns",
	}
	if err := store.CreateManagedNamespace(nsRow); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}

	// Create source file.
	srcFilePath := filepath.Join(srcDir, "file.bin")
	if err := os.WriteFile(srcFilePath, []byte("hello"), 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// Create partial destination file (simulating interrupted copy).
	dstFilePath := filepath.Join(dstDir, "file.bin")
	if err := os.WriteFile(dstFilePath, []byte("hel"), 0600); err != nil {
		t.Fatalf("write partial dst: %v", err)
	}

	objID := insertObject(t, store, nsRow.ID, "file.bin", srcTargetRow.ID)
	logID := insertMovementLog(t, store, objID, nsRow.ID, srcTargetRow.ID, dstTargetRow.ID, "file.bin", db.ZFSMoveLogCopyInProgress)

	a := zfsmgdadapter.NewAdapter(store, runDir)
	if err := a.ExportedRecoverMovementLog(); err != nil {
		t.Fatalf("RecoverMovementLog: %v", err)
	}

	if state := getMovementLogState(t, store, logID); state != db.ZFSMoveLogFailed {
		t.Errorf("movement_log state = %q, want %q", state, db.ZFSMoveLogFailed)
	}
	row, _ := store.GetZFSMovementLog(logID)
	if row.FailureReason != "interrupted_by_restart" {
		t.Errorf("failure_reason = %q, want interrupted_by_restart", row.FailureReason)
	}
	// Partial destination file must have been deleted.
	if _, err := os.Stat(dstFilePath); !os.IsNotExist(err) {
		t.Error("partial destination file should have been deleted")
	}
	// Source file must still be present (authoritative).
	if _, err := os.Stat(srcFilePath); err != nil {
		t.Errorf("source file should still be present: %v", err)
	}
}

// ---- copy_complete ----------------------------------------------------------

// TestReconcileCopyComplete: copy finished but switch transaction did not commit.
// Destination copy must be deleted; source remains authoritative.
func TestReconcileCopyComplete(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcTargetRow := &db.TierTargetRow{
		Name: "fast", PlacementDomain: "d2", BackendKind: "zfs-managed",
		Rank: 1, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p2/fast",
	}
	if err := store.CreateTierTarget(srcTargetRow); err != nil {
		t.Fatalf("create src target: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: srcTargetRow.ID, PoolName: "pool2", DatasetName: "fast",
		DatasetPath: srcDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert src zfs: %v", err)
	}
	dstTargetRow := &db.TierTargetRow{
		Name: "warm", PlacementDomain: "d2", BackendKind: "zfs-managed",
		Rank: 2, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p2/warm",
	}
	if err := store.CreateTierTarget(dstTargetRow); err != nil {
		t.Fatalf("create dst target: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: dstTargetRow.ID, PoolName: "pool2", DatasetName: "warm",
		DatasetPath: dstDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert dst zfs: %v", err)
	}
	nsRow := &db.ManagedNamespaceRow{
		Name: "ns2", PlacementDomain: "d2", BackendKind: "zfs-managed",
		NamespaceKind: "filespace", ExposedPath: "/mnt/tiering/ns2",
		PinState: "none", Health: "healthy", PlacementState: "unknown",
		BackendRef: "zfs-managed:pool2/ns2",
	}
	if err := store.CreateManagedNamespace(nsRow); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}

	// Source and destination both exist (copy completed but switch did not commit).
	srcFilePath := filepath.Join(srcDir, "obj.dat")
	if err := os.WriteFile(srcFilePath, []byte("source"), 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dstFilePath := filepath.Join(dstDir, "obj.dat")
	if err := os.WriteFile(dstFilePath, []byte("source"), 0600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	objID := insertObject(t, store, nsRow.ID, "obj.dat", srcTargetRow.ID)
	logID := insertMovementLog(t, store, objID, nsRow.ID, srcTargetRow.ID, dstTargetRow.ID, "obj.dat", db.ZFSMoveLogCopyComplete)

	a := zfsmgdadapter.NewAdapter(store, runDir)
	if err := a.ExportedRecoverMovementLog(); err != nil {
		t.Fatalf("RecoverMovementLog: %v", err)
	}

	if state := getMovementLogState(t, store, logID); state != db.ZFSMoveLogFailed {
		t.Errorf("movement_log state = %q, want %q", state, db.ZFSMoveLogFailed)
	}
	row, _ := store.GetZFSMovementLog(logID)
	if row.FailureReason != "interrupted_before_switch" {
		t.Errorf("failure_reason = %q, want interrupted_before_switch", row.FailureReason)
	}
	// Destination copy must be deleted.
	if _, err := os.Stat(dstFilePath); !os.IsNotExist(err) {
		t.Error("destination copy should have been deleted after copy_complete recovery")
	}
	// Source must still be present.
	if _, err := os.Stat(srcFilePath); err != nil {
		t.Errorf("source file should still be present: %v", err)
	}
}

// ---- switched ---------------------------------------------------------------

// TestReconcileSwitched: switch committed but cleanup did not run.
// Movement log is marked done; source deletion is scheduled in background.
func TestReconcileSwitched(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcTargetRow := &db.TierTargetRow{
		Name: "fast", PlacementDomain: "d3", BackendKind: "zfs-managed",
		Rank: 1, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p3/fast",
	}
	if err := store.CreateTierTarget(srcTargetRow); err != nil {
		t.Fatalf("create src: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: srcTargetRow.ID, PoolName: "pool3", DatasetName: "fast",
		DatasetPath: srcDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert src zfs: %v", err)
	}
	dstTargetRow := &db.TierTargetRow{
		Name: "warm", PlacementDomain: "d3", BackendKind: "zfs-managed",
		Rank: 2, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:p3/warm",
	}
	if err := store.CreateTierTarget(dstTargetRow); err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: dstTargetRow.ID, PoolName: "pool3", DatasetName: "warm",
		DatasetPath: dstDir, FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("upsert dst zfs: %v", err)
	}
	nsRow := &db.ManagedNamespaceRow{
		Name: "ns3", PlacementDomain: "d3", BackendKind: "zfs-managed",
		NamespaceKind: "filespace", ExposedPath: "/mnt/tiering/ns3",
		PinState: "none", Health: "healthy", PlacementState: "unknown",
		BackendRef: "zfs-managed:pool3/ns3",
	}
	if err := store.CreateManagedNamespace(nsRow); err != nil {
		t.Fatalf("CreateManagedNamespace: %v", err)
	}

	// Source still present, destination is authoritative (switch committed).
	srcFilePath := filepath.Join(srcDir, "doc.txt")
	if err := os.WriteFile(srcFilePath, []byte("content"), 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dstFilePath := filepath.Join(dstDir, "doc.txt")
	if err := os.WriteFile(dstFilePath, []byte("content"), 0600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	objID := insertObject(t, store, nsRow.ID, "doc.txt", dstTargetRow.ID)
	logID := insertMovementLog(t, store, objID, nsRow.ID, srcTargetRow.ID, dstTargetRow.ID, "doc.txt", db.ZFSMoveLogSwitched)

	a := zfsmgdadapter.NewAdapter(store, runDir)
	if err := a.ExportedRecoverMovementLog(); err != nil {
		t.Fatalf("RecoverMovementLog: %v", err)
	}

	if state := getMovementLogState(t, store, logID); state != db.ZFSMoveLogCleanupComplete {
		t.Errorf("movement_log state = %q, want %q", state, db.ZFSMoveLogCleanupComplete)
	}
	// Destination must still be present.
	if _, err := os.Stat(dstFilePath); err != nil {
		t.Errorf("destination file should still be present: %v", err)
	}
	// Source deletion is background; give the goroutine a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(srcFilePath); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(srcFilePath); !os.IsNotExist(err) {
		t.Error("source file should have been deleted in background after switched recovery")
	}
}

// ---- no non-terminal rows ---------------------------------------------------

// TestReconcileNoNonTerminalRows: all movement_log rows are terminal.
// No recovery actions are taken; no degraded states are emitted.
func TestReconcileNoNonTerminalRows(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()
	srcDir := t.TempDir()

	targetID, nsID := insertMinimalTargetAndNamespace(t, store, "pool", srcDir)
	objID := insertObject(t, store, nsID, "file.bin", targetID)

	// Insert terminal rows only.
	insertMovementLog(t, store, objID, nsID, targetID, targetID, "file.bin", db.ZFSMoveLogCleanupComplete)
	insertMovementLog(t, store, objID, nsID, targetID, targetID, "file.bin", db.ZFSMoveLogFailed)

	a := zfsmgdadapter.NewAdapter(store, runDir)
	if err := a.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	states, err := a.GetDegradedState()
	if err != nil {
		t.Fatalf("GetDegradedState: %v", err)
	}
	for _, s := range states {
		if s.Code == "reconciliation_required" {
			t.Errorf("unexpected reconciliation_required degraded state")
		}
	}
}

// ---- CreateNamespace pool-same validation -----------------------------------

// TestCreateNamespaceCrossPoolRejected: backing targets from different pools
// must be rejected before any ZFS operations are attempted.
func TestCreateNamespaceCrossPoolRejected(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()

	// Create two targets in different pools.
	t1 := &db.TierTargetRow{
		Name: "fast", PlacementDomain: "dom", BackendKind: "zfs-managed",
		Rank: 1, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:poolA/fast",
	}
	if err := store.CreateTierTarget(t1); err != nil {
		t.Fatalf("CreateTierTarget t1: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: t1.ID, PoolName: "poolA", DatasetName: "fast",
		DatasetPath: "/poolA/fast", FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("UpsertZFSManagedTarget t1: %v", err)
	}

	t2 := &db.TierTargetRow{
		Name: "warm", PlacementDomain: "dom", BackendKind: "zfs-managed",
		Rank: 2, TargetFillPct: 50, FullThresholdPct: 95,
		Health: "healthy", ActivityBand: tiering.ActivityBandCold,
		ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`, BackingRef: "zfs-managed:poolB/warm",
	}
	if err := store.CreateTierTarget(t2); err != nil {
		t.Fatalf("CreateTierTarget t2: %v", err)
	}
	if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
		TierTargetID: t2.ID, PoolName: "poolB", DatasetName: "warm",
		DatasetPath: "/poolB/warm", FUSEMode: "passthrough",
	}); err != nil {
		t.Fatalf("UpsertZFSManagedTarget t2: %v", err)
	}

	a := zfsmgdadapter.NewAdapter(store, runDir)
	_, err := a.CreateNamespace(tiering.NamespaceSpec{
		Name:            "myns",
		PlacementDomain: "dom",
		NamespaceKind:   "filespace",
		ExposedPath:     "/mnt/tiering/myns",
		PolicyTargetIDs: []string{t1.ID, t2.ID},
		BackendDetails: map[string]any{
			"pool_name": "poolA",
		},
	})
	adapterErr(t, err, tiering.ErrPermanent)
}

// TestCreateNamespaceRankGapRejected: targets with a gap in ranks are rejected.
func TestCreateNamespaceRankGapRejected(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()

	// Ranks 1 and 3 — rank 2 is missing.
	makeTarget := func(name string, rank int) string {
		row := &db.TierTargetRow{
			Name: name, PlacementDomain: "dom2", BackendKind: "zfs-managed",
			Rank: rank, TargetFillPct: 50, FullThresholdPct: 95,
			Health: "healthy", ActivityBand: tiering.ActivityBandCold,
			ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`,
			BackingRef: "zfs-managed:pool/" + name,
		}
		if err := store.CreateTierTarget(row); err != nil {
			t.Fatalf("CreateTierTarget %s: %v", name, err)
		}
		if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
			TierTargetID: row.ID, PoolName: "pool", DatasetName: name,
			DatasetPath: "/pool/" + name, FUSEMode: "passthrough",
		}); err != nil {
			t.Fatalf("UpsertZFSManagedTarget %s: %v", name, err)
		}
		return row.ID
	}

	id1 := makeTarget("rank1", 1)
	id3 := makeTarget("rank3", 3) // gap: rank 2 missing

	a := zfsmgdadapter.NewAdapter(store, runDir)
	_, err := a.CreateNamespace(tiering.NamespaceSpec{
		Name:            "gapns",
		PlacementDomain: "dom2",
		NamespaceKind:   "filespace",
		PolicyTargetIDs: []string{id1, id3},
		BackendDetails:  map[string]any{"pool_name": "pool"},
	})
	adapterErr(t, err, tiering.ErrPermanent)
}

// TestCreateNamespaceContiguousRanksAccepted: contiguous ranks 1,2 in the same
// pool pass validation (fails later at ZFS create, not at validation).
func TestCreateNamespaceContiguousRanksAccepted(t *testing.T) {
	store := openStore(t)
	runDir := t.TempDir()

	makeTarget := func(name string, rank int) string {
		row := &db.TierTargetRow{
			Name: name, PlacementDomain: "dom3", BackendKind: "zfs-managed",
			Rank: rank, TargetFillPct: 50, FullThresholdPct: 95,
			Health: "healthy", ActivityBand: tiering.ActivityBandCold,
			ActivityTrend: tiering.ActivityTrendStable, CapabilitiesJSON: `{}`,
			BackingRef: "zfs-managed:samepool/" + name,
		}
		if err := store.CreateTierTarget(row); err != nil {
			t.Fatalf("CreateTierTarget %s: %v", name, err)
		}
		if err := store.UpsertZFSManagedTarget(&db.ZFSManagedTargetRow{
			TierTargetID: row.ID, PoolName: "samepool", DatasetName: name,
			DatasetPath: "/samepool/" + name, FUSEMode: "passthrough",
		}); err != nil {
			t.Fatalf("UpsertZFSManagedTarget %s: %v", name, err)
		}
		return row.ID
	}
	id1 := makeTarget("fast", 1)
	id2 := makeTarget("warm", 2)

	a := zfsmgdadapter.NewAdapter(store, runDir)
	_, err := a.CreateNamespace(tiering.NamespaceSpec{
		Name:            "goodns",
		PlacementDomain: "dom3",
		NamespaceKind:   "filespace",
		ExposedPath:     t.TempDir(),
		PolicyTargetIDs: []string{id1, id2},
		BackendDetails:  map[string]any{"pool_name": "samepool"},
	})
	// Validation passes; error must come from ZFS create (no real pool).
	if err == nil {
		t.Fatal("expected an error from ZFS create (no real pool), got nil")
	}
	// The error must NOT be a validation-originated ErrPermanent about pool/rank.
	var ae *tiering.AdapterError
	if errors.As(err, &ae) {
		if ae.Kind == tiering.ErrPermanent &&
			(strings.Contains(ae.Message, "pool") || strings.Contains(ae.Message, "rank")) {
			t.Errorf("got unexpected validation error (should have passed validation): %v", err)
		}
	}
}
