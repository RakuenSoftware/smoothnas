package mdadm

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

// placementInterval is how often the planner runs per pool. Short enough
// that new pins move quickly, long enough not to thrash under steady load.
const placementInterval = 2 * time.Minute

// placementQuiescentPeriod is the minimum time a namespace must be idle
// before the planner will consider migrations. Prevents the planner from
// interfering with active backups or user workloads by walking tens of
// thousands of files every cycle and potentially starting migrations.
// Reset on every HandleOpen, so any user touching the namespace delays
// the next planner run by at least this long.
const placementQuiescentPeriod = 10 * time.Minute

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
		_ = s.Iterate(func(inode uint64, rec meta.Record) error {
			if rec.HeatCounter == 0 {
				return nil
			}
			rec.HeatCounter /= 2
			s.PutBlocking(inode, rec)
			halved++
			return nil
		})
		log.Printf("placement: pool %s heat decay halved %d records", pool, halved)
	}
}

func (a *Adapter) runPlacementCycle(ctx context.Context) {
	nss, err := a.store.ListMdadmManagedNamespaces()
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
// lives, how big it is, and what the user's pin state says.
type candidate struct {
	rel      string
	size     int64
	inode    uint64
	curRank  int
	curTarg  db.MdadmManagedTargetRow
	pin      meta.PinState
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

// planPoolPlacement gathers every file in a pool, runs a size-aware
// bin-packing pass that fills each tier up to its target_fill_pct
// before spilling to the next slower tier, and enqueues moves for any
// file whose current tier differs from its packed destination.
//
// Preference: always place on the fastest tier that can hold the file
// under target_fill. Smallest files go first so they lock in the
// highest tier; large files fall through. Pinned files force-place to
// fastest (PinHot) or slowest (PinCold) and consume capacity accordingly.
// If a tier has no room below target_fill, the packer falls through to
// full_threshold as a hard cap; files that don't fit anywhere stay put.
func (a *Adapter) planPoolPlacement(ctx context.Context, ns db.MdadmManagedNamespaceRow) {
	// Three idle gates — all must pass. If any of them reports activity
	// the planner skips the cycle entirely. This keeps 50k-file walks,
	// meta-store reads, and potential migrations out of the way of
	// anything actively touching the pool.
	if !a.poolIdleForPlacement(ns.NamespaceID) {
		return
	}

	ranked := a.poolRankedTargets(ns.PoolName)
	if len(ranked) < 2 {
		return
	}
	store := a.metaStoreFor(ns.PoolName)
	if store == nil {
		return
	}

	// Snapshot per-tier capacity from the filesystem.
	caps := make(map[int]*tierCapacity, len(ranked))
	for _, rt := range ranked {
		var st syscall.Statfs_t
		if err := syscall.Statfs(rt.target.MountPath, &st); err != nil {
			log.Printf("placement: statfs %s: %v", rt.target.MountPath, err)
			return
		}
		total := int64(st.Blocks) * int64(st.Bsize)
		used := total - int64(st.Bavail)*int64(st.Bsize)
		targetPct := int64(50)
		if rt.fullThresholdPct > 0 {
			// fullThresholdPct is authoritative for the hard cap; target_fill
			// is looked up separately by poolRankedTargets.
		}
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
	var cands []candidate
	for _, rt := range ranked {
		if ctx.Err() != nil {
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
			if d.IsDir() && (name == ".tierd-meta" || name == "lost+found") {
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
			rel, err := filepath.Rel(rt.target.MountPath, path)
			if err != nil {
				return nil
			}
			rec, _, _ := store.Get(st.Ino)
			cands = append(cands, candidate{
				rel:     rel,
				size:    info.Size(),
				inode:   st.Ino,
				curRank: rt.rank,
				curTarg: rt.target,
				pin:     rec.PinState,
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

	assignments := make(map[uint64]int, len(cands)) // inode → assigned rank

	// Pass 1: forced (pinned) placements.
	for _, c := range cands {
		switch c.pin {
		case meta.PinHot:
			assignments[c.inode] = admitWithFallback(caps, ranked, fastestRank, c.size)
		case meta.PinCold:
			assignments[c.inode] = admitWithFallback(caps, ranked, slowestRank, c.size)
		}
	}

	// Pass 2: unpinned, smallest-first fills from the top.
	var unpinned []candidate
	for _, c := range cands {
		if c.pin == meta.PinNone {
			unpinned = append(unpinned, c)
		}
	}
	sort.Slice(unpinned, func(i, j int) bool { return unpinned[i].size < unpinned[j].size })
	for _, c := range unpinned {
		assignments[c.inode] = admitWithFallback(caps, ranked, fastestRank, c.size)
	}

	// Enqueue moves for any file whose assigned rank != current rank.
	// Re-check the idle gate every few moves so the planner bails out
	// as soon as a user starts heavy I/O mid-cycle. Already-done moves
	// stay; remaining moves are dropped and retried next cycle.
	moved := 0
	skipped := 0
	const idleRecheckEvery = 8
	for i, c := range cands {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && i%idleRecheckEvery == 0 {
			if !a.poolIdleForPlacement(ns.NamespaceID) {
				log.Printf("placement: pool %s aborting mid-cycle after %d moves (activity resumed)",
					ns.PoolName, moved)
				return
			}
		}
		want, ok := assignments[c.inode]
		if !ok || want == c.curRank {
			continue
		}
		dest := caps[want]
		if dest == nil {
			skipped++
			continue
		}
		if err := a.moveForPlacement(ns, c.rel, c.curTarg, dest.target, want); err != nil {
			log.Printf("placement: move %s %s→rank%d: %v",
				c.rel, c.curTarg.TierName, want, err)
			continue
		}
		moved++
	}
	if len(cands) > 0 {
		log.Printf("placement: pool %s scanned=%d moved=%d skipped=%d",
			ns.PoolName, len(cands), moved, skipped)
	}

	// Meta store eviction: per-tier capacity check. If a tier has zero
	// headroom under target_fill, push the coldest non-pinned-hot meta
	// records to the next slower tier. Bounded batch size per cycle so
	// a heavily-overloaded fastest tier converges over a few cycles
	// rather than spending a full pass on a single eviction.
	a.evictColdMeta(ns.PoolName, caps, ranked)
}

// evictColdMeta moves meta records from any tier that is over its
// target_fill threshold down to the next slower tier. Called at the end
// of a placement cycle; the planner already bin-packed user data, so the
// remaining headroom calculation is up to date. Skips evictions when no
// tier has capacity to receive (slowest tier full).
func (a *Adapter) evictColdMeta(poolName string, caps map[int]*tierCapacity, ranked []rankedPoolTarget) {
	const maxPerEvict = 4096

	store := a.metaStoreFor(poolName)
	if store == nil || store.TierCount() < 2 {
		return
	}
	for i, rt := range ranked {
		if i >= store.TierCount()-1 {
			break // slowest tier — nowhere to evict to
		}
		c := caps[rt.rank]
		if c == nil {
			continue
		}
		if c.usedBytes <= c.targetCap {
			continue
		}
		moved, err := store.EvictColdest(i, maxPerEvict)
		if err != nil {
			log.Printf("placement: pool %s meta evict tier %s: %v", poolName, rt.target.TierName, err)
			continue
		}
		if moved > 0 {
			log.Printf("placement: pool %s meta evict tier %s → next: %d records",
				poolName, rt.target.TierName, moved)
		}
	}
}

// admitWithFallback finds the highest-ranking tier (fastest) at or slower
// than preferredRank whose remaining budget (targetCap - usedBytes) can
// absorb size. Falls through to fullCap if no tier has room under target.
// Returns the rank of the tier that accepted the file, or the preferred
// rank if no admission succeeded (in which case the caller just leaves
// the file where it is — assignments[] becomes a no-op compared to its
// current rank).
func admitWithFallback(caps map[int]*tierCapacity, ranked []rankedPoolTarget, preferredRank int, size int64) int {
	// Pass A: try each tier from preferred (inclusive) to slowest, honour
	// target_fill_pct as a soft cap. "Preferred" is usually fastest, so
	// the scan walks ranks ascending (fastest → slowest) from there.
	for _, rt := range ranked {
		if rt.rank < preferredRank {
			continue
		}
		c := caps[rt.rank]
		if c == nil {
			continue
		}
		if c.usedBytes+size <= c.targetCap {
			c.usedBytes += size
			return rt.rank
		}
	}
	// Pass B: target_fill exceeded everywhere from preferred downward.
	// Accept at full_threshold.
	for _, rt := range ranked {
		if rt.rank < preferredRank {
			continue
		}
		c := caps[rt.rank]
		if c == nil {
			continue
		}
		if c.usedBytes+size <= c.fullCap {
			c.usedBytes += size
			return rt.rank
		}
	}
	return preferredRank
}

// poolIdleForPlacement gates the planner on three signals:
//
//  1. No FUSE OPENs on this namespace for at least placementQuiescentPeriod.
//  2. Zero active opens right now (rsync mid-file, user mid-edit, etc).
//  3. No running backup_runs in the DB — backups often use passthrough
//     writes that bypass HandleOpen after the initial open, so a very
//     large file could age the activity stamp past the quiescent window
//     even while writes are ongoing. Consulting backup_runs catches that.
//
// Any "active" signal aborts this cycle; the next cycle re-checks.
func (a *Adapter) poolIdleForPlacement(namespaceID string) bool {
	if idle := a.namespaceIdleFor(namespaceID); idle < placementQuiescentPeriod {
		return false
	}
	if atomic.LoadInt64(a.nsOpenCounter(namespaceID)) > 0 {
		return false
	}
	runs, err := a.store.ListActiveBackupRuns()
	if err == nil && len(runs) > 0 {
		return false
	}
	return true
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
	targets, err := a.store.ListMdadmManagedTargets()
	if err != nil {
		log.Printf("placement: list targets: %v", err)
		return nil
	}
	var ranked []rankedPoolTarget
	for i := range targets {
		if targets[i].PoolName != poolName {
			continue
		}
		tt, err := a.store.GetTierTargetByBackingRef(
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
// the meta record, and unlinks the source. Bails out if the namespace has
// active opens (next planner cycle will retry).
func (a *Adapter) moveForPlacement(ns db.MdadmManagedNamespaceRow, rel string, src, dst db.MdadmManagedTargetRow, destRank int) error {
	if atomic.LoadInt64(a.nsOpenCounter(ns.NamespaceID)) > 0 {
		return fmt.Errorf("namespace has active opens; deferring")
	}

	srcPath := filepath.Join(src.MountPath, rel)
	dstPath := filepath.Join(dst.MountPath, rel)
	tmpPath := dstPath + ".tierd-move"

	// Ensure dest parent directory exists on the destination tier.
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("mkdir dest parent: %w", err)
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}

	if err := copyFileContents(srcPath, tmpPath, srcInfo.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy: %w", err)
	}

	// Preserve mtime so subsequent rsyncs don't re-transfer this file.
	if err := os.Chtimes(tmpPath, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		log.Printf("placement: chtimes %s: %v", tmpPath, err)
	}

	// Re-check active opens immediately before the swap — if someone
	// opened during the copy, back out.
	if atomic.LoadInt64(a.nsOpenCounter(ns.NamespaceID)) > 0 {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("active open raced migration; reverting")
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename tmp → dest: %w", err)
	}

	// Source copy is now redundant. Unlinking it here preserves the
	// existing openat-fastest-first semantics in openUnregisteredObject:
	// a subsequent OPEN hits the dest tier.
	if err := os.Remove(srcPath); err != nil {
		log.Printf("placement: unlink src %s after move: %v", srcPath, err)
	}

	// Update meta record: new tier, reset LastAccessNS to now (a move is
	// observable activity), preserve pin state + heat counter.
	store := a.metaStoreFor(ns.PoolName)
	if store != nil {
		dstStat, err := os.Stat(dstPath)
		if err == nil {
			if dstSt, ok := dstStat.Sys().(*syscall.Stat_t); ok {
				rec, _, _ := store.Get(dstSt.Ino)
				if rec.Version == 0 {
					rec = meta.Record{
						Version:     meta.RecordVersion,
						NamespaceID: meta.NamespaceID(ns.NamespaceID),
					}
				}
				rec.TierIdx = uint8(destRank)
				rec.LastAccessNS = uint64(time.Now().UnixNano())
				store.PutBlocking(dstSt.Ino, rec)
			}
		}
	}
	return nil
}

// copyFileContents opens src for read and dst for exclusive create-write,
// streams the bytes, and closes both.
func copyFileContents(src, dst string, mode os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return err
	}
	return df.Close()
}
