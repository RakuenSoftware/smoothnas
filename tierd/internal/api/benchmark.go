package api

import (
	"encoding/json"
	"net/http"

	"github.com/JBailes/SmoothNAS/tierd/internal/benchmark"
)

// BenchmarkHandler handles /api/benchmark/* endpoints.
type BenchmarkHandler struct{}

func NewBenchmarkHandler() *BenchmarkHandler { return &BenchmarkHandler{} }

func (h *BenchmarkHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch path {
	case "/api/benchmark/run":
		if r.Method == http.MethodPost {
			h.run(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/benchmark/system":
		if r.Method == http.MethodPost {
			h.runSystem(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *BenchmarkHandler) run(w http.ResponseWriter, r *http.Request) {
	var req benchmark.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if err := req.Validate(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	jobID := jobs.StartTagged("benchmark")
	go func() {
		result, err := benchmark.Run(req,
			func(progress string) { jobs.UpdateProgress(jobID, progress) },
			func(interim *benchmark.Result) { jobs.UpdateResult(jobID, interim) },
		)
		if err != nil {
			jobs.Fail(jobID, err)
			return
		}
		jobs.Complete(jobID, result)
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

func (h *BenchmarkHandler) runSystem(w http.ResponseWriter, r *http.Request) {
	var req benchmark.SystemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if err := req.ValidateSystem(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	jobID := jobs.StartTagged("benchmark")
	go func() {
		result, err := benchmark.RunSystem(req,
			func(progress string) { jobs.UpdateProgress(jobID, progress) },
		)
		if err != nil {
			jobs.Fail(jobID, err)
			return
		}
		jobs.Complete(jobID, result)
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}
