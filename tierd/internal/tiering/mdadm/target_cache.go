package mdadm

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// drainTick is the upper bound on persistence lag in write-back mode.
// A dirty write is guaranteed to hit SQLite within this window (usually
// much sooner — any enqueue signals the drain goroutine immediately).
// Kept short because the user specifically asked for "drain to the
// underlying SQL asap".
const drainTick = 10 * time.Millisecond

// ControlPlaneKeyCacheWriteThrough is the control_plane_config key
// that forces the target cache into write-through mode. Accepted
// values: "true" / "1" / "yes" / "on" enable write-through; anything
// else (empty, "false") leaves the cache in its default write-back
// mode.
//
// Write-back (default): every write updates the in-memory view
// immediately and enqueues a replay op for a background goroutine to
// persist to SQLite. Reads never block on disk, which is critical for
// the metadata hot path. Trade-off is a crash window — an op
// between cache-write and drain is lost.
//
// Write-through: every write updates the in-memory view and persists
// to SQLite synchronously on the caller's goroutine before returning.
// There is no crash window, but every write pays a SQL round-trip so
// the metadata hot path regains the serialisation cost that this cache was
// built to avoid. Offered as an advanced option when durability
// matters more than metadata throughput.
const ControlPlaneKeyCacheWriteThrough = "target_cache.write_through"

type cacheWriteMode int

const (
	writeBack cacheWriteMode = iota
	writeThrough
)

// targetCache is a write-back cache that fronts the
// mdadm_managed_targets, tier_targets, and mdadm_managed_namespaces
// tables. Reads are pure in-memory map lookups, which is the hot path
// for every metadata operation (HandleOpen / HandleLookup /
// HandleMkdir / …). Writes update the in-memory view immediately and
// enqueue a replay op for a background goroutine that drains the queue
// into SQLite.
//
// In write-through mode the drain step collapses back into the caller
// — see ControlPlaneKeyCacheWriteThrough.
//
// Crash window (write-back only): if tierd dies between a cache write
// and its drain, the op is lost. The tables describe the
// logical-to-physical tier mapping which the reconciler re-derives
// from LVM and the filesystem on startup, so the blast radius is an
// extra reconcile cycle rather than data loss.
type targetCache struct {
	store     *db.Store
	writeMode cacheWriteMode

	// Read-side state guarded by mu. All read APIs take RLock; write
	// APIs take Lock while mutating maps and then enqueue a drain op
	// (or persist synchronously in write-through mode).
	mu               sync.RWMutex
	mdadmByID        map[string]db.MdadmManagedTargetRow
	mdadmByPoolTier  map[poolTierKey]db.MdadmManagedTargetRow
	tierByID         map[string]db.TierTargetRow
	tierByBackingRef map[backingRefKey]db.TierTargetRow
	nsByID           map[string]db.MdadmManagedNamespaceRow
	nsByPool         map[string]db.MdadmManagedNamespaceRow

	// Drain-side state. queue is append-only between drains; drainLoop
	// swaps it for a fresh slice under drainMu and replays ops to the
	// store without holding the read lock. Unused in write-through mode.
	drainMu sync.Mutex
	queue   []cacheOp
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
}

type poolTierKey struct{ pool, tier string }
type backingRefKey struct{ kind, ref string }

type cacheOpKind int

const (
	opUpsertMdadm cacheOpKind = iota
	opDeleteMdadm
	opCreateTier
	opDeleteTier
	opUpdatePolicy
	opUpdateActivity
	opUpsertMdadmNs
	opDeleteMdadmNs
)

type cacheOp struct {
	kind     cacheOpKind
	mdadm    *db.MdadmManagedTargetRow
	mdadmID  string
	tier     *db.TierTargetRow
	tierID   string
	policy   *policyUpdate
	activity *activityUpdate
	ns       *db.MdadmManagedNamespaceRow
	nsID     string
}

type policyUpdate struct {
	id                              string
	targetFillPct, fullThresholdPct int
}

type activityUpdate struct {
	id, health, activityBand, activityTrend string
}

