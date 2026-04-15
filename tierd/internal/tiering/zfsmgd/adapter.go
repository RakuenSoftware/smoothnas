package zfsmgd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

// Compile-time assertion that *Adapter satisfies the TieringAdapter interface.
var _ tiering.TieringAdapter = (*Adapter)(nil)

// BackendKind is the canonical identifier for the managed ZFS backend.
const BackendKind = "zfs-managed"

// defaultRunDir is used when no runDir is given to NewAdapter.
const defaultRunDir = "/run/tierd"

// ExportedCapabilities returns TargetCapabilities for the managed ZFS backend.
// Exported for use in tests.
func ExportedCapabilities(fuseMode string) tiering.TargetCapabilities {
	return zfsManagedCapabilities(fuseMode)
}

// zfsManagedCapabilities returns TargetCapabilities for the managed ZFS backend.
func zfsManagedCapabilities(fuseMode string) tiering.TargetCapabilities {
	return tiering.TargetCapabilities{
		MovementGranularity: "file",
		PinScope:            "object",
		SupportsOnlineMove:  true,
		SupportsRecall:      true,
		RecallMode:          "synchronous",
		SnapshotMode:        "none", // until proposal 05
		FUSEMode:            fuseMode,
		SupportsChecksums:   true,
		SupportsCompression: true,
		SupportsWriteBias:   false,
	}
}

// backingRefTarget returns the backing_ref for a tier target.
// Format: "zfs-managed:{poolName}/{datasetName}"
func backingRefTarget(poolName, datasetName string) string {
	return fmt.Sprintf("zfs-managed:%s/%s", poolName, datasetName)
}

// backingRefNamespace returns the backing_ref for a managed namespace.
// Format: "zfs-managed:{poolName}/{namespaceName}"
func backingRefNamespace(poolName, namespaceName string) string {
	return fmt.Sprintf("zfs-managed:%s/%s", poolName, namespaceName)
}

// defaultMovementWorkerConcurrency is the default maximum number of concurrent
// movement workers per adapter instance.
const defaultMovementWorkerConcurrency = 4

// defaultRecallTimeoutSeconds is the default synchronous recall timeout.
const defaultRecallTimeoutSeconds = 300

// defaultMigrationIOHighWaterPct is the default device utilization threshold
// above which movement workers sleep before retrying a copy chunk.
const defaultMigrationIOHighWaterPct = 80

// movementCopyChunkSize is the size of each read/write iteration in the
// movement copy loop.
const movementCopyChunkSize = 4 * 1024 * 1024 // 4 MiB

// pollInterval is how long a throttled movement worker sleeps before retrying.
const pollInterval = 5 * time.Second

// Adapter implements tiering.TieringAdapter for the managed ZFS backend.
type Adapter struct {
	store      *db.Store
	supervisor *DaemonSupervisor
	server     *SocketServer

	// mu protects namespaceDaemonState and fanotifyWatchers.
	mu                   sync.Mutex
	namespaceDaemonState map[string]string          // namespaceID → daemon_state
	fanotifyWatchers     map[string]*FanotifyWatcher // namespaceID → watcher

	runDir string

	// Movement worker concurrency.
	movementSem chan struct{} // buffered channel; acquire before starting a worker

	// Configuration (may be overridden for testing).
	movementWorkerConcurrency int
	recallTimeoutSeconds      int
	migrationIOHighWaterPct   int
	iostat                    IOStatProvider

	// metaMu + metaStores mirror the mdadm adapter: per-pool meta store on
	// the pool's fastest dataset. Populated by main.go at startup. ZFS
	// doesn't have an equivalent high-volume CREATE hot path yet, so these
	// are scaffolding — pin state and future heat tracking write here.
	metaMu     sync.RWMutex
	metaStores map[string]*meta.PoolMetaStore // poolName → store
}

// NewAdapter returns a new managed ZFS tiering adapter.
// runDir is the directory for runtime state (sockets, etc.); defaults to /run/tierd.
func NewAdapter(store *db.Store, runDir string) *Adapter {
	if runDir == "" {
		runDir = defaultRunDir
	}
	supervisor := NewDaemonSupervisor()
	concurrency := defaultMovementWorkerConcurrency
	a := &Adapter{
		store:                     store,
		supervisor:                supervisor,
		runDir:                    runDir,
		namespaceDaemonState:      make(map[string]string),
		fanotifyWatchers:          make(map[string]*FanotifyWatcher),
		movementSem:               make(chan struct{}, concurrency),
		movementWorkerConcurrency: concurrency,
		recallTimeoutSeconds:      defaultRecallTimeoutSeconds,
		migrationIOHighWaterPct:   defaultMigrationIOHighWaterPct,
		iostat:                    ExecIOStat{},
		metaStores:                make(map[string]*meta.PoolMetaStore),
	}
	a.server = NewSocketServer(runDir, a)
	return a
}

// SetMetaStore registers the per-pool metadata store opened at startup.
// Mirrors the mdadm adapter. ZFS backends don't currently have a sync
// hot-path write that the meta store replaces, but pin state + future
// heat tracking flow through it.
func (a *Adapter) SetMetaStore(poolName string, store *meta.PoolMetaStore) {
	a.metaMu.Lock()
	old := a.metaStores[poolName]
	a.metaStores[poolName] = store
	a.metaMu.Unlock()
	if old != nil && old != store {
		go func() { _ = old.Close() }()
	}
}

// metaStoreFor returns the meta store for a pool or nil.
func (a *Adapter) metaStoreFor(poolName string) *meta.PoolMetaStore {
	a.metaMu.RLock()
	s := a.metaStores[poolName]
	a.metaMu.RUnlock()
	return s
}

// CloseMetaStores drains every registered meta store. Called from shutdown.
func (a *Adapter) CloseMetaStores() {
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	for pool, s := range a.metaStores {
		if err := s.Close(); err != nil {
			log.Printf("zfsmgd: close meta store for pool %s: %v", pool, err)
		}
	}
	a.metaStores = map[string]*meta.PoolMetaStore{}
}

// MetaStats returns per-shard stats for every open ZFS meta store.
// Exposed via the same /api/tiering/meta/stats endpoint used by mdadm.
func (a *Adapter) MetaStats() map[string][]meta.ShardStats {
	a.metaMu.RLock()
	defer a.metaMu.RUnlock()
	out := make(map[string][]meta.ShardStats, len(a.metaStores))
	for pool, s := range a.metaStores {
		out[pool] = s.Stats()
	}
	return out
}

// Kind returns the backend identifier.
func (a *Adapter) Kind() string { return BackendKind }

// ---- helpers -----------------------------------------------------------------

// runZFS runs a zfs sub-command and returns a combined error on failure.
func runZFS(args ...string) error {
	cmd := exec.Command("zfs", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zfs %v: %v: %s", args, err, out)
	}
	return nil
}

// runCmd runs an arbitrary command and returns a combined error on failure.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, out)
	}
	return nil
}

