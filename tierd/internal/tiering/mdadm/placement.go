package mdadm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	diskpkg "github.com/JBailes/SmoothNAS/tierd/internal/disk"
	mdadmraid "github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

// placementInterval is how often the planner runs per pool. Short enough
// that new pins move quickly, long enough not to thrash under steady load.
const placementInterval = 2 * time.Minute

// heatDecayEvery counts placement cycles between heat-decay passes. At 2
// min per cycle, 30 cycles = 1 hour — long enough that short-lived bursts
// don't evaporate, short enough that an old hot file cools within a day.
const heatDecayEvery = 30

// sizeBucketStep is the multiplicative size ratio that moves a file one
// tier slower under the pure-size heuristic. Every 16× in size demotes
// one rank. Used as a starting preference for unpinned admissions — the
// bin-packer still prefers higher tiers when capacity allows, so a large
// file on an empty fastest tier still lands there; the bucket only
// decides the order in which tiers are *attempted*.
const sizeBucketStep = 16

// sizeBucketBaseBytes is the ceiling for the fastest-tier bucket. Files
// under this size never drop below the fastest tier on size alone.
const sizeBucketBaseBytes int64 = 1 << 20 // 1 MB

// placementQuiescentPeriod is how recently a file must NOT have been
// modified before the planner will migrate it. Skipping recently-written
// files prevents copying a partial file off the NVME tier mid-rsync, which
// would truncate or corrupt the destination copy.
const placementQuiescentPeriod = 10 * time.Minute

// StartPlacementPlanner launches a per-pool goroutine that walks tier
// backings on a periodic interval, looks up each file's meta record, and
// migrates pinned files onto the correct tier.
//
// Heat-driven placement is intentionally out of scope for this first cut —
// HeatCounter is collected on every open but not yet consumed. Adding it
// requires a decay + threshold policy, which is its own design question.
func (a *Adapter) StartPlacementPlanner(ctx context.Context) {
	go a.placementLoop(ctx)
}

func (a *Adapter) placementLoop(ctx context.Context) {
	t := time.NewTicker(placementInterval)
	defer t.Stop()
	cycleCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runPlacementCycle(ctx)
			cycleCount++
			if cycleCount%heatDecayEvery == 0 {
				a.decayAllHeat()
			}
		}
	}
}

// decayAllHeat iterates every pool's meta store and halves HeatCounter on
// every record. Prevents HeatCounter from saturating at uint32 max on long-
// lived systems and makes the metric reflect recent activity rather than
// lifetime opens. Writes go through the normal async shard writer, so even
// a 50k-record pool commits in a couple of hundred milliseconds.
func (a *Adapter) decayAllHeat() {
	a.metaMu.RLock()
	stores := make(map[string]*meta.PoolMetaStore, len(a.metaStores))
	for pool, s := range a.metaStores {
		stores[pool] = s
	}
	a.metaMu.RUnlock()

	for pool, s := range stores {
		halved := 0
		_ = s.IterateAll(func(tierRank int, inode uint64, rec meta.Record) error {
			if rec.HeatCounter == 0 {
				return nil
			}
			rec.HeatCounter /= 2
			s.PutBlocking(inode, tierRank, rec)
			halved++
			return nil
		})
		log.Printf("placement: pool %s heat decay halved %d records", pool, halved)
	}
}

func (a *Adapter) runPlacementCycle(ctx context.Context) {
	nss, err := a.listManagedNamespaces()
	if err != nil {
		log.Printf("placement: list namespaces: %v", err)
		return
	}
	for _, ns := range nss {
		if ctx.Err() != nil {
			return
		}
		a.planPoolPlacement(ctx, ns)
	}
}

// rankedPoolTarget pairs a pool's backing target with its rank, which is
// stored on tier_targets (lowest rank = fastest tier).
type rankedPoolTarget struct {
	rank             int
	target           db.MdadmManagedTargetRow
	targetFillPct    int
	fullThresholdPct int
}

// candidate captures the planner's view of one file: where it currently
// lives, how big it is, what the user's pin state says, and how hot the file
// is based on its HeatCounter at scan time.
type candidate struct {
	rel     string
	size    int64
	inode   uint64
	curRank int
	curTarg db.MdadmManagedTargetRow
	pin     meta.PinState
	heat    uint32 // HeatCounter from the meta store; higher = accessed more recently
}

// tierCapacity tracks usage bookkeeping during the planning pass: current
// occupancy in bytes (from statvfs at scan time) plus the soft and hard
// caps in bytes, so the bin-packer can account for admissions without
// re-stat'ing.
type tierCapacity struct {
	totalBytes int64
	usedBytes  int64 // updated by planner as it places files
	targetCap  int64 // target_fill_pct of totalBytes
	fullCap    int64 // full_threshold_pct of totalBytes
	target     db.MdadmManagedTargetRow
}

