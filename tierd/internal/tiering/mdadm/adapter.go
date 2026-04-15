// Package mdadm implements the unified TieringAdapter for the mdadm/LVM
// backend. It maps existing tier_pools, tiers, and managed_volumes into the
// unified control-plane schema defined in proposal
// unified-tiering-01-common-model.
//
// Legacy pools (monolithic LV) continue to work via reconciliation.
// New pools use per-tier LVs with FUSE routing: each tier gets its own
// VG/LV/mount, and a FUSE daemon routes file opens to the correct backing
// tier via HandleOpen.
package mdadm

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	fusepkg "github.com/JBailes/SmoothNAS/tierd/internal/tiering/fuse"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

// statfsUsedBytes returns (totalBytes - availBytes) for a mount point.
// Used to capture an empty-FS baseline at CreateTarget time.
func statfsUsedBytes(mountPath string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountPath, &st); err != nil {
		return 0, false
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, true
}

// pathVIno derives a stable virtual inode number from a path. Using path-based
// virtual inodes avoids collisions between real filesystem inodes from different
// backing tiers (which have independent inode namespaces).
func pathVIno(path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	v := h.Sum64()
	if v < 2 {
		v += 2 // inode 0 and 1 are special in FUSE
	}
	return v
}

// BackendKind is the canonical identifier for the mdadm/LVM backend.
const BackendKind = "mdadm"

// legacyCapabilitiesJSON is the JSON-encoded TargetCapabilities for legacy
// mdadm tier targets (monolithic LV, no FUSE).
const legacyCapabilitiesJSON = `{"movement_granularity":"region","pin_scope":"volume","supports_online_move":true,"supports_recall":false,"recall_mode":"none","snapshot_mode":"none","fuse_mode":"n/a","supports_checksums":false,"supports_compression":false,"supports_write_bias":false}`

// capabilitiesJSON is the JSON-encoded TargetCapabilities for FUSE-capable
// mdadm tier targets (per-tier LV with passthrough FUSE routing).
const capabilitiesJSON = `{"movement_granularity":"object","pin_scope":"volume","supports_online_move":true,"supports_recall":false,"recall_mode":"none","snapshot_mode":"none","fuse_mode":"passthrough","supports_checksums":false,"supports_compression":false,"supports_write_bias":true}`

// legacyCapabilities returns the static TargetCapabilities for legacy mdadm pools.
func legacyCapabilities() tiering.TargetCapabilities {
	return tiering.TargetCapabilities{
		MovementGranularity: "region",
		PinScope:            "volume",
		SupportsOnlineMove:  true,
		SupportsRecall:      false,
		RecallMode:          "none",
		SnapshotMode:        "none",
		FUSEMode:            "n/a",
	}
}

// mdadmCapabilities returns the TargetCapabilities for FUSE-capable mdadm targets.
func mdadmCapabilities() tiering.TargetCapabilities {
	return tiering.TargetCapabilities{
		MovementGranularity: "object",
		PinScope:            "volume",
		SupportsOnlineMove:  true,
		SupportsRecall:      false,
		RecallMode:          "none",
		SnapshotMode:        "none",
		FUSEMode:            "passthrough",
		SupportsWriteBias:   true,
	}
}

// backingRefTarget returns the stable backing_ref for a tier target.
// Format: "mdadm:{poolName}:{slotName}"
func backingRefTarget(poolName, slotName string) string {
	return fmt.Sprintf("mdadm:%s:%s", poolName, slotName)
}

// backingRefNamespace returns the stable backing_ref for a managed namespace.
// Format: "mdadm:{vgName}/{lvName}"
func backingRefNamespace(vgName, lvName string) string {
	return fmt.Sprintf("mdadm:%s/%s", vgName, lvName)
}

// backingRefManagedNamespace returns the stable backing_ref for a FUSE-managed
// namespace. Format: "mdadm-fuse:{poolName}"
func backingRefManagedNamespace(poolName string) string {
	return fmt.Sprintf("mdadm-fuse:%s", poolName)
}

// parseBackingRefTarget parses "mdadm:pool:slot" into poolName and slotName.
func parseBackingRefTarget(ref string) (poolName, slotName string, err error) {
	inner := strings.TrimPrefix(ref, "mdadm:")
	if inner == ref {
		return "", "", fmt.Errorf("invalid mdadm target backing ref (missing prefix): %q", ref)
	}
	parts := strings.SplitN(inner, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid mdadm target backing ref: %q", ref)
	}
	return parts[0], parts[1], nil
}

// slotHealthToUnified maps a tier slot state to the control-plane health string.
func slotHealthToUnified(slotState string) string {
	switch slotState {
	case db.TierSlotStateAssigned:
		return "healthy"
	case db.TierSlotStateDegraded:
		return "degraded"
	case db.TierSlotStateMissing:
		return "degraded"
	default:
		return "unknown"
	}
}

// Adapter implements tiering.TieringAdapter for the mdadm/LVM backend.
// It supports both legacy pools (monolithic LV, no FUSE) and new pools
// (per-tier LV with FUSE passthrough routing).
type Adapter struct {
	store      *db.Store
	supervisor *fusepkg.DaemonSupervisor
	server     *fusepkg.SocketServer
	runDir     string

	// activeOpens tracks per-namespace open file handle counts. Movement
	// workers check this before starting: when users have active file handles,
	// migrations yield completely so user I/O gets full device bandwidth.
	activeOpensMu sync.Mutex
	activeOpens   map[string]*int64 // namespaceID → atomic counter

	// refreshMu serialises dir-cache refresh scans per namespace so that
	// concurrent refresh goroutines don't overwrite each other with stale
	// partial results. The "pending" flag requests one more scan after the
	// in-progress one finishes, ensuring callers never drop a wakeup.
	refreshMu      sync.Mutex
	refreshRunning map[string]bool
	refreshPending map[string]bool

	// metaMu guards metaStores. The map is populated at startup by the
	// orchestration in main.go once each pool's fastest tier is mounted.
	// Missing entries mean "no meta store for this pool yet" — the hot
	// path treats that as best-effort: writes are dropped, reads miss.
	metaMu     sync.RWMutex
	metaStores map[string]*meta.PoolMetaStore // poolName → store

	// activityMu guards lastActivity. HandleOpen stamps the namespace's
	// entry with time.Now so the placement planner can avoid interfering
	// with active write traffic (rsync, user opens).
	activityMu   sync.Mutex
	lastActivity map[string]time.Time // namespaceID → last OPEN timestamp
}

// NewAdapter returns a new mdadm tiering adapter bound to the given store.
// runDir is the directory for FUSE sockets (e.g. /run/tierd/mdadm).
func NewAdapter(store *db.Store, runDir string) *Adapter {
	a := &Adapter{
		store:          store,
		supervisor:     fusepkg.NewDaemonSupervisor(),
		runDir:         runDir,
		activeOpens:    make(map[string]*int64),
		refreshRunning: make(map[string]bool),
		refreshPending: make(map[string]bool),
		metaStores:     make(map[string]*meta.PoolMetaStore),
		lastActivity:   make(map[string]time.Time),
	}
	// The adapter itself implements OpenHandler for FUSE dispatching.
	a.server = fusepkg.NewSocketServer(runDir, a)
	a.server.SetLogPrefix("mdadm-fuse")
	return a
}

// Kind returns the backend identifier.
func (a *Adapter) Kind() string { return BackendKind }

// SetMetaStore registers the per-pool metadata store opened at startup.
// The store lives on the pool's fastest tier backing and replaces the
// synchronous managed_objects SQLite write that used to sit on the FUSE
// CREATE path. Safe to call multiple times (e.g., on reconcile) — later
// calls replace the existing entry; the old store is closed asynchronously
// to avoid blocking the caller.
func (a *Adapter) SetMetaStore(poolName string, store *meta.PoolMetaStore) {
	a.metaMu.Lock()
	old := a.metaStores[poolName]
	a.metaStores[poolName] = store
	a.metaMu.Unlock()
	if old != nil && old != store {
		go func() { _ = old.Close() }()
	}
}

// metaStoreFor returns the meta store for a pool, or nil if none is open.
func (a *Adapter) metaStoreFor(poolName string) *meta.PoolMetaStore {
	a.metaMu.RLock()
	s := a.metaStores[poolName]
	a.metaMu.RUnlock()
	return s
}

// MetaStats returns a map of poolName → per-shard stats for every open
// meta store. Exposed as /api/tiering/meta/stats for diagnostics.
func (a *Adapter) MetaStats() map[string][]meta.ShardStats {
	a.metaMu.RLock()
	defer a.metaMu.RUnlock()
	out := make(map[string][]meta.ShardStats, len(a.metaStores))
	for pool, s := range a.metaStores {
		out[pool] = s.Stats()
	}
	return out
}