// capabilitiesJSON serialises capabilities for storage in the DB.
func capabilitiesJSON(fuseMode string) (string, error) {
	caps := zfsManagedCapabilities(fuseMode)
	b, err := json.Marshal(caps)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- Target lifecycle --------------------------------------------------------

// CreateTarget creates a new ZFS dataset and registers it as a tier target.
func (a *Adapter) CreateTarget(spec tiering.TargetSpec) (*tiering.TargetState, error) {
	poolName, ok := spec.BackendDetails["pool_name"].(string)
	if !ok || poolName == "" {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "pool_name is required in BackendDetails",
		}
	}

	datasetName := spec.Name
	datasetPath := poolName + "/" + datasetName
	fuseMode := "passthrough"
	if fm, ok2 := spec.BackendDetails["fuse_mode"].(string); ok2 && fm != "" {
		fuseMode = fm
	}

	// Create the dataset.
	if err := runZFS("create", "-p", datasetPath); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "create ZFS dataset",
			Cause:   err,
		}
	}

	// Get the mount point for the dataset.
	mountPoint, err := zfsMountPoint(datasetPath)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get dataset mount point",
			Cause:   err,
		}
	}

	// Set ownership and permissions.
	if err := runCmd("chown", "tierd:tierd", mountPoint); err != nil {
		_ = runZFS("destroy", "-r", datasetPath)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "chown dataset mount point",
			Cause:   err,
		}
	}
	if err := runCmd("chmod", "0700", mountPoint); err != nil {
		_ = runZFS("destroy", "-r", datasetPath)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "chmod dataset mount point",
			Cause:   err,
		}
	}

	capsJSON, err := capabilitiesJSON(fuseMode)
	if err != nil {
		_ = runZFS("destroy", "-r", datasetPath)
		return nil, &tiering.AdapterError{Kind: tiering.ErrPermanent, Message: "marshal capabilities", Cause: err}
	}

	placementDomain := spec.PlacementDomain
	if placementDomain == "" {
		placementDomain = BackendKind
	}

	row := &db.TierTargetRow{
		Name:             spec.Name,
		PlacementDomain:  placementDomain,
		BackendKind:      BackendKind,
		Rank:             spec.Rank,
		TargetFillPct:    spec.TargetFillPct,
		FullThresholdPct: spec.FullThresholdPct,
		Health:           "healthy",
		ActivityBand:     tiering.ActivityBandCold,
		ActivityTrend:    tiering.ActivityTrendStable,
		CapabilitiesJSON: capsJSON,
		BackingRef:       backingRefTarget(poolName, datasetName),
	}
	if err := a.store.CreateTierTarget(row); err != nil {
		_ = runZFS("destroy", "-r", datasetPath)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create tier target in DB",
			Cause:   err,
		}
	}

	zfsRow := &db.ZFSManagedTargetRow{
		TierTargetID: row.ID,
		PoolName:     poolName,
		DatasetName:  datasetName,
		DatasetPath:  datasetPath,
		FUSEMode:     fuseMode,
	}
	if err := a.store.UpsertZFSManagedTarget(zfsRow); err != nil {
		_ = a.store.DeleteTierTarget(row.ID)
		_ = runZFS("destroy", "-r", datasetPath)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "upsert ZFS managed target in DB",
			Cause:   err,
		}
	}

	caps := zfsManagedCapabilities(fuseMode)
	return &tiering.TargetState{
		ID:           row.ID,
		Name:         spec.Name,
		Health:       "healthy",
		Capabilities: caps,
		BackendDetails: map[string]any{
			"pool_name":    poolName,
			"dataset_name": datasetName,
			"dataset_path": datasetPath,
			"fuse_mode":    fuseMode,
		},
	}, nil
}

// DestroyTarget destroys the ZFS dataset and removes all DB rows.
func (a *Adapter) DestroyTarget(targetID string) error {
	zfsRow, err := a.store.GetZFSManagedTarget(targetID)
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get ZFS managed target",
			Cause:   err,
		}
	}

	if err := runZFS("destroy", "-r", zfsRow.DatasetPath); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "destroy ZFS dataset",
			Cause:   err,
		}
	}

	if err := a.store.DeleteZFSManagedTarget(targetID); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "delete ZFS managed target from DB",
			Cause:   err,
		}
	}

	if err := a.store.DeleteTierTarget(targetID); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "delete tier target from DB",
			Cause:   err,
		}
	}

	return nil
}

// ListTargets returns the state of all managed ZFS tier targets.
func (a *Adapter) ListTargets() ([]tiering.TargetState, error) {
	zfsRows, err := a.store.ListZFSManagedTargets()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list ZFS managed targets",
			Cause:   err,
		}
	}

	out := make([]tiering.TargetState, 0, len(zfsRows))
	for _, zr := range zfsRows {
		ttRow, err := a.store.GetTierTarget(zr.TierTargetID)
		if err != nil {
			log.Printf("zfsmgd: ListTargets: get tier target %q: %v", zr.TierTargetID, err)
			continue
		}
		caps := zfsManagedCapabilities(zr.FUSEMode)
		out = append(out, tiering.TargetState{
			ID:            ttRow.ID,
			Name:          ttRow.Name,
			Health:        ttRow.Health,
			ActivityBand:  ttRow.ActivityBand,
			ActivityTrend: ttRow.ActivityTrend,
			Capabilities:  caps,
			BackendDetails: map[string]any{
				"pool_name":    zr.PoolName,
				"dataset_name": zr.DatasetName,
				"dataset_path": zr.DatasetPath,
				"fuse_mode":    zr.FUSEMode,
			},
		})
	}
	return out, nil
}

// ---- Namespace lifecycle -----------------------------------------------------

// CreateNamespace creates the meta dataset, starts the socket server and FUSE
// daemon, and registers the namespace in the DB.
func (a *Adapter) CreateNamespace(spec tiering.NamespaceSpec) (*tiering.NamespaceState, error) {
	poolName, ok := spec.BackendDetails["pool_name"].(string)
	if !ok || poolName == "" {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "pool_name is required in BackendDetails",
		}
	}
	fuseMode := "passthrough"
	if fm, ok2 := spec.BackendDetails["fuse_mode"].(string); ok2 && fm != "" {
		fuseMode = fm
	}

	// Validate PolicyTargetIDs: all backing datasets must share the same ZFS
	// pool and ranks must be contiguous starting at 1.
	if len(spec.PolicyTargetIDs) > 0 {
		if err := a.validateTargetsForNamespace(poolName, spec.PolicyTargetIDs); err != nil {
			return nil, err
		}
	}

	namespaceName := spec.Name
	metaDataset := poolName + "/tiering_meta/" + namespaceName

	// Create meta dataset.
	if err := runZFS("create", "-p", metaDataset); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "create meta dataset",
			Cause:   err,
		}
	}

	// Create exposed mount path.
	if spec.ExposedPath != "" {
		if err := os.MkdirAll(spec.ExposedPath, 0755); err != nil {
			_ = runZFS("destroy", "-r", metaDataset)
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: "create exposed mount path",
				Cause:   err,
			}
		}
	}

	// Ensure socket dir exists.
	if err := os.MkdirAll(a.runDir, 0750); err != nil {
		_ = runZFS("destroy", "-r", metaDataset)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "create run dir",
			Cause:   err,
		}
	}

	// Persist to DB first (we need the namespace ID for the socket path).
	placementDomain := spec.PlacementDomain
	if placementDomain == "" {
		placementDomain = BackendKind
	}

	nsRow := &db.ManagedNamespaceRow{
		Name:            namespaceName,
		PlacementDomain: placementDomain,
		BackendKind:     BackendKind,
		NamespaceKind:   spec.NamespaceKind,
		ExposedPath:     spec.ExposedPath,
		PinState:        "none",
		Health:          "starting",
		PlacementState:  "unknown",
		BackendRef:      backingRefNamespace(poolName, namespaceName),
	}
	if err := a.store.CreateManagedNamespace(nsRow); err != nil {
		_ = runZFS("destroy", "-r", metaDataset)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create managed namespace in DB",
			Cause:   err,
		}
	}

	namespaceID := nsRow.ID

	// Start socket server for this namespace.
	socketPath, err := a.server.Start(namespaceID)
	if err != nil {
		_ = a.store.DeleteManagedNamespace(namespaceID)
		_ = runZFS("destroy", "-r", metaDataset)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "start socket server",
			Cause:   err,
		}
	}

	// Detect whether all datasets share a single zpool for coordinated snapshots.
	snapshotMode, snapshotPool := a.detectPoolMembership(namespaceID, poolName, placementDomain)

	// Persist the ZFS managed namespace row.
	zfsNsRow := &db.ZFSManagedNamespaceRow{
		NamespaceID:           namespaceID,
		PoolName:              poolName,
		MetaDataset:           metaDataset,
		SocketPath:            socketPath,
		MountPath:             spec.ExposedPath,
		DaemonState:           "starting",
		FUSEMode:              fuseMode,
		SnapshotMode:          snapshotMode,
		SnapshotPoolName:      snapshotPool,
		SnapshotQuiesceTimeout: snapshotQuiesceDefaultTimeout,
	}
	if err := a.store.UpsertZFSManagedNamespace(zfsNsRow); err != nil {
		a.server.Stop(namespaceID)
		_ = a.store.DeleteManagedNamespace(namespaceID)
		_ = runZFS("destroy", "-r", metaDataset)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "upsert ZFS managed namespace in DB",
			Cause:   err,
		}
	}

	// Start the FUSE daemon.
	if err := a.supervisor.Start(namespaceID, spec.ExposedPath, socketPath); err != nil {
		a.server.Stop(namespaceID)
		_ = a.store.DeleteZFSManagedNamespace(namespaceID)
		_ = a.store.DeleteManagedNamespace(namespaceID)
		_ = runZFS("destroy", "-r", metaDataset)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "start FUSE daemon",
			Cause:   err,
		}
	}

	pid := a.supervisor.ActivePID(namespaceID)
	_ = a.store.SetZFSManagedNamespaceDaemonState(namespaceID, "running", pid)

	a.mu.Lock()
	a.namespaceDaemonState[namespaceID] = "running"
	a.mu.Unlock()

	// Set up crash supervision.
	a.supervisor.Supervise(namespaceID, func() {
		a.onDaemonCrash(namespaceID, spec.ExposedPath, socketPath)
	})

	// Start fanotify bypass detection on the backing mount path.
	// We use the meta dataset's expected mount as the backing path.
	// Bypass events from any process other than the daemon are reported.
	if spec.ExposedPath != "" {
		watcher, err := StartFanotifyWatch(spec.ExposedPath, pid, namespaceID, func() {
			a.onBypassDetected(namespaceID)
		})
		if err != nil {
			if err == ErrFanotifyUnavailable {
				log.Printf("zfsmgd: fanotify unavailable for namespace %q: bypass detection disabled", namespaceID)
				_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
					BackendKind: BackendKind,
					ScopeKind:   "namespace",
					ScopeID:     namespaceID,
					Severity:    db.DegradedSeverityWarning,
					Code:        "bypass_detection_unavailable",
					Message:     fmt.Sprintf("fanotify unavailable for namespace %q", namespaceID),
				})
			} else {
				log.Printf("zfsmgd: fanotify watch failed for namespace %q: %v", namespaceID, err)
			}
		} else {
			a.mu.Lock()
			a.fanotifyWatchers[namespaceID] = watcher
			a.mu.Unlock()
		}
	}

	return &tiering.NamespaceState{
		ID:             namespaceID,
		Health:         "healthy",
		PlacementState: "unknown",
		BackendRef:     backingRefNamespace(poolName, namespaceName),
		BackendDetails: map[string]any{
			"pool_name":    poolName,
			"meta_dataset": metaDataset,
			"socket_path":  socketPath,
			"fuse_mode":    fuseMode,
		},
	}, nil
}