// admissionCap returns the capacity ceiling the migration planner packs each
// tier to. The planner's target is target_fill_pct (fill%): it packs files
// into a tier up to fill%, assigning anything beyond to a slower tier, so every
// tier converges toward fill% as files shift both up and down.
//
// full_threshold_pct (full%) is NOT used here. full% is the smoothfs write cap:
// the write path admits new writes to a tier until it reaches full%, then
// spills to the next tier. That is a separate mechanism in the smoothfs kernel
// module — the migration planner only ever targets fill%.
func (c *tierCapacity) admissionCap() int64 {
	return c.targetCap
}

type fullFallbackOrder int

const (
	fullFallbackSlowestFirst fullFallbackOrder = iota
	fullFallbackFastestFirst
)

func assignCandidateRanks(cands []candidate, caps map[int]*tierCapacity, ranked []rankedPoolTarget, fastestRank, slowestRank int) ([]int, []bool) {
	assignments := make([]int, len(cands))
	assigned := make([]bool, len(cands))

	// Pass 1: forced (pinned) placements.
	for i, c := range cands {
		switch c.pin {
		case meta.PinHot:
			assignments[i] = admitWithFallbackOrder(caps, ranked, fastestRank, c.size, fullFallbackFastestFirst)
			assigned[i] = true
		case meta.PinCold:
			assignments[i] = admitWithFallback(caps, ranked, slowestRank, c.size)
			assigned[i] = true
		}
	}

	// Pass 2: unpinned, smallest-first fills from the top.
	var unpinned []int
	for i, c := range cands {
		if c.pin == meta.PinNone && !assigned[i] {
			unpinned = append(unpinned, i)
		}
	}
	// Primary sort: heat descending (hottest files compete first for the
	// fastest tier). Secondary sort: size ascending (smaller files are
	// preferred for fast tiers among files of equal heat). admitWithFallback
	// starts every unpinned file at fastestRank and packs each tier to its
	// target_fill_pct (fill%) ceiling — hot/small files win NVME; once a tier
	// reaches fill%, colder/larger files spill to the next slower tier.
	sort.Slice(unpinned, func(i, j int) bool {
		ci, cj := cands[unpinned[i]], cands[unpinned[j]]
		if ci.heat != cj.heat {
			return ci.heat > cj.heat
		}
		return ci.size < cj.size
	})
	for _, idx := range unpinned {
		assignments[idx] = admitWithFallback(caps, ranked, fastestRank, cands[idx].size)
		assigned[idx] = true
	}

	return assignments, assigned
}

// planPoolPlacement gathers every file in a pool and runs a heat+size-aware
// bin-packing pass that determines the ideal tier for each file, then executes
// moves to converge the pool toward that distribution.
//
// Placement policy (canonical design):
//   - Unpinned files are sorted hottest-first (HeatCounter DESC), then
//     smallest-first among equal-heat files. The packer assigns each file to
//     the fastest tier with capacity below target_fill_pct (fill%), spilling
//     to slower tiers as each fills. Hot files win fast-tier capacity first;
//     cold/large files naturally fall to HDD.
//   - fill% (target_fill_pct) is the migration target in both directions.
//     A tier over fill% drains cold/large files toward slower tiers; a tier
//     below fill% pulls hot files up from slower tiers. The bottom tier is the
//     exception — it has nowhere slower to drain to, so it absorbs whatever
//     does not fit above it (filling past fill% via the Pass B full% fallback).
//     full% itself is not a planner concept; it is the smoothfs write cap.
//   - Pinned files force-place: PinHot → fastest tier, PinCold → slowest tier.
//
// If no eligible tier has room below target_fill, the packer falls through
// to full_threshold as a hard cap from the bottom tier upward; files that
// don't fit anywhere stay put.
// tierTargetCapBytes is the byte ceiling the planner packs a tier up to under
// its target_fill_pct. The percentage is honoured verbatim, including 0: a
// target_fill_pct of 0 yields a cap of 0, making the tier a pure write-cache
// that holds no resident data — every unpinned file drains to a slower tier.
// (A previous version coerced 0 to a 50% default, so a "write-cache" tier still
// filled to half capacity.) targetFillPct is validated to [0, 100] at set time.
func tierTargetCapBytes(totalBytes int64, targetFillPct int) int64 {
	if targetFillPct < 0 {
		targetFillPct = 0
	}
	return totalBytes * int64(targetFillPct) / 100
}

// tierFullCapBytes is the hard byte ceiling the planner's Pass B fallback will
// place a tier up to under its full_threshold_pct. Like the target cap the
// percentage is honoured verbatim, including 0: full_threshold_pct of 0 yields
// a cap of 0, so Pass B never strands a file onto that tier either — combined
// with target_fill_pct 0 the planner fully evacuates and keeps the tier empty.
// (A previous version floored this to 95%, so full_threshold below 95 — and 0
// in particular — was ignored.) full_threshold_pct is validated to [0, 100].
func tierFullCapBytes(totalBytes int64, fullThresholdPct int) int64 {
	if fullThresholdPct < 0 {
		fullThresholdPct = 0
	}
	return totalBytes * int64(fullThresholdPct) / 100
}

