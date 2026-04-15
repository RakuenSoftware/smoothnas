package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
)

// DisksHandler handles /api/disks* endpoints.
type DisksHandler struct {
	history    *smart.HistoryStore
	alarms     *smart.AlarmStore
	disksCache *cache.Entry[[]disk.Disk]
}

func NewDisksHandler(history *smart.HistoryStore, alarms *smart.AlarmStore) *DisksHandler {
	return &DisksHandler{
		history:    history,
		alarms:     alarms,
		disksCache: cache.New[[]disk.Disk](30 * time.Second),
	}
}

// ListDisks handles GET /api/disks.
func (h *DisksHandler) ListDisks(w http.ResponseWriter, r *http.Request) {
	disks, err := h.disksCache.GetOrFetch(disk.List)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if disks == nil {
		disks = []disk.Disk{}
	}
	json.NewEncoder(w).Encode(disks)
}

// GetSMART handles GET /api/disks/{id}/smart.
func (h *DisksHandler) GetSMART(w http.ResponseWriter, r *http.Request, devicePath string) {
	data, err := smart.ReadData(devicePath)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(data)
}

// GetSMARTHistory handles GET /api/disks/{id}/smart/history.
func (h *DisksHandler) GetSMARTHistory(w http.ResponseWriter, r *http.Request, devicePath string) {
	var attrID *int
	if v := r.URL.Query().Get("attribute_id"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			attrID = &n
		}
	}

	var since *string
	if v := r.URL.Query().Get("since"); v != "" {
		since = &v
	}

	var until *string
	if v := r.URL.Query().Get("until"); v != "" {
		until = &v
	}

	entries, err := h.history.Query(devicePath, attrID, since, until)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []smart.HistoryEntry{}
	}
	json.NewEncoder(w).Encode(entries)
}

// StartSMARTTest handles POST /api/disks/{id}/smart/test.
func (h *DisksHandler) StartSMARTTest(w http.ResponseWriter, r *http.Request, devicePath string) {
	var req struct {
		Type string `json:"type"` // "short" or "long"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "short"
	}

	if err := smart.StartTest(devicePath, req.Type); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"status":"test started","type":"%s"}`, req.Type)
}

// GetSMARTTests handles GET /api/disks/{id}/smart/test.
func (h *DisksHandler) GetSMARTTests(w http.ResponseWriter, r *http.Request, devicePath string) {
	results, err := smart.GetTestResults(devicePath)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []smart.TestResult{}
	}
	json.NewEncoder(w).Encode(results)
}

// IdentifyDisk handles POST /api/disks/{id}/identify.
func (h *DisksHandler) IdentifyDisk(w http.ResponseWriter, r *http.Request, devicePath string) {
	if err := disk.Identify(devicePath); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, `{"status":"identifying"}`)
}

// WipeDisk handles POST /api/disks/{id}/wipe.
func (h *DisksHandler) WipeDisk(w http.ResponseWriter, r *http.Request, devicePath string) {
	// Safety: refuse to wipe OS disks or assigned disks.
	disks, err := h.disksCache.GetOrFetch(disk.List)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, d := range disks {
		if d.Path == devicePath {
			if d.Assignment != "unassigned" {
				jsonError(w, "cannot wipe disk: currently assigned as "+d.Assignment, http.StatusBadRequest)
				return
			}
			break
		}
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Wiping disk signatures...")
		if err := disk.Wipe(devicePath); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.disksCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "wiped", "path": devicePath})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

// --- SMART Alarms ---

// ListAlarmRules handles GET /api/smart/alarms.
func (h *DisksHandler) ListAlarmRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.alarms.ListRules()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rules == nil {
		rules = []smart.AlarmRule{}
	}
	json.NewEncoder(w).Encode(rules)
}

// CreateAlarmRule handles POST /api/smart/alarms.
func (h *DisksHandler) CreateAlarmRule(w http.ResponseWriter, r *http.Request) {
	var rule smart.AlarmRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if rule.AttributeID == 0 || rule.AttributeName == "" {
		http.Error(w, `{"error":"attribute_id and attribute_name required"}`, http.StatusBadRequest)
		return
	}

	id, err := h.alarms.CreateRule(rule)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rule.ID = id
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rule)
}

// UpdateAlarmRule handles PUT /api/smart/alarms/{id}.
func (h *DisksHandler) UpdateAlarmRule(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid alarm id"}`, http.StatusBadRequest)
		return
	}

	var rule smart.AlarmRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if err := h.alarms.UpdateRule(id, rule); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, `{"status":"updated"}`)
}

