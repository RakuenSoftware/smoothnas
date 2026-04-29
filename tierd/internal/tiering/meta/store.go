package meta

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// DefaultShards is the number of parallel bbolt DBs per tier-store. 8 is
// enough to keep NVMe write queues saturated without wasting fds.
const DefaultShards = 8

// ShardsFromEnv returns the shard count, honouring TIERD_META_SHARDS and
// clamping to [1, 64].
func ShardsFromEnv() int {
	if s := os.Getenv("TIERD_META_SHARDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > 64 {
				n = 64
			}
			return n
		}
	}
	return DefaultShards
}

// TierBacking describes one tier's place in the meta-store hierarchy.
// Rank is the tier_targets.rank value (lower = faster). BackingMount is
// the backing filesystem under which `.tierd-meta/` is created.
type TierBacking struct {
	Rank         int
	Name         string
	BackingMount string
}

// PoolMetaStore is the per-pool metadata store. It owns one tier-store
// per tier in the pool (NVMe, SSD, HDD…). Each record lives on exactly
// one tier — the same tier that holds the file it describes. Callers
// that touch a file already know which tier it's on (the open returns
// the chosen tier, placement scans walk one tier at a time, etc.) and
// pass the rank to every meta operation.
//
// Rationale: probing every tier on the read path makes the slowest tier
// dominate latency, so a degraded HDD (e.g. mid-rebuild) can hang the smoothfs kernel path
// creates that target NVMe. Per-tier records confine that contention to
// operations that actually touch the slow tier.
//
// Behaviour:
//
//   - Get(inode, tier) reads exactly one tier; absent records return ok=false.
//   - Put(inode, tier, rec) enqueues to that tier's writer.
//   - Delete(inode, tier) deletes from that tier only.
//   - Move(inode, src, dst) atomically (from the caller's view) shifts a
//     record between tiers when its file is migrated. Used by placement.
//   - Iterate(tier, fn) walks one tier; IterateAll(fn) walks every tier
//     and reports the rank for each record so callers can mutate in place.
type PoolMetaStore struct {
	tiers []*tierStore // sorted by rank ascending (fastest first)
	cache *inodeCache

	closeOnce sync.Once
}

// tierStore is one tier's slice of the meta store.
type tierStore struct {
	rank         int
	name         string
	backingMount string
	root         string // <backing>/.tierd-meta
	shards       []*shard
}

// Open creates (or opens existing) per-tier meta stores under each
// backing's `.tierd-meta/` subtree. tiers must be non-empty; the first
// element after sorting becomes the canonical "fastest" target for Puts.
func Open(tiers []TierBacking) (*PoolMetaStore, error) {
	if len(tiers) == 0 {
		return nil, fmt.Errorf("meta: Open requires at least one tier backing")
	}
	sorted := append([]TierBacking(nil), tiers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rank < sorted[j].Rank })

	p := &PoolMetaStore{cache: newInodeCache(defaultMaxCacheEntries)}
	for _, tb := range sorted {
		ts, err := openTierStore(tb)
		if err != nil {
			for _, opened := range p.tiers {
				_ = opened.close()
			}
			return nil, err
		}
		p.tiers = append(p.tiers, ts)
	}
	return p, nil
}

func openTierStore(tb TierBacking) (*tierStore, error) {
	root := filepath.Join(tb.BackingMount, ".tierd-meta")
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	objDir := filepath.Join(root, "objects")
	shardCount, err := discoverShardCount(objDir)
	if err != nil {
		return nil, err
	}
	if shardCount == 0 {
		shardCount = ShardsFromEnv()
	}
	if err := writeVersion(root); err != nil {
		return nil, err
	}
	ts := &tierStore{
		rank:         tb.Rank,
		name:         tb.Name,
		backingMount: tb.BackingMount,
		root:         root,
		shards:       make([]*shard, shardCount),
	}
	for i := 0; i < shardCount; i++ {
		dir := filepath.Join(objDir, fmt.Sprintf("%02d", i))
		sh, err := openShard(dir, i)
		if err != nil {
			for j := 0; j < i; j++ {
				_ = ts.shards[j].Close()
			}
			return nil, err
		}
		ts.shards[i] = sh
	}
	return ts, nil
}

