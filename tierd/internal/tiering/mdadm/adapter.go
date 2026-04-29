// Package mdadm implements the unified TieringAdapter for the mdadm/LVM
// backend. It provisions per-tier LVs and records control-plane state
// (tier_targets, managed_namespaces, placement). The data-plane filesystem
// is the smoothfs kernel module — the adapter no longer runs a user-space
// user-space daemon.
package mdadm

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier/backend"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

var managedTargetIsMounted = lvm.IsMounted
var smoothfsMountIsMounted = lvm.IsMounted
var createSmoothfsManagedPool = smoothfsclient.CreateManagedPool
var destroySmoothfsManagedPool = smoothfsclient.DestroyManagedPool
var renderSmoothfsUnit = smoothfsclient.RenderMountUnit
var readSmoothfsUnitFile = os.ReadFile

var ensureTierBackingMount = func(poolName, tierName, kind, ref, filesystem string) error {
	if kind == "" || kind == BackendKind {
		kind = "mdadm"
	}
	if kind == "mdadm" || strings.TrimSpace(ref) == "" {
		return nil
	}
	b, err := backend.Lookup(kind)
	if err != nil {
		return err
	}
	return b.Provision(poolName, tierName, ref, filepath.Join("/mnt/.tierd-backing", poolName, tierName), backend.ProvisionOpts{
		Filesystem: filesystem,
	})
}

func (a *Adapter) ensureSmoothfsPoolMount(pool db.TierInstance, slots []db.TierSlot) error {
	mountPoint := filepath.Join("/mnt", pool.Name)
	assigned := make([]db.TierSlot, 0, len(slots))
	for _, slot := range slots {
		if slot.State != db.TierSlotStateEmpty {
			assigned = append(assigned, slot)
		}
	}
	if len(assigned) == 0 {
		return nil
	}
	sort.Slice(assigned, func(i, j int) bool {
		if assigned[i].Rank == assigned[j].Rank {
			return assigned[i].Name < assigned[j].Name
		}
		return assigned[i].Rank < assigned[j].Rank
	})

	tiers := make([]string, 0, len(assigned))
	for _, slot := range assigned {
		mountPath := filepath.Join("/mnt/.tierd-backing", pool.Name, slot.Name)
		if slot.BackingKind != "" && slot.BackingKind != BackendKind && !managedTargetIsMounted(mountPath) {
			if err := ensureTierBackingMount(pool.Name, slot.Name, slot.BackingKind, slot.BackingRef, pool.Filesystem); err != nil {
				return fmt.Errorf("ensure backing mount for %s/%s (%s): %w", pool.Name, slot.Name, slot.BackingKind, err)
			}
		}
		if !managedTargetIsMounted(mountPath) {
			return fmt.Errorf("backing mount %s is not mounted", mountPath)
		}
		tiers = append(tiers, mountPath)
	}

	req := smoothfsclient.CreateManagedPoolRequest{
		Name:      pool.Name,
		Tiers:     tiers,
		MountBase: "/mnt",
	}
	desired := smoothfsclient.ManagedPool{
		Name:       pool.Name,
		Tiers:      tiers,
		Mountpoint: mountPoint,
		UnitPath:   filepath.Join(smoothfsclient.SystemdUnitDir, smoothfsclient.UnitFilenameFor(mountPoint)),
	}
	if existing, err := a.store.GetSmoothfsPool(pool.Name); err == nil {
		if parsed, parseErr := uuid.Parse(existing.UUID); parseErr == nil {
			req.UUID = parsed
			desired.UUID = parsed
		}
	}
	if desired.UUID == uuid.Nil {
		desired.UUID = req.UUID
	}
	if desired.UUID == uuid.Nil {
		desired.UUID = uuid.New()
		req.UUID = desired.UUID
	}
	if smoothfsMountIsMounted(mountPoint) {
		matches, err := smoothfsUnitMatches(desired)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
		if err := destroySmoothfsManagedPool(desired); err != nil {
			return fmt.Errorf("repair stale smoothfs mount %s: %w", mountPoint, err)
		}
	}
	mp, err := createSmoothfsManagedPool(req)
	if err != nil {
		return err
	}
	if _, err := a.store.GetSmoothfsPool(pool.Name); err == nil {
		return a.store.UpdateSmoothfsPool(db.SmoothfsPool{
			UUID:       mp.UUID.String(),
			Name:       mp.Name,
			Tiers:      mp.Tiers,
			Mountpoint: mp.Mountpoint,
			UnitPath:   mp.UnitPath,
		})
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}
	_, err = a.store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       mp.UUID.String(),
		Name:       mp.Name,
		Tiers:      mp.Tiers,
		Mountpoint: mp.Mountpoint,
		UnitPath:   mp.UnitPath,
	})
	if errors.Is(err, db.ErrDuplicate) {
		return nil
	}
	return err
}

