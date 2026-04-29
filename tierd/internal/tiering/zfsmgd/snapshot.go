package zfsmgd

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
)

// Compile-time assertion that *Adapter satisfies CoordinatedSnapshotAdapter.
var _ tiering.CoordinatedSnapshotAdapter = (*Adapter)(nil)

// snapshotQuiesceMinTimeout is the minimum allowed quiesce timeout (seconds).
const snapshotQuiesceMinTimeout = 5

// snapshotQuiesceMaxTimeout is the maximum allowed quiesce timeout (seconds).
const snapshotQuiesceMaxTimeout = 300

// snapshotQuiesceDefaultTimeout is the default quiesce timeout (seconds).
const snapshotQuiesceDefaultTimeout = 30

// snapshotMaxList is the maximum number of snapshot records returned by list.
const snapshotMaxList = 50

// snapMu and snapActive guard the per-namespace "snapshot in progress" flag.
var (
	snapMu     sync.Mutex
	snapActive = make(map[string]bool)
)

// quiesceMu and quiesces hold per-namespace worker quiesce state.
var (
	quiesceMu sync.Mutex
	quiesces  = make(map[string]*nsWorkerQuiesce)
)

// quiesceForNS returns (creating if needed) the quiesce controller for the namespace.
func quiesceForNS(namespaceID string) *nsWorkerQuiesce {
	quiesceMu.Lock()
	defer quiesceMu.Unlock()
	if q, ok := quiesces[namespaceID]; ok {
		return q
	}
	q := &nsWorkerQuiesce{}
	quiesces[namespaceID] = q
	return q
}

// detectPoolMembership checks whether the namespace meta dataset and all
// backing targets (in the same placement domain) reside in the same zpool.
// Returns (snapshotMode, poolName).
func (a *Adapter) detectPoolMembership(namespaceID, namespacePool, placementDomain string) (string, string) {
	targets, err := a.store.ListTierTargets()
	if err != nil {
		log.Printf("zfsmgd: detectPoolMembership: list targets: %v", err)
		return "none", ""
	}

	// Collect ZFS managed targets in the same placement domain.
	var targetPools []string
	found := false
	for _, t := range targets {
		if t.PlacementDomain != placementDomain || t.BackendKind != BackendKind {
			continue
		}
		zfsT, err := a.store.GetZFSManagedTarget(t.ID)
		if err != nil {
			log.Printf("zfsmgd: detectPoolMembership: get target %q: %v", t.ID, err)
			return "none", ""
		}
		targetPools = append(targetPools, zfsT.PoolName)
		found = true
	}

	if !found {
		// No backing targets in this domain — cannot do a coordinated snapshot.
		return "none", ""
	}

	// All target pools must equal the namespace's meta dataset pool.
	for _, p := range targetPools {
		if p != namespacePool {
			return "none", ""
		}
	}
	return "coordinated-namespace", namespacePool
}

// GetNamespaceSnapshotMode returns the stored snapshot_mode for the namespace.
func (a *Adapter) GetNamespaceSnapshotMode(namespaceID string) (string, error) {
	zfsNs, err := a.store.GetZFSManagedNamespace(namespaceID)
	if err != nil {
		return "none", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get ZFS managed namespace for snapshot mode",
			Cause:   err,
		}
	}
	return zfsNs.SnapshotMode, nil
}