func (t *tierStore) close() error {
	var firstErr error
	for _, sh := range t.shards {
		if sh == nil {
			continue
		}
		if err := sh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *tierStore) shardFor(inode uint64) *shard {
	idx := xxhash.Sum64(inode8(inode)) % uint64(len(t.shards))
	return t.shards[idx]
}

// ShardStats is a point-in-time snapshot of one shard's counters.
type ShardStats struct {
	TierRank         int    `json:"tier_rank"`
	TierName         string `json:"tier_name"`
	Index            int    `json:"index"`
	QueueDepth       int    `json:"queue_depth"`
	QueueCapacity    int    `json:"queue_capacity"`
	BatchesCommitted uint64 `json:"batches_committed"`
	RecordsWritten   uint64 `json:"records_written"`
	MaxBatchSeen     uint64 `json:"max_batch_seen"`
	DropsOnEnqueue   uint64 `json:"drops_on_enqueue"`
	FlushErrors      uint64 `json:"flush_errors"`
	SyncErrors       uint64 `json:"sync_errors"`
	LastFlushMicros  int64  `json:"last_flush_micros"`
}

// Stats returns per-shard counters across every tier.
func (p *PoolMetaStore) Stats() []ShardStats {
	var out []ShardStats
	for _, t := range p.tiers {
		for _, sh := range t.shards {
			if sh == nil {
				continue
			}
			out = append(out, ShardStats{
				TierRank:         t.rank,
				TierName:         t.name,
				Index:            sh.idx,
				QueueDepth:       len(sh.queue),
				QueueCapacity:    cap(sh.queue),
				BatchesCommitted: sh.batchesCommitted.Load(),
				RecordsWritten:   sh.recordsWritten.Load(),
				MaxBatchSeen:     sh.maxBatchSeen.Load(),
				DropsOnEnqueue:   sh.dropsOnEnqueue.Load(),
				FlushErrors:      sh.flushErrors.Load(),
				SyncErrors:       sh.syncErrors.Load(),
				LastFlushMicros:  sh.lastFlushNanos.Load() / 1000,
			})
		}
	}
	return out
}

// Close drains every tier's shards.
func (p *PoolMetaStore) Close() error {
	var firstErr error
	p.closeOnce.Do(func() {
		for _, t := range p.tiers {
			if err := t.close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// TierCount returns the number of tier stores backing this pool.
func (p *PoolMetaStore) TierCount() int { return len(p.tiers) }

// TierBackingPath returns the backing-mount path of tier index i. Used by
// the eviction loop to compute capacity / fill.
func (p *PoolMetaStore) TierBackingPath(i int) string {
	if i < 0 || i >= len(p.tiers) {
		return ""
	}
	return p.tiers[i].backingMount
}

// FastestRoot returns the root path of the fastest-tier meta directory,
// for callers that just need a stable location for stamp files etc.
func (p *PoolMetaStore) FastestRoot() string {
	if len(p.tiers) == 0 {
		return ""
	}
	return p.tiers[0].root
}

// tierByRank returns the tier-store with the given rank, or nil if none.
func (p *PoolMetaStore) tierByRank(rank int) *tierStore {
	for _, t := range p.tiers {
		if t.rank == rank {
			return t
		}
	}
	return nil
}

// Put enqueues a record onto the named tier. Returns false only if that
// tier's write queue is full or the rank is unknown.
func (p *PoolMetaStore) Put(inode uint64, tierRank int, rec Record) bool {
	t := p.tierByRank(tierRank)
	if t == nil {
		return false
	}
	ok := t.shardFor(inode).enqueue(InodeKey(inode), rec.Encode())
	p.cache.putIfPlacementChanged(inode, rec)
	return ok
}

// PutBlocking is the can't-drop variant of Put (e.g., reconcile, pin
// state changes). Returns silently if the rank is unknown — callers
// should not be passing tiers that don't exist.
func (p *PoolMetaStore) PutBlocking(inode uint64, tierRank int, rec Record) {
	t := p.tierByRank(tierRank)
	if t == nil {
		return
	}
	t.shardFor(inode).enqueueBlocking(InodeKey(inode), rec.Encode())
	p.cache.put(inode, rec)
}

// Delete removes the record from the named tier. No-op if absent or if
// the rank is unknown. Drops the cache entry unconditionally so a stale
// cached copy can't outlive the on-disk record.
func (p *PoolMetaStore) Delete(inode uint64, tierRank int) error {
	t := p.tierByRank(tierRank)
	if t == nil {
		return nil
	}
	p.cache.del(inode)
	return t.shardFor(inode).del(InodeKey(inode))
}

// Get returns the record for inode on the named tier. The in-memory
// cache is consulted first; a hit short-circuits the bbolt View. On a
// miss the on-disk shard is read and the cache populated.
//
// The cache is keyed by inode alone, not (tier, inode). That's safe in
// practice because XFS allocates from a 64-bit inode space and the
// chance of two tiers reusing the same inode value for unrelated files
// is vanishingly small, but it means a paranoid caller of Get on a
// non-authoritative tier could see a cached record from a different
// tier. The hot paths (placement, on-disk lookup) only ever Get on
// the tier where they just observed the file, so this is not exercised.
func (p *PoolMetaStore) Get(inode uint64, tierRank int) (Record, bool, error) {
	if rec, ok := p.cache.get(inode); ok && int(rec.TierIdx) == tierRank {
		return rec, true, nil
	}
	t := p.tierByRank(tierRank)
	if t == nil {
		return Record{}, false, nil
	}
	val, ok, err := t.shardFor(inode).get(InodeKey(inode))
	if err != nil {
		return Record{}, false, err
	}
	if !ok {
		return Record{}, false, nil
	}
	rec, err := DecodeRecord(val)
	if err != nil {
		return Record{}, false, err
	}
	p.cache.put(inode, rec)
	return rec, true, nil
}

// Move shifts a record from src to dst. Used when placement migrates a
// file between tiers — the data move must be paired with this call so
// the meta record follows. Returns nil (no-op) if the source has no
// record. Updates rec.TierIdx to dst before writing.
//
// Move bypasses PutBlocking + Delete for two reasons:
//
//   - The source shard may still have a queued Put for this inode that
//     hasn't been flushed; if our delete reached bbolt before that flush
//     ran, the queued Put would resurrect the record. drainQueue forces
//     the source shard to commit anything queued at call time before we
//     delete.
//   - PoolMetaStore.Delete unconditionally evicts the cache entry for
//     the inode. The cache is keyed by inode alone, so a Delete on the
//     source tier would clobber the cache entry we just wrote for the
//     destination tier. We manage the cache directly here instead.
func (p *PoolMetaStore) Move(inode uint64, fromRank, toRank int) error {
	if fromRank == toRank {
		return nil
	}
	rec, ok, err := p.Get(inode, fromRank)
	if err != nil || !ok {
		return err
	}
	rec.TierIdx = uint8(toRank)

	src := p.tierByRank(fromRank)
	dst := p.tierByRank(toRank)
	if dst == nil {
		return fmt.Errorf("meta: unknown destination tier rank %d", toRank)
	}

	if src != nil {
		src.shardFor(inode).drainQueue()
	}
	if err := dst.shardFor(inode).putSync(InodeKey(inode), rec.Encode()); err != nil {
		return err
	}
	p.cache.put(inode, rec)
	if src != nil {
		return src.shardFor(inode).del(InodeKey(inode))
	}
	return nil
}

// Iterate walks every record on the named tier. Returns immediately if
// the rank is unknown.
func (p *PoolMetaStore) Iterate(tierRank int, fn func(inode uint64, rec Record) error) error {
	t := p.tierByRank(tierRank)
	if t == nil {
		return nil
	}
	for _, sh := range t.shards {
		if sh == nil {
			continue
		}
		err := sh.iterate(func(ino uint64, val []byte) error {
			rec, derr := DecodeRecord(val)
			if derr != nil {
				return nil
			}
			return fn(ino, rec)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// IterateAll walks every record on every tier. The callback receives the
// tier rank so callers can mutate the right tier when needed (e.g.,
// reconcile sweeping ghost records).
func (p *PoolMetaStore) IterateAll(fn func(tierRank int, inode uint64, rec Record) error) error {
	for _, t := range p.tiers {
		rank := t.rank
		err := p.Iterate(rank, func(ino uint64, rec Record) error {
			return fn(rank, ino, rec)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// NamespaceID returns the stable 64-bit id used inside records for a
// given namespace string. xxhash collisions are astronomically rare
// within a single pool.
func NamespaceID(namespace string) uint64 {
	return xxhash.Sum64String(namespace)
}

// --- helpers ---

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	return nil
}

func discoverShardCount(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n, nil
}

func writeVersion(root string) error {
	p := filepath.Join(root, "VERSION")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte{byte(RecordVersion)}, 0o644)
}

func inode8(inode uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], inode)
	return b[:]
}