func smoothfsUnitMatches(pool smoothfsclient.ManagedPool) (bool, error) {
	want, err := renderSmoothfsUnit(pool)
	if err != nil {
		return false, err
	}
	got, err := readSmoothfsUnitFile(pool.UnitPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read smoothfs unit %s: %w", pool.UnitPath, err)
	}
	return string(got) == want, nil
}

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

// BackendKind is the canonical identifier for the mdadm/LVM backend.
const BackendKind = "mdadm"

// legacyCapabilitiesJSON is the JSON-encoded TargetCapabilities for legacy
// mdadm tier targets (monolithic LV).
const legacyCapabilitiesJSON = `{"movement_granularity":"region","pin_scope":"volume","supports_online_move":true,"supports_recall":false,"recall_mode":"none","snapshot_mode":"none","supports_checksums":false,"supports_compression":false,"supports_write_bias":false}`

// capabilitiesJSON is the JSON-encoded TargetCapabilities for per-tier mdadm
// tier targets that participate in a smoothfs pool.
const capabilitiesJSON = `{"movement_granularity":"object","pin_scope":"volume","supports_online_move":true,"supports_recall":false,"recall_mode":"none","snapshot_mode":"none","supports_checksums":false,"supports_compression":false,"supports_write_bias":true}`

var backingMountActive = isMountPointFast

// isMountPointFast reports whether path is a mount point by comparing
// its st_dev with its parent's. Returns false on any stat error.
func isMountPointFast(path string) bool {
	var st, pst syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return false
	}
	if err := syscall.Lstat(filepath.Dir(path), &pst); err != nil {
		return false
	}
	return st.Dev != pst.Dev
}

// legacyCapabilities returns the static TargetCapabilities for legacy mdadm pools.
func legacyCapabilities() tiering.TargetCapabilities {
	return tiering.TargetCapabilities{
		MovementGranularity: "region",
		PinScope:            "volume",
		SupportsOnlineMove:  true,
		SupportsRecall:      false,
		RecallMode:          "none",
		SnapshotMode:        "none",
	}
}

// mdadmCapabilities returns the TargetCapabilities for per-tier mdadm targets.
func mdadmCapabilities() tiering.TargetCapabilities {
	return tiering.TargetCapabilities{
		MovementGranularity: "object",
		PinScope:            "volume",
		SupportsOnlineMove:  true,
		SupportsRecall:      false,
		RecallMode:          "none",
		SnapshotMode:        "none",

		SupportsWriteBias: true,
	}
}

func (a *Adapter) slowestTierRank(poolName string) (int, error) {
	slots, err := a.store.ListTierSlots(poolName)
	if err != nil {
		return 0, err
	}
	slowest := 0
	for _, slot := range slots {
		if slot.Rank > slowest {
			slowest = slot.Rank
		}
	}
	return slowest, nil
}