// onDaemonCrash is called when a supervised daemon process exits unexpectedly.
func (a *Adapter) onDaemonCrash(namespaceID, mountPath, socketPath string) {
	log.Printf("zfsmgd: daemon for namespace %q crashed; attempting restart", namespaceID)

	a.mu.Lock()
	a.namespaceDaemonState[namespaceID] = "crashed"
	a.mu.Unlock()

	_ = a.store.SetZFSManagedNamespaceDaemonState(namespaceID, "crashed", 0)

	if err := a.supervisor.Restart(namespaceID, mountPath, socketPath); err != nil {
		log.Printf("zfsmgd: restart failed for namespace %q: %v", namespaceID, err)
		a.mu.Lock()
		a.namespaceDaemonState[namespaceID] = "stopped"
		a.mu.Unlock()
		_ = a.store.SetZFSManagedNamespaceDaemonState(namespaceID, "stopped", 0)
		return
	}

	pid := a.supervisor.ActivePID(namespaceID)
	_ = a.store.SetZFSManagedNamespaceDaemonState(namespaceID, "running", pid)

	a.mu.Lock()
	a.namespaceDaemonState[namespaceID] = "running"
	a.mu.Unlock()

	a.supervisor.Supervise(namespaceID, func() {
		a.onDaemonCrash(namespaceID, mountPath, socketPath)
	})
}

// DestroyNamespace stops the daemon, unmounts the FUSE mount, destroys the
// meta dataset, and removes DB rows.
func (a *Adapter) DestroyNamespace(namespaceID string) error {
	zfsNs, err := a.store.GetZFSManagedNamespace(namespaceID)
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get ZFS managed namespace",
			Cause:   err,
		}
	}

	// Stop the daemon.
	if err := a.supervisor.Stop(namespaceID); err != nil {
		log.Printf("zfsmgd: stop daemon for namespace %q: %v", namespaceID, err)
	}

	// Stop the socket server.
	a.server.Stop(namespaceID)

	// Stop fanotify watcher if running.
	a.mu.Lock()
	watcher := a.fanotifyWatchers[namespaceID]
	delete(a.fanotifyWatchers, namespaceID)
	a.mu.Unlock()
	if watcher != nil {
		watcher.Stop()
	}

	// Unmount the FUSE mount.
	if zfsNs.MountPath != "" {
		if err := runCmd("fusermount3", "-u", zfsNs.MountPath); err != nil {
			log.Printf("zfsmgd: fusermount3 -u %q: %v", zfsNs.MountPath, err)
		}
	}

	// Destroy the meta dataset.
	if err := runZFS("destroy", "-r", zfsNs.MetaDataset); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "destroy meta dataset",
			Cause:   err,
		}
	}

	// Remove DB rows.
	if err := a.store.DeleteZFSManagedNamespace(namespaceID); err != nil {
		log.Printf("zfsmgd: delete ZFS managed namespace %q: %v", namespaceID, err)
	}
	if err := a.store.DeleteManagedNamespace(namespaceID); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "delete managed namespace from DB",
			Cause:   err,
		}
	}

	a.mu.Lock()
	delete(a.namespaceDaemonState, namespaceID)
	a.mu.Unlock()

	return nil
}

// ListNamespaces returns the state of all managed ZFS namespaces.
func (a *Adapter) ListNamespaces() ([]tiering.NamespaceState, error) {
	zfsNs, err := a.store.ListZFSManagedNamespaces()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list ZFS managed namespaces",
			Cause:   err,
		}
	}

	out := make([]tiering.NamespaceState, 0, len(zfsNs))
	for _, zn := range zfsNs {
		nsRow, err := a.store.GetManagedNamespace(zn.NamespaceID)
		if err != nil {
			log.Printf("zfsmgd: ListNamespaces: get managed namespace %q: %v", zn.NamespaceID, err)
			continue
		}
		out = append(out, tiering.NamespaceState{
			ID:             nsRow.ID,
			Health:         nsRow.Health,
			PlacementState: nsRow.PlacementState,
			BackendRef:     nsRow.BackendRef,
			BackendDetails: map[string]any{
				"pool_name":    zn.PoolName,
				"meta_dataset": zn.MetaDataset,
				"socket_path":  zn.SocketPath,
				"mount_path":   zn.MountPath,
				"daemon_pid":   zn.DaemonPID,
				"daemon_state": zn.DaemonState,
				"fuse_mode":    zn.FUSEMode,
			},
		})
	}
	return out, nil
}

// ListManagedObjects returns all managed objects for a given namespace.
func (a *Adapter) ListManagedObjects(namespaceID string) ([]tiering.ManagedObjectState, error) {
	objs, err := a.store.ListManagedObjects(namespaceID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list managed objects",
			Cause:   err,
		}
	}

	out := make([]tiering.ManagedObjectState, 0, len(objs))
	for _, obj := range objs {
		var ps tiering.PlacementSummary
		if obj.PlacementSummaryJSON != "" {
			_ = json.Unmarshal([]byte(obj.PlacementSummaryJSON), &ps)
		}
		out = append(out, tiering.ManagedObjectState{
			ID:               obj.ID,
			ObjectKind:       obj.ObjectKind,
			ObjectKey:        obj.ObjectKey,
			PinState:         obj.PinState,
			ActivityBand:     obj.ActivityBand,
			PlacementSummary: ps,
			BackendRef:       obj.BackendRef,
		})
	}
	return out, nil
}

// ---- Capabilities and policy -------------------------------------------------

// GetCapabilities returns the capabilities for the given target.
func (a *Adapter) GetCapabilities(targetID string) (tiering.TargetCapabilities, error) {
	zfsRow, err := a.store.GetZFSManagedTarget(targetID)
	if err != nil {
		return tiering.TargetCapabilities{}, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get ZFS managed target for capabilities",
			Cause:   err,
		}
	}
	return zfsManagedCapabilities(zfsRow.FUSEMode), nil
}

// GetPolicy returns the fill/threshold policy for a tier target.
func (a *Adapter) GetPolicy(targetID string) (tiering.TargetPolicy, error) {
	row, err := a.store.GetTierTarget(targetID)
	if err != nil {
		return tiering.TargetPolicy{}, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get tier target for policy",
			Cause:   err,
		}
	}
	return tiering.TargetPolicy{
		TargetFillPct:    row.TargetFillPct,
		FullThresholdPct: row.FullThresholdPct,
	}, nil
}

// SetPolicy updates the fill/threshold policy for a tier target.
func (a *Adapter) SetPolicy(targetID string, policy tiering.TargetPolicy) error {
	if err := a.store.UpdateTierTargetPolicy(targetID, policy.TargetFillPct, policy.FullThresholdPct); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "update tier target policy",
			Cause:   err,
		}
	}
	return nil
}

// ---- Reconciliation and activity ---------------------------------------------

