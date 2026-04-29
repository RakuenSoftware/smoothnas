package meta

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// ReconcileSource describes one tier to walk during a reconcile pass.
type ReconcileSource struct {
	BackingMount string // absolute path, e.g. /mnt/.tierd-backing/media/NVME
	TierRank     int
}

// ReconcileStats is the outcome of a completed (or aborted) reconcile pass.
type ReconcileStats struct {
	FilesWalked     int64
	RecordsEnqueued int64
	DeadRecords     int64
	Errors          int64
	Duration        time.Duration
	Aborted         bool
}

// Reconcile walks every backing tier mount under the pool and enqueues a
// meta record for each regular file it finds. Existing records are
// overwritten — this is acceptable because the walker observes the inode
// and tier rank, which are the authoritative fields; PinState is preserved
// across updates by reading the current record first.
//
// Intended to run once at boot per pool, after the meta store has been
// opened. Non-blocking: callers should invoke it from a goroutine. Uses
// ctx for cancellation (shutdown).
//
// Hot files open via openCreateObject populate the store directly; this
// walk is for files that pre-exist the meta store (legacy rows from the
// SQLite era, or files placed outside of smoothfs).
func (p *PoolMetaStore) Reconcile(ctx context.Context, namespaceID string, sources []ReconcileSource) ReconcileStats {
	start := time.Now()
	nsID := NamespaceID(namespaceID)
	var stats ReconcileStats

	// Track every (tier, inode) pair the walker observes. Inodes are
	// unique per backing filesystem, not across tiers, so a single set
	// would conflate identical inode values from different XFS volumes
	// and falsely keep ghost records alive (or sweep live ones).
	live := make(map[int]map[uint64]struct{}, len(sources))

	for _, src := range sources {
		if src.BackingMount == "" {
			continue
		}
		if ctx.Err() != nil {
			stats.Aborted = true
			break
		}
		if live[src.TierRank] == nil {
			live[src.TierRank] = make(map[uint64]struct{}, 1<<15)
		}
		p.walkTier(ctx, nsID, src, &stats, live[src.TierRank])
	}

	if !stats.Aborted {
		p.sweepDead(ctx, nsID, live, &stats)
	}

	stats.Duration = time.Since(start)
	log.Printf("meta: reconcile ns=%q walked=%d enqueued=%d dead=%d errs=%d in %s",
		namespaceID, stats.FilesWalked, stats.RecordsEnqueued, stats.DeadRecords,
		stats.Errors, stats.Duration)
	return stats
}

// sweepDead removes any record whose (tier, inode) was not observed
// during the walk. Scoped to records whose NamespaceID matches the
// pool's namespace so a shared-pool multi-namespace future doesn't
// accidentally delete each other's records.
type victim struct {
	tier  int
	inode uint64
}

func (p *PoolMetaStore) sweepDead(ctx context.Context, nsID uint64, live map[int]map[uint64]struct{}, stats *ReconcileStats) {
	var victims []victim
	err := p.IterateAll(func(tierRank int, ino uint64, rec Record) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rec.NamespaceID != nsID {
			return nil
		}
		liveSet, hasTier := live[tierRank]
		if !hasTier {
			// No source listed for this tier; treat as "not walked",
			// don't sweep.
			return nil
		}
		if _, ok := liveSet[ino]; !ok {
			victims = append(victims, victim{tier: tierRank, inode: ino})
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		stats.Aborted = true
		return
	}
	for _, v := range victims {
		if err := p.Delete(v.inode, v.tier); err != nil {
			log.Printf("meta: sweepDead delete tier=%d inode=%d: %v", v.tier, v.inode, err)
			continue
		}
		stats.DeadRecords++
	}
}

// walkTier handles one tier's subtree, recording every regular file's
// inode into live so the caller can later detect ghost records.
func (p *PoolMetaStore) walkTier(ctx context.Context, nsID uint64, src ReconcileSource, stats *ReconcileStats, live map[uint64]struct{}) {
	_ = filepath.WalkDir(src.BackingMount, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			stats.Aborted = true
			return filepath.SkipAll
		}
		if err != nil {
			atomic.AddInt64(&stats.Errors, 1)
			// Don't abort the whole walk on a single readdir error.
			return nil
		}
		// Skip the meta directory itself and any dotfile subtrees.
		name := d.Name()
		if d.IsDir() && (name == ".tierd-meta" || name == "lost+found") {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		atomic.AddInt64(&stats.FilesWalked, 1)

		info, err := d.Info()
		if err != nil {
			atomic.AddInt64(&stats.Errors, 1)
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		live[st.Ino] = struct{}{}

		// Preserve pin state if the record already exists on this tier.
		existing, have, _ := p.Get(st.Ino, src.TierRank)
		rec := Record{
			Version:     RecordVersion,
			TierIdx:     uint8(src.TierRank),
			NamespaceID: nsID,
		}
		if have {
			rec.PinState = existing.PinState
			rec.HeatCounter = existing.HeatCounter
			rec.LastAccessNS = existing.LastAccessNS
		}
		// Use blocking put so a large backlog never silently drops records on
		// the cold startup path.
		p.PutBlocking(st.Ino, src.TierRank, rec)
		atomic.AddInt64(&stats.RecordsEnqueued, 1)
		return nil
	})
	// Mark completion (best-effort): touch a stamp file inside the meta root.
	// We don't use it for anything yet; it's here so operators can see when
	// the last reconcile finished.
	if root := p.FastestRoot(); root != "" {
		_ = os.WriteFile(filepath.Join(root, "last-reconcile"),
			[]byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
	}
}
