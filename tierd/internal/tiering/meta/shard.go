package meta

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// objectsBucket is the single bucket name used inside every shard DB.
// One bucket keeps the tree shallow and scans cheap.
var objectsBucket = []byte("o")

// shardWriteQueueCap is the per-shard channel buffer. Sized so bursts of
// thousands of rsync creates don't block the FUSE handler; exceeding it is
// treated as backpressure.
const shardWriteQueueCap = 4096

// shardBatchMax is the largest number of records a writer will commit in a
// single bbolt transaction. bbolt amortises the COW cost over the batch; too
// small wastes commits, too large delays durability.
const shardBatchMax = 256

// shardBatchMaxAge caps how long a partially-filled batch may wait before
// being flushed. Keeps worst-case staleness bounded when the write rate is
// low.
const shardBatchMaxAge = 10 * time.Millisecond

// shardSyncInterval drives background fsync. bbolt with NoSync=true never
// fsyncs on commit; we call Sync periodically to limit crash-loss window.
const shardSyncInterval = 5 * time.Second

// pendingWrite is an item in a shard's writer queue.
type pendingWrite struct {
	key []byte
	val []byte
}

// shard is one bbolt database plus its batched writer goroutine.
type shard struct {
	idx int
	db  *bolt.DB

	queue chan pendingWrite

	// wg tracks the writer and sync goroutines so Close can drain them.
	wg sync.WaitGroup

	// cancel stops the sync ticker; the writer exits when queue is closed.
	cancel context.CancelFunc

	// Counters updated via sync/atomic. Published through PoolMetaStore.Stats.
	batchesCommitted atomic.Uint64
	recordsWritten   atomic.Uint64
	maxBatchSeen     atomic.Uint64
	dropsOnEnqueue   atomic.Uint64
	flushErrors      atomic.Uint64
	syncErrors       atomic.Uint64
	lastFlushNanos   atomic.Int64
}

// openShard opens (creating if absent) a bbolt DB for one shard of the
// objects index. path is the directory that will hold "shard.db".
//
// The DB is opened with NoSync — commits return as soon as pages are written
// to the mmap. A background goroutine calls db.Sync() every shardSyncInterval
// so data durably lands within that window. On a clean shutdown Close()
// forces a final sync.
func openShard(dir string, idx int) (*shard, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "shard.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{
		Timeout:      3 * time.Second,
		NoSync:       true,
		NoGrowSync:   true,
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("open shard %s: %w", dbPath, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(objectsBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init shard %s: %w", dbPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &shard{
		idx:    idx,
		db:     db,
		queue:  make(chan pendingWrite, shardWriteQueueCap),
		cancel: cancel,
	}
	s.wg.Add(2)
	go s.writerLoop()
	go s.syncLoop(ctx)
	return s, nil
}

// enqueue submits a write without blocking. Returns false if the queue is
// full; callers can retry, drop, or fall back to a synchronous put.
func (s *shard) enqueue(key, val []byte) bool {
	select {
	case s.queue <- pendingWrite{key: key, val: val}:
		return true
	default:
		s.dropsOnEnqueue.Add(1)
		return false
	}
}

// enqueueBlocking submits a write, blocking briefly if the queue is full.
// Use for code paths that must not drop writes (e.g., pin state changes).
func (s *shard) enqueueBlocking(key, val []byte) {
	s.queue <- pendingWrite{key: key, val: val}
}

// del removes a record synchronously. Used by UNLINK so the next boot
// reconcile doesn't resurrect a ghost record from a file that no longer
// exists. Uses bbolt's own batch semantics to coalesce with nearby deletes
// under concurrent load.
func (s *shard) del(key []byte) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		return b.Delete(key)
	})
}

// iterate invokes fn for each record in the shard. Iteration order is
// big-endian byte order of the inode key (i.e. numeric inode order). fn
// returning a non-nil error aborts and returns it. The record bytes passed
// to fn are only valid for the duration of the call.
func (s *shard) iterate(fn func(inode uint64, val []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) != 8 {
				return nil
			}
			ino := uint64(k[0])<<56 | uint64(k[1])<<48 | uint64(k[2])<<40 | uint64(k[3])<<32 |
				uint64(k[4])<<24 | uint64(k[5])<<16 | uint64(k[6])<<8 | uint64(k[7])
			return fn(ino, v)
		})
	})
}

// get reads a record by key. Returns ok=false if not present. Never blocks
// on the writer.
func (s *shard) get(key []byte) (val []byte, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		// Copy out — the bbolt slice is only valid for the life of the txn.
		val = append([]byte(nil), v...)
		ok = true
		return nil
	})
	return
}

// writerLoop drains the queue in batches. Each iteration either fills a
// batch up to shardBatchMax, or flushes whatever's buffered once
// shardBatchMaxAge elapses since the first item arrived.
func (s *shard) writerLoop() {
	defer s.wg.Done()
	batch := make([]pendingWrite, 0, shardBatchMax)
	for {
		first, ok := <-s.queue
		if !ok {
			return
		}
		batch = append(batch[:0], first)
		deadline := time.After(shardBatchMaxAge)
	fill:
		for len(batch) < shardBatchMax {
			select {
			case w, ok := <-s.queue:
				if !ok {
					s.flush(batch)
					return
				}
				batch = append(batch, w)
			case <-deadline:
				break fill
			}
		}
		s.flush(batch)
	}
}

// flush commits a batch in a single bbolt transaction. Errors are logged and
// dropped — the store is a cache, so a single failed batch can be
// reconstructed from the backing FS on next reconcile.
func (s *shard) flush(batch []pendingWrite) {
	if len(batch) == 0 {
		return
	}
	start := time.Now()
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return fmt.Errorf("objects bucket missing")
		}
		for _, w := range batch {
			if err := b.Put(w.key, w.val); err != nil {
				return err
			}
		}
		return nil
	})
	s.lastFlushNanos.Store(time.Since(start).Nanoseconds())
	if err != nil {
		s.flushErrors.Add(1)
		log.Printf("meta: shard %d flush (%d records): %v", s.idx, len(batch), err)
		return
	}
	s.batchesCommitted.Add(1)
	s.recordsWritten.Add(uint64(len(batch)))
	if n := uint64(len(batch)); n > s.maxBatchSeen.Load() {
		s.maxBatchSeen.Store(n)
	}
}

// syncLoop periodically calls db.Sync() so NoSync'd writes hit disk within
// shardSyncInterval.
func (s *shard) syncLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(shardSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.db.Sync(); err != nil {
				s.syncErrors.Add(1)
				log.Printf("meta: shard %d sync: %v", s.idx, err)
			}
		}
	}
}

// Close drains pending writes, stops the sync goroutine, forces a final
// sync, and closes the bbolt DB.
func (s *shard) Close() error {
	s.cancel()
	close(s.queue)
	s.wg.Wait()
	if err := s.db.Sync(); err != nil {
		log.Printf("meta: shard %d final sync: %v", s.idx, err)
	}
	return s.db.Close()
}