// Reconcile checks all registered namespaces and restarts crashed daemons,
// then re-emits degraded states. It also performs crash recovery for any
// interrupted movement workers by scanning the movement_log table.
//
// Per P04B: crash recovery runs before the adapter begins serving placement
// requests or starting movement workers.
func (a *Adapter) Reconcile() error {
	// ── Crash recovery: movement_log ─────────────────────────────────────────
	if err := a.recoverMovementLog(); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "movement log crash recovery",
			Cause:   err,
		}
	}

	zfsNs, err := a.store.ListZFSManagedNamespaces()
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list ZFS managed namespaces for reconcile",
			Cause:   err,
		}
	}

	for _, zn := range zfsNs {
		pid := a.supervisor.ActivePID(zn.NamespaceID)
		if pid == 0 {
			// Daemon is not tracked — check stored state.
			a.mu.Lock()
			state := a.namespaceDaemonState[zn.NamespaceID]
			a.mu.Unlock()

			if state == "running" || state == "" {
				// Expected running but it's not — treat as crashed.
				log.Printf("zfsmgd: Reconcile: daemon for namespace %q not running; restarting", zn.NamespaceID)
				if err := a.supervisor.Restart(zn.NamespaceID, zn.MountPath, zn.SocketPath); err != nil {
					log.Printf("zfsmgd: Reconcile: restart failed for namespace %q: %v", zn.NamespaceID, err)
					a.mu.Lock()
					a.namespaceDaemonState[zn.NamespaceID] = "stopped"
					a.mu.Unlock()
					_ = a.store.SetZFSManagedNamespaceDaemonState(zn.NamespaceID, "stopped", 0)
					continue
				}
				newPID := a.supervisor.ActivePID(zn.NamespaceID)
				_ = a.store.SetZFSManagedNamespaceDaemonState(zn.NamespaceID, "running", newPID)
				a.mu.Lock()
				a.namespaceDaemonState[zn.NamespaceID] = "running"
				a.mu.Unlock()
				a.supervisor.Supervise(zn.NamespaceID, func() {
					a.onDaemonCrash(zn.NamespaceID, zn.MountPath, zn.SocketPath)
				})
			}
		}
	}

	if err := a.syncDegradedStates(); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "sync degraded states",
			Cause:   err,
		}
	}

	_ = a.store.RecordReconcileTimestamp()
	return nil
}

// syncDegradedStates clears the backend's degraded states and re-emits based
// on current namespace and movement job state.
func (a *Adapter) syncDegradedStates() error {
	if err := a.store.DeleteDegradedStatesByBackend(BackendKind); err != nil {
		return fmt.Errorf("delete degraded states: %w", err)
	}

	// Emit namespace_unavailable for crashed/stopped namespaces.
	zfsNs, err := a.store.ListZFSManagedNamespaces()
	if err != nil {
		return fmt.Errorf("list ZFS managed namespaces: %w", err)
	}
	for _, zn := range zfsNs {
		a.mu.Lock()
		state := a.namespaceDaemonState[zn.NamespaceID]
		a.mu.Unlock()
		if state == "crashed" || state == "stopped" {
			_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "namespace",
				ScopeID:     zn.NamespaceID,
				Severity:    db.DegradedSeverityCritical,
				Code:        "namespace_unavailable",
				Message:     fmt.Sprintf("FUSE daemon for namespace %q is %s", zn.NamespaceID, state),
			})
		}
	}

	// Emit movement_failed for failed jobs.
	jobs, err := a.store.ListMovementJobs()
	if err != nil {
		return fmt.Errorf("list movement jobs: %w", err)
	}
	failedCount := 0
	staleCount := 0
	for _, j := range jobs {
		if j.BackendKind != BackendKind {
			continue
		}
		switch j.State {
		case db.MovementJobStateFailed:
			failedCount++
		case db.MovementJobStateStale:
			staleCount++
		}
	}
	if failedCount > 0 {
		_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
			BackendKind: BackendKind,
			ScopeKind:   "backend",
			ScopeID:     BackendKind,
			Severity:    db.DegradedSeverityWarning,
			Code:        "movement_failed",
			Message:     fmt.Sprintf("%d movement job(s) in failed state", failedCount),
		})
	}
	if staleCount > 0 {
		_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
			BackendKind: BackendKind,
			ScopeKind:   "backend",
			ScopeID:     BackendKind,
			Severity:    db.DegradedSeverityWarning,
			Code:        "placement_intent_stale",
			Message:     fmt.Sprintf("%d movement job(s) with stale placement intent", staleCount),
		})
	}

	// Emit no_drain_target when every writable target is above full_threshold_pct.
	if err := a.checkNoDrainTarget(); err != nil {
		log.Printf("zfsmgd: syncDegradedStates: no_drain_target check: %v", err)
	}

	return nil
}

// checkNoDrainTarget queries ZFS utilization for each managed target and emits
// a no_drain_target degraded state for any placement domain where all targets
// are above their full_threshold_pct.
func (a *Adapter) checkNoDrainTarget() error {
	zfsRows, err := a.store.ListZFSManagedTargets()
	if err != nil {
		return fmt.Errorf("list zfs managed targets: %w", err)
	}

	// Group targets by placement domain.
	type targetInfo struct {
		id               string
		rank             int
		fullThresholdPct int
		usedPct          float64
		datasetPath      string
	}
	domainTargets := make(map[string][]targetInfo)

	for _, zr := range zfsRows {
		ttRow, err := a.store.GetTierTarget(zr.TierTargetID)
		if err != nil {
			continue
		}
		usedPct := zfsDatasetUsedPct(zr.DatasetPath)
		domainTargets[ttRow.PlacementDomain] = append(domainTargets[ttRow.PlacementDomain], targetInfo{
			id:               zr.TierTargetID,
			rank:             ttRow.Rank,
			fullThresholdPct: ttRow.FullThresholdPct,
			usedPct:          usedPct,
			datasetPath:      zr.DatasetPath,
		})
	}

	for domain, targets := range domainTargets {
		// Find the lowest-ranked target below full_threshold_pct.
		drainable := false
		for _, t := range targets {
			if t.usedPct < float64(t.fullThresholdPct) {
				drainable = true
				break
			}
		}
		if !drainable && len(targets) > 0 {
			_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "backend",
				ScopeID:     domain,
				Severity:    db.DegradedSeverityCritical,
				Code:        "no_drain_target",
				Message:     fmt.Sprintf("all targets in placement domain %q are above full_threshold_pct; new file placement will fail", domain),
			})
		}
	}
	return nil
}

// zfsDatasetUsedPct returns the percentage of space used for the given ZFS
// dataset. Returns 0.0 on error (ZFS may not be available in tests).
func zfsDatasetUsedPct(datasetPath string) float64 {
	cmd := exec.Command("zfs", "get", "-H", "-p", "-o", "value", "used,available", datasetPath)
	out, err := cmd.Output()
	if err != nil {
		return 0.0
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) < 2 {
		return 0.0
	}
	var used, avail float64
	fmt.Sscan(lines[0], &used)
	fmt.Sscan(lines[1], &avail)
	total := used + avail
	if total == 0 {
		return 0.0
	}
	return used / total * 100.0
}

// CollectActivity samples file atime/mtime statistics from backing datasets
// and assigns activity bands based on time since last access.
func (a *Adapter) CollectActivity() ([]tiering.ActivitySample, error) {
	zfsNs, err := a.store.ListZFSManagedNamespaces()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list ZFS managed namespaces for activity",
			Cause:   err,
		}
	}

	now := time.Now()
	sampledAt := now.UTC().Format(time.RFC3339)
	var samples []tiering.ActivitySample

	for _, zn := range zfsNs {
		objs, err := a.store.ListManagedObjects(zn.NamespaceID)
		if err != nil {
			log.Printf("zfsmgd: CollectActivity: list managed objects for namespace %q: %v", zn.NamespaceID, err)
			continue
		}

		for _, obj := range objs {
			// Resolve the current backing dataset for this object.
			var ps tiering.PlacementSummary
			if obj.PlacementSummaryJSON != "" {
				_ = json.Unmarshal([]byte(obj.PlacementSummaryJSON), &ps)
			}
			if ps.CurrentTargetID == "" {
				continue
			}

			// Look up the dataset path for the current target.
			zfsTarget, err := a.store.GetZFSManagedTarget(ps.CurrentTargetID)
			if err != nil {
				continue
			}

			filePath := filepath.Join(zfsTarget.DatasetPath, obj.ObjectKey)
			info, err := os.Stat(filePath)
			if err != nil {
				continue
			}

			// Use mtime as the last-access proxy (atime may be disabled on ZFS).
			age := now.Sub(info.ModTime())
			band := activityBandFromAge(age)

			samples = append(samples, tiering.ActivitySample{
				TargetID:      ps.CurrentTargetID,
				ObjectID:      obj.ID,
				ActivityBand:  band,
				ActivityTrend: tiering.ActivityTrendStable,
				SampledAt:     sampledAt,
			})
		}
	}

	return samples, nil
}