func (a *Adapter) targetMountReady(target db.MdadmManagedTargetRow) bool {
	if backingMountActive(target.MountPath) {
		return true
	}
	log.Printf("mdadm: skipping unmounted backing tier %s/%s at %s",
		target.PoolName, target.TierName, target.MountPath)
	_ = a.updateTierTargetActivity(target.TierTargetID, "degraded", "", "")
	return false
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

// backingRefManagedNamespace returns the stable backing_ref for a
// per-tier-managed namespace. Format: "mdadm:ns:{poolName}"
func backingRefManagedNamespace(poolName string) string {
	return fmt.Sprintf("mdadm:ns:%s", poolName)
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
// It provisions per-tier LVs and records control-plane state; data-plane
// presentation is handled by the smoothfs kernel module.
type Adapter struct {
	store *db.Store
	// cache fronts mdadm_managed_targets and tier_targets in memory so
	// metadata ops don't hit SQLite on every call.
	cache  *targetCache
	runDir string

	// metaMu guards metaStores. The map is populated at startup by the
	// orchestration in main.go once each pool's fastest tier is mounted.
	metaMu     sync.RWMutex
	metaStores map[string]*meta.PoolMetaStore // poolName → store
}

// NewAdapter returns a new mdadm tiering adapter bound to the given store.
// runDir is a scratch directory for per-namespace state (e.g. /run/tierd/mdadm).
func NewAdapter(store *db.Store, runDir string) *Adapter {
	a := &Adapter{
		store:      store,
		runDir:     runDir,
		metaStores: make(map[string]*meta.PoolMetaStore),
	}
	if c, err := newTargetCache(store); err != nil {
		log.Printf("mdadm adapter: target cache init failed, falling back to direct SQL: %v", err)
	} else {
		a.cache = c
	}
	return a
}

// Kind returns the backend identifier.
func (a *Adapter) Kind() string { return BackendKind }

// listManagedTargets returns all managed targets, preferring the cache.
// Falls back to the store on a nil cache (test harnesses or init failure).
func (a *Adapter) listManagedTargets() ([]db.MdadmManagedTargetRow, error) {
	if a.cache != nil {
		return a.cache.listMdadmTargets(), nil
	}
	return a.store.ListMdadmManagedTargets()
}

// getTierTargetByBackingRef resolves a tier_target by (ref, kind), preferring
// the cache.
func (a *Adapter) getTierTargetByBackingRef(ref, kind string) (*db.TierTargetRow, error) {
	if a.cache != nil {
		if r, ok := a.cache.getTierByBackingRef(ref, kind); ok {
			rr := r
			return &rr, nil
		}
		return nil, db.ErrNotFound
	}
	return a.store.GetTierTargetByBackingRef(ref, kind)
}

// upsertManagedTarget writes through the cache. With a nil cache the call
// falls through to the store.
func (a *Adapter) upsertManagedTarget(row *db.MdadmManagedTargetRow) error {
	if a.cache != nil {
		a.cache.upsertMdadmTarget(row)
		return nil
	}
	return a.store.UpsertMdadmManagedTarget(row)
}

func (a *Adapter) deleteManagedTarget(id string) error {
	if a.cache != nil {
		a.cache.deleteMdadmTarget(id)
		return nil
	}
	return a.store.DeleteMdadmManagedTarget(id)
}

func (a *Adapter) createTierTarget(row *db.TierTargetRow) error {
	if a.cache != nil {
		return a.cache.createTierTarget(row)
	}
	return a.store.CreateTierTarget(row)
}

func (a *Adapter) deleteTierTarget(id string) error {
	if a.cache != nil {
		a.cache.deleteTierTarget(id)
		return nil
	}
	return a.store.DeleteTierTarget(id)
}

func (a *Adapter) updateTierTargetActivity(id, health, band, trend string) error {
	if a.cache != nil {
		a.cache.updateTierTargetActivity(id, health, band, trend)
		return nil
	}
	return a.store.UpdateTierTargetActivity(id, health, band, trend)
}

// getManagedNamespace returns the mdadm managed namespace, preferring
// the cache. Hot path: every the hot path / HandleLookup / HandleMkdir
// resolves the namespace first to find the backing mount, so this must
// never touch SQLite when the cache is populated.
func (a *Adapter) getManagedNamespace(namespaceID string) (*db.MdadmManagedNamespaceRow, error) {
	if a.cache != nil {
		if r, ok := a.cache.getMdadmNs(namespaceID); ok {
			rr := r
			return &rr, nil
		}
		return nil, db.ErrNotFound
	}
	return a.store.GetMdadmManagedNamespace(namespaceID)
}

// getManagedNamespaceByPool returns the managed namespace for a pool
// name, preferring the cache.
func (a *Adapter) getManagedNamespaceByPool(poolName string) (*db.MdadmManagedNamespaceRow, error) {
	if a.cache != nil {
		if r, ok := a.cache.getMdadmNsByPool(poolName); ok {
			rr := r
			return &rr, nil
		}
		return nil, db.ErrNotFound
	}
	return a.store.GetMdadmManagedNamespaceByPool(poolName)
}

// listManagedNamespaces returns all managed namespaces, preferring the
// cache.
func (a *Adapter) listManagedNamespaces() ([]db.MdadmManagedNamespaceRow, error) {
	if a.cache != nil {
		return a.cache.listMdadmNs(), nil
	}
	return a.store.ListMdadmManagedNamespaces()
}

// upsertManagedNamespace writes through the cache. With a nil cache
// the call falls through to the store.
func (a *Adapter) upsertManagedNamespace(row *db.MdadmManagedNamespaceRow) error {
	if a.cache != nil {
		a.cache.upsertMdadmNs(row)
		return nil
	}
	return a.store.UpsertMdadmManagedNamespace(row)
}

// deleteManagedNamespace writes through the cache.
func (a *Adapter) deleteManagedNamespace(namespaceID string) error {
	if a.cache != nil {
		a.cache.deleteMdadmNs(namespaceID)
		return nil
	}
	return a.store.DeleteMdadmManagedNamespace(namespaceID)
}

// SetMetaStore registers the per-pool metadata store opened at startup.
// The store lives on the pool's fastest tier backing and replaces the
// synchronous managed_objects SQLite write that used to sit on the smoothfs
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
	rec, _, _ := store.Get(inode, tierRank)
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
	_ = store.Put(inode, tierRank, rec)
}

// resolveObjectByPath locates a file in a namespace's backing tiers by
// probing each tier (fastest first) until one contains it. Returns the
// pool name, tier rank, and backing inode. Used by the pin API and other
// path-oriented lookups that need to reach the meta store.
func (a *Adapter) resolveObjectByPath(namespaceID, key string) (poolName string, tierRank int, inode uint64, err error) {
	mdNs, err := a.getManagedNamespace(namespaceID)
	if err != nil || mdNs == nil {
		return "", 0, 0, fmt.Errorf("namespace %q not found", namespaceID)
	}
	key = strings.TrimPrefix(key, "/")

	targets, err := a.listManagedTargets()
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
		tt, err := a.getTierTargetByBackingRef(
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
	rec, ok, err := store.Get(inode, tierRank)
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
	store.PutBlocking(inode, tierRank, rec)
	return nil
}

// FileEntry is a single entry returned by ListNamespaceFiles.
type FileEntry struct {
	Path     string `json:"path"` // namespace-relative path
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
	mn, err := a.getManagedNamespace(namespaceID)
	if err != nil || mn == nil {
		return nil, fmt.Errorf("namespace %q not found", namespaceID)
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	targets, err := a.listManagedTargets()
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
		tt, err := a.getTierTargetByBackingRef(
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
				if rec, have, _ := store.Get(st.Ino, rt.rank); have {
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
	poolName, tierRank, inode, err := a.resolveObjectByPath(namespaceID, key)
	if err != nil {
		return "", err
	}
	store := a.metaStoreFor(poolName)
	if store == nil {
		return "none", nil
	}
	rec, ok, err := store.Get(inode, tierRank)
	if err != nil {
		return "", err
	}
	if !ok {
		return "none", nil
	}
	return pinStateFromMeta(rec.PinState), nil
}

// ---------------------------------------------------------------------------
// CreateTarget — per-tier LV provisioning for smoothfs-backed pools
// ---------------------------------------------------------------------------

// CreateTarget provisions a per-tier LV for a smoothfs-backed mdadm pool.
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
	existingTT, _ := a.getTierTargetByBackingRef(bref, BackendKind)
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
	if err := a.createTierTarget(row); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "create tier_target row",
			Cause:   err,
		}
	}

	// Record in mdadm_managed_targets.
	if err := a.upsertManagedTarget(&db.MdadmManagedTargetRow{
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
	// Check if this is a smoothfs-backed target.
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
		_ = a.deleteManagedTarget(targetID)
		return nil
	}

	return &tiering.AdapterError{
		Kind:    tiering.ErrCapabilityViolation,
		Message: "mdadm tier targets are destroyed via the tier management API",
	}
}

// ListTargets returns one TargetState per assigned tier slot across all pools.
// Empty slots are omitted. Also includes smoothfs-backed targets.
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

	// smoothfs-backed targets from mdadm_managed_targets.
	managedTargets, err := a.listManagedTargets()
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
// CreateNamespace — per-pool namespace record for smoothfs-backed pools
// ---------------------------------------------------------------------------

// CreateNamespace records a per-tier-managed namespace for an mdadm pool.
// The data-plane filesystem (smoothfs) is mounted separately by the
// smoothfs service via its systemd mount unit; this method only persists
// the control-plane rows.
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
	pool, err := a.store.GetTierInstance(poolName)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get tier pool",
			Cause:   err,
		}
	}
	slots, err := a.store.ListTierSlots(poolName)
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier slots",
			Cause:   err,
		}
	}
	if err := a.ensureSmoothfsPoolMount(*pool, slots); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "ensure smoothfs mount",
			Cause:   err,
		}
	}

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

	if err := a.upsertManagedNamespace(&db.MdadmManagedNamespaceRow{
		NamespaceID: namespaceID,
		PoolName:    poolName,
		MountPath:   mountPath,
	}); err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "upsert mdadm_managed_namespace",
			Cause:   err,
		}
	}

	return &tiering.NamespaceState{
		ID:             namespaceID,
		Health:         "healthy",
		PlacementState: "placed",
		BackendRef:     bref,
	}, nil
}