// ClosePoolMetaStore drains and closes the meta store for a single pool.
// Called from destroyTierPool before unmount because the meta store's
// bbolt files live on the pool's fastest tier — its mmap keeps the
// backing mount busy and blocks lvremove if not closed first.
func (a *Adapter) ClosePoolMetaStore(poolName string) {
	a.metaMu.Lock()
	store, ok := a.metaStores[poolName]
	if !ok {
		a.metaMu.Unlock()
		return
	}
	delete(a.metaStores, poolName)
	a.metaMu.Unlock()
	if err := store.Close(); err != nil {
		log.Printf("mdadm: close meta store for pool %s: %v", poolName, err)
	}
}

// CloseMetaStores drains every registered meta store. Called from shutdown.
func (a *Adapter) CloseMetaStores() {
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	for pool, s := range a.metaStores {
		if err := s.Close(); err != nil {
			log.Printf("mdadm: close meta store for pool %s: %v", pool, err)
		}
	}
	a.metaStores = map[string]*meta.PoolMetaStore{}
}

// recordObjectAccess enqueues an updated record describing a file that was
// just opened (or just created). It increments HeatCounter and refreshes
// LastAccessNS, preserving existing PinState. Sets TierIdx to the tier
// the file was found on, so migrations that land new data elsewhere get
// reflected on next access.
//
// Never blocks and never errors visibly: the meta store is a cache that
// can be rebuilt from the backing FS, so a dropped enqueue just means the
// next lookup pays a scan instead of a mmap read and one heat tick is
// missed.
func (a *Adapter) recordObjectAccess(poolName, namespaceID string, inode uint64, tierRank int) {
	store := a.metaStoreFor(poolName)
	if store == nil {
		return
	}
	rec, _, _ := store.Get(inode)
	if rec.Version == 0 {
		rec = meta.Record{
			Version:     meta.RecordVersion,
			PinState:    meta.PinNone,
			NamespaceID: meta.NamespaceID(namespaceID),
		}
	}
	rec.TierIdx = uint8(tierRank)
	// Saturating increment keeps uint32 from wrapping to zero on hot files.
	if rec.HeatCounter < ^uint32(0) {
		rec.HeatCounter++
	}
	rec.LastAccessNS = uint64(time.Now().UnixNano())
	_ = store.Put(inode, rec)
}

// resolveObjectByPath locates a file in a namespace's backing tiers by
// probing each tier (fastest first) until one contains it. Returns the
// pool name, tier rank, and backing inode. Used by the pin API and other
// path-oriented lookups that need to reach the meta store.
func (a *Adapter) resolveObjectByPath(namespaceID, key string) (poolName string, tierRank int, inode uint64, err error) {
	mdNs, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil || mdNs == nil {
		return "", 0, 0, fmt.Errorf("namespace %q not found", namespaceID)
	}
	key = strings.TrimPrefix(key, "/")

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return "", 0, 0, err
	}
	type rankedTarget struct {
		rank   int
		target db.MdadmManagedTargetRow
	}
	var ranked []rankedTarget
	for i := range targets {
		if targets[i].PoolName != mdNs.PoolName {
			continue
		}
		tt, err := a.store.GetTierTargetByBackingRef(
			backingRefTarget(targets[i].PoolName, targets[i].TierName), BackendKind)
		if err != nil {
			continue
		}
		ranked = append(ranked, rankedTarget{rank: tt.Rank, target: targets[i]})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })

	for _, rt := range ranked {
		filePath := filepath.Join(rt.target.MountPath, key)
		var st syscall.Stat_t
		if err := syscall.Stat(filePath, &st); err != nil {
			continue
		}
		return rt.target.PoolName, rt.rank, st.Ino, nil
	}
	return "", 0, 0, fmt.Errorf("object %q not found in any tier of namespace %q", key, namespaceID)
}

// pinStateToMeta maps the external string pin state to the meta store enum.
func pinStateToMeta(s string) meta.PinState {
	switch s {
	case "pinned-hot":
		return meta.PinHot
	case "pinned-cold":
		return meta.PinCold
	default:
		return meta.PinNone
	}
}

// pinStateFromMeta maps the meta store enum back to the external string.
func pinStateFromMeta(p meta.PinState) string {
	switch p {
	case meta.PinHot:
		return "pinned-hot"
	case meta.PinCold:
		return "pinned-cold"
	default:
		return "none"
	}
}

// SetObjectPinStateByPath updates the pin state for an object addressed by
// (namespace, relative path). Writes through the meta store on the pool's
// fastest tier — no SQLite commit on the pin path.
func (a *Adapter) SetObjectPinStateByPath(namespaceID, key, pinState string) error {
	poolName, tierRank, inode, err := a.resolveObjectByPath(namespaceID, key)
	if err != nil {
		return err
	}
	store := a.metaStoreFor(poolName)
	if store == nil {
		return fmt.Errorf("meta store for pool %q not open", poolName)
	}
	// Preserve any existing record fields; overwrite only PinState.
	rec, ok, err := store.Get(inode)
	if err != nil {
		return err
	}
	if !ok {
		rec = meta.Record{
			Version:     meta.RecordVersion,
			TierIdx:     uint8(tierRank),
			NamespaceID: meta.NamespaceID(namespaceID),
		}
	}
	rec.PinState = pinStateToMeta(pinState)
	store.PutBlocking(inode, rec)
	return nil
}

// FileEntry is a single entry returned by ListNamespaceFiles.
type FileEntry struct {
	Path     string `json:"path"`     // namespace-relative path
	Size     int64  `json:"size"`
	Inode    uint64 `json:"inode"`
	TierRank int    `json:"tier_rank"`
	PinState string `json:"pin_state"`
}

// ListNamespaceFiles walks the namespace's tier backings (fastest first)
// and returns up to limit file entries whose path starts with prefix.
// Each entry carries pin state read from the meta store.
//
// This is a scan — expensive for large trees. Callers should pass a
// specific prefix (e.g., a subdirectory) when possible and a reasonable
// limit (e.g., 500). There is no pagination cursor yet; a larger limit
// walks more of the tree.
func (a *Adapter) ListNamespaceFiles(namespaceID, prefix string, limit int) ([]FileEntry, error) {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil || mn == nil {
		return nil, fmt.Errorf("namespace %q not found", namespaceID)
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return nil, err
	}
	type rankedTarget struct {
		rank   int
		target db.MdadmManagedTargetRow
	}
	var ranked []rankedTarget
	for i := range targets {
		if targets[i].PoolName != mn.PoolName {
			continue
		}
		tt, err := a.store.GetTierTargetByBackingRef(
			backingRefTarget(targets[i].PoolName, targets[i].TierName), BackendKind)
		if err != nil {
			continue
		}
		ranked = append(ranked, rankedTarget{rank: tt.Rank, target: targets[i]})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })

	store := a.metaStoreFor(mn.PoolName)
	cleanPrefix := strings.TrimPrefix(strings.Trim(prefix, "/"), "/")

	seenInodes := make(map[uint64]struct{}, limit)
	entries := make([]FileEntry, 0, limit)

	for _, rt := range ranked {
		if len(entries) >= limit {
			break
		}
		startDir := rt.target.MountPath
		if cleanPrefix != "" {
			startDir = filepath.Join(rt.target.MountPath, cleanPrefix)
		}
		err := filepath.WalkDir(startDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(entries) >= limit {
				return filepath.SkipAll
			}
			if d.Name() == ".tierd-meta" && d.IsDir() {
				return filepath.SkipDir
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			if _, dup := seenInodes[st.Ino]; dup {
				return nil
			}
			seenInodes[st.Ino] = struct{}{}

			rel, rerr := filepath.Rel(rt.target.MountPath, path)
			if rerr != nil {
				return nil
			}

			fe := FileEntry{
				Path:     rel,
				Size:     info.Size(),
				Inode:    st.Ino,
				TierRank: rt.rank,
				PinState: "none",
			}
			if store != nil {
				if rec, have, _ := store.Get(st.Ino); have {
					fe.PinState = pinStateFromMeta(rec.PinState)
				}
			}
			entries = append(entries, fe)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("mdadm: ListNamespaceFiles walk %s: %v", startDir, err)
		}
	}

	return entries, nil
}