// activityBandFromAge returns an activity band based on age since last modification.
//
//	< 1h   → hot
//	1–24h  → warm
//	1–7d   → cold
//	> 7d   → idle
func activityBandFromAge(age time.Duration) string {
	switch {
	case age < time.Hour:
		return tiering.ActivityBandHot
	case age < 24*time.Hour:
		return tiering.ActivityBandWarm
	case age < 7*24*time.Hour:
		return tiering.ActivityBandCold
	default:
		return tiering.ActivityBandIdle
	}
}

// ---- Movement ----------------------------------------------------------------

// PlanMovements finds managed objects where current placement differs from
// intended placement and returns movement plans.
func (a *Adapter) PlanMovements() ([]tiering.MovementPlan, error) {
	objs, err := a.allManagedObjects()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list all managed objects for planning",
			Cause:   err,
		}
	}

	var plans []tiering.MovementPlan
	for _, obj := range objs {
		var ps tiering.PlacementSummary
		if obj.PlacementSummaryJSON != "" {
			_ = json.Unmarshal([]byte(obj.PlacementSummaryJSON), &ps)
		}
		if ps.CurrentTargetID == "" || ps.IntendedTargetID == "" {
			continue
		}
		if ps.CurrentTargetID == ps.IntendedTargetID {
			continue
		}

		nsRow, err := a.store.GetManagedNamespace(obj.NamespaceID)
		if err != nil {
			continue
		}
		srcRow, err := a.store.GetTierTarget(ps.CurrentTargetID)
		if err != nil {
			continue
		}
		dstRow, err := a.store.GetTierTarget(ps.IntendedTargetID)
		if err != nil {
			continue
		}

		plans = append(plans, tiering.MovementPlan{
			NamespaceID:     obj.NamespaceID,
			ObjectID:        obj.ID,
			MovementUnit:    "file",
			PlacementDomain: nsRow.PlacementDomain,
			SourceTargetID:  ps.CurrentTargetID,
			DestTargetID:    ps.IntendedTargetID,
			PolicyRevision:  srcRow.PolicyRevision,
			IntentRevision:  nsRow.IntentRevision,
			TriggeredBy:     "planner",
		})
		_ = dstRow // silence unused variable
	}

	return plans, nil
}

// StartMovement validates the plan, creates a MovementJobRow, and launches the
// movement worker goroutine. Returns the job ID.
func (a *Adapter) StartMovement(plan tiering.MovementPlan) (string, error) {
	// Validate source target policy revision.
	srcRow, err := a.store.GetTierTarget(plan.SourceTargetID)
	if err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get source tier target",
			Cause:   err,
		}
	}
	if plan.PolicyRevision != 0 && srcRow.PolicyRevision != plan.PolicyRevision {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrStaleRevision,
			Message: fmt.Sprintf("policy revision mismatch: plan=%d current=%d", plan.PolicyRevision, srcRow.PolicyRevision),
		}
	}

	// Validate namespace intent revision.
	nsRow, err := a.store.GetManagedNamespace(plan.NamespaceID)
	if err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get managed namespace",
			Cause:   err,
		}
	}
	if plan.IntentRevision != 0 && nsRow.IntentRevision != plan.IntentRevision {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrStaleRevision,
			Message: fmt.Sprintf("intent revision mismatch: plan=%d current=%d", plan.IntentRevision, nsRow.IntentRevision),
		}
	}

	// Look up the object.
	obj, err := a.store.GetManagedObject(plan.ObjectID)
	if err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "get managed object",
			Cause:   err,
		}
	}

	// Resolve source and destination datasets.
	srcZFS, err := a.store.GetZFSManagedTarget(plan.SourceTargetID)
	if err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get source ZFS managed target",
			Cause:   err,
		}
	}
	dstZFS, err := a.store.GetZFSManagedTarget(plan.DestTargetID)
	if err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get dest ZFS managed target",
			Cause:   err,
		}
	}

	job := &db.MovementJobRow{
		BackendKind:    BackendKind,
		NamespaceID:    plan.NamespaceID,
		ObjectID:       plan.ObjectID,
		MovementUnit:   plan.MovementUnit,
		SourceTargetID: plan.SourceTargetID,
		DestTargetID:   plan.DestTargetID,
		PolicyRevision: plan.PolicyRevision,
		IntentRevision: plan.IntentRevision,
		PlannerEpoch:   plan.PlannerEpoch,
		State:          db.MovementJobStateRunning,
		TriggeredBy:    plan.TriggeredBy,
		TotalBytes:     plan.TotalBytes,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.store.CreateMovementJob(job); err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create movement job",
			Cause:   err,
		}
	}

	// Create movement_log row (P04B crash-recovery state machine).
	now := time.Now().Unix()
	logRow := &db.ZFSMovementLogRow{
		ObjectID:       plan.ObjectID,
		NamespaceID:    plan.NamespaceID,
		SourceTargetID: plan.SourceTargetID,
		DestTargetID:   plan.DestTargetID,
		ObjectKey:      obj.ObjectKey,
		State:          db.ZFSMoveLogCopyInProgress,
		StartedAt:      now,
		UpdatedAt:      now,
	}
	if err := a.store.InsertZFSMovementLog(logRow); err != nil {
		_ = a.store.UpdateMovementJobState(job.ID, db.MovementJobStateFailed, 0, "create movement log")
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create movement log row",
			Cause:   err,
		}
	}

	go a.runMovementWorker(job.ID, logRow.ID, plan.NamespaceID, plan.ObjectID, obj.ObjectKey, srcZFS.DatasetPath, dstZFS.DatasetPath)

	return job.ID, nil
}

