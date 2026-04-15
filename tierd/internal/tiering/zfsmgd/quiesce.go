package zfsmgd

import (
	"fmt"
	"sync"
	"time"
)

// nsWorkerQuiesce manages coordinated quiesce for movement workers in one
// namespace. Workers acquire the copy lock for the duration of their cp
// invocation. Quiesce acquires the write lock, which waits for all in-progress
// copies to complete and prevents new ones from starting.
//
// The design ensures that when beginQuiesce returns successfully, no movement
// worker is mid-copy: all active copies have finished and no new copy can start
// until endQuiesce is called.
type nsWorkerQuiesce struct {
	mu          sync.Mutex
	quiescing   bool
	releaseCh   chan struct{} // closed when quiesce is released
	parkedCh    chan struct{} // one send per worker that parks or exits during quiesce
	copiesActive int          // number of cp commands currently running
}

// enterCopy is called by a movement worker immediately before starting the cp
// command. It blocks if a quiesce is active, waiting for the current snapshot
// cycle to complete before allowing the copy to proceed.
func (q *nsWorkerQuiesce) enterCopy() {
	for {
		q.mu.Lock()
		if !q.quiescing {
			q.copiesActive++
			q.mu.Unlock()
			return
		}
		ch := q.releaseCh
		q.mu.Unlock()
		<-ch // wait for current quiesce to release
	}
}

// exitCopy is called by a movement worker immediately after the cp command
// completes (success or failure). It decrements the active copy count. If a
// quiesce is waiting and this was the last active copy, it signals completion.
func (q *nsWorkerQuiesce) exitCopy() {
	q.mu.Lock()
	q.copiesActive--
	remaining := q.copiesActive
	var pch chan struct{}
	if q.quiescing && remaining == 0 {
		pch = q.parkedCh
	}
	q.mu.Unlock()

	if pch != nil {
		select {
		case pch <- struct{}{}:
		default:
		}
	}
}

// beginQuiesce activates quiesce and waits for all in-progress copies to
// complete. It returns nil when the quiesce is established (no copies in
// progress) or an error if the timeout expires before copies complete.
func (q *nsWorkerQuiesce) beginQuiesce(timeout time.Duration) error {
	q.mu.Lock()
	if q.quiescing {
		q.mu.Unlock()
		return fmt.Errorf("quiesce already in progress for this namespace")
	}
	q.quiescing = true
	q.releaseCh = make(chan struct{})
	if q.copiesActive == 0 {
		// No active copies; quiesce is immediately established.
		q.mu.Unlock()
		return nil
	}
	// Create a buffered signal channel; exitCopy sends when count hits zero.
	pch := make(chan struct{}, 1)
	q.parkedCh = pch
	q.mu.Unlock()

	select {
	case <-pch:
		return nil
	case <-time.After(timeout):
		q.endQuiesce()
		return fmt.Errorf("movement worker quiesce timed out after %v: copies still in progress", timeout)
	}
}

// endQuiesce releases all workers that were blocked in enterCopy during
// the quiesce period.
func (q *nsWorkerQuiesce) endQuiesce() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.quiescing {
		return
	}
	q.quiescing = false
	q.parkedCh = nil
	close(q.releaseCh)
	q.releaseCh = nil
}
