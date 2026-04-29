package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
)

const diskSpindownConfigPrefix = "disk.spindown."

var diskPowerObserver = spindown.NewPowerObserver()

type diskPowerResponse struct {
	disk.PowerStatus
	spindown.PowerSummary
}

// DisksHandler handles /api/disks* endpoints.
type DisksHandler struct {
	history    *smart.HistoryStore
	alarms     *smart.AlarmStore
	disksCache *cache.Entry[[]disk.Disk]
	store      *db.Store
}

func NewDisksHandler(store *db.Store, history *smart.HistoryStore, alarms *smart.AlarmStore) *DisksHandler {
	return &DisksHandler{
		history:    history,
		alarms:     alarms,
		disksCache: cache.New[[]disk.Disk](30 * time.Second),
		store:      store,
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

func (h *DisksHandler) GetPower(w http.ResponseWriter, r *http.Request, devicePath string) {
	d, ok, err := h.diskByPath(devicePath)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		jsonErrorCoded(w, "disk not found", http.StatusNotFound, "disks.not_found")
		return
	}
	timer := h.configuredSpindownMinutes(d.Name)
	status := disk.PowerStatusFor(d, timer)
	json.NewEncoder(w).Encode(powerResponse(devicePath, status, "operator poll"))
}

func (h *DisksHandler) SetSpindown(w http.ResponseWriter, r *http.Request, devicePath string) {
	var req struct {
		Enabled     bool `json:"enabled"`
		IdleMinutes int  `json:"idle_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	d, ok, err := h.diskByPath(devicePath)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		jsonErrorCoded(w, "disk not found", http.StatusNotFound, "disks.not_found")
		return
	}
	status := disk.PowerStatusFor(d, h.configuredSpindownMinutes(d.Name))
	if req.Enabled {
		if !status.Eligible {
			jsonError(w, "disk is not spindown eligible: "+status.IneligibleWhy, http.StatusBadRequest)
			return
		}
		if req.IdleMinutes <= 0 {
			jsonErrorCoded(w, "idle_minutes is required when enabled", http.StatusBadRequest, "disks.idle_minutes_required")
			return
		}
		if err := disk.DisableAPM(devicePath); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := disk.SetSpindownTimer(devicePath, req.IdleMinutes); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if h.store != nil {
			if err := h.store.SetConfig(spindownConfigKey(d.Name), strconv.Itoa(req.IdleMinutes)); err != nil {
				serverError(w, err)
				return
			}
		}
		status = disk.PowerStatusFor(d, req.IdleMinutes)
		json.NewEncoder(w).Encode(powerResponse(devicePath, status, "spindown timer enabled"))
		return
	}
	if err := disk.SetSpindownTimer(devicePath, 0); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.store != nil {
		if err := h.store.SetConfig(spindownConfigKey(d.Name), "0"); err != nil {
			serverError(w, err)
			return
		}
	}
	status = disk.PowerStatusFor(d, 0)
	json.NewEncoder(w).Encode(powerResponse(devicePath, status, "spindown timer disabled"))
}

func (h *DisksHandler) StandbyDisk(w http.ResponseWriter, r *http.Request, devicePath string) {
	d, ok, err := h.diskByPath(devicePath)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		jsonErrorCoded(w, "disk not found", http.StatusNotFound, "disks.not_found")
		return
	}
	status := disk.PowerStatusFor(d, h.configuredSpindownMinutes(d.Name))
	if !status.Eligible {
		jsonError(w, "disk is not spindown eligible: "+status.IneligibleWhy, http.StatusBadRequest)
		return
	}
	if err := disk.StandbyNow(devicePath); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	diskPowerObserver.RecordEvent(devicePath, status.State, "standby", "manual standby")
	json.NewEncoder(w).Encode(powerResponse(devicePath, disk.PowerStatusFor(d, h.configuredSpindownMinutes(d.Name)), "manual standby"))
}

func powerResponse(devicePath string, status disk.PowerStatus, reason string) diskPowerResponse {
	return diskPowerResponse{
		PowerStatus:  status,
		PowerSummary: diskPowerObserver.Observe(devicePath, status.State, reason),
	}
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
		jsonInvalidRequestBody(w)
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
	// Safety: refuse to wipe OS disks. Everything else is eligible because
	// wipe is the operator escape hatch for stale ZFS/mdadm/LVM signatures.
	disks, err := h.disksCache.GetOrFetch(disk.List)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, d := range disks {
		if d.Path == devicePath {
			if d.Assignment == "os" {
				jsonErrorCoded(w, "cannot wipe OS disk", http.StatusBadRequest, "disks.cannot_wipe_os")
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
		jsonInvalidRequestBody(w)
		return
	}

	if rule.AttributeID == 0 || rule.AttributeName == "" {
		jsonErrorCoded(w, "attribute_id and attribute_name required", http.StatusBadRequest, "disks.smart_attr_fields_required")
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
		jsonErrorCoded(w, "invalid alarm id", http.StatusBadRequest, "disks.invalid_alarm_id")
		return
	}

	var rule smart.AlarmRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		jsonInvalidRequestBody(w)
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
		jsonErrorCoded(w, "invalid alarm id", http.StatusBadRequest, "disks.invalid_alarm_id")
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
			jsonMethodNotAllowed(w)
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

	jsonNotFound(w)
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
			jsonMethodNotAllowed(w)
		}
	case "power":
		switch r.Method {
		case http.MethodGet:
			h.GetPower(w, r, devicePath)
		case http.MethodPut:
			h.SetSpindown(w, r, devicePath)
		default:
			jsonMethodNotAllowed(w)
		}
	case "standby":
		if r.Method == http.MethodPost {
			h.StandbyDisk(w, r, devicePath)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "smart/history":
		if r.Method == http.MethodGet {
			h.GetSMARTHistory(w, r, devicePath)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "smart/test":
		switch r.Method {
		case http.MethodGet:
			h.GetSMARTTests(w, r, devicePath)
		case http.MethodPost:
			h.StartSMARTTest(w, r, devicePath)
		default:
			jsonMethodNotAllowed(w)
		}
	case "identify":
		if r.Method == http.MethodPost {
			h.IdentifyDisk(w, r, devicePath)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "wipe":
		if r.Method == http.MethodPost {
			h.WipeDisk(w, r, devicePath)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *DisksHandler) diskByPath(devicePath string) (disk.Disk, bool, error) {
	disks, err := h.disksCache.GetOrFetch(disk.List)
	if err != nil {
		return disk.Disk{}, false, err
	}
	for _, d := range disks {
		if d.Path == devicePath {
			return d, true, nil
		}
	}
	return disk.Disk{}, false, nil
}

func (h *DisksHandler) configuredSpindownMinutes(diskName string) int {
	if h.store == nil {
		return 0
	}
	val, err := h.store.GetConfig(spindownConfigKey(diskName))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func spindownConfigKey(diskName string) string {
	return diskSpindownConfigPrefix + diskName + ".idle_minutes"
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
			jsonMethodNotAllowed(w)
		}
		return
	}

	// GET /api/smart/alarms/history
	if path == "/api/smart/alarms/history" {
		if r.Method == http.MethodGet {
			h.ListAlarmEvents(w, r)
		} else {
			jsonMethodNotAllowed(w)
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
			jsonMethodNotAllowed(w)
		}
		return
	}

	jsonNotFound(w)
}