// newTargetCache loads the cached tables from the store, reads the
// write-mode toggle, and starts the drain goroutine. Returns the cache
// ready for reads and writes.
func newTargetCache(store *db.Store) (*targetCache, error) {
	c := &targetCache{
		store:            store,
		mdadmByID:        map[string]db.MdadmManagedTargetRow{},
		mdadmByPoolTier:  map[poolTierKey]db.MdadmManagedTargetRow{},
		tierByID:         map[string]db.TierTargetRow{},
		tierByBackingRef: map[backingRefKey]db.TierTargetRow{},
		nsByID:           map[string]db.MdadmManagedNamespaceRow{},
		nsByPool:         map[string]db.MdadmManagedNamespaceRow{},
		wake:             make(chan struct{}, 1),
		stop:             make(chan struct{}),
		stopped:          make(chan struct{}),
	}
	c.writeMode = resolveWriteMode(store)
	if err := c.reload(); err != nil {
		return nil, err
	}
	go c.drainLoop()
	return c, nil
}

// resolveWriteMode reads the write-mode toggle from
// control_plane_config. Defaults to write-back; switches to
// write-through only if the configured value is truthy. A lookup
// failure also defaults to write-back — the toggle is a durability
// knob, not a correctness one.
func resolveWriteMode(store *db.Store) cacheWriteMode {
	v, err := store.GetControlPlaneConfig(ControlPlaneKeyCacheWriteThrough)
	if err != nil {
		return writeBack
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return writeThrough
	}
	return writeBack
}

// reload drops the in-memory view and re-reads the backing tables from
// the store. Used on startup; callers may re-invoke after an external
// migration that mutated rows behind the cache's back.
func (c *targetCache) reload() error {
	mdadms, err := c.store.ListMdadmManagedTargets()
	if err != nil {
		return err
	}
	tiers, err := c.store.ListTierTargets()
	if err != nil {
		return err
	}
	namespaces, err := c.store.ListMdadmManagedNamespaces()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mdadmByID = make(map[string]db.MdadmManagedTargetRow, len(mdadms))
	c.mdadmByPoolTier = make(map[poolTierKey]db.MdadmManagedTargetRow, len(mdadms))
	for _, r := range mdadms {
		c.mdadmByID[r.TierTargetID] = r
		c.mdadmByPoolTier[poolTierKey{r.PoolName, r.TierName}] = r
	}
	c.tierByID = make(map[string]db.TierTargetRow, len(tiers))
	c.tierByBackingRef = make(map[backingRefKey]db.TierTargetRow, len(tiers))
	for _, r := range tiers {
		c.tierByID[r.ID] = r
		if r.BackingRef != "" {
			c.tierByBackingRef[backingRefKey{r.BackendKind, r.BackingRef}] = r
		}
	}
	c.nsByID = make(map[string]db.MdadmManagedNamespaceRow, len(namespaces))
	c.nsByPool = make(map[string]db.MdadmManagedNamespaceRow, len(namespaces))
	for _, r := range namespaces {
		c.nsByID[r.NamespaceID] = r
		c.nsByPool[r.PoolName] = r
	}
	return nil
}

// ---- Read API ----

// listMdadmTargets returns a snapshot of every mdadm managed target.
// The slice is a copy — callers can iterate without holding any lock.
func (c *targetCache) listMdadmTargets() []db.MdadmManagedTargetRow {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]db.MdadmManagedTargetRow, 0, len(c.mdadmByID))
	for _, r := range c.mdadmByID {
		out = append(out, r)
	}
	return out
}

// getMdadmByPoolTier returns the managed target for a pool/tier pair.
func (c *targetCache) getMdadmByPoolTier(pool, tier string) (db.MdadmManagedTargetRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.mdadmByPoolTier[poolTierKey{pool, tier}]
	return r, ok
}

// getMdadmByID returns the managed target for a tier_target_id.
func (c *targetCache) getMdadmByID(id string) (db.MdadmManagedTargetRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.mdadmByID[id]
	return r, ok
}

// getTier returns a tier_target row by ID.
func (c *targetCache) getTier(id string) (db.TierTargetRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.tierByID[id]
	return r, ok
}

// getTierByBackingRef returns the tier_target for a backing ref +
// backend kind (the hot lookup used by every HandleOpen to resolve
// rank from a managed target row).
func (c *targetCache) getTierByBackingRef(ref, kind string) (db.TierTargetRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.tierByBackingRef[backingRefKey{kind, ref}]
	return r, ok
}