// DeleteAlarmRule handles DELETE /api/smart/alarms/{id}.
func (h *DisksHandler) DeleteAlarmRule(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid alarm id"}`, http.StatusBadRequest)
		return
	}

	if err := h.alarms.DeleteRule(id); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, `{"status":"deleted"}`)
}

// ListAlarmEvents handles GET /api/smart/alarms/history.
func (h *DisksHandler) ListAlarmEvents(w http.ResponseWriter, r *http.Request) {
	var devicePath *string
	if v := r.URL.Query().Get("device"); v != "" {
		devicePath = &v
	}
	var severity *string
	if v := r.URL.Query().Get("severity"); v != "" {
		severity = &v
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	events, err := h.alarms.ListEvents(devicePath, severity, limit)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []smart.AlarmEvent{}
	}
	json.NewEncoder(w).Encode(events)
}

// Route dispatches disk and SMART requests based on path.
func (h *DisksHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// GET /api/disks
	if path == "/api/disks" || path == "/api/disks/" {
		if r.Method == http.MethodGet {
			h.ListDisks(w, r)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	// /api/smart/alarms routes.
	if strings.HasPrefix(path, "/api/smart/alarms") {
		h.routeAlarms(w, r)
		return
	}

	// /api/disks/{name}/...
	if strings.HasPrefix(path, "/api/disks/") {
		h.routeDisk(w, r)
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func (h *DisksHandler) routeDisk(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/disks/{name}/...
	rest := strings.TrimPrefix(r.URL.Path, "/api/disks/")
	parts := strings.SplitN(rest, "/", 2)
	diskName := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	// Reconstruct device path from name.
	devicePath := "/dev/" + diskName

	switch subpath {
	case "smart":
		if r.Method == http.MethodGet {
			h.GetSMART(w, r, devicePath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "smart/history":
		if r.Method == http.MethodGet {
			h.GetSMARTHistory(w, r, devicePath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "smart/test":
		switch r.Method {
		case http.MethodGet:
			h.GetSMARTTests(w, r, devicePath)
		case http.MethodPost:
			h.StartSMARTTest(w, r, devicePath)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "identify":
		if r.Method == http.MethodPost {
			h.IdentifyDisk(w, r, devicePath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "wipe":
		if r.Method == http.MethodPost {
			h.WipeDisk(w, r, devicePath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (h *DisksHandler) routeAlarms(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// GET/POST /api/smart/alarms
	if path == "/api/smart/alarms" || path == "/api/smart/alarms/" {
		switch r.Method {
		case http.MethodGet:
			h.ListAlarmRules(w, r)
		case http.MethodPost:
			h.CreateAlarmRule(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	// GET /api/smart/alarms/history
	if path == "/api/smart/alarms/history" {
		if r.Method == http.MethodGet {
			h.ListAlarmEvents(w, r)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	// PUT/DELETE /api/smart/alarms/{id}
	if strings.HasPrefix(path, "/api/smart/alarms/") {
		idStr := strings.TrimPrefix(path, "/api/smart/alarms/")
		switch r.Method {
		case http.MethodPut:
			h.UpdateAlarmRule(w, r, idStr)
		case http.MethodDelete:
			h.DeleteAlarmRule(w, r, idStr)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}