// GetObjectPinStateByPath returns the pin state for an object.
func (a *Adapter) GetObjectPinStateByPath(namespaceID, key string) (string, error) {
	poolName, _, inode, err := a.resolveObjectByPath(namespaceID, key)
	if err != nil {
		return "", err
	}
	store := a.metaStoreFor(poolName)
	if store == nil {
		return "none", nil
	}
	rec, ok, err := store.Get(inode)
	if err != nil {
		return "", err
	}
	if !ok {
		return "none", nil
	}
	return pinStateFromMeta(rec.PinState), nil
}

// ---------------------------------------------------------------------------
// CreateTarget — per-tier LV provisioning for FUSE-capable pools
// ---------------------------------------------------------------------------

// CreateTarget provisions a per-tier LV for a FUSE-capable mdadm pool.
// It creates a VG named "tier-{poolName}-{tierName}", an LV "data" on that
// VG, formats it with XFS, mounts it, and records the target in both the
// control-plane and mdadm_managed_targets tables.
func (a *Adapter) CreateTarget(spec tiering.TargetSpec) (*tiering.TargetState, error) {
	poolName := spec.PlacementDomain
	tierName := spec.Name
	if poolName == "" || tierName == "" {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "CreateTarget requires non-empty PlacementDomain and Name",
		}
	}

	vgName := fmt.Sprintf("tier-%s-%s", poolName, tierName)
	lvName := "data"
	mountPath := filepath.Join("/mnt/.tierd-backing", poolName, tierName)

	// Ensure the VG exists. If not, create it empty so the LV creation
	// will pick up whatever PVs are added later.
	exists, err := lvm.VGExists(vgName)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: fmt.Sprintf("check VG %s existence", vgName),
			Cause:   err,
		}
	}
	if !exists {
		if err := lvm.VGCreateEmpty(vgName); err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: fmt.Sprintf("create VG %s", vgName),
				Cause:   err,
			}
		}
	}

	// Create the LV if it does not already exist.
	lvExists, err := lvm.LVExists(vgName, lvName)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: fmt.Sprintf("check LV %s/%s existence", vgName, lvName),
			Cause:   err,
		}
	}
	if !lvExists {
		if err := lvm.CreateLV(vgName, lvName, "100%FREE", ""); err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: fmt.Sprintf("create LV %s/%s", vgName, lvName),
				Cause:   err,
			}
		}
	}

	// Format with XFS. Two scenarios warrant a format:
	//  - Fresh create (no tier_targets row yet) — a "delete + create"
	//    sequence must always produce a clean filesystem even if a prior
	//    destroy couldn't remove the LV and the old FS signature survived.
	//    mkfs.xfs -f overwrites any stale signature.
	//  - LV simply has no filesystem yet (partial prior create).
	// If a tier_targets row already exists, this is an idempotent retry
	// of a successful create; leave the FS intact.
	bref := backingRefTarget(poolName, tierName)
	existingTT, _ := a.store.GetTierTargetByBackingRef(bref, BackendKind)
	needFormat := existingTT == nil || !lvm.LVHasFilesystem(vgName, lvName)
	if needFormat {
		if err := lvm.FormatLV(vgName, lvName, "xfs"); err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: fmt.Sprintf("format LV %s/%s", vgName, lvName),
				Cause:   err,
			}
		}
	}

	// Mount if not already mounted.
	if !lvm.IsMounted(mountPath) {
		if err := lvm.Mount(vgName, lvName, mountPath); err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: fmt.Sprintf("mount %s/%s at %s", vgName, lvName, mountPath),
				Cause:   err,
			}
		}
	}

	// Persist in fstab.
	if err := lvm.EnsureFSTabEntry(vgName, lvName, mountPath, "xfs"); err != nil {
		log.Printf("mdadm: warning: failed to persist fstab entry for %s/%s: %v", vgName, lvName, err)
	}

	// Record the empty-FS metadata baseline *now*, while the filesystem is
	// still pristine. Lazy baseline discovery on first UI read was racy:
	// if any data landed on the tier before the first backingFSUsage call
	// (e.g., rsync started immediately after create) that data got baked
	// into the baseline and permanently hidden from the capacity display.
	// mkfs + mount here guarantees the statvfs used-bytes we capture is
	// pure metadata reservation, nothing else.
	if used, ok := statfsUsedBytes(mountPath); ok {
		_ = a.store.SetControlPlaneConfig(
			"tier_baseline."+poolName+"."+tierName,
			strconv.FormatUint(used, 10),
		)
	}

	// Create the tier_targets control-plane row (bref was computed above).
	row := &db.TierTargetRow{
		Name:             tierName,
		PlacementDomain:  poolName,
		BackendKind:      BackendKind,
		Rank:             spec.Rank,
		TargetFillPct:    spec.TargetFillPct,
		FullThresholdPct: spec.FullThresholdPct,
		Health:           "healthy",
		BackingRef:       bref,
		CapabilitiesJSON: capabilitiesJSON,
	}
	if err := a.store.CreateTierTarget(row); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create tier_target row",
			Cause:   err,
		}
	}

	// Record in mdadm_managed_targets.
	if err := a.store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
		TierTargetID: row.ID,
		PoolName:     poolName,
		TierName:     tierName,
		VGName:       vgName,
		LVName:       lvName,
		MountPath:    mountPath,
	}); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "upsert mdadm_managed_target",
			Cause:   err,
		}
	}

	return &tiering.TargetState{
		ID:           row.ID,
		Name:         tierName,
		Health:       "healthy",
		Capabilities: mdadmCapabilities(),
	}, nil
}

// DestroyTarget destroys a per-tier LV target or returns an error for legacy
// targets that are managed via the tier management API.
func (a *Adapter) DestroyTarget(targetID string) error {
	// Check if this is a FUSE-managed target.
	mt, err := a.store.GetMdadmManagedTarget(targetID)
	if err == nil && mt != nil {
		if lvm.IsMounted(mt.MountPath) {
			if err := lvm.LazyUnmount(mt.MountPath); err != nil {
				log.Printf("mdadm: warning: lazy unmount %s: %v", mt.MountPath, err)
			}
		}
		_ = lvm.RemoveFSTabEntry(mt.VGName, mt.LVName, mt.MountPath)
		_ = lvm.RemoveLV(mt.VGName, mt.LVName)
		_ = lvm.VGRemoveIfEmpty(mt.VGName)
		_ = a.store.DeleteMdadmManagedTarget(targetID)
		return nil
	}

	return &tiering.AdapterError{
		Kind:    tiering.ErrCapabilityViolation,
		Message: "mdadm tier targets are destroyed via the tier management API",
	}
}