// getMdadmNs returns the managed namespace for a namespace_id. This is
// the hottest read in the smoothfs module — HandleOpen / HandleLookup /
// HandleMkdir all resolve the namespace first to find the backing
// mount.
func (c *targetCache) getMdadmNs(namespaceID string) (db.MdadmManagedNamespaceRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.nsByID[namespaceID]
	return r, ok
}

// getMdadmNsByPool returns the managed namespace for a pool name.
func (c *targetCache) getMdadmNsByPool(poolName string) (db.MdadmManagedNamespaceRow, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.nsByPool[poolName]
	return r, ok
}

// listMdadmNs returns a snapshot of every managed namespace.
func (c *targetCache) listMdadmNs() []db.MdadmManagedNamespaceRow {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]db.MdadmManagedNamespaceRow, 0, len(c.nsByID))
	for _, r := range c.nsByID {
		out = append(out, r)
	}
	return out
}

// ---- Write API ----

func (c *targetCache) upsertMdadmTarget(row *db.MdadmManagedTargetRow) {
	c.mu.Lock()
	c.mdadmByID[row.TierTargetID] = *row
	c.mdadmByPoolTier[poolTierKey{row.PoolName, row.TierName}] = *row
	c.mu.Unlock()
	rowCopy := *row
	c.persist(cacheOp{kind: opUpsertMdadm, mdadm: &rowCopy})
}

func (c *targetCache) deleteMdadmTarget(tierTargetID string) {
	c.mu.Lock()
	if r, ok := c.mdadmByID[tierTargetID]; ok {
		delete(c.mdadmByID, tierTargetID)
		delete(c.mdadmByPoolTier, poolTierKey{r.PoolName, r.TierName})
	}
	c.mu.Unlock()
	c.persist(cacheOp{kind: opDeleteMdadm, mdadmID: tierTargetID})
}

// createTierTarget caches a new tier_target and persists it. The row's
// ID must be assigned before the cache stores it — otherwise
// reads-by-backing-ref return an empty ID and any mdadm_managed_targets
// upsert keyed off that ID fails the FK at drain time. Mirrors the
// ID-assignment that store.CreateTierTarget does, so callers see the
// generated ID on their row pointer exactly as with the store path.
func (c *targetCache) createTierTarget(row *db.TierTargetRow) error {
	if row.ID == "" {
		id, err := db.NewControlPlaneID()
		if err != nil {
			return err
		}
		row.ID = id
	}
	c.mu.Lock()
	c.tierByID[row.ID] = *row
	if row.BackingRef != "" {
		c.tierByBackingRef[backingRefKey{row.BackendKind, row.BackingRef}] = *row
	}
	c.mu.Unlock()
	rowCopy := *row
	c.persist(cacheOp{kind: opCreateTier, tier: &rowCopy})
	return nil
}

func (c *targetCache) deleteTierTarget(id string) {
	c.mu.Lock()
	if r, ok := c.tierByID[id]; ok {
		delete(c.tierByID, id)
		if r.BackingRef != "" {
			delete(c.tierByBackingRef, backingRefKey{r.BackendKind, r.BackingRef})
		}
	}
	c.mu.Unlock()
	c.persist(cacheOp{kind: opDeleteTier, tierID: id})
}

func (c *targetCache) updateTierTargetPolicy(id string, targetFill, fullThresh int) {
	c.mu.Lock()
	if r, ok := c.tierByID[id]; ok {
		r.TargetFillPct = targetFill
		r.FullThresholdPct = fullThresh
		c.tierByID[id] = r
		if r.BackingRef != "" {
			c.tierByBackingRef[backingRefKey{r.BackendKind, r.BackingRef}] = r
		}
	}
	c.mu.Unlock()
	c.persist(cacheOp{kind: opUpdatePolicy, policy: &policyUpdate{id, targetFill, fullThresh}})
}