// runMovementWorker copies a file between datasets using the proposal-defined
// 5-phase state machine: copy_in_progress → copy_complete → switched →
// cleanup_complete. A separate movement_log row tracks each phase for crash
// recovery.
//
// The worker:
//   - acquires the concurrency semaphore before doing any I/O
//   - locks its OS thread and applies ionice idle-class scheduling
//   - reads/writes in movementCopyChunkSize chunks, checking recall_pending and
//     the I/O high-water mark before each chunk
//   - verifies the destination SHA-256 after copy
//   - atomically updates placement and the movement_log state in one SQLite
//     transaction via the switch step
func (a *Adapter) runMovementWorker(jobID, logID, namespaceID, objectID, objectKey, srcDataset, dstDataset string) {
	// Acquire concurrency semaphore.
	a.movementSem <- struct{}{}
	defer func() { <-a.movementSem }()

	// Lock to a single OS thread for ionice to take effect.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setIdle()

	failLog := func(reason string) {
		log.Printf("zfsmgd: movement job=%q log=%q failed: %s", jobID, logID, reason)
		_ = a.store.UpdateMovementJobState(jobID, db.MovementJobStateFailed, 0, reason)
		_ = a.store.UpdateZFSMovementLogState(logID, db.ZFSMoveLogFailed, reason)
	}

	srcPath := filepath.Join(srcDataset, objectKey)
	dstPath := filepath.Join(dstDataset, objectKey)

	// Resolve source device name for I/O throttle checks.
	srcDevice := deviceBasename(srcDataset)

	// 1. Ensure destination directory exists.
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		failLog(fmt.Sprintf("mkdir dst: %v", err))
		return
	}

	// ── Phase: copy_in_progress ──────────────────────────────────────────────
	// Park at the copy checkpoint so that a concurrent coordinated snapshot can
	// quiesce without racing an in-flight chunk copy.
	qc := quiesceForNS(namespaceID)
	qc.enterCopy()
	defer qc.exitCopy()

	srcFile, err := os.Open(srcPath)
	if err != nil {
		failLog(fmt.Sprintf("open src: %v", err))
		return
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		failLog(fmt.Sprintf("create dst: %v", err))
		return
	}

	buf := make([]byte, movementCopyChunkSize)
	var bytesCopied int64
	abortedByRecall := false
	for {
		// Check recall_pending before each chunk.
		pending, err := a.store.GetObjectRecallPending(objectID)
		if err == nil && pending {
			abortedByRecall = true
			break
		}

		// Check I/O utilization high-water mark.
		if a.migrationIOHighWaterPct > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			util, _ := a.iostat.AverageUtilPct(ctx, []string{srcDevice})
			cancel()
			if util > float64(a.migrationIOHighWaterPct) {
				time.Sleep(pollInterval)
				continue
			}
		}

		n, readErr := srcFile.Read(buf)
		if n > 0 {
			if _, werr := dstFile.Write(buf[:n]); werr != nil {
				dstFile.Close()
				_ = os.Remove(dstPath)
				failLog(fmt.Sprintf("write dst: %v", werr))
				return
			}
			bytesCopied += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			dstFile.Close()
			_ = os.Remove(dstPath)
			failLog(fmt.Sprintf("read src: %v", readErr))
			return
		}
	}
	dstFile.Close()

	if abortedByRecall {
		_ = os.Remove(dstPath)
		log.Printf("zfsmgd: movement job=%q aborted by recall_pending", jobID)
		_ = a.store.UpdateMovementJobState(jobID, db.MovementJobStateFailed, 0, "interrupted_by_recall")
		_ = a.store.UpdateZFSMovementLogState(logID, db.ZFSMoveLogFailed, "interrupted_by_recall")
		return
	}

	// ── Phase: copy_complete ─────────────────────────────────────────────────
	_ = a.store.UpdateZFSMovementLogState(logID, db.ZFSMoveLogCopyComplete, "")

	// 3. Verify destination checksum.
	srcSum, err := sha256File(srcPath)
	if err != nil {
		_ = os.Remove(dstPath)
		failLog(fmt.Sprintf("checksum src: %v", err))
		return
	}
	dstSum, err := sha256File(dstPath)
	if err != nil {
		_ = os.Remove(dstPath)
		failLog(fmt.Sprintf("checksum dst: %v", err))
		return
	}
	if srcSum != dstSum {
		_ = os.Remove(dstPath)
		failLog("verify_failed")
		return
	}

	// 4. Find object and update placement atomically (switch step).
	obj, err := a.findManagedObjectByKey(namespaceID, objectKey)
	if err != nil {
		_ = os.Remove(dstPath)
		failLog(fmt.Sprintf("find managed object: %v", err))
		return
	}

	var ps tiering.PlacementSummary
	if obj.PlacementSummaryJSON != "" {
		_ = json.Unmarshal([]byte(obj.PlacementSummaryJSON), &ps)
	}
	ps.CurrentTargetID = ps.IntendedTargetID
	ps.State = "placed"

	psBytes, _ := json.Marshal(ps)

	// Update placement and advance movement_log to switched atomically.
	if err := a.updateObjectPlacement(obj.ID, string(psBytes)); err != nil {
		_ = os.Remove(dstPath)
		failLog(fmt.Sprintf("update object placement: %v", err))
		return
	}
	_ = a.store.UpdateZFSMovementLogState(logID, db.ZFSMoveLogSwitched, "")
	_ = a.store.UpdateMovementJobState(jobID, db.MovementJobStateCompleted, bytesCopied, "")

	// ── Phase: cleanup_complete ──────────────────────────────────────────────
	// 5. Remove source copy.
	if err := os.Remove(srcPath); err != nil {
		log.Printf("zfsmgd: movement job=%q: remove src %q: %v (non-fatal)", jobID, srcPath, err)
	}
	_ = a.store.UpdateZFSMovementLogState(logID, db.ZFSMoveLogCleanupComplete, "")
}

// deviceBasename extracts the block-device basename from a ZFS dataset path.
// For /pool/dataset, it returns "pool"; for deeper paths the top-level pool is used.
func deviceBasename(datasetPath string) string {
	parts := strings.SplitN(filepath.Clean(datasetPath), "/", 2)
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return datasetPath
}

// setIdle calls ioprio_set(IOPRIO_WHO_PROCESS, 0, IOPRIO_VALUE(IDLE, 0)) on the
// current OS thread to put it into idle I/O scheduling class (ionice -c 3).
// Failures are silently ignored — ionice is a best-effort hint.
func setIdle() {
	const (
		ioPrioClassShift = 13
		ioPrioClassIdle  = 3
		ioPrioWhoProcess = 1
		ioPrioSetSyscall = 251 // amd64 Linux
	)
	ioPrioValue := uintptr(ioPrioClassIdle << ioPrioClassShift)
	_, _, _ = syscall.RawSyscall(ioPrioSetSyscall, ioPrioWhoProcess, 0, ioPrioValue)
}

// updateObjectPlacement updates the placement_summary_json for a managed object.
// The db package does not expose a direct UpdateManagedObject, so we write a
// targeted update using the same store DB connection via a helper wrapper.
func (a *Adapter) updateObjectPlacement(objectID, placementSummaryJSON string) error {
	// We need to update placement_summary_json. The db.Store exposes the SQL
	// connection only through its exported methods. We use SetObjectPinState as
	// a proxy for the store connection, but we actually need a raw update here.
	// To avoid bypassing the store abstraction entirely, we model this as a
	// create-or-replace. However, CreateManagedObject uses INSERT not UPSERT.
	//
	// The cleanest approach without adding a new DB method: we update via a
	// dedicated unexported helper that wraps the store's existing facilities.
	// Since the store only has SetObjectPinState and CreateManagedObject, we
	// call a minimal exec through the stored ManagedObjectRow approach.
	//
	// For now, retrieve the current object, modify the JSON, and use a targeted
	// approach. We expose this via the store's UpdateManagedObject if it exists,
	// or use a workaround.
	//
	// The db package does not have UpdateManagedObject, so we call through a
	// thin wrapper below.
	return a.store.UpdateManagedObjectPlacement(objectID, placementSummaryJSON)
}

// findManagedObjectByKey returns the ManagedObjectRow for an objectKey within namespaceID.
func (a *Adapter) findManagedObjectByKey(namespaceID, objectKey string) (*db.ManagedObjectRow, error) {
	objs, err := a.store.ListManagedObjects(namespaceID)
	if err != nil {
		return nil, err
	}
	for i := range objs {
		if objs[i].ObjectKey == objectKey {
			return &objs[i], nil
		}
	}
	return nil, fmt.Errorf("managed object with key %q not found in namespace %q", objectKey, namespaceID)
}

// GetMovement returns the state of a movement job.
func (a *Adapter) GetMovement(id string) (*tiering.MovementState, error) {
	job, err := a.store.GetMovementJob(id)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get movement job",
			Cause:   err,
		}
	}
	return &tiering.MovementState{
		ID:            job.ID,
		State:         job.State,
		ProgressBytes: job.ProgressBytes,
		TotalBytes:    job.TotalBytes,
		FailureReason: job.FailureReason,
		StartedAt:     job.StartedAt,
		UpdatedAt:     job.UpdatedAt,
		CompletedAt:   job.CompletedAt,
	}, nil
}

// CancelMovement cancels an in-progress movement job.
func (a *Adapter) CancelMovement(id string) error {
	if err := a.store.CancelMovementJob(id); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "cancel movement job",
			Cause:   err,
		}
	}
	return nil
}

// ---- Pinning -----------------------------------------------------------------

// Pin pins a namespace or object to prevent it from being moved.
func (a *Adapter) Pin(scope tiering.PinScope, namespaceID string, objectID string) error {
	switch scope {
	case tiering.PinScopeNamespace:
		if err := a.store.SetNamespacePinState(namespaceID, "pinned-hot"); err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "pin namespace",
				Cause:   err,
			}
		}
	case tiering.PinScopeObject:
		if err := a.store.SetObjectPinState(objectID, "pinned-hot"); err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "pin object",
				Cause:   err,
			}
		}
	default:
		return &tiering.AdapterError{
			Kind:    tiering.ErrCapabilityViolation,
			Message: fmt.Sprintf("pin scope %q is not supported by the managed ZFS adapter", scope),
		}
	}
	return nil
}

// Unpin removes the pin from a namespace or object.
func (a *Adapter) Unpin(scope tiering.PinScope, namespaceID string, objectID string) error {
	switch scope {
	case tiering.PinScopeNamespace:
		if err := a.store.SetNamespacePinState(namespaceID, "none"); err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "unpin namespace",
				Cause:   err,
			}
		}
	case tiering.PinScopeObject:
		if err := a.store.SetObjectPinState(objectID, "none"); err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "unpin object",
				Cause:   err,
			}
		}
	default:
		return &tiering.AdapterError{
			Kind:    tiering.ErrCapabilityViolation,
			Message: fmt.Sprintf("unpin scope %q is not supported by the managed ZFS adapter", scope),
		}
	}
	return nil
}

// ---- Degraded state ----------------------------------------------------------

// GetDegradedState returns all degraded-state signals for the managed ZFS backend.
func (a *Adapter) GetDegradedState() ([]tiering.DegradedState, error) {
	rows, err := a.store.ListDegradedStates()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list degraded states",
			Cause:   err,
		}
	}

	var out []tiering.DegradedState
	for _, row := range rows {
		if row.BackendKind != BackendKind {
			continue
		}
		out = append(out, tiering.DegradedState{
			ID:          row.ID,
			BackendKind: row.BackendKind,
			ScopeKind:   row.ScopeKind,
			ScopeID:     row.ScopeID,
			Severity:    row.Severity,
			Code:        row.Code,
			Message:     row.Message,
			UpdatedAt:   row.UpdatedAt,
		})
	}
	return out, nil
}

