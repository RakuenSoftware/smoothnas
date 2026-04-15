package health

import (
	"encoding/json"
	"net/http"
	"time"
)

// Handler serves the /api/health endpoint. Unauthenticated.
type Handler struct {
	version   string
	startTime time.Time
}

func NewHandler(version string, startTime time.Time) *Handler {
	return &Handler{version: version, startTime: startTime}
}

type healthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Uptime    string `json:"uptime"`
	Timestamp string `json:"timestamp"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	resp := healthResponse{
		Status:    "ok",
		Version:   h.version,
		Uptime:    time.Since(h.startTime).Truncate(time.Second).String(),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