// ListTargets returns one TargetState per assigned tier slot across all pools.
// Empty slots are omitted. Also includes FUSE-managed targets.
func (a *Adapter) ListTargets() ([]tiering.TargetState, error) {
	pools, err := a.store.ListTierInstances()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier pools",
			Cause:   err,
		}
	}
	var out []tiering.TargetState
	for _, pool := range pools {
		slots, err := a.store.ListTierSlots(pool.Name)
		if err != nil {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "list tier slots for pool " + pool.Name,
				Cause:   err,
			}
		}
		for _, slot := range slots {
			if slot.State == db.TierSlotStateEmpty {
				continue
			}
			out = append(out, tiering.TargetState{
				ID:           backingRefTarget(pool.Name, slot.Name),
				Name:         slot.Name,
				Health:       slotHealthToUnified(slot.State),
				Capabilities: legacyCapabilities(),
			})
		}
	}

	// FUSE-managed targets from mdadm_managed_targets.
	managedTargets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list mdadm managed targets",
			Cause:   err,
		}
	}
	for _, mt := range managedTargets {
		health := "healthy"
		if !lvm.IsMounted(mt.MountPath) {
			health = "degraded"
		}
		out = append(out, tiering.TargetState{
			ID:           mt.TierTargetID,
			Name:         mt.TierName,
			Health:       health,
			Capabilities: mdadmCapabilities(),
		})
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// CreateNamespace — FUSE daemon provisioning for new pools
// ---------------------------------------------------------------------------

// CreateNamespace creates a FUSE-routed namespace for a new-style mdadm pool.
func (a *Adapter) CreateNamespace(spec tiering.NamespaceSpec) (*tiering.NamespaceState, error) {
	poolName := spec.PlacementDomain
	if poolName == "" {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "CreateNamespace requires non-empty PlacementDomain",
		}
	}

	mountPath := filepath.Join("/mnt", poolName)
	bref := backingRefManagedNamespace(poolName)

	ns := &db.ManagedNamespaceRow{
		Name:            spec.Name,
		PlacementDomain: poolName,
		BackendKind:     BackendKind,
		NamespaceKind:   spec.NamespaceKind,
		ExposedPath:     mountPath,
		PinState:        "none",
		Health:          "healthy",
		PlacementState:  "placed",
		BackendRef:      bref,
	}
	if ns.NamespaceKind == "" {
		ns.NamespaceKind = "filespace"
	}
	if err := a.store.CreateManagedNamespace(ns); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create managed_namespace row",
			Cause:   err,
		}
	}

	namespaceID := ns.ID

	if err := os.MkdirAll(a.runDir, 0755); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create socket directory",
			Cause:   err,
		}
	}

	socketPath, err := a.server.Start(namespaceID)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "start socket server",
			Cause:   err,
		}
	}

	if err := os.MkdirAll(mountPath, 0755); err != nil {
		a.server.Stop(namespaceID)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: fmt.Sprintf("create mount point %s", mountPath),
			Cause:   err,
		}
	}

	if err := a.supervisor.Start(namespaceID, mountPath, socketPath); err != nil {
		a.server.Stop(namespaceID)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "start FUSE daemon",
			Cause:   err,
		}
	}

	daemonPID := a.supervisor.ActivePID(namespaceID)

	if err := a.store.UpsertMdadmManagedNamespace(&db.MdadmManagedNamespaceRow{
		NamespaceID: namespaceID,
		PoolName:    poolName,
		SocketPath:  socketPath,
		MountPath:   mountPath,
		DaemonPID:   daemonPID,
		DaemonState: "running",
	}); err != nil {
		a.supervisor.Stop(namespaceID)
		a.server.Stop(namespaceID)
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "upsert mdadm_managed_namespace",
			Cause:   err,
		}
	}

	// Set up crash supervision.
	a.supervisor.Supervise(namespaceID, func() {
		log.Printf("mdadm-fuse: daemon for namespace %s crashed, restarting", namespaceID)
		_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "crashed", 0)
		if err := a.supervisor.Restart(namespaceID, mountPath, socketPath); err != nil {
			log.Printf("mdadm-fuse: failed to restart daemon for namespace %s: %v", namespaceID, err)
			_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "failed", 0)
			return
		}
		newPID := a.supervisor.ActivePID(namespaceID)
		_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "running", newPID)
		a.supervisor.Supervise(namespaceID, func() {
			log.Printf("mdadm-fuse: daemon for namespace %s crashed again", namespaceID)
			_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "failed", 0)
		})
	})

	return &tiering.NamespaceState{
		ID:             namespaceID,
		Health:         "healthy",
		PlacementState: "placed",
		BackendRef:     bref,
	}, nil
}

// DestroyNamespace destroys a FUSE-managed namespace and all of its backing
// tier targets. Specifically:
//  1. The FUSE daemon is stopped and the kernel mount entry cleared.
//  2. Every per-tier backing LVM mount is lazy-unmounted.
//  3. The backing LV, VG, and PV labels are removed for each target.
//  4. The namespace and target rows are deleted from the database.
func (a *Adapter) DestroyNamespace(namespaceID string) error {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil || mn == nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: fmt.Sprintf("namespace %q not found", namespaceID),
		}
	}

	// 1. Stop the FUSE daemon and clear the kernel mount entry.
	a.supervisor.Stop(namespaceID)
	a.server.Stop(namespaceID)
	fusepkg.ClearStaleFuseMount(mn.MountPath)

	// 2–3. Tear down every backing target for this pool.
	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		log.Printf("mdadm: DestroyNamespace %s: list targets: %v", namespaceID, err)
	}
	for i := range targets {
		if targets[i].PoolName != mn.PoolName {
			continue
		}
		mt := &targets[i]
		if lvm.IsMounted(mt.MountPath) {
			if err := lvm.LazyUnmount(mt.MountPath); err != nil {
				log.Printf("mdadm: DestroyNamespace %s: lazy unmount %s: %v",
					namespaceID, mt.MountPath, err)
			}
		}
		_ = lvm.RemoveFSTabEntry(mt.VGName, mt.LVName, mt.MountPath)
		_ = lvm.RemoveLV(mt.VGName, mt.LVName)
		_ = lvm.VGRemoveIfEmpty(mt.VGName)
		_ = a.store.DeleteMdadmManagedTarget(mt.TierTargetID)
	}

	// 4. Remove the namespace record.
	_ = a.store.DeleteMdadmManagedNamespace(namespaceID)
	return nil
}

// ListNamespaces returns one NamespaceState per FUSE-managed namespace.
func (a *Adapter) ListNamespaces() ([]tiering.NamespaceState, error) {
	var out []tiering.NamespaceState

	managedNS, err := a.store.ListMdadmManagedNamespaces()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list mdadm managed namespaces",
			Cause:   err,
		}
	}
	for _, mn := range managedNS {
		health := "healthy"
		if mn.DaemonState != "running" {
			health = "degraded"
		}
		out = append(out, tiering.NamespaceState{
			ID:             mn.NamespaceID,
			Health:         health,
			PlacementState: "placed",
			BackendRef:     backingRefManagedNamespace(mn.PoolName),
		})
	}

	return out, nil
}

// ListManagedObjects returns managed objects for a namespace. Legacy managed
// volumes have been removed; only FUSE-managed namespaces remain.
func (a *Adapter) ListManagedObjects(namespaceID string) ([]tiering.ManagedObjectState, error) {
	// FUSE-managed namespaces do not expose sub-objects.
	if _, err := a.store.GetMdadmManagedNamespace(namespaceID); err == nil {
		return nil, nil
	}
	return nil, &tiering.AdapterError{
		Kind:    tiering.ErrPermanent,
		Message: fmt.Sprintf("namespace %q not found", namespaceID),
	}
}

// GetCapabilities returns the static capabilities for the named mdadm target.
func (a *Adapter) GetCapabilities(targetID string) (tiering.TargetCapabilities, error) {
	// Check if this is a FUSE-managed target first.
	mt, err := a.store.GetMdadmManagedTarget(targetID)
	if err == nil && mt != nil {
		return mdadmCapabilities(), nil
	}

	poolName, slotName, err := parseBackingRefTarget(targetID)
	if err != nil {
		return tiering.TargetCapabilities{}, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "invalid target id",
			Cause:   err,
		}
	}
	if _, err := a.store.GetTierSlot(poolName, slotName); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return tiering.TargetCapabilities{}, &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("tier slot %q not found in pool %q", slotName, poolName),
			}
		}
		return tiering.TargetCapabilities{}, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get tier slot",
			Cause:   err,
		}
	}
	return legacyCapabilities(), nil
}

// GetPolicy returns the fill policy for the named mdadm target.
func (a *Adapter) GetPolicy(targetID string) (tiering.TargetPolicy, error) {
	poolName, slotName, err := parseBackingRefTarget(targetID)
	if err != nil {
		return tiering.TargetPolicy{}, &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "invalid target id",
			Cause:   err,
		}
	}
	slot, err := a.store.GetTierSlot(poolName, slotName)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return tiering.TargetPolicy{}, &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("tier slot %q not found in pool %q", slotName, poolName),
			}
		}
		return tiering.TargetPolicy{}, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get tier slot",
			Cause:   err,
		}
	}
	return tiering.TargetPolicy{
		TargetFillPct:    slot.TargetFillPct,
		FullThresholdPct: slot.FullThresholdPct,
	}, nil
}

// SetPolicy updates the fill policy for the named mdadm target.
func (a *Adapter) SetPolicy(targetID string, policy tiering.TargetPolicy) error {
	poolName, slotName, err := parseBackingRefTarget(targetID)
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "invalid target id",
			Cause:   err,
		}
	}
	if err := a.store.SetTierSlotFill(poolName, slotName, policy.TargetFillPct, policy.FullThresholdPct); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("tier slot %q not found in pool %q", slotName, poolName),
			}
		}
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "set tier slot fill",
			Cause:   err,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// FUSE OpenHandler implementation
// ---------------------------------------------------------------------------

// nsOpenCounter returns the atomic counter for the given namespace, creating
// it if needed. Used to track active user file handles for I/O priority.
func (a *Adapter) nsOpenCounter(namespaceID string) *int64 {
	a.activeOpensMu.Lock()
	defer a.activeOpensMu.Unlock()
	c, ok := a.activeOpens[namespaceID]
	if !ok {
		var v int64
		c = &v
		a.activeOpens[namespaceID] = c
	}
	return c
}

// ActiveOpenCount returns the number of active user file handles for a
// namespace. Movement workers check this before starting work.
func (a *Adapter) ActiveOpenCount(namespaceID string) int64 {
	return atomic.LoadInt64(a.nsOpenCounter(namespaceID))
}

