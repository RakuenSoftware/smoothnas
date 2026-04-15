// Package tiering — Scheduler runs one background goroutine per registered
// adapter. Each cycle it collects activity samples, plans and starts movements,
// polls running jobs to completion, invalidates stale jobs, and purges old
// records from the control-plane tables.
package tiering

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// ControlPlaneStore is the subset of db.Store used by the scheduler.
type ControlPlaneStore interface {
	GetControlPlaneConfig(key string) (string, error)
	CreateMovementJob(job *db.MovementJobRow) error
	GetMovementJob(id string) (*db.MovementJobRow, error)
	ListMovementJobs() ([]db.MovementJobRow, error)
	UpdateMovementJobState(id, state string, progressBytes int64, failureReason string) error
	UpsertPlacementIntent(intent *db.PlacementIntentRow) error
	ListPlacementIntents() ([]db.PlacementIntentRow, error)
	PurgeResolvedDegradedStates(olderThan time.Duration) (int64, error)
	PurgeTerminalMovementJobs(olderThan time.Duration) (int64, error)
	PurgeSatisfiedPlacementIntents(olderThan time.Duration) (int64, error)
	MarkRunningJobsFailed(backendKind, reason string) error
}

// Scheduler runs one background goroutine for a single adapter. It is safe to
// create multiple Schedulers for different adapters concurrently.
type Scheduler struct {
	adapter TieringAdapter
	store   ControlPlaneStore

	epoch atomic.Int64 // planner epoch; incremented once per cycle

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// NewScheduler creates a Scheduler bound to the given adapter and store.
// Call Start to begin the background goroutine.
func NewScheduler(adapter TieringAdapter, store ControlPlaneStore) *Scheduler {
	return &Scheduler{
		adapter: adapter,
		store:   store,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start launches the background scheduler goroutine. It returns immediately.
func (s *Scheduler) Start() {
	go s.run()
}

// Stop signals the scheduler to stop and waits for it to finish.
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}

// Epoch returns the current planner epoch (the number of completed cycles).
func (s *Scheduler) Epoch() int64 {
	return s.epoch.Load()
}

func (s *Scheduler) run() {
	defer close(s.done)

	interval := s.plannerInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.cycle()
			interval = s.plannerInterval()
			timer.Reset(interval)
		}
	}
}

// plannerInterval reads planner_interval_minutes from control_plane_config.
// Falls back to 15 minutes if unset or unparseable.
func (s *Scheduler) plannerInterval() time.Duration {
	val, _ := s.store.GetControlPlaneConfig("planner_interval_minutes")
	if n, err := strconv.Atoi(val); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return 15 * time.Minute
}

// cycle runs one planner cycle. The epoch is incremented at the start.
func (s *Scheduler) cycle() {
	epoch := s.epoch.Add(1)
	kind := s.adapter.Kind()
	log.Printf("tiering scheduler: %s epoch %d start", kind, epoch)

	// 1. Collect activity samples from the adapter.
	if samples, err := s.adapter.CollectActivity(); err != nil {
		log.Printf("tiering scheduler: %s CollectActivity: %v", kind, err)
	} else {
		log.Printf("tiering scheduler: %s collected %d activity samples", kind, len(samples))
	}

	// 2. Plan movements and record pending jobs.
	plans, err := s.adapter.PlanMovements()
	if err != nil {
		log.Printf("tiering scheduler: %s PlanMovements: %v", kind, err)
	} else {
		s.enqueuePlans(plans, epoch)
	}

	// 3. Start pending jobs.
	s.startPending(epoch)

	// 4. Poll running jobs to terminal state.
	s.pollRunning()

	// 5. Invalidate stale jobs.
	s.invalidateStale()

	// 6. Purge old records.
	s.purge()

	log.Printf("tiering scheduler: %s epoch %d done", kind, epoch)
}

// enqueuePlans records each movement plan as a pending movement_job. Plans for
// which a pending or running job already exists are skipped so the same
// placement intent does not produce duplicate jobs.
func (s *Scheduler) enqueuePlans(plans []MovementPlan, epoch int64) {
	kind := s.adapter.Kind()
	existing, err := s.store.ListMovementJobs()
	if err != nil {
		log.Printf("tiering scheduler: %s list jobs: %v", kind, err)
		return
	}
	activeByNamespaceObject := make(map[string]struct{}, len(existing))
	for _, j := range existing {
		if j.BackendKind == kind && (j.State == db.MovementJobStatePending || j.State == db.MovementJobStateRunning) {
			activeByNamespaceObject[j.NamespaceID+":"+j.ObjectID] = struct{}{}
		}
	}

	for _, plan := range plans {
		key := plan.NamespaceID + ":" + plan.ObjectID
		if _, ok := activeByNamespaceObject[key]; ok {
			continue // existing active job; skip
		}
		job := &db.MovementJobRow{
			BackendKind:    kind,
			NamespaceID:    plan.NamespaceID,
			ObjectID:       plan.ObjectID,
			MovementUnit:   plan.MovementUnit,
			PlacementDomain: plan.PlacementDomain,
			SourceTargetID: plan.SourceTargetID,
			DestTargetID:   plan.DestTargetID,
			PolicyRevision: plan.PolicyRevision,
			IntentRevision: plan.IntentRevision,
			PlannerEpoch:   epoch,
			TriggeredBy:    fmt.Sprintf("scheduler-epoch-%d", epoch),
			TotalBytes:     plan.TotalBytes,
		}
		if err := s.store.CreateMovementJob(job); err != nil {
			log.Printf("tiering scheduler: %s enqueue job %s→%s: %v",
				kind, plan.SourceTargetID, plan.DestTargetID, err)
		}
	}
}

