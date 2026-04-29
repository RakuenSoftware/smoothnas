package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type CheckProvider func(context.Context) []Check

// Handler serves the /api/health endpoint. Unauthenticated.
type Handler struct {
	version   string
	startTime time.Time
	checks    CheckProvider
}

func NewHandler(version string, startTime time.Time, checks ...CheckProvider) *Handler {
	var provider CheckProvider
	if len(checks) > 0 {
		provider = checks[0]
	}
	return &Handler{version: version, startTime: startTime, checks: provider}
}

type healthResponse struct {
	Status    string  `json:"status"`
	Version   string  `json:"version"`
	Uptime    string  `json:"uptime"`
	Timestamp string  `json:"timestamp"`
	Checks    []Check `json:"checks,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	checks := h.runtimeChecks(r.Context())
	status := aggregateStatus(checks)

	resp := healthResponse{
		Status:    status,
		Version:   h.version,
		Uptime:    time.Since(h.startTime).Truncate(time.Second).String(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Checks:    checks,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) runtimeChecks(ctx context.Context) []Check {
	if h.checks == nil {
		return nil
	}
	return h.checks(ctx)
}

func aggregateStatus(checks []Check) string {
	status := "ok"
	for _, check := range checks {
		switch check.Status {
		case "critical":
			return "critical"
		case "warning":
			if status == "ok" {
				status = "warning"
			}
		}
	}
	return status
}