// WaitForQuiet blocks until there are no active user file handles for the
// namespace, polling every 500ms. Returns immediately if there are none.
// Useful for movement workers that must yield to user I/O.
func (a *Adapter) WaitForQuiet(namespaceID string) {
	c := a.nsOpenCounter(namespaceID)
	for atomic.LoadInt64(c) > 0 {
		time.Sleep(500 * time.Millisecond)
	}
}

// HandleOpen is called by the socket server when the FUSE daemon reports an
// open() call on a managed file. It resolves the object to its current tier
// target, opens the backing file, and returns the fd and inode.
func (a *Adapter) HandleOpen(namespaceID, objectKey string, flags uint32) (int, uint64, error) {
	mdNs, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return -1, 0, syscall.EIO
	}

	key := strings.TrimPrefix(objectKey, "/")

	// Skip the SQLite managed_objects lookup that previously sat on every
	// OPEN — it was an artefact of sync-INSERT-on-CREATE, which is now gone.
	// Probe tiers fastest-first in openUnregisteredObject instead; a cold
	// file on a slower tier costs one extra openat on a negative dentry.
	var (
		fd  int
		ino uint64
	)
	if flags&uint32(syscall.O_CREAT) != 0 {
		fd, ino, err = a.openCreateObject(mdNs, namespaceID, key, flags)
	} else {
		fd, ino, err = a.openUnregisteredObject(mdNs, namespaceID, key, flags)
	}

	if err != nil {
		return -1, 0, err
	}

	// Track the active open so movement workers yield to user I/O, and
	// stamp the namespace as recently active so the placement planner
	// backs off for a quiescent period after this.
	atomic.AddInt64(a.nsOpenCounter(namespaceID), 1)
	a.markNamespaceActive(namespaceID)
	return fd, ino, nil
}

// markNamespaceActive records that a namespace saw user activity at now.
func (a *Adapter) markNamespaceActive(namespaceID string) {
	a.activityMu.Lock()
	a.lastActivity[namespaceID] = time.Now()
	a.activityMu.Unlock()
}

// namespaceIdleFor returns how long since the namespace's last recorded
// activity. If no activity has been recorded yet, the entry is seeded
// to now() and zero duration is returned — this prevents the planner
// from firing immediately after tierd restart before we've had a chance
// to observe user traffic. A genuinely idle system will pass the
// quiescent window on the second cycle.
func (a *Adapter) namespaceIdleFor(namespaceID string) time.Duration {
	a.activityMu.Lock()
	defer a.activityMu.Unlock()
	t, ok := a.lastActivity[namespaceID]
	if !ok {
		a.lastActivity[namespaceID] = time.Now()
		return 0
	}
	return time.Since(t)
}

// openUnregisteredObject handles opens for files that exist on the backing
// filesystem but have no managed_objects row (placed outside of FUSE, or
// pre-dating the tiering layer). It scans all tier mount points for the
// pool, opens the first match, and lazily registers it so future opens are
// fast without requiring a full rescan.
func (a *Adapter) openUnregisteredObject(mdNs *db.MdadmManagedNamespaceRow, namespaceID, key string, flags uint32) (int, uint64, error) {
	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return -1, 0, syscall.EIO
	}

	accessMode := int(flags & uint32(syscall.O_ACCMODE))

	// Probe tiers fastest-first so hot files resolve in one openat.
	type rankedTarget struct {
		rank   int
		target db.MdadmManagedTargetRow
	}
	var ranked []rankedTarget
	for i := range targets {
		if targets[i].PoolName != mdNs.PoolName {
			continue
		}
		tt, err := a.store.GetTierTargetByBackingRef(
			backingRefTarget(targets[i].PoolName, targets[i].TierName), BackendKind)
		if err != nil {
			continue
		}
		ranked = append(ranked, rankedTarget{rank: tt.Rank, target: targets[i]})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })

	for _, rt := range ranked {
		filePath := filepath.Join(rt.target.MountPath, key)
		fd, err := syscall.Open(filePath, accessMode, 0)
		if err != nil {
			continue // not on this tier, try next
		}

		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			_ = syscall.Close(fd)
			continue
		}

		// Record discovery in the meta store so future opens skip the scan.
		a.recordObjectAccess(rt.target.PoolName, namespaceID, st.Ino, rt.rank)

		return fd, pathVIno(key), nil
	}

	return -1, 0, syscall.ENOENT
}

// openCreateObject auto-registers a new file and creates it on the fastest
// tier that has space. If the fastest tier returns ENOSPC it falls through to
// the next fastest, continuing until a tier accepts the create or all tiers
// are exhausted — at which point ENOSPC is returned to the caller.
func (a *Adapter) openCreateObject(mdNs *db.MdadmManagedNamespaceRow, namespaceID, key string, flags uint32) (int, uint64, error) {
	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return -1, 0, syscall.EIO
	}

	// Build a rank-ordered slice of (rank, target) pairs for this pool.
	type rankedTarget struct {
		rank   int
		target db.MdadmManagedTargetRow
	}
	var ranked []rankedTarget
	for i := range targets {
		if targets[i].PoolName != mdNs.PoolName {
			continue
		}
		tt, err := a.store.GetTierTargetByBackingRef(
			backingRefTarget(targets[i].PoolName, targets[i].TierName), BackendKind)
		if err != nil {
			continue
		}
		ranked = append(ranked, rankedTarget{rank: tt.Rank, target: targets[i]})
	}
	if len(ranked) == 0 {
		return -1, 0, syscall.EIO
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })

	openFlags := int(flags&uint32(syscall.O_ACCMODE)) | syscall.O_CREAT

	for _, rt := range ranked {
		chosen := rt.target
		filePath := filepath.Join(chosen.MountPath, key)

		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			// If we can't create the parent on this tier, try the next.
			continue
		}

		fd, err := syscall.Open(filePath, openFlags, 0644)
		if err != nil {
			if err == syscall.ENOSPC {
				log.Printf("openCreateObject: tier %s full (ENOSPC), trying next tier", chosen.TierName)
				continue
			}
			return -1, 0, err
		}

		// Record tier placement in the per-pool meta store (best-effort, async).
		// The file itself is already durable on the backing FS — the meta record
		// is a hint that lets a future OPEN skip the tier scan. If this enqueue
		// is dropped (queue full, no store open yet) the next OPEN falls through
		// to openUnregisteredObject which is correct just slightly slower.
		var st syscall.Stat_t
		if fserr := syscall.Fstat(fd, &st); fserr == nil {
			a.recordObjectAccess(chosen.PoolName, namespaceID, st.Ino, rt.rank)
		}
		return fd, pathVIno(key), nil
	}

	// All tiers exhausted.
	return -1, 0, syscall.ENOSPC
}

// ---------------------------------------------------------------------------
// ConnectHandler — initial DIR_UPDATE on daemon connect
// ---------------------------------------------------------------------------

// HandleConnect is called when a FUSE daemon establishes a connection.
// It resets the per-namespace refresh state and runs an initial full scan
// through the shared serialisation lock so any refresh already in flight is
// absorbed into this one.
func (a *Adapter) HandleConnect(namespaceID string) {
	log.Printf("mdadm-fuse: HandleConnect: %s", namespaceID)
	a.refreshMu.Lock()
	// Clear any stale pending/running flags from a previous connection.
	a.refreshRunning[namespaceID] = false
	a.refreshPending[namespaceID] = false
	a.refreshMu.Unlock()

	a.refreshDirUpdate(namespaceID)
	log.Printf("mdadm-fuse: HandleConnect done: %s", namespaceID)
}

// refreshDirUpdate rescans backing tiers and sends an updated DIR_UPDATE.
// Concurrent calls are serialised: if a scan is already running, the next
// call sets a "pending" flag so that exactly one more scan runs after the
// current one finishes. This prevents stale partial results from overwriting
// a more-complete scan that started earlier.
func (a *Adapter) refreshDirUpdate(namespaceID string) {
	a.refreshMu.Lock()
	if a.refreshRunning[namespaceID] {
		// Another scan is already in progress; request one more run after it.
		a.refreshPending[namespaceID] = true
		a.refreshMu.Unlock()
		log.Printf("mdadm-fuse: refreshDirUpdate: queued (scan already running) for %s", namespaceID)
		return
	}
	a.refreshRunning[namespaceID] = true
	a.refreshMu.Unlock()

	for {
		log.Printf("mdadm-fuse: refreshDirUpdate: scanning %s", namespaceID)
		entries, err := a.scanNamespaceEntries(namespaceID)
		if err != nil {
			log.Printf("mdadm-fuse: refreshDirUpdate: scan failed for %s: %v", namespaceID, err)
		} else {
			log.Printf("mdadm-fuse: refreshDirUpdate: sending DIR_UPDATE with %d entries for %s", len(entries), namespaceID)
			if err := a.server.SendDirUpdate(namespaceID, entries); err != nil {
				log.Printf("mdadm-fuse: refreshDirUpdate: SendDirUpdate failed for %s: %v", namespaceID, err)
			}
		}

		a.refreshMu.Lock()
		if a.refreshPending[namespaceID] {
			// A caller requested another scan while we were running; do it now.
			a.refreshPending[namespaceID] = false
			a.refreshMu.Unlock()
			continue
		}
		a.refreshRunning[namespaceID] = false
		a.refreshMu.Unlock()
		return
	}
}

