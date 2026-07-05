package meta

import (
	"testing"
	"time"
)

// The placement planner deletes and sync-writes meta records one at a time
// from a single goroutine (moveForPlacement -> store.Delete / Move). bbolt's
// DB.Batch is documented as "only useful when there are multiple goroutines
// calling it": a lone serial caller waits the full MaxBatchDelay (10ms) per
// call for the batch timer to fire, because a single call never reaches
// MaxBatchSize. That 10ms-per-op stall serialized behind the single planner
// goroutine is what wedged tiering placement on a large pool.
//
// del/putSync must therefore commit via db.Update (immediate, NoSync = no
// fsync), not db.Batch. This guards the fix: 300 serial ops must finish far
// under the ~3s a 10ms-per-call batch path would take.
func TestSerialWriteThroughput(t *testing.T) {
	s, err := openShard(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("openShard: %v", err)
	}
	defer s.Close()

	const n = 300
	val := []byte("record-bytes")

	start := time.Now()
	for i := 0; i < n; i++ {
		if err := s.putSync(InodeKey(uint64(i)), val); err != nil {
			t.Fatalf("putSync %d: %v", i, err)
		}
	}
	putElapsed := time.Since(start)

	start = time.Now()
	for i := 0; i < n; i++ {
		if err := s.del(InodeKey(uint64(i))); err != nil {
			t.Fatalf("del %d: %v", i, err)
		}
	}
	delElapsed := time.Since(start)

	t.Logf("%d serial putSync in %v (%.2fms/op), %d serial del in %v (%.2fms/op)",
		n, putElapsed, float64(putElapsed.Milliseconds())/n,
		n, delElapsed, float64(delElapsed.Milliseconds())/n)

	// On the old db.Batch path each op waits ~10ms for the batch timer, so
	// 300 ops take ~3s each. db.Update commits immediately (sub-ms with
	// NoSync). Budget generously to stay robust on slow CI disks while still
	// catching the 10ms-per-op regression.
	const budget = 1500 * time.Millisecond
	if putElapsed > budget {
		t.Fatalf("%d serial putSync took %v (>%v): bbolt.Batch timer stall", n, putElapsed, budget)
	}
	if delElapsed > budget {
		t.Fatalf("%d serial del took %v (>%v): bbolt.Batch timer stall", n, delElapsed, budget)
	}
}