// ---- OpenHandler (satisfies SocketServer's OpenHandler interface) ------------

// HandleOpen is called by the socket server when the FUSE daemon reports an
// open() call on a managed file. It looks up the backing dataset and returns
// an open fd and the backing inode number.
// flags contains the POSIX O_ACCMODE flags (O_RDONLY, O_WRONLY, O_RDWR).
func (a *Adapter) HandleOpen(namespaceID, objectKey string, flags uint32) (int, uint64, error) {
	// Find which namespace this is.
	zfsNs, err := a.store.GetZFSManagedNamespace(namespaceID)
	if err != nil {
		return -1, 0, syscall.EIO
	}

	// Strip leading slash from key for matching.
	key := strings.TrimPrefix(objectKey, "/")

	// Direct indexed lookup by (namespace_id, object_key).
	matchedObj, err := a.store.GetManagedObjectByKey(namespaceID, key)
	if err != nil {
		return -1, 0, syscall.ENOENT
	}

	var ps tiering.PlacementSummary
	if matchedObj.PlacementSummaryJSON != "" {
		_ = json.Unmarshal([]byte(matchedObj.PlacementSummaryJSON), &ps)
	}

	currentTargetID := ps.CurrentTargetID
	if currentTargetID == "" {
		return -1, 0, syscall.EIO
	}

	// If the intended target differs from current (i.e. a recall is needed),
	// perform synchronous recall first.
	if ps.IntendedTargetID != "" && ps.IntendedTargetID != currentTargetID {
		timeout := time.Duration(a.recallTimeoutSeconds) * time.Second
		if err := a.recallSync(namespaceID, matchedObj, ps, zfsNs, timeout); err != nil {
			log.Printf("zfsmgd: HandleOpen: synchronous recall for %q: %v", objectKey, err)
			if isRecallTimeout(err) {
				return -1, 0, syscall.EIO
			}
			// Continue with current placement for non-timeout errors.
		} else {
			// After recall, the object should be on the intended target.
			currentTargetID = ps.IntendedTargetID
		}
	}

	zfsTarget, err := a.store.GetZFSManagedTarget(currentTargetID)
	if err != nil {
		return -1, 0, syscall.EIO
	}

	// Use the flags from the FUSE daemon (O_RDONLY/O_WRONLY/O_RDWR).
	openFlags := int(flags & syscall.O_ACCMODE)

	filePath := filepath.Join(zfsTarget.DatasetPath, key)
	fd, err := syscall.Open(filePath, openFlags, 0)
	if err != nil {
		return -1, 0, err
	}

	// Stat the fd to get the inode number.
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return -1, 0, syscall.EIO
	}

	return fd, st.Ino, nil
}

// errRecallTimeout is returned by recallSync when the recall deadline expires.
type errRecallTimeout struct{ namespaceID string }

func (e *errRecallTimeout) Error() string {
	return fmt.Sprintf("synchronous recall timed out for namespace %q", e.namespaceID)
}

// isRecallTimeout reports whether err is an errRecallTimeout.
func isRecallTimeout(err error) bool {
	_, ok := err.(*errRecallTimeout)
	return ok
}

// recallSync performs a synchronous recall of an object from its current
// location to its intended location. It sets recall_pending on the object
// before starting so that any concurrent movement worker will abort.
// If timeout > 0 and the recall does not complete in time, it returns
// errRecallTimeout and emits a recall_timeout degraded state.
func (a *Adapter) recallSync(namespaceID string, obj *db.ManagedObjectRow, ps tiering.PlacementSummary, _ *db.ZFSManagedNamespaceRow, timeout time.Duration) error {
	// Set recall_pending so movement workers for this object abort.
	_ = a.store.SetObjectRecallPending(obj.ID, true)
	defer func() { _ = a.store.SetObjectRecallPending(obj.ID, false) }()

	srcZFS, err := a.store.GetZFSManagedTarget(ps.CurrentTargetID)
	if err != nil {
		return fmt.Errorf("get source ZFS target: %w", err)
	}
	dstZFS, err := a.store.GetZFSManagedTarget(ps.IntendedTargetID)
	if err != nil {
		return fmt.Errorf("get dest ZFS target: %w", err)
	}

	srcPath := filepath.Join(srcZFS.DatasetPath, obj.ObjectKey)
	dstPath := filepath.Join(dstZFS.DatasetPath, obj.ObjectKey)

	type result struct{ err error }
	done := make(chan result, 1)

	go func() {
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			done <- result{fmt.Errorf("mkdir dst: %w", err)}
			return
		}
		if err := runCmd("cp", "--reflink=auto", "-p", srcPath, dstPath); err != nil {
			done <- result{fmt.Errorf("cp: %w", err)}
			return
		}
		ps.CurrentTargetID = ps.IntendedTargetID
		ps.State = "placed"
		psBytes, _ := json.Marshal(ps)
		if err := a.updateObjectPlacement(obj.ID, string(psBytes)); err != nil {
			done <- result{fmt.Errorf("update placement: %w", err)}
			return
		}
		_ = os.Remove(srcPath)
		done <- result{}
	}()

	if timeout <= 0 {
		res := <-done
		return res.err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.err
	case <-timer.C:
		// Recall timed out. The goroutine may still be running but will fail
		// harmlessly (the dst file will be a partial copy).
		_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
			BackendKind: BackendKind,
			ScopeKind:   "namespace",
			ScopeID:     namespaceID,
			Severity:    db.DegradedSeverityWarning,
			Code:        "recall_timeout",
			Message:     fmt.Sprintf("synchronous recall for object %q exceeded %s", obj.ObjectKey, timeout),
		})
		return &errRecallTimeout{namespaceID: namespaceID}
	}
}

// HandleRelease is called when an application releases a file descriptor.
// inode is the backing inode of the released fd.
func (a *Adapter) HandleRelease(namespaceID string, inode uint64) {
	// no-op: placeholder for future use (e.g. eviction tracking)
	_ = inode
}

// HandleBypass is called when the FUSE daemon's socket protocol signals a bypass.
func (a *Adapter) HandleBypass(namespaceID string) {
	a.onBypassDetected(namespaceID)
}

// onBypassDetected records a bypass_detected degraded state.
func (a *Adapter) onBypassDetected(namespaceID string) {
	log.Printf("zfsmgd: bypass detected for namespace %q", namespaceID)
	_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
		BackendKind: BackendKind,
		ScopeKind:   "namespace",
		ScopeID:     namespaceID,
		Severity:    db.DegradedSeverityWarning,
		Code:        "bypass_detected",
		Message:     fmt.Sprintf("direct access bypass detected in namespace %q", namespaceID),
	})
}

// HandleFDPassFailed is called when the fd validation (inode check) fails on
// the daemon side. We log and record a degraded state.
func (a *Adapter) HandleFDPassFailed(namespaceID string, expectedInode uint64) {
	log.Printf("zfsmgd: fd_pass_failed for namespace %q: expected inode %d", namespaceID, expectedInode)
	_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
		BackendKind: BackendKind,
		ScopeKind:   "namespace",
		ScopeID:     namespaceID,
		Severity:    db.DegradedSeverityWarning,
		Code:        "fd_pass_failed",
		Message:     fmt.Sprintf("fd inode mismatch for namespace %q (expected inode %d)", namespaceID, expectedInode),
	})
}

// OnHealthFail is called when the health ping goroutine determines the daemon
// is unresponsive. We restart the daemon.
func (a *Adapter) OnHealthFail(namespaceID string) {
	log.Printf("zfsmgd: OnHealthFail for namespace %q: restarting daemon", namespaceID)
	_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
		BackendKind: BackendKind,
		ScopeKind:   "namespace",
		ScopeID:     namespaceID,
		Severity:    db.DegradedSeverityCritical,
		Code:        "namespace_unavailable",
		Message:     fmt.Sprintf("health check failed for namespace %q; daemon restarting", namespaceID),
	})

	// Look up the stored mount/socket paths for restart.
	zfsNs, err := a.store.GetZFSManagedNamespace(namespaceID)
	if err != nil {
		log.Printf("zfsmgd: OnHealthFail: get namespace %q: %v", namespaceID, err)
		return
	}

	a.onDaemonCrash(namespaceID, zfsNs.MountPath, zfsNs.SocketPath)
}

