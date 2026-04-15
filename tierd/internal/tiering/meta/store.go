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
// per tier in the pool (NVMe, SSD, HDD…) and routes operations across
// them so metadata records mirror the tiering of user data: hot records
// live on the fastest tier, cold records spill to slower tiers.
//
// Behaviour:
//
//   - Put always writes to the fastest tier and removes any stale copy
//     on slower tiers, so a record only ever lives in one place.
//   - Get probes tiers fastest-first; on hit-from-slower, the next Put
//     (which the FUSE OPEN path issues for heat tracking) naturally
//     promotes the record back to fastest.
//   - Delete removes from every tier (cheap; absent keys are no-ops).
//   - Iterate visits every tier with a dedup set so callers see each
//     inode once (fastest-tier copy wins).
//   - EvictColdest moves records from a hot tier to the next slower tier
//     when the placement planner detects capacity pressure.
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

// Put enqueues a record onto the fastest tier. Returns false only if the
// fastest tier's write queue is full.
//
// Maintains the "exactly one tier per record" invariant: if a stale copy
// exists on a slower tier (from prior eviction), Put deletes it. The
// cleanup uses a cheap mmap'd `get` to probe before issuing the
// expensive `del` transaction, so the hot path (record already on
// fastest, nothing on slower) pays only a handful of pointer-dereferences
// and never a write transaction.
func (p *PoolMetaStore) Put(inode uint64, rec Record) bool {
	cleanupSlowerTiers(p.tiers, inode)
	ok := p.tiers[0].shardFor(inode).enqueue(InodeKey(inode), rec.Encode())
	p.cache.putIfPlacementChanged(inode, rec)
	return ok
}

// PutBlocking is the can't-drop variant of Put (e.g., user-driven pin
// state changes). Same one-tier-at-a-time invariant as Put.
func (p *PoolMetaStore) PutBlocking(inode uint64, rec Record) {
	cleanupSlowerTiers(p.tiers, inode)
	p.tiers[0].shardFor(inode).enqueueBlocking(InodeKey(inode), rec.Encode())
	p.cache.put(inode, rec)
}

// cleanupSlowerTiers issues a `del` on any slower tier that *currently*
// holds a record for inode. The `get` probe is a lock-free mmap deref —
// negligible cost on a miss. The `del` runs only on a hit, which is rare
// (only after promotion-from-cold). Walks every slower tier, not just
// tier 1, in case eviction races left copies on multiple tiers.
func cleanupSlowerTiers(tiers []*tierStore, inode uint64) {
	key := InodeKey(inode)
	for i := 1; i < len(tiers); i++ {
		sh := tiers[i].shardFor(inode)
		if _, ok, _ := sh.get(key); ok {
			_ = sh.del(key)
		}
	}
}

// Delete removes the record from every tier. No-op for absent keys.
func (p *PoolMetaStore) Delete(inode uint64) error {
	p.cache.del(inode)
	var firstErr error
	for _, t := range p.tiers {
		if err := t.shardFor(inode).del(InodeKey(inode)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Get returns the record for inode. It checks the in-memory cache first;
// on a miss it probes tiers fastest-first and populates the cache on hit.
func (p *PoolMetaStore) Get(inode uint64) (Record, bool, error) {
	if rec, ok := p.cache.get(inode); ok {
		return rec, true, nil
	}
	for _, t := range p.tiers {
		val, ok, err := t.shardFor(inode).get(InodeKey(inode))
		if err != nil {
			return Record{}, false, err
		}
		if ok {
			rec, err := DecodeRecord(val)
			if err != nil {
				return Record{}, false, err
			}
			p.cache.put(inode, rec)
			return rec, true, nil
		}
	}
	return Record{}, false, nil
}

// Iterate visits every record across every tier exactly once. Faster-tier
// copies win when the same inode appears in multiple tiers (which can
// happen briefly during eviction races; under steady state Put + Delete
// keep records exclusive).
func (p *PoolMetaStore) Iterate(fn func(inode uint64, rec Record) error) error {
	seen := make(map[uint64]struct{})
	for _, t := range p.tiers {
		for _, sh := range t.shards {
			if sh == nil {
				continue
			}
			err := sh.iterate(func(ino uint64, val []byte) error {
				if _, dup := seen[ino]; dup {
					return nil
				}
				seen[ino] = struct{}{}
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
	}
	return nil
}

// EvictColdest moves up to maxRecords records with the oldest
// LastAccessNS from tier index srcTierIdx to srcTierIdx+1. Skips records
// pinned-hot — the user explicitly wants those on fast storage. Returns
// the number actually moved. No-op if srcTierIdx is the slowest tier.
func (p *PoolMetaStore) EvictColdest(srcTierIdx, maxRecords int) (int, error) {
	if srcTierIdx < 0 || srcTierIdx >= len(p.tiers)-1 || maxRecords <= 0 {
		return 0, nil
	}
	src := p.tiers[srcTierIdx]
	dst := p.tiers[srcTierIdx+1]

	type cand struct {
		ino uint64
		rec Record
	}
	var cands []cand
	for _, sh := range src.shards {
		if sh == nil {
			continue
		}
		_ = sh.iterate(func(ino uint64, val []byte) error {
			rec, derr := DecodeRecord(val)
			if derr != nil {
				return nil
			}
			if rec.PinState == PinHot {
				return nil
			}
			cands = append(cands, cand{ino, rec})
			return nil
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].rec.LastAccessNS < cands[j].rec.LastAccessNS
	})
	if len(cands) > maxRecords {
		cands = cands[:maxRecords]
	}
	moved := 0
	for _, c := range cands {
		dst.shardFor(c.ino).enqueueBlocking(InodeKey(c.ino), c.rec.Encode())
		if err := src.shardFor(c.ino).del(InodeKey(c.ino)); err != nil {
			continue
		}
		moved++
	}
	return moved, nil
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