// scanNamespaceEntries walks all backing tiers for the namespace's pool and
// builds a merged slice of DirEntry values for the DIR_UPDATE payload.
// Paths from multiple tiers are de-duplicated (first tier wins on conflict).
func (a *Adapter) scanNamespaceEntries(namespaceID string) ([]fusepkg.DirEntry, error) {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return nil, fmt.Errorf("get namespace %q: %w", namespaceID, err)
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return nil, fmt.Errorf("list managed targets: %w", err)
	}

	seen := make(map[string]fusepkg.DirEntry)
	for _, mt := range targets {
		if mt.PoolName != mn.PoolName {
			continue
		}
		if _, statErr := os.Stat(mt.MountPath); statErr != nil {
			continue // backing mount not ready
		}
		_ = filepath.WalkDir(mt.MountPath, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			rel, relErr := filepath.Rel(mt.MountPath, path)
			if relErr != nil || rel == "." {
				return nil
			}
			if _, exists := seen[rel]; exists {
				return nil // already have this path from a hotter tier
			}
			var st syscall.Stat_t
			if syscall.Lstat(path, &st) != nil {
				return nil
			}
			entryType := uint8(0)
			if d.IsDir() {
				entryType = 1
			}
			seen[rel] = fusepkg.DirEntry{
				// Use a path-derived virtual inode so that entries from
				// different backing tiers (which have independent inode
				// namespaces) never collide in the FUSE dir cache.
				Inode:     pathVIno(rel),
				Type:      entryType,
				Path:      rel,
				Mode:      st.Mode, // POSIX mode_t from syscall.Lstat, not Go os.FileMode
				UID:       st.Uid,
				GID:       st.Gid,
				Size:      uint64(st.Size),
				MtimeSec:  st.Mtim.Sec,
				MtimeNsec: uint32(st.Mtim.Nsec),
			}
			return nil
		})
	}

	entries := make([]fusepkg.DirEntry, 0, len(seen))
	for _, e := range seen {
		entries = append(entries, e)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// FSOpHandler — mkdir, unlink, rmdir, rename on backing filesystem
// ---------------------------------------------------------------------------

// HandleMkdir creates a directory at path on all backing tiers for the pool.
// Returns the inode and mtime of the directory on the first (fastest) tier.
func (a *Adapter) HandleMkdir(namespaceID, path string, mode uint32) (uint64, int64, uint32, error) {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return 0, 0, 0, syscall.EIO
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return 0, 0, 0, syscall.EIO
	}

	var created bool
	var firstMtimeSec int64
	var firstMtimeNsec uint32

	for _, mt := range targets {
		if mt.PoolName != mn.PoolName {
			continue
		}
		fullPath := filepath.Join(mt.MountPath, filepath.Clean("/"+path))
		if mkErr := os.MkdirAll(fullPath, os.FileMode(mode)|0o111); mkErr != nil && !os.IsExist(mkErr) {
			log.Printf("mdadm-fuse: HandleMkdir %q on %q: %v", path, mt.MountPath, mkErr)
			continue
		}
		if !created {
			var st syscall.Stat_t
			if syscall.Stat(fullPath, &st) == nil {
				firstMtimeSec = st.Mtim.Sec
				firstMtimeNsec = uint32(st.Mtim.Nsec)
				created = true
			}
		}
	}

	if !created {
		return 0, 0, 0, syscall.EIO
	}

	// Do NOT trigger a refreshDirUpdate here. fuse_ns_mkdir already calls
	// dir_cache_add immediately after receiving our response, which keeps the
	// FUSE daemon's cache current without any round-trip. A concurrent
	// refreshDirUpdate scan snapshots backing-store state at a point in time
	// that may predate sibling mkdirs, so apply_dir_update would atomically
	// replace the cache with a snapshot missing those directories — causing
	// the next mkdir for a child of a just-created dir to get ENOENT from
	// dir_cache_find_ino. Incremental cache ops (dir_cache_add / _remove /
	// inline rename) are always correct; full-replacement refreshes belong
	// only on connect and explicit external-change events.
	return pathVIno(strings.TrimPrefix(filepath.Clean("/"+path), "/")), firstMtimeSec, firstMtimeNsec, nil
}

// HandleUnlink deletes a file from backing tiers and clears its meta record.
func (a *Adapter) HandleUnlink(namespaceID, path string) error {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return syscall.EIO
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return syscall.EIO
	}

	clean := filepath.Clean("/" + path)

	// Collect inodes before unlinking so we can clear their meta records
	// after the unlinks succeed. A file may exist on more than one tier
	// during a migration; delete every distinct inode we observe.
	store := a.metaStoreFor(mn.PoolName)
	var inodes []uint64
	if store != nil {
		for _, mt := range targets {
			if mt.PoolName != mn.PoolName {
				continue
			}
			var st syscall.Stat_t
			if err := syscall.Stat(filepath.Join(mt.MountPath, clean), &st); err == nil {
				inodes = append(inodes, st.Ino)
			}
		}
	}

	var lastErr error
	for _, mt := range targets {
		if mt.PoolName != mn.PoolName {
			continue
		}
		fullPath := filepath.Join(mt.MountPath, clean)
		if rmErr := os.Remove(fullPath); rmErr != nil && !os.IsNotExist(rmErr) {
			lastErr = rmErr
		}
	}

	// Also delete legacy managed_objects row for pre-switch files.
	_ = a.store.DeleteManagedObjectByKey(namespaceID, strings.TrimPrefix(path, "/"))

	if store != nil {
		for _, ino := range inodes {
			if err := store.Delete(ino); err != nil {
				log.Printf("mdadm: HandleUnlink meta delete inode %d: %v", ino, err)
			}
		}
	}
	// fuse_ns_unlink calls dir_cache_remove immediately; no refresh needed.
	return lastErr
}

// HandleRmdir removes an empty directory from all backing tiers.
func (a *Adapter) HandleRmdir(namespaceID, path string) error {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return syscall.EIO
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return syscall.EIO
	}

	clean := filepath.Clean("/" + path)
	var lastErr error
	for _, mt := range targets {
		if mt.PoolName != mn.PoolName {
			continue
		}
		fullPath := filepath.Join(mt.MountPath, clean)
		if rmErr := os.Remove(fullPath); rmErr != nil && !os.IsNotExist(rmErr) {
			lastErr = rmErr
		}
	}

	// fuse_ns_rmdir calls dir_cache_remove immediately; no refresh needed.
	return lastErr
}

// HandleRename renames a path on all backing tiers that have the old path.
func (a *Adapter) HandleRename(namespaceID, oldPath, newPath string) error {
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		return syscall.EIO
	}

	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		return syscall.EIO
	}

	oldClean := filepath.Clean("/" + oldPath)
	newClean := filepath.Clean("/" + newPath)
	var lastErr error
	for _, mt := range targets {
		if mt.PoolName != mn.PoolName {
			continue
		}
		oldFull := filepath.Join(mt.MountPath, oldClean)
		newFull := filepath.Join(mt.MountPath, newClean)
		if _, statErr := os.Lstat(oldFull); os.IsNotExist(statErr) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(newFull), 0o755); err != nil {
			lastErr = err
			continue
		}
		if renErr := os.Rename(oldFull, newFull); renErr != nil {
			lastErr = renErr
		}
	}

	// Update managed_objects table so HandleOpen can find the renamed key.
	oldKey := strings.TrimPrefix(oldClean, "/")
	newKey := strings.TrimPrefix(newClean, "/")
	_ = a.store.RenameObjectKey(namespaceID, oldKey, newKey)

	// fuse_ns_rename updates the cache entry inline; no refresh needed.
	return lastErr
}

// HandleRelease is called when the FUSE daemon closes a backing fd.
// Decrements the active-opens counter so movement workers can resume.
func (a *Adapter) HandleRelease(namespaceID string, inode uint64) {
	c := a.nsOpenCounter(namespaceID)
	if atomic.LoadInt64(c) > 0 {
		atomic.AddInt64(c, -1)
	}
}