// startPending dequeues pending jobs and calls adapter.StartMovement for each.
func (s *Scheduler) startPending(epoch int64) {
	kind := s.adapter.Kind()
	jobs, err := s.store.ListMovementJobs()
	if err != nil {
		log.Printf("tiering scheduler: %s list jobs for start: %v", kind, err)
		return
	}
	for _, j := range jobs {
		if j.BackendKind != kind || j.State != db.MovementJobStatePending {
			continue
		}
		plan := MovementPlan{
			NamespaceID:     j.NamespaceID,
			ObjectID:        j.ObjectID,
			MovementUnit:    j.MovementUnit,
			PlacementDomain: j.PlacementDomain,
			SourceTargetID:  j.SourceTargetID,
			DestTargetID:    j.DestTargetID,
			PolicyRevision:  j.PolicyRevision,
			IntentRevision:  j.IntentRevision,
			PlannerEpoch:    epoch,
			TotalBytes:      j.TotalBytes,
		}
		if _, err := s.adapter.StartMovement(plan); err != nil {
			log.Printf("tiering scheduler: %s StartMovement %s: %v", kind, j.ID, err)
			_ = s.store.UpdateMovementJobState(j.ID, db.MovementJobStateFailed, 0, err.Error())
		} else {
			_ = s.store.UpdateMovementJobState(j.ID, db.MovementJobStateRunning, 0, "")
		}
	}
}

// pollRunning calls adapter.GetMovement for each running job and updates state.
func (s *Scheduler) pollRunning() {
	kind := s.adapter.Kind()
	jobs, err := s.store.ListMovementJobs()
	if err != nil {
		log.Printf("tiering scheduler: %s list jobs for poll: %v", kind, err)
		return
	}
	for _, j := range jobs {
		if j.BackendKind != kind || j.State != db.MovementJobStateRunning {
			continue
		}
		state, err := s.adapter.GetMovement(j.ID)
		if err != nil {
			log.Printf("tiering scheduler: %s GetMovement %s: %v", kind, j.ID, err)
			continue
		}
		if state.State != j.State {
			_ = s.store.UpdateMovementJobState(j.ID, state.State, state.ProgressBytes, state.FailureReason)
		}
	}
}

// invalidateStale marks movement jobs as stale when their policy_revision or
// intent_revision no longer match current state.
func (s *Scheduler) invalidateStale() {
	kind := s.adapter.Kind()
	jobs, err := s.store.ListMovementJobs()
	if err != nil {
		log.Printf("tiering scheduler: %s list jobs for invalidation: %v", kind, err)
		return
	}
	intents, err := s.store.ListPlacementIntents()
	if err != nil {
		log.Printf("tiering scheduler: %s list intents for invalidation: %v", kind, err)
		return
	}
	currentRevision := make(map[string]int64, len(intents))
	for _, intent := range intents {
		currentRevision[intent.NamespaceID+":"+intent.ObjectID] = intent.IntentRevision
	}
	for _, j := range jobs {
		if j.BackendKind != kind || (j.State != db.MovementJobStatePending && j.State != db.MovementJobStateRunning) {
			continue
		}
		key := j.NamespaceID + ":" + j.ObjectID
		if rev, ok := currentRevision[key]; ok && rev != j.IntentRevision {
			log.Printf("tiering scheduler: %s stale job %s (intent_revision %d → %d)",
				kind, j.ID, j.IntentRevision, rev)
			_ = s.store.UpdateMovementJobState(j.ID, db.MovementJobStateStale, j.ProgressBytes, "intent_revision_changed")
		}
	}
}

// purge removes old records from the control-plane tables.
func (s *Scheduler) purge() {
	kind := s.adapter.Kind()
	if n, err := s.store.PurgeResolvedDegradedStates(7 * 24 * time.Hour); err != nil {
		log.Printf("tiering scheduler: %s purge degraded states: %v", kind, err)
	} else if n > 0 {
		log.Printf("tiering scheduler: %s purged %d resolved degraded states", kind, n)
	}
	if n, err := s.store.PurgeTerminalMovementJobs(30 * 24 * time.Hour); err != nil {
		log.Printf("tiering scheduler: %s purge terminal jobs: %v", kind, err)
	} else if n > 0 {
		log.Printf("tiering scheduler: %s purged %d terminal movement jobs", kind, n)
	}
	if n, err := s.store.PurgeSatisfiedPlacementIntents(7 * 24 * time.Hour); err != nil {
		log.Printf("tiering scheduler: %s purge satisfied intents: %v", kind, err)
	} else if n > 0 {
		log.Printf("tiering scheduler: %s purged %d satisfied placement intents", kind, n)
	}
}