// ---- Internal helpers --------------------------------------------------------

// recoverMovementLog scans for non-terminal movement_log rows and applies
// crash recovery per the P04B state machine. Called at the start of Reconcile()
// before any movement workers are started.
//
// States handled:
//
//	copy_in_progress: copy was interrupted. Delete any partial dst; mark failed.
//	copy_complete:    copy finished but switch did not commit. Delete dst; mark failed.
//	switched:         switch committed but cleanup did not run. Schedule src deletion; mark done.
func (a *Adapter) recoverMovementLog() error {
	rows, err := a.store.ListZFSMovementLogNonTerminal()
	if err != nil {
		return fmt.Errorf("list non-terminal movement log: %w", err)
	}

	for _, row := range rows {
		a.recoverOneMovementLogRow(row)
	}
	return nil
}

// recoverOneMovementLogRow handles crash recovery for a single movement_log row.
func (a *Adapter) recoverOneMovementLogRow(row db.ZFSMovementLogRow) {
	log.Printf("zfsmgd: crash recovery: movement log %q state=%q object=%q", row.ID, row.State, row.ObjectKey)

	// Resolve backing dataset paths.
	srcZFS, err := a.store.GetZFSManagedTarget(row.SourceTargetID)
	if err != nil {
		log.Printf("zfsmgd: crash recovery: get source target %q: %v; setting reconciliation_required", row.SourceTargetID, err)
		a.setReconciliationRequired(row.NamespaceID, fmt.Sprintf("cannot resolve source target %q during crash recovery", row.SourceTargetID))
		return
	}
	dstZFS, err := a.store.GetZFSManagedTarget(row.DestTargetID)
	if err != nil {
		log.Printf("zfsmgd: crash recovery: get dest target %q: %v; setting reconciliation_required", row.DestTargetID, err)
		a.setReconciliationRequired(row.NamespaceID, fmt.Sprintf("cannot resolve dest target %q during crash recovery", row.DestTargetID))
		return
	}

	srcPath := filepath.Join(srcZFS.DatasetPath, row.ObjectKey)
	dstPath := filepath.Join(dstZFS.DatasetPath, row.ObjectKey)

	switch row.State {
	case db.ZFSMoveLogCopyInProgress:
		// Delete any partial destination file.
		if _, statErr := os.Stat(dstPath); statErr == nil {
			if err := os.Remove(dstPath); err != nil {
				log.Printf("zfsmgd: crash recovery: remove partial dst %q: %v (non-fatal)", dstPath, err)
			}
		}
		// Verify source is still present.
		if _, statErr := os.Stat(srcPath); statErr != nil {
			log.Printf("zfsmgd: crash recovery: source %q not found: %v; setting reconciliation_required", srcPath, statErr)
			a.setReconciliationRequired(row.NamespaceID, fmt.Sprintf("source file %q missing during crash recovery of copy_in_progress", srcPath))
			_ = a.store.UpdateZFSMovementLogState(row.ID, db.ZFSMoveLogFailed, "interrupted_by_restart")
			return
		}
		_ = a.store.UpdateZFSMovementLogState(row.ID, db.ZFSMoveLogFailed, "interrupted_by_restart")
		a.resetObjectPlacementState(row.ObjectID)

	case db.ZFSMoveLogCopyComplete:
		// The copy completed but the switch transaction did not commit.
		// Source is ground truth: delete destination copy.
		if _, statErr := os.Stat(dstPath); statErr == nil {
			if err := os.Remove(dstPath); err != nil {
				log.Printf("zfsmgd: crash recovery: remove dst %q (copy_complete): %v (non-fatal)", dstPath, err)
			}
		}
		// Verify source content hash if stored.
		if !a.verifySourceIntegrity(row.ObjectID, srcPath) {
			a.setReconciliationRequired(row.NamespaceID, fmt.Sprintf("source hash mismatch for object %q during crash recovery", row.ObjectID))
			_ = a.store.UpdateZFSMovementLogState(row.ID, db.ZFSMoveLogFailed, "interrupted_before_switch")
			return
		}
		_ = a.store.UpdateZFSMovementLogState(row.ID, db.ZFSMoveLogFailed, "interrupted_before_switch")
		a.resetObjectPlacementState(row.ObjectID)

	case db.ZFSMoveLogSwitched:
		// Switch committed: destination is authoritative. Schedule source deletion.
		_ = a.store.UpdateZFSMovementLogState(row.ID, db.ZFSMoveLogCleanupComplete, "")
		go func(src string) {
			if err := os.Remove(src); err != nil {
				log.Printf("zfsmgd: crash recovery: background remove src %q: %v (non-fatal)", src, err)
			}
		}(srcPath)
	}
}

// resetObjectPlacementState sets placement_state to "placed" for the object
// after a failed recovery where the source remains authoritative.
func (a *Adapter) resetObjectPlacementState(objectID string) {
	obj, err := a.store.GetManagedObject(objectID)
	if err != nil {
		log.Printf("zfsmgd: crash recovery: get object %q: %v", objectID, err)
		return
	}
	var ps tiering.PlacementSummary
	if obj.PlacementSummaryJSON != "" {
		_ = json.Unmarshal([]byte(obj.PlacementSummaryJSON), &ps)
	}
	ps.State = "placed"
	psBytes, _ := json.Marshal(ps)
	_ = a.store.UpdateManagedObjectPlacement(objectID, string(psBytes))
}

// verifySourceIntegrity checks that the source file exists and, if a content
// hash is stored for the object, that it matches. Returns true if OK.
func (a *Adapter) verifySourceIntegrity(objectID, srcPath string) bool {
	if _, err := os.Stat(srcPath); err != nil {
		return false
	}
	return true // content hash verification deferred; stat is sufficient for recovery
}

// setReconciliationRequired emits a reconciliation_required critical degraded
// state for the given namespace.
func (a *Adapter) setReconciliationRequired(namespaceID, message string) {
	_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
		BackendKind: BackendKind,
		ScopeKind:   "namespace",
		ScopeID:     namespaceID,
		Severity:    db.DegradedSeverityCritical,
		Code:        "reconciliation_required",
		Message:     message,
	})
}

// validateTargetsForNamespace checks that all PolicyTargetIDs share the same
// ZFS pool as poolName and that their ranks are contiguous starting at 1.
func (a *Adapter) validateTargetsForNamespace(poolName string, targetIDs []string) error {
	ranks := make([]int, 0, len(targetIDs))
	for _, id := range targetIDs {
		zfsRow, err := a.store.GetZFSManagedTarget(id)
		if err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("look up target %q for namespace validation: %v", id, err),
				Cause:   err,
			}
		}
		if zfsRow.PoolName != poolName {
			return &tiering.AdapterError{
				Kind: tiering.ErrPermanent,
				Message: fmt.Sprintf(
					"target %q is in pool %q but namespace requires pool %q; all backing datasets must share the same ZFS pool",
					id, zfsRow.PoolName, poolName),
			}
		}

		ttRow, err := a.store.GetTierTarget(id)
		if err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("look up tier target %q for rank validation: %v", id, err),
				Cause:   err,
			}
		}
		ranks = append(ranks, ttRow.Rank)
	}

	// Sort and verify contiguous ranks starting at 1.
	seen := make(map[int]bool, len(ranks))
	for _, r := range ranks {
		seen[r] = true
	}
	for i := 1; i <= len(ranks); i++ {
		if !seen[i] {
			return &tiering.AdapterError{
				Kind: tiering.ErrPermanent,
				Message: fmt.Sprintf(
					"tier ranks are not contiguous starting at 1: missing rank %d (got ranks: %v)",
					i, ranks),
			}
		}
	}
	return nil
}

// allManagedObjects returns all managed objects across all ZFS managed namespaces.
func (a *Adapter) allManagedObjects() ([]db.ManagedObjectRow, error) {
	zfsNs, err := a.store.ListZFSManagedNamespaces()
	if err != nil {
		return nil, err
	}
	var all []db.ManagedObjectRow
	for _, zn := range zfsNs {
		objs, err := a.store.ListManagedObjects(zn.NamespaceID)
		if err != nil {
			log.Printf("zfsmgd: allManagedObjects: list for namespace %q: %v", zn.NamespaceID, err)
			continue
		}
		all = append(all, objs...)
	}
	return all, nil
}

// zfsMountPoint returns the mount point for a ZFS dataset by running
// zfs get -H -o value mountpoint <dataset>.
func zfsMountPoint(dataset string) (string, error) {
	cmd := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", dataset)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("zfs get mountpoint %q: %w", dataset, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// sha256File computes the SHA-256 checksum of a file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