// HandleBypass is called when the daemon detects a bypass condition.
func (a *Adapter) HandleBypass(namespaceID string) {
	log.Printf("mdadm-fuse: bypass detected for namespace %s", namespaceID)
}

// HandleFDPassFailed is called when SCM_RIGHTS fd pass fails after open.
func (a *Adapter) HandleFDPassFailed(namespaceID string, expectedInode uint64) {
	log.Printf("mdadm-fuse: fd pass failed for namespace %s, expected inode %d", namespaceID, expectedInode)
}

// OnHealthFail is called when the daemon fails consecutive health checks.
func (a *Adapter) OnHealthFail(namespaceID string) {
	log.Printf("mdadm-fuse: health check failed for namespace %s, restarting daemon", namespaceID)
	mn, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil {
		log.Printf("mdadm-fuse: cannot look up namespace %s for restart: %v", namespaceID, err)
		return
	}
	_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "restarting", 0)
	if err := a.supervisor.Restart(namespaceID, mn.MountPath, mn.SocketPath); err != nil {
		log.Printf("mdadm-fuse: restart failed for namespace %s: %v", namespaceID, err)
		_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "failed", 0)
		return
	}
	newPID := a.supervisor.ActivePID(namespaceID)
	_ = a.store.SetMdadmManagedNamespaceDaemonState(namespaceID, "running", newPID)
}

// ---------------------------------------------------------------------------
// Reconcile — syncs native + managed state into control-plane tables
// ---------------------------------------------------------------------------

// Reconcile syncs the native mdadm tier state into the unified control-plane
// tables. For each assigned tier slot it upserts a tier_target; for each
// managed volume it upserts a managed_namespace. Also discovers
// mdadm_managed_targets/namespaces for FUSE-capable pools.
//
// Reconcile is idempotent.
func (a *Adapter) Reconcile() error {
	pools, err := a.store.ListTierInstances()
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier pools",
			Cause:   err,
		}
	}

	// Sync legacy tier targets.
	for _, pool := range pools {
		slots, err := a.store.ListTierSlots(pool.Name)
		if err != nil {
			return &tiering.AdapterError{
				Kind:    tiering.ErrTransient,
				Message: "list tier slots for pool " + pool.Name,
				Cause:   err,
			}
		}
		for _, slot := range slots {
			if slot.State == db.TierSlotStateEmpty {
				continue
			}
			bref := backingRefTarget(pool.Name, slot.Name)
			health := slotHealthToUnified(slot.State)

			existing, lookupErr := a.store.GetTierTargetByBackingRef(bref, BackendKind)
			if lookupErr != nil && !errors.Is(lookupErr, db.ErrNotFound) {
				return &tiering.AdapterError{
					Kind:    tiering.ErrTransient,
					Message: "lookup tier target",
					Cause:   lookupErr,
				}
			}
			if errors.Is(lookupErr, db.ErrNotFound) {
				row := &db.TierTargetRow{
					Name:             slot.Name,
					PlacementDomain:  pool.Name,
					BackendKind:      BackendKind,
					Rank:             slot.Rank,
					TargetFillPct:    slot.TargetFillPct,
					FullThresholdPct: slot.FullThresholdPct,
					Health:           health,
					BackingRef:       bref,
					CapabilitiesJSON: legacyCapabilitiesJSON,
				}
				if err := a.store.CreateTierTarget(row); err != nil {
					return &tiering.AdapterError{
						Kind:    tiering.ErrTransient,
						Message: "create tier target",
						Cause:   err,
					}
				}
			} else if existing.Health != health {
				if err := a.store.UpdateTierTargetActivity(
					existing.ID, health,
					existing.ActivityBand, existing.ActivityTrend,
				); err != nil {
					return &tiering.AdapterError{
						Kind:    tiering.ErrTransient,
						Message: "update tier target activity",
						Cause:   err,
					}
				}
			}
		}
	}

	// Ensure mdadm_managed_targets rows exist for per-tier slots.
	// These rows map tier_targets to their VG/LV/mount so the FUSE daemon's
	// HandleOpen can find the backing filesystem.
	for _, pool := range pools {
		slots, _ := a.store.ListTierSlots(pool.Name)
		for _, slot := range slots {
			if slot.State == db.TierSlotStateEmpty {
				continue
			}
			if _, err := a.store.GetMdadmManagedTargetByPoolTier(pool.Name, slot.Name); err == nil {
				continue // already exists
			}
			bref := backingRefTarget(pool.Name, slot.Name)
			tt, err := a.store.GetTierTargetByBackingRef(bref, BackendKind)
			if err != nil {
				continue // no tier_target row yet
			}
			vgName := fmt.Sprintf("tier-%s-%s", pool.Name, slot.Name)
			mountPath := filepath.Join("/mnt/.tierd-backing", pool.Name, slot.Name)
			log.Printf("mdadm reconcile: creating managed target for %s/%s", pool.Name, slot.Name)
			_ = a.store.UpsertMdadmManagedTarget(&db.MdadmManagedTargetRow{
				TierTargetID: tt.ID,
				PoolName:     pool.Name,
				TierName:     slot.Name,
				VGName:       vgName,
				LVName:       "data",
				MountPath:    mountPath,
			})
		}
	}

	// Sync FUSE-managed targets: update health based on mount status.
	managedTargets, err := a.store.ListMdadmManagedTargets()
	if err == nil {
		for _, mt := range managedTargets {
			health := "healthy"
			if !lvm.IsMounted(mt.MountPath) {
				health = "degraded"
			}
			_ = a.store.UpdateTierTargetActivity(mt.TierTargetID, health, "", "")
		}
	}

	// Ensure FUSE-managed namespaces exist for pools that have
	// at least one assigned tier slot. This handles pools provisioned before
	// the auto-create logic, or where the namespace was lost.
	for _, pool := range pools {
		if pool.State != db.TierPoolStateHealthy {
			continue
		}
		bref := backingRefManagedNamespace(pool.Name)
		if _, err := a.store.GetManagedNamespaceByBackingRef(bref, BackendKind); err == nil {
			continue // namespace already exists
		}
		slots, _ := a.store.ListTierSlots(pool.Name)
		hasAssigned := false
		for _, s := range slots {
			if s.State != db.TierSlotStateEmpty {
				hasAssigned = true
				break
			}
		}
		if !hasAssigned {
			continue
		}
		log.Printf("mdadm reconcile: auto-creating FUSE namespace for pool %q", pool.Name)
		if _, nsErr := a.CreateNamespace(tiering.NamespaceSpec{
			Name:            pool.Name,
			PlacementDomain: pool.Name,
			NamespaceKind:   "filespace",
			ExposedPath:     filepath.Join("/mnt", pool.Name),
		}); nsErr != nil {
			log.Printf("mdadm reconcile: failed to create namespace for pool %q: %v", pool.Name, nsErr)
		}
	}

	// Sync FUSE-managed namespaces: restart stopped daemons and update state.
	managedNS, err := a.store.ListMdadmManagedNamespaces()
	if err == nil {
		for _, mn := range managedNS {
			pid := a.supervisor.ActivePID(mn.NamespaceID)
			if pid != 0 {
				if mn.DaemonState != "running" {
					_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "running", pid)
				}
				continue
			}
			// Daemon is not running — restart it.
			log.Printf("mdadm reconcile: restarting FUSE daemon for namespace %s (pool %s)", mn.NamespaceID, mn.PoolName)
			socketPath, sockErr := a.server.Start(mn.NamespaceID)
			if sockErr != nil {
				log.Printf("mdadm reconcile: failed to start socket server for %s: %v", mn.NamespaceID, sockErr)
				_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "failed", 0)
				continue
			}
			if err := a.supervisor.Start(mn.NamespaceID, mn.MountPath, socketPath); err != nil {
				log.Printf("mdadm reconcile: failed to start FUSE daemon for %s: %v", mn.NamespaceID, err)
				a.server.Stop(mn.NamespaceID)
				_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "failed", 0)
				continue
			}
			newPID := a.supervisor.ActivePID(mn.NamespaceID)
			_ = a.store.UpsertMdadmManagedNamespace(&db.MdadmManagedNamespaceRow{
				NamespaceID: mn.NamespaceID,
				PoolName:    mn.PoolName,
				SocketPath:  socketPath,
				MountPath:   mn.MountPath,
				DaemonPID:   newPID,
				DaemonState: "running",
			})
			a.supervisor.Supervise(mn.NamespaceID, func() {
				log.Printf("mdadm-fuse: daemon for namespace %s crashed, restarting", mn.NamespaceID)
				_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "crashed", 0)
				if err := a.supervisor.Restart(mn.NamespaceID, mn.MountPath, socketPath); err != nil {
					log.Printf("mdadm-fuse: failed to restart daemon for namespace %s: %v", mn.NamespaceID, err)
					_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "failed", 0)
					return
				}
				restartPID := a.supervisor.ActivePID(mn.NamespaceID)
				_ = a.store.SetMdadmManagedNamespaceDaemonState(mn.NamespaceID, "running", restartPID)
			})
		}
	}

	return a.syncDegradedStates(pools)
}