// CreateNamespaceSnapshot implements CoordinatedSnapshotAdapter.
func (a *Adapter) CreateNamespaceSnapshot(namespaceID string) (*tiering.NamespaceSnapshot, error) {
	// 1. Load namespace ZFS row.
	zfsNs, err := a.store.GetZFSManagedNamespace(namespaceID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get ZFS managed namespace",
			Cause:   err,
		}
	}

	// 2. Check SnapshotMode.
	if zfsNs.SnapshotMode != "coordinated-namespace" {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrCapabilityViolation,
			Message: "coordinated namespace snapshots are not available: SnapshotMode is " + zfsNs.SnapshotMode,
		}
	}

	// 3. Prevent concurrent snapshots for this namespace.
	snapMu.Lock()
	if snapActive[namespaceID] {
		snapMu.Unlock()
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "snapshot_in_progress: a snapshot operation is already active for this namespace",
		}
	}
	snapActive[namespaceID] = true
	snapMu.Unlock()
	defer func() {
		snapMu.Lock()
		delete(snapActive, namespaceID)
		snapMu.Unlock()
	}()

	// 4. Resolve backing datasets and re-verify pool topology.
	ns, err := a.store.GetManagedNamespace(namespaceID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get managed namespace for snapshot",
			Cause:   err,
		}
	}
	targets, err := a.store.ListTierTargets()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier targets for snapshot",
			Cause:   err,
		}
	}

	var backingDatasets []string
	for _, t := range targets {
		if t.PlacementDomain != ns.PlacementDomain || t.BackendKind != BackendKind {
			continue
		}
		zfsT, err := a.store.GetZFSManagedTarget(t.ID)
		if err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "get backing ZFS target for snapshot",
				Cause:   err,
			}
		}
		// Re-verify pool membership (safety check for topology changes).
		if zfsT.PoolName != zfsNs.SnapshotPoolName {
			_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "namespace",
				ScopeID:     namespaceID,
				Severity:    db.DegradedSeverityCritical,
				Code:        "snapshot_topology_changed",
				Message:     fmt.Sprintf("backing dataset %q pool %q no longer matches namespace pool %q; coordinated snapshot aborted", zfsT.DatasetPath, zfsT.PoolName, zfsNs.SnapshotPoolName),
			})
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("pool topology changed since namespace creation: backing dataset pool %q != namespace pool %q", zfsT.PoolName, zfsNs.SnapshotPoolName),
			}
		}
		backingDatasets = append(backingDatasets, zfsT.DatasetPath)
	}

	// Determine effective quiesce timeout.
	quiesceTimeout := zfsNs.SnapshotQuiesceTimeout
	if quiesceTimeout < snapshotQuiesceMinTimeout {
		quiesceTimeout = snapshotQuiesceDefaultTimeout
	}
	if quiesceTimeout > snapshotQuiesceMaxTimeout {
		quiesceTimeout = snapshotQuiesceMaxTimeout
	}
	qtDur := time.Duration(quiesceTimeout) * time.Second

	// 5. Quiesce movement workers (wait for in-progress copies to complete).
	qc := quiesceForNS(namespaceID)
	if err := qc.beginQuiesce(qtDur); err != nil {
		a.recordSnapshotTimeoutState(namespaceID)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "movement worker quiesce timed out: " + err.Error(),
		}
	}
	defer qc.endQuiesce()

	// 7. Generate snapshot ID and ZFS snapshot name suffix.
	snapID, err := db.NewControlPlaneID()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "generate snapshot ID",
			Cause:   err,
		}
	}
	zfsSnapSuffix := "tiering-snap-" + snapID

	// 8. Build zfs snapshot arguments: all backing datasets + meta dataset.
	var snapArgs []string
	var backingSnaps []tiering.BackingSnapshot
	for _, ds := range backingDatasets {
		snapName := ds + "@" + zfsSnapSuffix
		snapArgs = append(snapArgs, snapName)
		backingSnaps = append(backingSnaps, tiering.BackingSnapshot{
			DatasetPath:  ds,
			SnapshotName: snapName,
		})
	}
	metaSnapName := zfsNs.MetaDataset + "@" + zfsSnapSuffix
	snapArgs = append(snapArgs, metaSnapName)
	metaSnap := tiering.BackingSnapshot{
		DatasetPath:  zfsNs.MetaDataset,
		SnapshotName: metaSnapName,
	}

	// 9. Issue single atomic zfs snapshot command.
	if err := runZFS(append([]string{"snapshot"}, snapArgs...)...); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "zfs snapshot failed",
			Cause:   err,
		}
	}

	// Release movement-worker quiesce now that snapshot is committed.
	qc.endQuiesce()

	// 11. Persist snapshot record.
	createdAt := time.Now().UTC().Format(time.RFC3339)
	backingJSON, _ := json.Marshal(backingSnaps)
	metaJSON, _ := json.Marshal(metaSnap)
	snapRow := &db.ZFSManagedNamespaceSnapshotRow{
		ID:                   snapID,
		NamespaceID:          namespaceID,
		PoolName:             zfsNs.SnapshotPoolName,
		ZFSSnapshotName:      zfsSnapSuffix,
		BackingSnapshotsJSON: string(backingJSON),
		MetaSnapshotJSON:     string(metaJSON),
		CreatedAt:            createdAt,
		Consistency:          "atomic",
	}
	if err := a.store.CreateZFSManagedNamespaceSnapshot(snapRow); err != nil {
		// Snapshot was taken but persistence failed. Log and continue.
		log.Printf("zfsmgd: snapshot %q committed to ZFS but metadata persist failed: %v", snapID, err)
	}

	return &tiering.NamespaceSnapshot{
		SnapshotID:      snapID,
		NamespaceID:     namespaceID,
		PoolName:        zfsNs.SnapshotPoolName,
		ZFSSnapshotName: zfsSnapSuffix,
		BackingSnaps:    backingSnaps,
		MetaSnapshot:    metaSnap,
		CreatedAt:       createdAt,
		Consistency:     "atomic",
	}, nil
}

// ListNamespaceSnapshots implements CoordinatedSnapshotAdapter.
func (a *Adapter) ListNamespaceSnapshots(namespaceID string) ([]tiering.NamespaceSnapshotSummary, error) {
	rows, err := a.store.ListZFSManagedNamespaceSnapshots(namespaceID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list namespace snapshots",
			Cause:   err,
		}
	}
	out := make([]tiering.NamespaceSnapshotSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, tiering.NamespaceSnapshotSummary{
			SnapshotID:  r.ID,
			NamespaceID: r.NamespaceID,
			PoolName:    r.PoolName,
			CreatedAt:   r.CreatedAt,
			Consistency: r.Consistency,
		})
	}
	return out, nil
}

