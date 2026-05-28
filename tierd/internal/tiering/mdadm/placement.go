package mdadm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/bits"
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
// lives, how big it is, how hot it is, and what the user's pin state says.
type candidate struct {
	rel     string
	size    int64
	inode   uint64
	curRank int
	curTarg db.MdadmManagedTargetRow
	pin     meta.PinState
	heat    uint32 // HeatCounter from the meta record; 0 = cold
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

// admissionCap returns the threshold at which this tier is considered full
// and files must go to a slower tier. A file goes down only when placing it
// here would exceed full_threshold_pct — not target_fill_pct. target_fill_pct
// is the long-term equilibrium reached as files cool over many migration
// cycles; it is not enforced per-cycle.
func (c *tierCapacity) admissionCap() int64 {
	return c.fullCap
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

	// Pass 2: unpinned files, hottest-first then smallest-first.
	//
	// Admission always starts at the fastest rank and walks toward slower
	// tiers. A file spills to a slower tier only when the faster tier would
	// exceed full_threshold_pct (full%) — full% is the hard ceiling, the
	// same "maximum write size" rule the smoothfs write path uses. A tier is
	// not considered full at target_fill_pct; that is the migration target a
	// tier drains toward over time as files cool, not a per-cycle spill cap.
	//
	// Ordering by heat descending ensures the hottest files claim fast-tier
	// capacity first. As files cool (HeatCounter decays hourly) the coldest
	// content sorts last and is the first to be demoted on later cycles, so
	// a tier that a burst write pushed up to full% drains back toward fill%
	// as that written data goes cold.
	//
	// Size is the secondary sort key: within equal heat, smaller files get
	// preference for fast tiers so large files don't crowd out small ones.
	var unpinned []int
	for i, c := range cands {
		if c.pin == meta.PinNone && !assigned[i] {
			unpinned = append(unpinned, i)
		}
	}
	sort.SliceStable(unpinned, func(i, j int) bool {
		ci, cj := cands[unpinned[i]], cands[unpinned[j]]
		if ci.heat != cj.heat {
			return ci.heat > cj.heat // hottest first
		}
		return ci.size < cj.size // smallest within equal heat
	})
	for _, idx := range unpinned {
		c := cands[idx]
		assignments[idx] = admitWithFallback(caps, ranked, fastestRank, c.size)
		assigned[idx] = true
	}

	return assignments, assigned
}

// planPoolPlacement gathers every file in a pool, runs a size-aware
// bin-packing pass that fills each tier up to its target_fill_pct
// before spilling to the next slower tier, and enqueues moves for any
// file whose current tier differs from its packed destination.
//
// Unpinned files use their current tier as the preferred starting point,
// so they can only drain to slower tiers — never be promoted to faster
// ones. Pinned files force-place to fastest (PinHot) or slowest (PinCold).
// If no eligible tier has room below target_fill, the packer falls through
// to full_threshold as a hard cap from the bottom tier upward; files that
// don't fit anywhere stay put.
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
		targetPct := int64(50)
		if rt.targetFillPct > 0 {
			targetPct = int64(rt.targetFillPct)
		}
		caps[rt.rank] = &tierCapacity{
			totalBytes: total,
			usedBytes:  used,
			targetCap:  total * targetPct / 100,
			fullCap:    total * int64(max1(rt.fullThresholdPct, 95)) / 100,
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
	planned := 0
	for idx, c := range cands {
		if ctx.Err() != nil {
			balanceStatus.Reason = "target-balance placement canceled"
			break
		}
		if !assigned[idx] || assignments[idx] == c.curRank {
			continue
		}
		want := assignments[idx]
		planned++
		dest := caps[want]
		if dest == nil {
			skipped++
			continue
		}
		if err := a.moveForPlacement(ns, c.rel, c.curTarg, dest.target, c.curRank, want); err != nil {
			log.Printf("placement: move %s %s→rank%d: %v",
				c.rel, c.curTarg.TierName, want, err)
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

// admitWithFallback places a file on the fastest tier at or slower than
// preferredRank that can absorb size without exceeding full_threshold_pct
// (full%). full% is the hard ceiling — the "maximum write size" rule shared
// with the smoothfs write path. A file only descends to a slower tier when
// every faster eligible tier is already at full%.
//
// target_fill_pct (fill%) is deliberately NOT consulted here. fill% is the
// migration *equilibrium* a tier drains toward over many cycles as files
// cool — not a per-placement spill threshold. Spilling at fill% would dump
// data onto the slowest tier while faster tiers still had usable headroom.
//
// Returns the rank of the tier that accepted the file, or the preferred
// rank if no tier had room (the caller then leaves the file where it is).
func admitWithFallback(caps map[int]*tierCapacity, ranked []rankedPoolTarget, preferredRank int, size int64) int {
	return admitWithFallbackOrder(caps, ranked, preferredRank, size, fullFallbackSlowestFirst)
}

// admitWithFallbackOrder is admitWithFallback. The order parameter is retained
// only for call-site readability at the pinned-hot/pinned-cold sites; both
// orders now resolve to the same single fastest-fitting-under-full% scan,
// since full% is the only admission ceiling.
func admitWithFallbackOrder(caps map[int]*tierCapacity, ranked []rankedPoolTarget, preferredRank int, size int64, _ fullFallbackOrder) int {
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
	return preferredRank
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

// heatBucketRank maps a heat counter to a target tier rank using a log2
// scale: each doubling of the counter moves the file one tier toward the
// fastest tier. HeatCounter is halved each hour, so the mapping naturally
// tracks recency — a file that was hot but has gone untouched for several
// hours will cool tier-by-tier back toward the slowest tier.
//
//	heat == 0            → slowestRank  (cold: demote to slowest)
//	heat == 1            → slowestRank-1 (any recent access: one tier up)
//	heat in [2,3]        → slowestRank-2
//	heat in [4,7]        → slowestRank-3
//	...
//	heat >= 2^(numTiers-2) → fastestRank
func heatBucketRank(heatCounter uint32, fastestRank, slowestRank int) int {
	if heatCounter == 0 {
		return slowestRank
	}
	numTiers := slowestRank - fastestRank
	if numTiers <= 0 {
		return fastestRank
	}
	// floor(log2(heatCounter)); each doubling = one tier toward fastest.
	steps := int(bits.Len32(heatCounter)) - 1
	r := slowestRank - 1 - steps // heat=1 → one above slowest
	if r < fastestRank {
		r = fastestRank
	}
	return r
}

// idealRank returns the tier a file "ideally" wants to live on, for use
// in UI hints and telemetry. It is NOT used in the placement admission
// path — admission always starts from the fastest rank and the candidate
// sort order (hottest first, smallest-within-heat) determines what ends
// up on each tier.
//
// Heat is primary: hot files target faster tiers, cold files target the
// slowest. Size nudges one tier slower for larger files within the same
// heat level. Pin overrides both: PinHot → fastestRank, PinCold → slowestRank.
func idealRank(pin meta.PinState, heatCounter uint32, sizeBytes int64, fastestRank, slowestRank int) int {
	switch pin {
	case meta.PinHot:
		return fastestRank
	case meta.PinCold:
		return slowestRank
	}
	if heatCounter == 0 {
		// Cold: demote to slowest regardless of size.
		return slowestRank
	}
	// Heat is primary: map log2(heat) to a tier rank.
	target := heatBucketRank(heatCounter, fastestRank, slowestRank)
	// Size is secondary: if the size bucket would prefer a slower tier
	// than heat demands, nudge one tier in that direction.
	if sizeBucketRank(sizeBytes, fastestRank, slowestRank) > target {
		target++
		if target > slowestRank {
			target = slowestRank
		}
	}
	return target
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
		// unlinking the source. If size and mtime match, the copy is complete:
		// remove the stale source and update the meta record.
		srcInfo2, srcErr := os.Stat(srcPath)
		if srcErr == nil && dstInfo.Size() == srcInfo2.Size() && dstInfo.ModTime().Equal(srcInfo2.ModTime()) {
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
			if store2 != nil {
				dstStat, statErr := os.Stat(dstPath)
				if statErr == nil {
					if dstSt, ok := dstStat.Sys().(*syscall.Stat_t); ok {
						rec := meta.Record{
							Version:     meta.RecordVersion,
							NamespaceID: meta.NamespaceID(ns.NamespaceID),
							TierIdx:     uint8(destRank),
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