// syncDegradedStates clears all mdadm degraded-state rows and re-emits
// current signals from pool/slot health, FUSE targets, and region migration failures.
func (a *Adapter) syncDegradedStates(pools []db.TierInstance) error {
	if err := a.store.DeleteDegradedStatesByBackend(BackendKind); err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "clear degraded states",
			Cause:   err,
		}
	}

	for _, pool := range pools {
		slots, err := a.store.ListTierSlots(pool.Name)
		if err != nil {
			continue
		}

		switch pool.State {
		case db.TierPoolStateDegraded:
			if upsertErr := a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "placement_domain",
				ScopeID:     pool.Name,
				Severity:    db.DegradedSeverityWarning,
				Code:        "reconciliation_required",
				Message:     fmt.Sprintf("tier pool %q is degraded", pool.Name),
			}); upsertErr != nil {
				return &tiering.AdapterError{
					Kind:    tiering.ErrTransient,
					Message: "upsert pool degraded state",
					Cause:   upsertErr,
				}
			}
		case db.TierPoolStateError:
			if upsertErr := a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "placement_domain",
				ScopeID:     pool.Name,
				Severity:    db.DegradedSeverityCritical,
				Code:        "reconciliation_required",
				Message:     fmt.Sprintf("tier pool %q is in error state: %s", pool.Name, pool.ErrorReason),
			}); upsertErr != nil {
				return &tiering.AdapterError{
					Kind:    tiering.ErrTransient,
					Message: "upsert pool error state",
					Cause:   upsertErr,
				}
			}
		}

		for _, slot := range slots {
			var code, message string
			var severity string
			switch slot.State {
			case db.TierSlotStateMissing:
				code = "reconciliation_required"
				severity = db.DegradedSeverityCritical
				message = fmt.Sprintf("tier slot %q in pool %q is missing", slot.Name, pool.Name)
			case db.TierSlotStateDegraded:
				code = "reconciliation_required"
				severity = db.DegradedSeverityWarning
				message = fmt.Sprintf("tier slot %q in pool %q is degraded", slot.Name, pool.Name)
			default:
				continue
			}
			if upsertErr := a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "tier_target",
				ScopeID:     backingRefTarget(pool.Name, slot.Name),
				Severity:    severity,
				Code:        code,
				Message:     message,
			}); upsertErr != nil {
				return &tiering.AdapterError{
					Kind:    tiering.ErrTransient,
					Message: "upsert slot degraded state",
					Cause:   upsertErr,
				}
			}
		}
	}

	// FUSE-managed target degraded states.
	managedTargets, _ := a.store.ListMdadmManagedTargets()
	for _, mt := range managedTargets {
		if !lvm.IsMounted(mt.MountPath) {
			_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "tier_target",
				ScopeID:     mt.TierTargetID,
				Severity:    db.DegradedSeverityCritical,
				Code:        "mount_missing",
				Message:     fmt.Sprintf("backing mount %s is not mounted", mt.MountPath),
			})
		}
	}

	// FUSE daemon degraded states.
	managedNS, _ := a.store.ListMdadmManagedNamespaces()
	for _, mn := range managedNS {
		if mn.DaemonState == "failed" || mn.DaemonState == "crashed" {
			_ = a.store.UpsertDegradedState(&db.DegradedStateRow{
				BackendKind: BackendKind,
				ScopeKind:   "managed_namespace",
				ScopeID:     mn.NamespaceID,
				Severity:    db.DegradedSeverityCritical,
				Code:        "daemon_down",
				Message:     fmt.Sprintf("FUSE daemon for namespace %s is %s", mn.NamespaceID, mn.DaemonState),
			})
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Activity collection
// ---------------------------------------------------------------------------

// CollectActivity returns activity samples. Legacy managed-volume region
// sampling has been removed; FUSE-managed namespaces do not yet emit activity.
func (a *Adapter) CollectActivity() ([]tiering.ActivitySample, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Movement planning and execution
// ---------------------------------------------------------------------------

// PlanMovements returns movement plans. Legacy managed-volume region planning
// has been removed; FUSE-managed namespaces use object-level movement.
func (a *Adapter) PlanMovements() ([]tiering.MovementPlan, error) {
	return nil, nil
}

// StartMovement creates a movement_jobs control-plane row for the given plan.
// If users have active file handles in the namespace, the movement is deferred
// (returns ErrTransient) so user I/O gets full device bandwidth.
func (a *Adapter) StartMovement(plan tiering.MovementPlan) (string, error) {
	if a.ActiveOpenCount(plan.NamespaceID) > 0 {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "deferring movement: active user I/O in namespace",
		}
	}

	job := &db.MovementJobRow{
		BackendKind:     BackendKind,
		NamespaceID:     plan.NamespaceID,
		ObjectID:        plan.ObjectID,
		MovementUnit:    plan.MovementUnit,
		PlacementDomain: plan.PlacementDomain,
		SourceTargetID:  plan.SourceTargetID,
		DestTargetID:    plan.DestTargetID,
		PolicyRevision:  plan.PolicyRevision,
		IntentRevision:  plan.IntentRevision,
		PlannerEpoch:    plan.PlannerEpoch,
		TriggeredBy:     plan.TriggeredBy,
		TotalBytes:      plan.TotalBytes,
		State:           db.MovementJobStatePending,
	}
	if err := a.store.CreateMovementJob(job); err != nil {
		return "", &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create movement job",
			Cause:   err,
		}
	}
	return job.ID, nil
}

// GetMovement returns the current state of a movement job.
func (a *Adapter) GetMovement(id string) (*tiering.MovementState, error) {
	job, err := a.store.GetMovementJob(id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: "movement not found",
			}
		}
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

// CancelMovement cancels a pending or running movement job.
func (a *Adapter) CancelMovement(id string) error {
	if err := a.store.CancelMovementJob(id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: "movement not found",
			}
		}
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "cancel movement job",
			Cause:   err,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pin/Unpin
// ---------------------------------------------------------------------------

// Pin sets a pin on the namespace identified by namespaceID. Legacy managed
// volume pins have been removed.
func (a *Adapter) Pin(scope tiering.PinScope, namespaceID string, objectID string) error {
	if scope != tiering.PinScopeVolume && scope != tiering.PinScopeNone {
		return &tiering.AdapterError{
			Kind:    tiering.ErrCapabilityViolation,
			Message: fmt.Sprintf("mdadm only supports volume-scoped pins; got scope %q", scope),
		}
	}
	ns, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil || ns == nil {
		return &tiering.AdapterError{Kind: tiering.ErrPermanent, Message: fmt.Sprintf("namespace %q not found", namespaceID)}
	}
	return a.store.SetNamespacePinState(ns.NamespaceID, "pinned-hot")
}

// Unpin clears a pin on the namespace identified by namespaceID. Legacy managed
// volume pins have been removed.
func (a *Adapter) Unpin(scope tiering.PinScope, namespaceID string, objectID string) error {
	if scope != tiering.PinScopeVolume && scope != tiering.PinScopeNone {
		return &tiering.AdapterError{
			Kind:    tiering.ErrCapabilityViolation,
			Message: fmt.Sprintf("mdadm only supports volume-scoped pins; got scope %q", scope),
		}
	}
	ns, err := a.store.GetMdadmManagedNamespace(namespaceID)
	if err != nil || ns == nil {
		return &tiering.AdapterError{Kind: tiering.ErrPermanent, Message: fmt.Sprintf("namespace %q not found", namespaceID)}
	}
	return a.store.SetNamespacePinState(ns.NamespaceID, "none")
}

// ---------------------------------------------------------------------------
// Degraded state
// ---------------------------------------------------------------------------

// GetDegradedState returns all active degraded-state signals for the mdadm backend.
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
	for _, d := range rows {
		if d.BackendKind != BackendKind {
			continue
		}
		out = append(out, tiering.DegradedState{
			ID:          d.ID,
			BackendKind: d.BackendKind,
			ScopeKind:   d.ScopeKind,
			ScopeID:     d.ScopeID,
			Severity:    d.Severity,
			Code:        d.Code,
			Message:     d.Message,
			UpdatedAt:   d.UpdatedAt,
		})
	}
	return out, nil
}
