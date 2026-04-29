package meta

import (
	"sync"
	"sync/atomic"
)

// cacheShards is the number of independent shards in inodeCache. 64 shards
// means 64 independent RWMutexes, spreading lock contention across concurrent
// opens from multiple clients.
const cacheShards = 64

// defaultMaxCacheEntries caps the in-memory cache. Each entry is ~32 bytes
// for the Record plus ~80 bytes of Go map overhead (~110 bytes total), so
// 4.7 M entries ≈ 512 MB — a fixed reservation that leaves ample headroom
// on a 16 GB NAS.
const defaultMaxCacheEntries int64 = 4_700_000

// inodeCache is a sharded read cache that sits in front of the per-tier bbolt
// stores. The hot path calls Get once per open; the cache avoids the
// bbolt View() transaction overhead on repeat accesses to the same inode.
//
// Correctness guarantees:
//
//   - Put/PutBlocking update the cache synchronously. The bbolt write is
//     batched-async, but the cache reflects the latest intent immediately.
//   - Delete removes the entry so the next Get re-reads from bbolt.
//   - Move (placement-driven tier change) issues a Delete on the src tier
//     and a PutBlocking on the dst tier; the cache ends up with the new
//     record carrying the destination TierIdx.
//   - When the cache is full, new inodes are not admitted. Existing entries
//     continue to be served and updated — hot files stay cached, cold files
//     that have never been accessed remain uncached.
type inodeCache struct {
	shards [cacheShards]cacheShard
	max    int64
	count  atomic.Int64
}

type cacheShard struct {
	mu      sync.RWMutex
	entries map[uint64]Record
}

func newInodeCache(max int64) *inodeCache {
	c := &inodeCache{max: max}
	for i := range c.shards {
		c.shards[i].entries = make(map[uint64]Record)
	}
	return c
}

func (c *inodeCache) shardFor(inode uint64) *cacheShard {
	return &c.shards[inode%cacheShards]
}

// get returns the cached Record for inode if present.
func (c *inodeCache) get(inode uint64) (Record, bool) {
	s := c.shardFor(inode)
	s.mu.RLock()
	r, ok := s.entries[inode]
	s.mu.RUnlock()
	return r, ok
}

// put inserts or updates the cached Record for inode. When the cache is full,
// new inodes are silently dropped; existing entries are always updated so that
// PutBlocking callers (pin state changes) see their write reflected immediately.
func (c *inodeCache) put(inode uint64, rec Record) {
	s := c.shardFor(inode)
	s.mu.Lock()
	_, exists := s.entries[inode]
	if !exists {
		if c.max > 0 && c.count.Load() >= c.max {
			s.mu.Unlock()
			return
		}
		c.count.Add(1)
	}
	s.entries[inode] = rec
	s.mu.Unlock()
}

// putIfPlacementChanged updates the cache only when TierIdx or PinState
// differs from what is already cached — or when the inode is not yet cached.
// Heat-tracking fields (HeatCounter, LastAccessNS) are intentionally ignored:
// they are advisory and the placement planner reads them via Iterate() directly
// from bbolt, so stale values in the cache have no correctness impact.
//
// This is the right call for the Put() hot path (fired on every open)
// because it avoids taking a write lock for the common case where only the
// heat counter changed. The write lock is only taken when placement actually
// changed or the entry is new.
func (c *inodeCache) putIfPlacementChanged(inode uint64, rec Record) {
	s := c.shardFor(inode)

	// Fast path under read lock: if the entry exists and placement fields are
	// unchanged, skip the write entirely.
	s.mu.RLock()
	cached, exists := s.entries[inode]
	unchanged := exists && cached.TierIdx == rec.TierIdx && cached.PinState == rec.PinState
	s.mu.RUnlock()
	if unchanged {
		return
	}

	// Slow path: new entry or placement changed — take the write lock.
	s.mu.Lock()
	_, exists = s.entries[inode]
	if !exists {
		if c.max > 0 && c.count.Load() >= c.max {
			s.mu.Unlock()
			return
		}
		c.count.Add(1)
	}
	s.entries[inode] = rec
	s.mu.Unlock()
}

// del removes the cached entry for inode. No-op if not present.
func (c *inodeCache) del(inode uint64) {
	s := c.shardFor(inode)
	s.mu.Lock()
	if _, ok := s.entries[inode]; ok {
		delete(s.entries, inode)
		c.count.Add(-1)
	}
	s.mu.Unlock()
}