// GetNamespaceSnapshot implements CoordinatedSnapshotAdapter.
func (a *Adapter) GetNamespaceSnapshot(namespaceID, snapshotID string) (*tiering.NamespaceSnapshot, error) {
	row, err := a.store.GetZFSManagedNamespaceSnapshot(namespaceID, snapshotID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get namespace snapshot",
			Cause:   err,
		}
	}
	return snapshotRowToModel(row)
}

// DeleteNamespaceSnapshot implements CoordinatedSnapshotAdapter.
func (a *Adapter) DeleteNamespaceSnapshot(namespaceID, snapshotID string) error {
	row, err := a.store.GetZFSManagedNamespaceSnapshot(namespaceID, snapshotID)
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get namespace snapshot for delete",
			Cause:   err,
		}
	}

	// Parse backing and meta snapshots.
	var backingSnaps []tiering.BackingSnapshot
	if err := json.Unmarshal([]byte(row.BackingSnapshotsJSON), &backingSnaps); err != nil {
		log.Printf("zfsmgd: delete snapshot %q: unmarshal backing snapshots: %v", snapshotID, err)
	}
	var metaSnap tiering.BackingSnapshot
	if err := json.Unmarshal([]byte(row.MetaSnapshotJSON), &metaSnap); err != nil {
		log.Printf("zfsmgd: delete snapshot %q: unmarshal meta snapshot: %v", snapshotID, err)
	}

	// Build list of snapshot names.
	var allSnaps []string
	for _, bs := range backingSnaps {
		allSnaps = append(allSnaps, bs.SnapshotName)
	}
	if metaSnap.SnapshotName != "" {
		allSnaps = append(allSnaps, metaSnap.SnapshotName)
	}

	// Destroy existing snapshots in a single zfs destroy invocation.
	if err := zfsDestroySnapshots(allSnaps); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "zfs destroy snapshots failed",
			Cause:   err,
		}
	}

	// Remove metadata record.
	if err := a.store.DeleteZFSManagedNamespaceSnapshot(namespaceID, snapshotID); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "delete snapshot metadata record",
			Cause:   err,
		}
	}
	return nil
}

// zfsDestroySnapshots destroys the given ZFS snapshot names. Snapshots that do
// not exist are silently skipped (logged as a warning). The remaining existing
// snapshots are destroyed in a single zfs destroy invocation.
func zfsDestroySnapshots(snapNames []string) error {
	if len(snapNames) == 0 {
		return nil
	}

	// Filter to only existing snapshots.
	var existing []string
	for _, name := range snapNames {
		cmd := exec.Command("zfs", "list", "-H", "-o", "name", "-t", "snapshot", name)
		if err := cmd.Run(); err == nil {
			existing = append(existing, name)
		} else {
			log.Printf("zfsmgd: snapshot %q not found (out-of-band removal?), skipping destroy", name)
		}
	}
	if len(existing) == 0 {
		return nil
	}

	args := append([]string{"destroy"}, existing...)
	return runZFS(args...)
}

// snapshotRowToModel converts a DB row to a NamespaceSnapshot model.
func snapshotRowToModel(row *db.ZFSManagedNamespaceSnapshotRow) (*tiering.NamespaceSnapshot, error) {
	var backingSnaps []tiering.BackingSnapshot
	if err := json.Unmarshal([]byte(row.BackingSnapshotsJSON), &backingSnaps); err != nil {
		backingSnaps = nil
	}
	var metaSnap tiering.BackingSnapshot
	if err := json.Unmarshal([]byte(row.MetaSnapshotJSON), &metaSnap); err != nil {
		metaSnap = tiering.BackingSnapshot{}
	}
	return &tiering.NamespaceSnapshot{
		SnapshotID:      row.ID,
		NamespaceID:     row.NamespaceID,
		PoolName:        row.PoolName,
		ZFSSnapshotName: row.ZFSSnapshotName,
		BackingSnaps:    backingSnaps,
		MetaSnapshot:    metaSnap,
		CreatedAt:       row.CreatedAt,
		Consistency:     row.Consistency,
	}, nil
}

// recordSnapshotTimeoutState records a snapshot_timeout degraded state.
func (a *Adapter) recordSnapshotTimeoutState(namespaceID string) {
	_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
		BackendKind: BackendKind,
		ScopeKind:   "namespace",
		ScopeID:     namespaceID,
		Severity:    db.DegradedSeverityWarning,
		Code:        "snapshot_timeout",
		Message:     "Snapshot aborted: quiesce timed out. The namespace is operating normally.",
	})
}

// snapshotInProgress returns true if a snapshot is currently being created
// for the given namespace.
func snapshotInProgress(namespaceID string) bool {
	snapMu.Lock()
	defer snapMu.Unlock()
	return snapActive[namespaceID]
}