// DestroyNamespace tears down a managed namespace and all of its backing
// tier targets. Steps:
//  1. Every per-tier backing LVM mount is lazy-unmounted.
//  2. The backing LV, VG, and PV labels are removed for each target.
//  3. The namespace and target rows are deleted from the database.
func (a *Adapter) DestroyNamespace(namespaceID string) error {
	mn, err := a.getManagedNamespace(namespaceID)
	if err != nil || mn == nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: fmt.Sprintf("namespace %q not found", namespaceID),
		}
	}

	// 1–2. Tear down every backing target for this pool.
	targets, err := a.listManagedTargets()
	if err != nil {
		log.Printf("mdadm: DestroyNamespace %s: list targets: %v", namespaceID, err)
	}
	for i := range targets {
		mt := &targets[i]
		if mt.PoolName != mn.PoolName {
			continue
		}
		if lvm.IsMounted(mt.MountPath) {
			if err := lvm.LazyUnmount(mt.MountPath); err != nil {
				log.Printf("mdadm: DestroyNamespace %s: lazy unmount %s: %v",
					namespaceID, mt.MountPath, err)
			}
		}
		_ = lvm.RemoveFSTabEntry(mt.VGName, mt.LVName, mt.MountPath)
		_ = lvm.RemoveLV(mt.VGName, mt.LVName)
		_ = lvm.VGRemoveIfEmpty(mt.VGName)
		_ = a.deleteManagedTarget(mt.TierTargetID)
	}

	// 3. Remove the namespace record.
	_ = a.deleteManagedNamespace(namespaceID)
	return nil
}