// plannedMove is one file the packer wants relocated: its index into cands and
// the rank it should end up on.
type plannedMove struct {
	idx  int
	want int
}

// planMoveOrder splits the packer's assignments into the moves that free space
// on a faster tier (demotions) and those that consume it (promotions), so the
// caller can drain before it fills. Files already on their assigned rank, and
// any the packer left unassigned, produce no move.
//
// Rank numbers grow as tiers get slower, so want > curRank is a demotion.
func planMoveOrder(cands []candidate, assignments []int, assigned []bool) (demotions, promotions []plannedMove) {
	for idx, c := range cands {
		if !assigned[idx] || assignments[idx] == c.curRank {
			continue
		}
		m := plannedMove{idx: idx, want: assignments[idx]}
		if m.want > c.curRank {
			demotions = append(demotions, m)
		} else {
			promotions = append(promotions, m)
		}
	}
	return demotions, promotions
}

func (a *Adapter) planPoolPlacement(ctx context.Context, ns db.MdadmManagedNamespaceRow) {
	maintenanceMode, ok := a.poolReadyForSmoothNASMaintenance(ns.PoolName)
	if !ok {
		return
	}
	trackTargetBalance := maintenanceMode == placementMaintenanceSpindownActive
	balanceStatus := spindown.TargetBalanceStatus{}
	if trackTargetBalance {
		now := time.Now().UTC().Format(time.RFC3339)
		balanceStatus = spindown.TargetBalanceStatus{
			Active:    true,
			StartedAt: now,
			CheckedAt: now,
			Reason:    "target-balance placement running",
		}
		_ = spindown.StoreTargetBalanceStatus(a.store, ns.PoolName, balanceStatus)
		defer func() {
			if ctx.Err() != nil && balanceStatus.Reason == "target-balance placement running" {
				balanceStatus.Reason = "target-balance placement canceled"
			}
			if balanceStatus.Reason == "target-balance placement running" {
				balanceStatus.Reason = "target-balance placement complete"
			}
			now := time.Now().UTC().Format(time.RFC3339)
			balanceStatus.Active = false
			balanceStatus.FinishedAt = now
			balanceStatus.CheckedAt = now
			_ = spindown.StoreTargetBalanceStatus(a.store, ns.PoolName, balanceStatus)
		}()
	}
	ranked := a.poolRankedTargets(ns.PoolName)
	if len(ranked) < 2 {
		balanceStatus.Reason = "target-balance placement skipped; pool has fewer than two tiers"
		return
	}
	store := a.metaStoreFor(ns.PoolName)
	if store == nil {
		balanceStatus.Reason = "target-balance placement skipped; metadata store is unavailable"
		return
	}

	// Snapshot per-tier capacity from the filesystem.
	caps := make(map[int]*tierCapacity, len(ranked))
	for _, rt := range ranked {
		var st syscall.Statfs_t
		if err := syscall.Statfs(rt.target.MountPath, &st); err != nil {
			log.Printf("placement: statfs %s: %v", rt.target.MountPath, err)
			balanceStatus.Reason = "target-balance placement skipped; tier capacity unavailable"
			return
		}
		total := int64(st.Blocks) * int64(st.Bsize)
		used := total - int64(st.Bavail)*int64(st.Bsize)
		caps[rt.rank] = &tierCapacity{
			totalBytes: total,
			usedBytes:  used,
			targetCap:  tierTargetCapBytes(total, rt.targetFillPct),
			fullCap:    tierFullCapBytes(total, rt.fullThresholdPct),
			target:     rt.target,
		}
	}

	// Walk every tier and collect candidates. We'll sort+pack below.
	now := time.Now()
	var cands []candidate
	for _, rt := range ranked {
		if ctx.Err() != nil {
			balanceStatus.Reason = "target-balance placement canceled"
			return
		}
		_ = filepath.WalkDir(rt.target.MountPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil {
				if ctx.Err() != nil {
					return filepath.SkipAll
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() && placementExcludedDir(rt.target.MountPath, path, name) {
				return filepath.SkipDir
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if now.Sub(info.ModTime()) < placementQuiescentPeriod {
				return nil
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(rt.target.MountPath, path)
			if err != nil {
				return nil
			}
			rec, _, _ := store.Get(st.Ino, rt.rank)
			cands = append(cands, candidate{
				rel:     rel,
				size:    info.Size(),
				inode:   st.Ino,
				curRank: rt.rank,
				curTarg: rt.target,
				pin:     rec.PinState,
				heat:    rec.HeatCounter,
			})
			return nil
		})
	}

	// caps.usedBytes currently includes every candidate file's bytes —
	// they were counted by statvfs. If we leave them in, admission will
	// double-count: adding each candidate's size on top of a used value
	// that already covers it. Subtract them so caps represents only the
	// data the planner is NOT re-placing (XFS metadata and anything
	// non-regular this walk skipped). Admission then rebuilds the
	// per-tier layout cleanly.
	for _, c := range cands {
		cap, ok := caps[c.curRank]
		if !ok {
			continue
		}
		cap.usedBytes -= c.size
		if cap.usedBytes < 0 {
			cap.usedBytes = 0
		}
	}

	// Place files. Pinned-hot/cold are forced first so their capacity is
	// accounted for in the shared budget. Unpinned files then pack
	// smallest-first onto the fastest tier with room under target.
	fastestRank := ranked[0].rank
	slowestRank := ranked[len(ranked)-1].rank
	assignments, assigned := assignCandidateRanks(cands, caps, ranked, fastestRank, slowestRank)

	moved := 0
	skipped := 0

	// Run every move that FREES fast-tier space before any move that consumes
	// it: demotions (toward a slower tier) first, then promotions.
	//
	// The admission pass above computes a steady-state plan — "this tier should
	// end up holding targetCap bytes" — after subtracting the candidates it is
	// re-placing. Those evicted bytes are still physically resident until their
	// move completes, so the plan is only reachable if the drains happen first.
	// Executing in walk order interleaves the two, and a promotion then lands on
	// a tier still holding everything the plan intends to evict: it fails
	// ENOSPC, and keeps failing every cycle because the next pass re-plans from
	// the same over-full starting point. Ordering is the whole fix — the
	// packer's arithmetic was already correct, just unreachable in that order.
	//
	// Draining first is always safe: a demotion targets a tier the packer had
	// capacity to admit it to, so it cannot be starved by this reordering.
	demotions, promotions := planMoveOrder(cands, assignments, assigned)
	planned := 0

	// planned counts moves ATTEMPTED, not moves plannable — matching the
	// pre-reordering behaviour exactly. Counting the full plan up front would
	// quietly change what balanceStatus reports on cancellation (PendingMoves is
	// derived as planned-moved), and this change is about execution order only.
	for _, m := range append(demotions, promotions...) {
		if ctx.Err() != nil {
			balanceStatus.Reason = "target-balance placement canceled"
			break
		}
		c := cands[m.idx]
		planned++
		dest := caps[m.want]
		if dest == nil {
			skipped++
			continue
		}
		if err := a.moveForPlacement(ns, c.rel, c.curTarg, dest.target, c.curRank, m.want); err != nil {
			log.Printf("placement: move %s %s→rank%d: %v",
				c.rel, c.curTarg.TierName, m.want, err)
			continue
		}
		moved++
	}
	balanceStatus.CandidateCount = len(cands)
	balanceStatus.PlannedMoves = planned
	balanceStatus.PendingMoves = max1(planned-moved, 0)
	balanceStatus.Moved = moved
	balanceStatus.Skipped = skipped
	switch {
	case balanceStatus.PendingMoves > 0:
		balanceStatus.Reason = "target-balance placement has pending moves"
	case planned == 0:
		balanceStatus.CandidateExhausted = true
		balanceStatus.Reason = "target-balance placement exhausted candidates"
	}
	if len(cands) > 0 {
		log.Printf("placement: pool %s scanned=%d moved=%d skipped=%d",
			ns.PoolName, len(cands), moved, skipped)
	}

	// Meta records always live on the same tier as their data file, so
	// no separate meta-eviction step is needed; moveForPlacement updates
	// the meta as part of every successful data move.

	// Per-inode reclaim now happens inline in moveForPlacement via
	// forgetLowerInode: each source copy we remove out-of-band is immediately
	// released from smoothfs's replay pin so the backing frees its blocks. The
	// old pool-wide drop_caches=2 hammer that used to run here could not
	// reclaim these at all — the replay pin holds a reference, so the inode is
	// never cache-idle and drop_caches skips it.
}

func placementExcludedDir(root, path, name string) bool {
	if name == "lost+found" {
		return true
	}
	switch name {
	case ".tierd-meta", ".smoothnas", ".smoothfs", ".plugins":
	default:
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return filepath.Dir(rel) == "."
}

type placementMaintenanceMode int

const (
	placementMaintenanceNormal placementMaintenanceMode = iota
	placementMaintenanceSpindownActive
)

func (a *Adapter) poolReadyForSmoothNASMaintenance(poolName string) (placementMaintenanceMode, bool) {
	enabled, err := spindown.Enabled(a.store, spindown.PoolEnabledKey(poolName))
	if err != nil || !enabled {
		return placementMaintenanceNormal, err == nil
	}
	devices, err := a.poolBackingDevices(poolName)
	if err != nil {
		log.Printf("placement: pool %s spindown backing lookup: %v", poolName, err)
		return placementMaintenanceNormal, false
	}
	if len(devices) == 0 {
		decision, _, err := spindown.DecisionFor(a.store, spindown.PoolEnabledKey(poolName), spindown.PoolWindowsKey(poolName), time.Now())
		if err != nil {
			log.Printf("placement: pool %s spindown policy: %v", poolName, err)
			return placementMaintenanceNormal, false
		}
		if !decision.Allowed {
			log.Printf("placement: pool %s deferred outside active window; next_active_at=%s", poolName, decision.NextActiveAt)
		}
		if decision.Allowed {
			return placementMaintenanceSpindownActive, true
		}
		return placementMaintenanceNormal, false
	}
	blocked, reason := backingDevicesStandbyBlocked(devices)
	if blocked {
		log.Printf("placement: pool %s deferred: %s", poolName, reason)
		return placementMaintenanceNormal, false
	}
	return placementMaintenanceSpindownActive, true
}

func (a *Adapter) poolBackingDevices(poolName string) ([]string, error) {
	slots, err := a.store.ListTierSlots(poolName)
	if err != nil {
		return nil, err
	}
	mdadmMembers := map[string][]string{}
	if arrays, err := mdadmraid.List(); err == nil {
		for _, array := range arrays {
			mdadmMembers[array.Path] = append([]string(nil), array.MemberDisks...)
		}
	}
	seen := map[string]bool{}
	var devices []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		devices = append(devices, path)
	}
	for _, slot := range slots {
		if slot.State == db.TierSlotStateEmpty {
			continue
		}
		if slot.BackingKind == "zfs" {
			for _, dev := range zfs.MemberDevices(slot.BackingRef) {
				add(dev)
			}
			continue
		}
		if slot.PVDevice == nil {
			continue
		}
		if members := mdadmMembers[*slot.PVDevice]; len(members) > 0 {
			for _, dev := range members {
				add(dev)
			}
		} else {
			add(*slot.PVDevice)
		}
	}
	return devices, nil
}

func backingDevicesStandbyBlocked(devices []string) (bool, string) {
	disks, err := diskpkg.List()
	if err != nil {
		return true, "could not list disks to confirm backing HDDs are already active"
	}
	rotational := make(map[string]bool, len(disks))
	for _, d := range disks {
		rotational[diskpkg.BaseDiskPath(d.Path)] = d.Rotational
	}
	for _, device := range devices {
		base := diskpkg.BaseDiskPath(device)
		isRotational, known := rotational[base]
		if !known {
			return true, "could not confirm backing disks are already active"
		}
		if !isRotational {
			continue
		}
		state, err := diskpkg.QueryPowerState(base)
		if err != nil {
			return true, "could not confirm backing HDDs are already active"
		}
		if state == "standby" || state == "sleeping" {
			return true, "backing HDD is in standby; waiting for external activity"
		}
	}
	return false, ""
}

// admitWithFallback finds an eligible tier at or slower than preferredRank
// whose remaining budget (cap - usedBytes) can absorb size. Two passes:
//
//	Pass A — each tier's target_fill_pct is honoured so migration drains
//	  excess data down to slower tiers.
//	Pass B — fall back to full_threshold_pct from the slowest eligible tier
//	  upward. full_threshold_pct is a hard ceiling; target_fill_pct is the
//	  migration target for every tier above the bottom tier.
//
// Returns the rank of the tier that accepted the file, or the preferred
// rank if no admission succeeded (in which case the caller just leaves
// the file where it is — assignments[] becomes a no-op compared to its
// current rank).
func admitWithFallback(caps map[int]*tierCapacity, ranked []rankedPoolTarget, preferredRank int, size int64) int {
	return admitWithFallbackOrder(caps, ranked, preferredRank, size, fullFallbackSlowestFirst)
}

func admitWithFallbackOrder(caps map[int]*tierCapacity, ranked []rankedPoolTarget, preferredRank int, size int64, order fullFallbackOrder) int {
	// Pass A: honour each tier's fill target. "Preferred" is usually fastest,
	// so the scan walks ranks ascending (fastest → slowest) from there.
	for _, rt := range ranked {
		if rt.rank < preferredRank {
			continue
		}
		c := caps[rt.rank]
		if c == nil {
			continue
		}
		if c.usedBytes+size <= c.admissionCap() {
			c.usedBytes += size
			return rt.rank
		}
	}
	// Pass B: admission cap exceeded everywhere from preferred downward.
	// Accept at full_threshold so we don't strand the file, but for normal
	// migration try lower tiers first so upper tiers drain toward target_fill.
	if order == fullFallbackFastestFirst {
		for _, rt := range ranked {
			if r, ok := admitAtFullCap(caps, rt, preferredRank, size); ok {
				return r
			}
		}
	} else {
		for i := len(ranked) - 1; i >= 0; i-- {
			if r, ok := admitAtFullCap(caps, ranked[i], preferredRank, size); ok {
				return r
			}
		}
	}
	return preferredRank
}

func admitAtFullCap(caps map[int]*tierCapacity, rt rankedPoolTarget, preferredRank int, size int64) (int, bool) {
	if rt.rank < preferredRank {
		return 0, false
	}
	c := caps[rt.rank]
	if c == nil {
		return 0, false
	}
	if c.usedBytes+size <= c.fullCap {
		c.usedBytes += size
		return rt.rank, true
	}
	return 0, false
}

func max1(x, floor int) int {
	if x <= 0 {
		return floor
	}
	return x
}

// sizeBucketRank maps a file size onto a tier rank in [fastestRank,
// slowestRank]. Rank moves one slower every sizeBucketStep (16×) in size
// starting from sizeBucketBaseBytes (1 MB).
//
// Example with ranks 1..3 (NVMe / SSD / HDD):
//
//	<1 MB         → 1 (NVMe)
//	1 MB – 16 MB  → 1 (NVMe)
//	16 MB – 256MB → 2 (SSD)
//	≥256 MB       → 3 (HDD)
//
// This is the pure-size bias used to seed the bin-packer's admission
// preference for unpinned files. It is intentionally symmetric so the UI
// and telemetry can report "ideal tier under size bias" without running
// a full planning pass. Capacity-aware admission (admitWithFallback) may
// still promote the file to a higher tier when that tier has room under
// its target_fill — "prefer higher tier when we can fit it".
func sizeBucketRank(sizeBytes int64, fastestRank, slowestRank int) int {
	if sizeBytes < sizeBucketBaseBytes {
		return fastestRank
	}
	units := sizeBytes / sizeBucketBaseBytes
	steps := 0
	for units >= sizeBucketStep {
		units /= sizeBucketStep
		steps++
	}
	r := fastestRank + steps
	if r > slowestRank {
		r = slowestRank
	}
	return r
}

// idealRank is a pure size+pin view of where a file "wants" to live,
// absent capacity pressure. The planner consults it to seed the sort
// order and to display UI hints. Final placement comes from
// admitWithFallback which considers target_fill and full_threshold.
func idealRank(pin meta.PinState, sizeBytes int64, fastestRank, slowestRank int) int {
	switch pin {
	case meta.PinHot:
		return fastestRank
	case meta.PinCold:
		return slowestRank
	}
	return sizeBucketRank(sizeBytes, fastestRank, slowestRank)
}

// poolRankedTargets returns the pool's tier backings sorted by rank
// ascending (fastest first), each annotated with its full-threshold so
// the capacity gate can be applied without an extra DB round-trip.
func (a *Adapter) poolRankedTargets(poolName string) []rankedPoolTarget {
	targets, err := a.listManagedTargets()
	if err != nil {
		log.Printf("placement: list targets: %v", err)
		return nil
	}
	var ranked []rankedPoolTarget
	for i := range targets {
		if targets[i].PoolName != poolName {
			continue
		}
		tt, err := a.getTierTargetByBackingRef(
			backingRefTarget(targets[i].PoolName, targets[i].TierName), BackendKind)
		if err != nil {
			continue
		}
		ranked = append(ranked, rankedPoolTarget{
			rank:             tt.Rank,
			target:           targets[i],
			targetFillPct:    tt.TargetFillPct,
			fullThresholdPct: tt.FullThresholdPct,
		})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })
	return ranked
}

// moveForPlacement copies a file from source tier to dest tier, updates
// the meta record (which now lives on the destination tier instead of
// the source), and unlinks the source.
// filesHaveSameContent reports whether two files hold byte-identical content.
// It streams both files so large ones are compared without buffering them whole,
// and short-circuits on the first differing byte. Callers should compare sizes
// first; this is the authoritative content check used to reconcile a stalled
// tier move whose destination duplicate has an mtime skew.
func filesHaveSameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	const chunk = 128 * 1024
	ba := make([]byte, chunk)
	bb := make([]byte, chunk)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		aDone := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bDone := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if aDone || bDone {
			// Equal only if both streams ended together (same length).
			return aDone == bDone, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}

func (a *Adapter) moveForPlacement(ns db.MdadmManagedNamespaceRow, rel string, src, dst db.MdadmManagedTargetRow, srcRank, destRank int) error {
	if !a.targetMountReady(src) {
		return fmt.Errorf("source tier %s is not mounted", src.TierName)
	}
	if !a.targetMountReady(dst) {
		return fmt.Errorf("destination tier %s is not mounted", dst.TierName)
	}

	srcPath := filepath.Join(src.MountPath, rel)
	dstPath := filepath.Join(dst.MountPath, rel)
	tmpPath := dstPath + ".tierd-move"

	// Ensure dest parent directory exists on the destination tier.
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("mkdir dest parent: %w", err)
	}
	dstInfo, dstErr := os.Lstat(dstPath)
	if dstErr == nil {
		// Destination already exists. This is the expected state after a move
		// that installed the destination but crashed or was interrupted before
		// unlinking the source. When the two files are identical the copy is
		// complete: remove the stale source and update the meta record.
		//
		// Reconcile on size + CONTENT, never mtime. Two reasons:
		//
		//   1. A tier copy does not always preserve the source mtime, so a
		//      byte-identical duplicate can legitimately carry a different
		//      mtime. The old code gated reconciliation on an exact mtime match,
		//      which left such duplicates un-reconcilable; because the source
		//      then stayed on the wrong tier the scanner re-queued the very same
		//      doomed move every cycle — forever. That spun the reconciler
		//      (scanned>0 moved=0), flooded the log, and starved unrelated work
		//      such as image pulls.
		//
		//   2. This reconcile DELETES the source, so trusting a size+mtime match
		//      (which two genuinely different files can share) risked destroying
		//      real data. A content compare is authoritative.
		//
		// A same-size but content-different file at the same path is a genuine
		// conflict: it still errors and both copies are preserved (below).
		srcInfo2, srcErr := os.Stat(srcPath)
		identical := false
		if srcErr == nil && !srcInfo2.IsDir() && dstInfo.Size() == srcInfo2.Size() {
			same, cmpErr := filesHaveSameContent(srcPath, dstPath)
			if cmpErr != nil {
				return fmt.Errorf("compare stalled-move duplicate %s: %w", rel, cmpErr)
			}
			identical = same
		}
		if identical {
			log.Printf("placement: completing stalled move for %s: destination already exists and matches source", rel)
			store2 := a.metaStoreFor(ns.PoolName)
			srcSt2, _ := srcInfo2.Sys().(*syscall.Stat_t)
			var srcRec2 meta.Record
			var hadSrcRec2 bool
			if store2 != nil && srcSt2 != nil {
				srcRec2, hadSrcRec2, _ = store2.Get(srcSt2.Ino, srcRank)
			}
			if err := os.Remove(srcPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("unlink stalled src: %w", err)
			}
			if srcSt2 != nil {
				forgetLowerInode(a, ns.PoolName, src.MountPath, srcSt2.Ino)
			}
			if store2 != nil {
				dstStat, statErr := os.Stat(dstPath)
				if statErr == nil {
					if dstSt, ok := dstStat.Sys().(*syscall.Stat_t); ok {
						rec := meta.Record{
							Version:      meta.RecordVersion,
							NamespaceID:  meta.NamespaceID(ns.NamespaceID),
							TierIdx:      uint8(destRank),
							LastAccessNS: uint64(time.Now().UnixNano()),
						}
						if hadSrcRec2 {
							rec.PinState = srcRec2.PinState
							rec.HeatCounter = srcRec2.HeatCounter
						}
						store2.PutBlocking(dstSt.Ino, destRank, rec)
					}
				}
				if srcSt2 != nil {
					if err := store2.Delete(srcSt2.Ino, srcRank); err != nil {
						log.Printf("placement: delete stalled src meta tier=%d inode=%d: %v", srcRank, srcSt2.Ino, err)
					}
				}
			}
			return nil
		}
		return fmt.Errorf("destination already exists: %s", dstPath)
	} else if !errors.Is(dstErr, os.ErrNotExist) {
		return fmt.Errorf("stat dest: %w", dstErr)
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}

	// Remove any temp file left by an interrupted previous move. copyFileContents
	// uses O_EXCL so it will fail with EEXIST if the temp already exists; the
	// stale file is worthless (the copy never completed) so just clear it now
	// rather than waiting for the next cycle's error-path removal.
	_ = os.Remove(tmpPath)
	srcSt, _ := srcInfo.Sys().(*syscall.Stat_t)

	// Read the source-tier meta record now (before any disk mutation) so
	// we can preserve pin state and heat counter on the destination.
	store := a.metaStoreFor(ns.PoolName)
	var srcRec meta.Record
	var hadSrcRec bool
	if store != nil && srcSt != nil {
		srcRec, hadSrcRec, _ = store.Get(srcSt.Ino, srcRank)
	}

	if err := copyFileContents(srcPath, tmpPath, srcInfo.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy: %w", err)
	}

	// Preserve mtime so subsequent rsyncs don't re-transfer this file.
	if err := os.Chtimes(tmpPath, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		log.Printf("placement: chtimes %s: %v", tmpPath, err)
	}

	if err := installPlacementCopy(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("install tmp copy: %w", err)
	}

	// Source copy is now redundant. Unlinking it here preserves the
	// existing openat-fastest-first semantics in openUnregisteredObject:
	// a subsequent OPEN hits the dest tier.
	if err := os.Remove(srcPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Roll back the installed destination so the source remains the
		// sole copy. The next placement cycle will retry the move cleanly
		// rather than seeing duplicate copies on two tiers.
		_ = os.Remove(dstPath)
		return fmt.Errorf("unlink src after copy: %w", err)
	}
	// We removed the source out-of-band (bypassing the smoothfs VFS), so tell
	// smoothfs to drop the replay pin it holds on that inode; otherwise the
	// backing blocks leak until unmount. Replaces the old drop_caches hammer.
	if srcSt != nil {
		forgetLowerInode(a, ns.PoolName, src.MountPath, srcSt.Ino)
	}

	// Move the meta record from src tier to dest tier. The dest file has
	// its own inode (different filesystem) so we read it post-rename.
	if store != nil {
		dstStat, err := os.Stat(dstPath)
		if err == nil {
			if dstSt, ok := dstStat.Sys().(*syscall.Stat_t); ok {
				rec := meta.Record{
					Version:     meta.RecordVersion,
					NamespaceID: meta.NamespaceID(ns.NamespaceID),
				}
				if hadSrcRec {
					rec.PinState = srcRec.PinState
					rec.HeatCounter = srcRec.HeatCounter
				}
				rec.TierIdx = uint8(destRank)
				rec.LastAccessNS = uint64(time.Now().UnixNano())
				store.PutBlocking(dstSt.Ino, destRank, rec)
			}
		}
		// Clean up the src-tier record. The dst inode is different, so
		// the new dest write doesn't supersede it.
		if srcSt != nil {
			if err := store.Delete(srcSt.Ino, srcRank); err != nil {
				log.Printf("placement: delete src meta tier=%d inode=%d: %v", srcRank, srcSt.Ino, err)
			}
		}
	}
	return nil
}

func installPlacementCopy(tmpPath, dstPath string) error {
	if err := os.Link(tmpPath, dstPath); err != nil {
		return err
	}
	return os.Remove(tmpPath)
}

// forgetLowerInode tells smoothfs to release the replay pin it holds on the
// shadow inode for a lower copy we just removed out-of-band (copy-to-dest +
// os.Remove of the source, bypassing the smoothfs VFS). smoothfs holds a
// path_get() reference to each lower inode via smoothfs_inode_info.lower_path;
// placement/spill/replay inodes are additionally replay-pinned, so their VFS
// refcount is never zero and smoothfs_evict_inode never runs for them. That
// leaves the source's XFS blocks allocated (df >> du) until unmount.
//
// Writing "<tier> <lower_ino>" to the pool's forget_lower sysfs makes smoothfs
// drop exactly that inode's pin and evict it, freeing the backing blocks now.
// This replaces the old pool-wide drop_caches=2 call, which could not reclaim
// these inodes at all: drop_caches only evicts cache-idle inodes, and the
// replay pin keeps a reference so they are never idle.
//
// Best-effort: a missing pool/uuid or a write error just defers reclaim to the
// next opportunity (or unmount); it is never a move failure. Var for test hook.
var forgetLowerInode = func(a *Adapter, poolName string, tierMountPath string, lowerIno uint64) {
	if a == nil || a.store == nil {
		return
	}
	pool, err := a.store.GetSmoothfsPool(poolName)
	if err != nil || pool == nil || pool.UUID == "" {
		return
	}
	tier, ok := smoothfsTierIndex(pool.Tiers, tierMountPath)
	if !ok {
		log.Printf("placement: forget_lower pool=%s tier=%s inode=%d: tier not in pool tiers=%v",
			poolName, tierMountPath, lowerIno, pool.Tiers)
		return
	}
	path := filepath.Join("/sys/fs/smoothfs", pool.UUID, "forget_lower")
	line := fmt.Sprintf("%d %d", tier, lowerIno)
	if err := os.WriteFile(path, []byte(line), 0); err != nil {
		log.Printf("placement: forget_lower pool=%s tier=%d(%s) inode=%d: %v",
			poolName, tier, tierMountPath, lowerIno, err)
	}
}

// smoothfsTierIndex resolves a tier's backing mount path to the tier index the
// smoothfs kernel module uses.
//
// These are NOT the same number as tierd's rank, and conflating them silently
// corrupts reclaim. smoothfs indexes tiers by position in the pool's `tiers=`
// mount option — the colon-joined list stored in smoothfs_pools.tiers, ordered
// fastest-first — so a two-tier pool has indices 0 and 1. tierd's ranks for the
// same pool are 1 and 2. Passing a rank straight through means:
//
//	rank 2 (slowest) -> tier 2 -> rejected, `tier >= ntiers` -> EINVAL
//	rank 1 (fastest) -> tier 1 -> ACCEPTED, but forgets the SECOND tier
//
// The first is loud; the second is silent and worse — the fast tier's source
// inode keeps its pin, so its blocks are never reclaimed and the tier cannot
// drain no matter how many demotions succeed. On the .254 appliance that pinned
// NVMe at 100% (df >> du) and turned every subsequent promotion into ENOSPC:
// ~331k rejected forgets an hour, and every NVMe forget silently aimed at HDD.
//
// Resolving through the stored tier list keeps this correct for any rank
// numbering, contiguous or not.
func smoothfsTierIndex(tiers []string, mountPath string) (int, bool) {
	// The two sides come from different columns — smoothfs_pools.tiers (split on
	// ":") and mdadm_managed_targets.mount_path — so normalise rather than trust
	// them to stay byte-identical. A trailing slash creeping into either would
	// otherwise silently stop all reclaim, which is the same class of quiet
	// failure this function exists to fix.
	want := filepath.Clean(mountPath)
	for i, t := range tiers {
		if filepath.Clean(t) == want {
			return i, true
		}
	}
	return 0, false
}

// copyFileContents opens src for read and dst for exclusive create-write,
// streams the bytes, and closes both.
func copyFileContents(src, dst string, mode os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return err
	}
	return df.Close()
}