// updateTierTargetActivity mirrors store.UpdateTierTargetActivity: empty
// band/trend mean "leave unchanged". Health is always written.
func (c *targetCache) updateTierTargetActivity(id, health, band, trend string) {
	c.mu.Lock()
	if r, ok := c.tierByID[id]; ok {
		r.Health = health
		if band != "" {
			r.ActivityBand = band
		}
		if trend != "" {
			r.ActivityTrend = trend
		}
		c.tierByID[id] = r
		if r.BackingRef != "" {
			c.tierByBackingRef[backingRefKey{r.BackendKind, r.BackingRef}] = r
		}
	}
	c.mu.Unlock()
	c.persist(cacheOp{kind: opUpdateActivity, activity: &activityUpdate{id, health, band, trend}})
}

func (c *targetCache) upsertMdadmNs(row *db.MdadmManagedNamespaceRow) {
	c.mu.Lock()
	c.nsByID[row.NamespaceID] = *row
	c.nsByPool[row.PoolName] = *row
	c.mu.Unlock()
	rowCopy := *row
	c.persist(cacheOp{kind: opUpsertMdadmNs, ns: &rowCopy})
}

func (c *targetCache) deleteMdadmNs(namespaceID string) {
	c.mu.Lock()
	if r, ok := c.nsByID[namespaceID]; ok {
		delete(c.nsByID, namespaceID)
		delete(c.nsByPool, r.PoolName)
	}
	c.mu.Unlock()
	c.persist(cacheOp{kind: opDeleteMdadmNs, nsID: namespaceID})
}

// ---- Persistence ----

// persist routes a write op to its configured persistence path. In
// write-back mode it enqueues for the drain goroutine. In write-through
// mode it applies synchronously to SQLite before returning.
func (c *targetCache) persist(op cacheOp) {
	if c.writeMode == writeThrough {
		c.applyOp(op)
		return
	}
	c.enqueue(op)
}

func (c *targetCache) enqueue(op cacheOp) {
	c.drainMu.Lock()
	c.queue = append(c.queue, op)
	c.drainMu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// drainLoop runs until Close. It wakes on enqueue signals and on a
// periodic ticker so stragglers never sit in memory for more than
// drainTick. Not used in write-through mode (the queue stays empty),
// but the loop runs anyway so Close's shutdown path is the same for
// both modes.
func (c *targetCache) drainLoop() {
	defer close(c.stopped)
	ticker := time.NewTicker(drainTick)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			c.drain()
			return
		case <-c.wake:
			c.drain()
		case <-ticker.C:
			c.drain()
		}
	}
}

func (c *targetCache) drain() {
	c.drainMu.Lock()
	if len(c.queue) == 0 {
		c.drainMu.Unlock()
		return
	}
	batch := c.queue
	c.queue = nil
	c.drainMu.Unlock()

	for _, op := range batch {
		c.applyOp(op)
	}
}

// applyOp persists a single op to SQLite. Shared between write-through
// (called inline by persist) and write-back (called by drain for each
// queued op).
func (c *targetCache) applyOp(op cacheOp) {
	var err error
	switch op.kind {
	case opUpsertMdadm:
		err = c.store.UpsertMdadmManagedTarget(op.mdadm)
	case opDeleteMdadm:
		err = c.store.DeleteMdadmManagedTarget(op.mdadmID)
	case opCreateTier:
		err = c.store.CreateTierTarget(op.tier)
	case opDeleteTier:
		err = c.store.DeleteTierTarget(op.tierID)
	case opUpdatePolicy:
		err = c.store.UpdateTierTargetPolicy(op.policy.id, op.policy.targetFillPct, op.policy.fullThresholdPct)
	case opUpdateActivity:
		err = c.store.UpdateTierTargetActivity(op.activity.id, op.activity.health, op.activity.activityBand, op.activity.activityTrend)
	case opUpsertMdadmNs:
		err = c.store.UpsertMdadmManagedNamespace(op.ns)
	case opDeleteMdadmNs:
		err = c.store.DeleteMdadmManagedNamespace(op.nsID)
	}
	if err != nil {
		log.Printf("target_cache: persist op %d: %v", op.kind, err)
	}
}

// Close stops the drain goroutine after flushing any remaining ops.
func (c *targetCache) Close() {
	select {
	case <-c.stop:
		// already closed
		return
	default:
		close(c.stop)
	}
	<-c.stopped
}