// ListNamespaces returns one NamespaceState per per-tier-managed namespace.
func (a *Adapter) ListNamespaces() ([]tiering.NamespaceState, error) {
	var out []tiering.NamespaceState

	managedNS, err := a.listManagedNamespaces()
	if err != nil {
		return nil, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list mdadm managed namespaces",
			Cause:   err,
		}
	}
	for _, mn := range managedNS {
		out = append(out, tiering.NamespaceState{
			ID:             mn.NamespaceID,
			Health:         "healthy",
			PlacementState: "placed",
			BackendRef:     backingRefManagedNamespace(mn.PoolName),
		})
	}

	return out, nil
}

// ListManagedObjects returns managed objects for a namespace. Legacy managed
// volumes have been removed; only smoothfs-backed namespaces remain.
func (a *Adapter) ListManagedObjects(namespaceID string) ([]tiering.ManagedObjectState, error) {
	// smoothfs-backed namespaces do not expose sub-objects.
	if _, err := a.getManagedNamespace(namespaceID); err == nil {
		return nil, nil
	}
	return nil, &tiering.AdapterError{
		Kind:    tiering.ErrPermanent,
		Message: fmt.Sprintf("namespace %q not found", namespaceID),
	}
}

// GetCapabilities returns the static capabilities for the named mdadm target.
func (a *Adapter) GetCapabilities(targetID string) (tiering.TargetCapabilities, error) {
	// Check if this is a smoothfs-backed target first.
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
	slowestRank, err := a.slowestTierRank(poolName)
	if err != nil {
		return tiering.TargetPolicy{}, &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier slots",
			Cause:   err,
		}
	}
	return tiering.TargetPolicy{
		TargetFillPct:    effectiveTargetFillPct(slot.Rank, slot.TargetFillPct, slot.FullThresholdPct, slowestRank),
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
	slot, err := a.store.GetTierSlot(poolName, slotName)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return &tiering.AdapterError{
				Kind:    tiering.ErrPermanent,
				Message: fmt.Sprintf("tier slot %q not found in pool %q", slotName, poolName),
			}
		}
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "get tier slot",
			Cause:   err,
		}
	}
	slowestRank, err := a.slowestTierRank(poolName)
	if err != nil {
		return &tiering.AdapterError{
			Kind:    tiering.ErrTransient,
			Message: "list tier slots",
			Cause:   err,
		}
	}
	targetFillPct := policy.TargetFillPct
	if slot.Rank == slowestRank {
		targetFillPct = policy.FullThresholdPct
	} else if targetFillPct >= policy.FullThresholdPct {
		return &tiering.AdapterError{
			Kind:    tiering.ErrPermanent,
			Message: "target_fill_pct must be less than full_threshold_pct",
		}
	}
	if err := a.store.SetTierSlotFill(poolName, slotName, targetFillPct, policy.FullThresholdPct); err != nil {
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
// Reconcile — syncs native + managed state into control-plane tables
// ---------------------------------------------------------------------------

// Reconcile syncs the native mdadm tier state into the unified control-plane
// tables. For each assigned tier slot it upserts a tier_target; for each
// managed volume it upserts a managed_namespace. Also discovers
// mdadm_managed_targets/namespaces for smoothfs-backed pools.
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
	pools = a.reconcileEligiblePools(pools)

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

			existing, lookupErr := a.getTierTargetByBackingRef(bref, BackendKind)
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
				if err := a.createTierTarget(row); err != nil {
					return &tiering.AdapterError{
						Kind:    tiering.ErrTransient,
						Message: "create tier target",
						Cause:   err,
					}
				}
			} else if existing.Health != health {
				if err := a.updateTierTargetActivity(
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
	// These rows map tier_targets to their VG/LV/mount so the smoothfs kernel module's
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
			tt, err := a.getTierTargetByBackingRef(bref, BackendKind)
			if err != nil {
				continue // no tier_target row yet
			}
			if _, err := a.store.GetTierTarget(tt.ID); errors.Is(err, db.ErrNotFound) {
				if a.cache != nil {
					a.cache.deleteTierTarget(tt.ID)
				}
				row := &db.TierTargetRow{
					Name:             slot.Name,
					PlacementDomain:  pool.Name,
					BackendKind:      BackendKind,
					Rank:             slot.Rank,
					TargetFillPct:    slot.TargetFillPct,
					FullThresholdPct: slot.FullThresholdPct,
					Health:           slotHealthToUnified(slot.State),
					BackingRef:       bref,
					CapabilitiesJSON: legacyCapabilitiesJSON,
				}
				if err := a.createTierTarget(row); err != nil {
					log.Printf("mdadm reconcile: recreate target for %s/%s: %v", pool.Name, slot.Name, err)
					continue
				}
				tt = row
			} else if err != nil {
				log.Printf("mdadm reconcile: verify target for %s/%s: %v", pool.Name, slot.Name, err)
				continue
			}
			vgName := fmt.Sprintf("tier-%s-%s", pool.Name, slot.Name)
			mountPath := filepath.Join("/mnt/.tierd-backing", pool.Name, slot.Name)
			log.Printf("mdadm reconcile: creating managed target for %s/%s", pool.Name, slot.Name)
			_ = a.upsertManagedTarget(&db.MdadmManagedTargetRow{
				TierTargetID: tt.ID,
				PoolName:     pool.Name,
				TierName:     slot.Name,
				VGName:       vgName,
				LVName:       "data",
				MountPath:    mountPath,
			})
		}
	}

	// Sync smoothfs-backed targets: update health based on mount status.
	managedTargets, err := a.listManagedTargets()
	if err == nil {
		for _, mt := range managedTargets {
			health := "healthy"
			if !managedTargetIsMounted(mt.MountPath) {
				health = "degraded"
			}
			_ = a.updateTierTargetActivity(mt.TierTargetID, health, "", "")
		}
	}

	// Ensure smoothfs-backed namespaces exist for pools that have
	// at least one assigned tier slot. This handles pools provisioned before
	// the auto-create logic, or where the namespace was lost.
	for _, pool := range pools {
		if pool.State != db.TierPoolStateHealthy {
			continue
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
		if err := a.ensureSmoothfsPoolMount(pool, slots); err != nil {
			log.Printf("mdadm reconcile: ensure smoothfs mount for pool %q: %v", pool.Name, err)
			continue
		}
		bref := backingRefManagedNamespace(pool.Name)
		if _, err := a.store.GetManagedNamespaceByBackingRef(bref, BackendKind); err == nil {
			continue // namespace already exists
		}
		log.Printf("mdadm reconcile: auto-creating namespace for pool %q", pool.Name)
		if _, nsErr := a.CreateNamespace(tiering.NamespaceSpec{
			Name:            pool.Name,
			PlacementDomain: pool.Name,
			NamespaceKind:   "filespace",
			ExposedPath:     filepath.Join("/mnt", pool.Name),
		}); nsErr != nil {
			log.Printf("mdadm reconcile: failed to create namespace for pool %q: %v", pool.Name, nsErr)
		}
	}

	return a.syncDegradedStates(pools)
}

