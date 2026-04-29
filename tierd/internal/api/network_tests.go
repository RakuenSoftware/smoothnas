package api

import (
	"encoding/json"
	"net/http"

	"github.com/JBailes/SmoothNAS/tierd/internal/nettest"
)

// NetworkTestsHandler handles /api/network-tests/* endpoints.
type NetworkTestsHandler struct{}

func NewNetworkTestsHandler() *NetworkTestsHandler { return &NetworkTestsHandler{} }

func (h *NetworkTestsHandler) Route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/network-tests/external/servers":
		if r.Method == http.MethodGet {
			h.listExternalServers(w, r)
			return
		}
		jsonMethodNotAllowed(w)
	case "/api/network-tests/run":
		if r.Method == http.MethodPost {
			h.run(w, r)
			return
		}
		jsonMethodNotAllowed(w)
	default:
		jsonNotFound(w)
	}
}

func (h *NetworkTestsHandler) run(w http.ResponseWriter, r *http.Request) {
	var req nettest.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if err := req.Validate(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	jobID := jobs.StartTagged("network-test")
	go func() {
		result, err := nettest.Run(req,
			func(progress string) { jobs.UpdateProgress(jobID, progress) },
			func(interim *nettest.Result) { jobs.UpdateResult(jobID, interim) },
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

func (h *NetworkTestsHandler) listExternalServers(w http.ResponseWriter, r *http.Request) {
	servers, err := nettest.ListExternalServers()
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	json.NewEncoder(w).Encode(servers)
}
