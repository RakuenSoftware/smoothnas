package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/nonraid"
)

var listNonRaidDisks = disk.List

type nonRaidActivator interface {
	Activate(r *http.Request, store *db.Store, row *db.NonRaidArrayRow, destructive bool) error
	Deactivate(r *http.Request, store *db.Store, row *db.NonRaidArrayRow) error
}

// NonRaidHandler handles /api/nonraid/arrays.
type NonRaidHandler struct {
	store     *db.Store
	activator nonRaidActivator
}

func NewNonRaidHandler(store *db.Store) *NonRaidHandler {
	return &NonRaidHandler{store: store, activator: nonraid.NewRuntime()}
}

func (h *NonRaidHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/api/nonraid/arrays" || path == "/api/nonraid/arrays/":
		switch r.Method {
		case http.MethodGet:
			h.listArrays(w, r)
		case http.MethodPost:
			h.createArray(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
	case path == "/api/nonraid/arrays/validate":
		if r.Method == http.MethodPost {
			h.validateArray(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
	case strings.HasPrefix(path, "/api/nonraid/arrays/"):
		rest := strings.TrimPrefix(path, "/api/nonraid/arrays/")
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		if name == "" || strings.Contains(action, "/") {
			jsonNotFound(w)
			return
		}
		switch action {
		case "":
			switch r.Method {
			case http.MethodGet:
				h.getArray(w, r, name)
			case http.MethodDelete:
				h.deleteArray(w, r, name)
			default:
				jsonMethodNotAllowed(w)
			}
		case "activate":
			if r.Method == http.MethodPost {
				h.activateArray(w, r, name)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "deactivate":
			if r.Method == http.MethodPost {
				h.deactivateArray(w, r, name)
			} else {
				jsonMethodNotAllowed(w)
			}
		default:
			jsonNotFound(w)
		}
	default:
		jsonNotFound(w)
	}
}

type createNonRaidArrayRequest struct {
	Name        string   `json:"name"`
	Filesystem  string   `json:"filesystem,omitempty"`
	MountBase   string   `json:"mount_base,omitempty"`
	DataDisks   []string `json:"data_disks"`
	ParityDisks []string `json:"parity_disks"`
}

type activateNonRaidArrayRequest struct {
	Destructive bool `json:"destructive"`
}

type nonRaidArrayResponse struct {
	ID               int64                   `json:"id"`
	Name             string                  `json:"name"`
	State            string                  `json:"state"`
	UUID             string                  `json:"uuid"`
	Filesystem       string                  `json:"filesystem"`
	MountPath        string                  `json:"mount_path"`
	ParityCount      int                     `json:"parity_count"`
	MinParityBytes   uint64                  `json:"min_parity_bytes"`
	CapacityBytes    uint64                  `json:"capacity_bytes"`
	ErrorReason      string                  `json:"error_reason,omitempty"`
	Devices          []nonRaidDeviceResponse `json:"devices"`
	CreatedAt        string                  `json:"created_at"`
	UpdatedAt        string                  `json:"updated_at"`
	DataPlaneReady   bool                    `json:"data_plane_ready"`
	DataPlaneWarning string                  `json:"data_plane_warning,omitempty"`
}

type nonRaidDeviceResponse struct {
	ID                int64  `json:"id"`
	Role              string `json:"role"`
	Slot              int    `json:"slot"`
	DevicePath        string `json:"device_path"`
	VirtualDevicePath string `json:"virtual_device_path,omitempty"`
	Serial            string `json:"serial,omitempty"`
	SizeBytes         uint64 `json:"size_bytes"`
	UsableBytes       uint64 `json:"usable_bytes"`
	MountPath         string `json:"mount_path,omitempty"`
	State             string `json:"state"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func (h *NonRaidHandler) listArrays(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListNonRaidArrays()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]nonRaidArrayResponse, 0, len(rows))
	for i := range rows {
		out = append(out, nonRaidArrayToResponse(&rows[i]))
	}
	json.NewEncoder(w).Encode(out)
}

func (h *NonRaidHandler) getArray(w http.ResponseWriter, r *http.Request, name string) {
	row, err := h.store.GetNonRaidArray(name)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "nonRaid array not found", http.StatusNotFound, "nonraid.not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(nonRaidArrayToResponse(row))
}

func (h *NonRaidHandler) validateArray(w http.ResponseWriter, r *http.Request) {
	plan, ok := h.planFromRequest(w, r)
	if !ok {
		return
	}
	json.NewEncoder(w).Encode(plan)
}

func (h *NonRaidHandler) createArray(w http.ResponseWriter, r *http.Request) {
	plan, ok := h.planFromRequest(w, r)
	if !ok {
		return
	}

	row := &db.NonRaidArrayRow{
		Name:           plan.Name,
		State:          plan.State,
		UUID:           uuid.NewString(),
		Filesystem:     plan.Filesystem,
		MountPath:      plan.MountPath,
		ParityCount:    plan.ParityCount,
		MinParityBytes: plan.MinParityBytes,
		CapacityBytes:  plan.CapacityBytes,
	}
	devices := make([]db.NonRaidDeviceRow, 0, len(plan.Devices))
	for _, dev := range plan.Devices {
		devices = append(devices, db.NonRaidDeviceRow{
			Role:              dev.Role,
			Slot:              dev.Slot,
			DevicePath:        dev.DevicePath,
			VirtualDevicePath: dev.VirtualDevicePath,
			Serial:            dev.Serial,
			SizeBytes:         dev.SizeBytes,
			UsableBytes:       dev.UsableBytes,
			MountPath:         dev.MountPath,
			State:             dev.State,
		})
	}

	created, err := h.store.CreateNonRaidArray(row, devices)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "nonraid.create_failed")
		return
	}
	if h.activator != nil {
		if err := h.activator.Activate(r, h.store, created, true); err != nil {
			_ = h.store.SetNonRaidArrayState(created.Name, nonraid.StateError, err.Error())
			jsonErrorCoded(w, err.Error(), http.StatusInternalServerError, "nonraid.activate_failed")
			return
		}
		if refreshed, err := h.store.GetNonRaidArray(created.Name); err == nil {
			created = refreshed
		}
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(nonRaidArrayToResponse(created))
}

func (h *NonRaidHandler) deleteArray(w http.ResponseWriter, r *http.Request, name string) {
	row, err := h.store.GetNonRaidArray(name)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "nonRaid array not found", http.StatusNotFound, "nonraid.not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if row.State == nonraid.StateActive && h.activator != nil {
		if err := h.activator.Deactivate(r, h.store, row); err != nil {
			jsonErrorCoded(w, err.Error(), http.StatusConflict, "nonraid.deactivate_failed")
			return
		}
	}
	if err := h.store.DeleteNonRaidArray(name); err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func (h *NonRaidHandler) activateArray(w http.ResponseWriter, r *http.Request, name string) {
	row, err := h.store.GetNonRaidArray(name)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "nonRaid array not found", http.StatusNotFound, "nonraid.not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	var req activateNonRaidArrayRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if h.activator == nil {
		jsonErrorCoded(w, "nonRaid activator is not available", http.StatusServiceUnavailable, "nonraid.activator_unavailable")
		return
	}
	if err := h.activator.Activate(r, h.store, row, req.Destructive); err != nil {
		_ = h.store.SetNonRaidArrayState(row.Name, nonraid.StateError, err.Error())
		jsonErrorCoded(w, err.Error(), http.StatusInternalServerError, "nonraid.activate_failed")
		return
	}
	refreshed, err := h.store.GetNonRaidArray(name)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(nonRaidArrayToResponse(refreshed))
}

func (h *NonRaidHandler) deactivateArray(w http.ResponseWriter, r *http.Request, name string) {
	row, err := h.store.GetNonRaidArray(name)
	if errors.Is(err, db.ErrNotFound) {
		jsonErrorCoded(w, "nonRaid array not found", http.StatusNotFound, "nonraid.not_found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if h.activator == nil {
		jsonErrorCoded(w, "nonRaid activator is not available", http.StatusServiceUnavailable, "nonraid.activator_unavailable")
		return
	}
	if err := h.activator.Deactivate(r, h.store, row); err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusConflict, "nonraid.deactivate_failed")
		return
	}
	refreshed, err := h.store.GetNonRaidArray(name)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(nonRaidArrayToResponse(refreshed))
}

func (h *NonRaidHandler) planFromRequest(w http.ResponseWriter, r *http.Request) (*nonraid.Plan, bool) {
	var req createNonRaidArrayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return nil, false
	}

	devices, err := candidateNonRaidDevices(h.store)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	data, err := resolveNonRaidDevices(req.DataDisks, devices)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "nonraid.invalid_layout")
		return nil, false
	}
	parity, err := resolveNonRaidDevices(req.ParityDisks, devices)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "nonraid.invalid_layout")
		return nil, false
	}

	plan, err := nonraid.BuildPlan(req.Name, req.Filesystem, req.MountBase, data, parity)
	if err != nil {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, "nonraid.invalid_layout")
		return nil, false
	}
	return plan, true
}

func candidateNonRaidDevices(store *db.Store) (map[string]nonraid.Device, error) {
	disks, err := listNonRaidDisks()
	if err != nil {
		return nil, err
	}
	reserved := map[string]struct{}{}
	if store != nil {
		rows, err := store.ListNonRaidDevices("")
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			reserved[disk.BaseDiskPath(row.DevicePath)] = struct{}{}
		}
	}

	out := make(map[string]nonraid.Device, len(disks)*2)
	for _, d := range disks {
		base := disk.BaseDiskPath(d.Path)
		if _, ok := reserved[base]; ok {
			continue
		}
		if d.Assignment != "" && d.Assignment != "unassigned" {
			continue
		}
		dev := nonraid.Device{Path: d.Path, Serial: d.Serial, Size: d.Size}
		out[d.Path] = dev
		out[base] = dev
		out["/dev/"+d.Name] = dev
	}
	return out, nil
}

func resolveNonRaidDevices(paths []string, candidates map[string]nonraid.Device) ([]nonraid.Device, error) {
	out := make([]nonraid.Device, 0, len(paths))
	for _, path := range paths {
		dev, ok := candidates[path]
		if !ok {
			return nil, fmt.Errorf("disk %s is not available for nonRaid", path)
		}
		out = append(out, dev)
	}
	return out, nil
}

func nonRaidArrayToResponse(row *db.NonRaidArrayRow) nonRaidArrayResponse {
	out := nonRaidArrayResponse{
		ID:             row.ID,
		Name:           row.Name,
		State:          row.State,
		UUID:           row.UUID,
		Filesystem:     row.Filesystem,
		MountPath:      row.MountPath,
		ParityCount:    row.ParityCount,
		MinParityBytes: row.MinParityBytes,
		CapacityBytes:  row.CapacityBytes,
		ErrorReason:    row.ErrorReason,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		DataPlaneReady: row.State == nonraid.StateActive,
	}
	if out.State != nonraid.StateActive {
		out.DataPlaneWarning = "nonRaid array is not active"
	}
	for _, dev := range row.Devices {
		out.Devices = append(out.Devices, nonRaidDeviceResponse{
			ID:                dev.ID,
			Role:              dev.Role,
			Slot:              dev.Slot,
			DevicePath:        dev.DevicePath,
			VirtualDevicePath: dev.VirtualDevicePath,
			Serial:            dev.Serial,
			SizeBytes:         dev.SizeBytes,
			UsableBytes:       dev.UsableBytes,
			MountPath:         dev.MountPath,
			State:             dev.State,
			CreatedAt:         dev.CreatedAt,
			UpdatedAt:         dev.UpdatedAt,
		})
	}
	return out
}