func (a *Adapter) reconcileEligiblePools(pools []db.TierInstance) []db.TierInstance {
	out := make([]db.TierInstance, 0, len(pools))
	for _, pool := range pools {
		decision, _, err := spindown.DecisionFor(a.store, spindown.PoolEnabledKey(pool.Name), spindown.PoolWindowsKey(pool.Name), time.Now())
		if err != nil {
			log.Printf("mdadm reconcile: spindown policy for %s: %v", pool.Name, err)
			continue
		}
		if !decision.Allowed {
			log.Printf("mdadm reconcile: skipping spindown pool %s outside active window; next_active_at=%s",
				pool.Name, decision.NextActiveAt)
			continue
		}
		out = append(out, pool)
	}
	return out
}

// syncDegradedStates clears all mdadm degraded-state rows and re-emits
// current signals from pool/slot health, targets, and region migration failures.
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

	// smoothfs-backed target degraded states.
	managedTargets, _ := a.listManagedTargets()
	for _, mt := range managedTargets {
		if !managedTargetIsMounted(mt.MountPath) {
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
	return nil
}

// ---------------------------------------------------------------------------
// Activity collection
// ---------------------------------------------------------------------------

// CollectActivity returns activity samples. Legacy managed-volume region
// sampling has been removed; smoothfs-backed namespaces do not yet emit activity.
func (a *Adapter) CollectActivity() ([]tiering.ActivitySample, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Movement planning and execution
// ---------------------------------------------------------------------------

// PlanMovements returns movement plans. Legacy managed-volume region planning
// has been removed; smoothfs-backed namespaces use object-level movement.
func (a *Adapter) PlanMovements() ([]tiering.MovementPlan, error) {
	return nil, nil
}

// StartMovement creates a movement_jobs control-plane row for the given plan.
func (a *Adapter) StartMovement(plan tiering.MovementPlan) (string, error) {
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
	ns, err := a.getManagedNamespace(namespaceID)
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
	ns, err := a.getManagedNamespace(namespaceID)
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
