package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/monitor"
	"github.com/JBailes/SmoothNAS/tierd/internal/updater"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

// logAlertRequest is the body accepted by POST /api/system/alerts.
type logAlertRequest struct {
	Source   string `json:"source"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Device   string `json:"device"`
}

// SystemHandler handles /api/system/* endpoints.
type SystemHandler struct {
	mon *monitor.Monitor
}

func NewSystemHandler(mon *monitor.Monitor) *SystemHandler {
	return &SystemHandler{mon: mon}
}

// Route dispatches system requests.
func (h *SystemHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch path {
	case "/api/system/status":
		if r.Method == http.MethodGet {
			h.getStatus(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/hardware":
		if r.Method == http.MethodGet {
			h.getHardware(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/tuning":
		switch r.Method {
		case http.MethodGet:
			h.getTuning(w, r)
		case http.MethodPut:
			h.setTuning(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
	case "/api/system/alerts":
		switch r.Method {
		case http.MethodGet:
			h.getAlerts(w, r)
		case http.MethodPost:
			h.logAlert(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
	case "/api/system/alerts/count":
		if r.Method == http.MethodGet {
			h.getAlertCount(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/reboot":
		switch r.Method {
		case http.MethodGet:
			// Updates install automatically (apt); this says whether one is
			// waiting for a restart, which the operator starts.
			json.NewEncoder(w).Encode(updater.GetRebootStatus())
		case http.MethodPost:
			h.reboot(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
	case "/api/system/updates":
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(updater.GetUpdateStatus())
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/updates/check", "/api/system/updates/install":
		if r.Method == http.MethodPost {
			h.startUpdate(w, strings.TrimPrefix(path, "/api/system/updates/"))
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/updates/log":
		if r.Method == http.MethodGet {
			h.updateLog(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "/api/system/shutdown":
		if r.Method == http.MethodPost {
			h.shutdown(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		if strings.HasPrefix(path, "/api/system/alerts/") && r.Method == http.MethodDelete {
			alertID := strings.TrimPrefix(path, "/api/system/alerts/")
			h.clearAlert(w, r, alertID)
		} else {
			jsonNotFound(w)
		}
	}
}

func (h *SystemHandler) getStatus(w http.ResponseWriter, r *http.Request) {
	alertCount := 0
	if h.mon != nil {
		alertCount = h.mon.AlertCount()
	}

	status := map[string]any{
		"alerts_active": alertCount,
		"zfs_pools":     zfs.GetPoolsSummary(),
	}

	json.NewEncoder(w).Encode(status)
}

// --- Tuning ---

type tuningParams struct {
	DirtyRatio           string `json:"dirty_ratio"`
	DirtyBackgroundRatio string `json:"dirty_background_ratio"`
	DirtyExpireCentisecs string `json:"dirty_expire_centisecs"`
	ZfsArcMax            string `json:"zfs_arc_max"`
	ZfsArcMin            string `json:"zfs_arc_min"`
	ZfsTxgTimeout        string `json:"zfs_txg_timeout"`
}

func (h *SystemHandler) getTuning(w http.ResponseWriter, r *http.Request) {
	params := tuningParams{
		DirtyRatio:           readSysctl("vm.dirty_ratio"),
		DirtyBackgroundRatio: readSysctl("vm.dirty_background_ratio"),
		DirtyExpireCentisecs: readSysctl("vm.dirty_expire_centisecs"),
		ZfsArcMax:            readZfsParam("zfs_arc_max"),
		ZfsArcMin:            readZfsParam("zfs_arc_min"),
		ZfsTxgTimeout:        readZfsParam("zfs_txg_timeout"),
	}

	json.NewEncoder(w).Encode(params)
}

func (h *SystemHandler) setTuning(w http.ResponseWriter, r *http.Request) {
	var req tuningParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	var errors []string

	// Page cache tuning (sysctl).
	if req.DirtyRatio != "" {
		if err := writeSysctl("vm.dirty_ratio", req.DirtyRatio); err != nil {
			errors = append(errors, err.Error())
		}
	}
	if req.DirtyBackgroundRatio != "" {
		if err := writeSysctl("vm.dirty_background_ratio", req.DirtyBackgroundRatio); err != nil {
			errors = append(errors, err.Error())
		}
	}
	if req.DirtyExpireCentisecs != "" {
		if err := writeSysctl("vm.dirty_expire_centisecs", req.DirtyExpireCentisecs); err != nil {
			errors = append(errors, err.Error())
		}
	}

	// ZFS ARC tuning.
	if req.ZfsArcMax != "" {
		if err := writeZfsParam("zfs_arc_max", req.ZfsArcMax); err != nil {
			errors = append(errors, err.Error())
		}
	}
	if req.ZfsArcMin != "" {
		if err := writeZfsParam("zfs_arc_min", req.ZfsArcMin); err != nil {
			errors = append(errors, err.Error())
		}
	}
	if req.ZfsTxgTimeout != "" {
		if err := writeZfsParam("zfs_txg_timeout", req.ZfsTxgTimeout); err != nil {
			errors = append(errors, err.Error())
		}
	}

	if len(errors) > 0 {
		fmt.Fprintf(w, `{"status":"partial","errors":["%s"]}`, strings.Join(errors, `","`))
		return
	}

	fmt.Fprintf(w, `{"status":"updated"}`)
}

// --- Alerts ---

func (h *SystemHandler) getAlerts(w http.ResponseWriter, r *http.Request) {
	if h.mon == nil {
		json.NewEncoder(w).Encode([]monitor.Alert{})
		return
	}

	alerts := h.mon.GetAlerts()
	if alerts == nil {
		alerts = []monitor.Alert{}
	}
	json.NewEncoder(w).Encode(alerts)
}

func (h *SystemHandler) getAlertCount(w http.ResponseWriter, r *http.Request) {
	count := 0
	if h.mon != nil {
		count = h.mon.AlertCount()
	}
	fmt.Fprintf(w, `{"count":%d}`, count)
}

func (h *SystemHandler) logAlert(w http.ResponseWriter, r *http.Request) {
	if h.mon == nil {
		jsonErrorCoded(w, "monitor not running", http.StatusInternalServerError, "system.monitor_not_running")
		return
	}

	var req logAlertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if req.Message == "" {
		jsonErrorCoded(w, "message required", http.StatusBadRequest, "system.alert_message_required")
		return
	}

	severity := req.Severity
	if severity != "warning" && severity != "critical" {
		severity = "warning"
	}

	source := req.Source
	if source == "" {
		source = "gui"
	}

	h.mon.AddAlert(monitor.Alert{
		Source:    source,
		Severity:  severity,
		Message:   req.Message,
		Device:    req.Device,
		Timestamp: time.Now(),
	})

	fmt.Fprintf(w, `{"status":"logged"}`)
}

func (h *SystemHandler) clearAlert(w http.ResponseWriter, r *http.Request, id string) {
	if h.mon == nil {
		jsonErrorCoded(w, "monitor not running", http.StatusInternalServerError, "system.monitor_not_running")
		return
	}
	h.mon.ClearAlert(id)
	fmt.Fprintf(w, `{"status":"cleared"}`)
}

// --- sysctl helpers ---

func readSysctl(key string) string {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func writeSysctl(key, value string) error {
	cmd := exec.Command("sysctl", "-w", key+"="+value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sysctl %s=%s: %s", key, value, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- ZFS param helpers ---

func readZfsParam(name string) string {
	path := "/sys/module/zfs/parameters/" + name
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeZfsParam(name, value string) error {
	path := "/sys/module/zfs/parameters/" + name
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if name == "zfs_arc_max" {
		content := fmt.Sprintf("options zfs zfs_arc_max=%s\n", value)
		_ = os.WriteFile("/etc/modprobe.d/smoothnas-zfs.conf", []byte(content), 0644)
	}
	return nil
}

// --- Update channel ---

// --- Power management ---

func (h *SystemHandler) reboot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"status":"ok"}`)
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("systemctl", "reboot").Run()
	}()
}

// startUpdate starts an operator update run (check or install). The run
// executes in its own systemd unit so it survives tierd restarting when the
// smoothnas package itself is upgraded.
func (h *SystemHandler) startUpdate(w http.ResponseWriter, action string) {
	if err := updater.Start(action); err != nil {
		status := http.StatusInternalServerError
		if err == updater.ErrRunning {
			status = http.StatusConflict
		}
		jsonError(w, err.Error(), status)
		return
	}
	fmt.Fprintf(w, `{"status":"started"}`)
}

// updateLog returns the update run log from ?offset= on, for the live view.
func (h *SystemHandler) updateLog(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	text, next, err := updater.ReadLog(offset)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"text": text, "next_offset": next})
}

func (h *SystemHandler) shutdown(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"status":"ok"}`)
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("systemctl", "poweroff").Run()
	}()
}
