package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JobStatus represents the state of an async job.
type JobStatus struct {
	ID        string    `json:"id"`
	Tag       string    `json:"tag,omitempty"` // e.g. "array-create", "array-destroy"
	Status    string    `json:"status"`        // "running", "completed", "failed"
	Progress  string    `json:"progress,omitempty"`
	Error     string    `json:"error,omitempty"`
	Result    any       `json:"result,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// JobTracker manages async job state.
type JobTracker struct {
	mu   sync.RWMutex
	jobs map[string]*JobStatus
}

var jobs = &JobTracker{jobs: make(map[string]*JobStatus)}

// Start creates a new job and returns its ID.
func (jt *JobTracker) Start() string {
	return jt.StartTagged("")
}

// StartTagged creates a new job with a tag and returns its ID.
func (jt *JobTracker) StartTagged(tag string) string {
	id := randomID()
	jt.mu.Lock()
	jt.jobs[id] = &JobStatus{
		ID:        id,
		Tag:       tag,
		Status:    "running",
		CreatedAt: time.Now(),
	}
	jt.mu.Unlock()
	return id
}

// Complete marks a job as successfully completed.
func (jt *JobTracker) Complete(id string, result any) {
	jt.mu.Lock()
	if j, ok := jt.jobs[id]; ok {
		j.Status = "completed"
		j.Result = result
	}
	jt.mu.Unlock()
}

// Fail marks a job as failed.
func (jt *JobTracker) Fail(id string, err error) {
	jt.mu.Lock()
	if j, ok := jt.jobs[id]; ok {
		j.Status = "failed"
		j.Error = err.Error()
	}
	jt.mu.Unlock()
}

// UpdateResult sets the result on a running job without marking it complete.
func (jt *JobTracker) UpdateResult(id string, result any) {
	jt.mu.Lock()
	if j, ok := jt.jobs[id]; ok {
		j.Result = result
	}
	jt.mu.Unlock()
}

// UpdateProgress sets the progress message on a running job.
func (jt *JobTracker) UpdateProgress(id, progress string) {
	jt.mu.Lock()
	if j, ok := jt.jobs[id]; ok {
		j.Progress = progress
	}
	jt.mu.Unlock()
}

// Get returns the current status of a job.
func (jt *JobTracker) Get(id string) *JobStatus {
	jt.mu.RLock()
	defer jt.mu.RUnlock()
	return jt.jobs[id]
}

// ListByTag returns all jobs with the given tag.
func (jt *JobTracker) ListByTag(tag string) []*JobStatus {
	jt.mu.RLock()
	defer jt.mu.RUnlock()
	var out []*JobStatus
	for _, j := range jt.jobs {
		if j.Tag == tag {
			out = append(out, j)
		}
	}
	return out
}

// Cleanup removes jobs older than the given duration.
func (jt *JobTracker) Cleanup(maxAge time.Duration) {
	jt.mu.Lock()
	cutoff := time.Now().Add(-maxAge)
	for id, j := range jt.jobs {
		if j.CreatedAt.Before(cutoff) {
			delete(jt.jobs, id)
		}
	}
	jt.mu.Unlock()
}

// JobsHandler handles /api/jobs/* endpoints.
type JobsHandler struct{}

func NewJobsHandler() *JobsHandler { return &JobsHandler{} }

func (h *JobsHandler) Route(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonNotFound(w)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	if rest == "" {
		// GET /api/jobs?tag=...
		tag := r.URL.Query().Get("tag")
		if tag == "" {
			jsonErrorCoded(w, "tag query param required", http.StatusBadRequest, "jobs.tag_required")
			return
		}
		result := jobs.ListByTag(tag)
		if result == nil {
			result = []*JobStatus{}
		}
		json.NewEncoder(w).Encode(result)
		return
	}

	// GET /api/jobs/{id}
	job := jobs.Get(rest)
	if job == nil {
		jsonErrorCoded(w, "job not found", http.StatusNotFound, "jobs.not_found")
		return
	}
	json.NewEncoder(w).Encode(job)
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
