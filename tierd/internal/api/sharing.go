package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/firewall"
	"github.com/JBailes/SmoothNAS/tierd/internal/iscsi"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/network"
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
	"github.com/google/uuid"
)

// SharingHandler handles /api/protocols*, /api/smb/*, /api/nfs/*, /api/iscsi/* endpoints.
type SharingHandler struct {
	store          *db.Store
	protocolsCache *cache.Entry[[]protocolStatus]
}

type iscsiLUNMoveIntent struct {
	IQN             string `json:"iqn"`
	BackingFile     string `json:"backing_file"`
	DestinationTier string `json:"destination_tier"`
	State           string `json:"state"`
	StateUpdatedAt  string `json:"state_updated_at,omitempty"`
	Reason          string `json:"reason,omitempty"`
	CreatedAt       string `json:"created_at"`
}

// Active-LUN movement state machine values (Phase 8).
//
// Forward path:
//
//   planned → executing → unpinned → moving → cutover → repinning → completed
//
// Any error transitions to `failed` with a recorded reason. Operators
// can `abort` from any non-terminal state to drop back to `planned`.
const (
	iscsiLUNMoveIntentStatePlanned   = "planned"
	iscsiLUNMoveIntentStateExecuting = "executing"
	iscsiLUNMoveIntentStateUnpinned  = "unpinned"
	iscsiLUNMoveIntentStateMoving    = "moving"
	iscsiLUNMoveIntentStateCutover   = "cutover"
	iscsiLUNMoveIntentStateRepinning = "repinning"
	iscsiLUNMoveIntentStateCompleted = "completed"
	iscsiLUNMoveIntentStateFailed    = "failed"
)

// iscsiLUNMoveIntentNonTerminal returns true if the intent is in a
// state that the executor can still drive forward. Terminal states
// are `planned`, `completed`, `failed`. Used by the crash-recovery
// sweep to skip intents that don't need recovery action.
func iscsiLUNMoveIntentNonTerminal(state string) bool {
	switch state {
	case iscsiLUNMoveIntentStateExecuting,
		iscsiLUNMoveIntentStateUnpinned,
		iscsiLUNMoveIntentStateMoving,
		iscsiLUNMoveIntentStateCutover,
		iscsiLUNMoveIntentStateRepinning:
		return true
	}
	return false
}

// iscsiLUNMoveIntentAbortable returns true if `abort` should accept
// the intent and drop it back to `planned`. That's any in-flight
// state plus `failed` (so an operator can retry without first
// having to `clear` + re-record intent). `planned` is refused (no
// forward progress to roll back) and `completed` is refused (clear
// is the right tool to discard a successful move).
func iscsiLUNMoveIntentAbortable(state string) bool {
	if iscsiLUNMoveIntentNonTerminal(state) {
		return true
	}
	return state == iscsiLUNMoveIntentStateFailed
}

var (
	enableNFSServiceForExports = nfs.EnableService
	applyFirewallForExports    = firewall.Apply
	enabledProtocolsForExports = firewall.GetEnabledProtocols
	writeNFSExports            = nfs.WriteExports
	writeSMBConfig             = smb.WriteConfigWithOptions
	inspectLUNPin              = iscsi.InspectLUNPin
	quiesceISCSITarget         = iscsi.QuiesceTarget
	resumeISCSITarget          = iscsi.ResumeTarget
)

const smbCompatibilityModeConfigKey = "smb.compatibility_mode"

func NewSharingHandler(store *db.Store) *SharingHandler {
	return &SharingHandler{
		store:          store,
		protocolsCache: cache.New[[]protocolStatus](60 * time.Second),
	}
}

// ReconcileSharingConfig rewrites service config from the DB. It is used at
// daemon startup so generator changes are applied without requiring an edit.
//
// Phase 8c also runs the active-LUN move-intent crash-recovery sweep here:
// every file-backed iSCSI target with an intent stuck in a non-terminal
// state has its backing file best-effort re-pinned and the intent marked
// `failed`, so a tierd restart mid-execution never leaves a live LIO
// target on an unpinned backing file.
func ReconcileSharingConfig(store *db.Store) error {
	h := NewSharingHandler(store)
	if err := h.regenerateSmbConf(); err != nil {
		return fmt.Errorf("regenerate smb config: %w", err)
	}
	if err := h.regenerateExports(); err != nil {
		return fmt.Errorf("regenerate nfs exports: %w", err)
	}
	if err := recoverActiveLUNMoveIntents(h); err != nil {
		return fmt.Errorf("recover active-lun move intents: %w", err)
	}
	return nil
}

// Route dispatches sharing requests.
func (h *SharingHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/protocols"):
		h.routeProtocols(w, r)
	case strings.HasPrefix(path, "/api/smb/"):
		h.routeSMB(w, r)
	case strings.HasPrefix(path, "/api/nfs/"):
		h.routeNFS(w, r)
	case strings.HasPrefix(path, "/api/iscsi/"):
		h.routeISCSI(w, r)
	case path == "/api/filesystem/paths" || path == "/api/filesystem/paths/":
		h.listFilesystemPaths(w, r)
	case path == "/api/filesystem/browse" || path == "/api/filesystem/browse/":
		h.browseFilesystem(w, r)
	default:
		jsonNotFound(w)
	}
}

// --- Protocols ---

type protocolStatus struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func (h *SharingHandler) routeProtocols(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/protocols" || path == "/api/protocols/" {
		if r.Method == http.MethodGet {
			h.listProtocols(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
		return
	}

	rest := strings.TrimPrefix(path, "/api/protocols/")
	if r.Method == http.MethodPut {
		h.toggleProtocol(w, r, rest)
	} else {
		jsonMethodNotAllowed(w)
	}
}

func fetchProtocols() ([]protocolStatus, error) {
	return []protocolStatus{
		{Name: "smb", Enabled: smb.IsEnabled()},
		{Name: "nfs", Enabled: nfs.IsEnabled()},
		{Name: "iscsi", Enabled: iscsi.IsEnabled()},
	}, nil
}

func (h *SharingHandler) listProtocols(w http.ResponseWriter, r *http.Request) {
	protocols, _ := h.protocolsCache.GetOrFetch(fetchProtocols)
	json.NewEncoder(w).Encode(protocols)
}

func (h *SharingHandler) toggleProtocol(w http.ResponseWriter, r *http.Request, proto string) {
	if err := firewall.ValidateProtocol(proto); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	var err error
	if req.Enabled {
		switch proto {
		case "smb":
			if err = h.regenerateSmbConf(); err != nil {
				serverError(w, err)
				return
			}
			err = smb.EnableService()
		case "nfs":
			err = nfs.EnableService(true)
		case "iscsi":
			err = iscsi.EnableService()
		}
	} else {
		switch proto {
		case "smb":
			err = smb.DisableService()
		case "nfs":
			err = nfs.DisableService()
		case "iscsi":
			err = iscsi.DisableService()
		}
	}

	if err != nil {
		serverError(w, err)
		return
	}

	enabled := firewall.GetEnabledProtocols()
	if fwErr := firewall.Apply(enabled); fwErr != nil {
		fmt.Fprintf(w, `{"status":"toggled","firewall_warning":"%s"}`, fwErr)
		return
	}

	h.protocolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"toggled","protocol":"%s","enabled":%t}`, proto, req.Enabled)
}

// --- SMB ---

func (h *SharingHandler) routeSMB(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/smb/config" || path == "/api/smb/config/" {
		switch r.Method {
		case http.MethodGet:
			h.getSMBConfig(w, r)
		case http.MethodPut:
			h.updateSMBConfig(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	if path == "/api/smb/shares" || path == "/api/smb/shares/" {
		switch r.Method {
		case http.MethodGet:
			h.listSMBShares(w, r)
		case http.MethodPost:
			h.createSMBShare(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	if strings.HasPrefix(path, "/api/smb/shares/") {
		name := strings.TrimPrefix(path, "/api/smb/shares/")
		switch r.Method {
		case http.MethodDelete:
			h.deleteSMBShare(w, r, name)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	jsonNotFound(w)
}

type smbConfigResponse struct {
	CompatibilityMode  bool `json:"compatibility_mode"`
	PerformanceMode    bool `json:"performance_mode"`
	SmoothFSVFSEnabled bool `json:"smoothfs_vfs_enabled"`
	SmoothFSVFSFound   bool `json:"smoothfs_vfs_available"`
}

func (h *SharingHandler) currentSMBOptions() (smb.Options, error) {
	compatibility, err := h.store.GetBoolConfig(smbCompatibilityModeConfigKey, false)
	if err != nil {
		return smb.Options{}, err
	}
	return smb.Options{
		SmoothFSVFS:       smb.SmoothFSVFSEnabled(),
		CompatibilityMode: compatibility,
		// Best-effort population of the active IP set so SMB
		// Multichannel can advertise per-NIC IPs in the broken-
		// bond shape. ListActiveIPs swallows network-probe errors
		// (the function returns an empty slice if `ip` isn't
		// reachable) so an unreachable network layer doesn't
		// block smb.conf regeneration on a fresh appliance.
		Interfaces: network.ListActiveIPv4(),
	}, nil
}

func (h *SharingHandler) smbConfigResponse() (smbConfigResponse, error) {
	opts, err := h.currentSMBOptions()
	if err != nil {
		return smbConfigResponse{}, err
	}
	return smbConfigResponse{
		CompatibilityMode:  opts.CompatibilityMode,
		PerformanceMode:    !opts.CompatibilityMode,
		SmoothFSVFSEnabled: opts.SmoothFSVFS,
		SmoothFSVFSFound:   smb.SmoothFSVFSInstalled(),
	}, nil
}

func (h *SharingHandler) getSMBConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := h.smbConfigResponse()
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *SharingHandler) updateSMBConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CompatibilityMode bool `json:"compatibility_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if err := h.store.SetBoolConfig(smbCompatibilityModeConfigKey, req.CompatibilityMode); err != nil {
		serverError(w, err)
		return
	}
	if err := h.regenerateSmbConf(); err != nil {
		serverError(w, err)
		return
	}
	resp, err := h.smbConfigResponse()
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *SharingHandler) listSMBShares(w http.ResponseWriter, r *http.Request) {
	shares, err := h.store.ListSmbShares()
	if err != nil {
		serverError(w, err)
		return
	}
	if shares == nil {
		shares = []db.SmbShare{}
	}
	json.NewEncoder(w).Encode(shares)
}

func (h *SharingHandler) createSMBShare(w http.ResponseWriter, r *http.Request) {
	var req db.SmbShare
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if err := smb.ValidateShareName(req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := smb.ValidateSharePath(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	share, err := h.store.CreateSmbShare(req)
	if err != nil {
		if err == db.ErrDuplicate {
			jsonErrorCoded(w, "share name already exists", http.StatusConflict, "sharing.share_name_taken")
		} else {
			serverError(w, err)
		}
		return
	}

	// Regenerate smb.conf from all shares.
	if err := h.regenerateSmbConf(); err != nil {
		serverError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(share)
}

func (h *SharingHandler) deleteSMBShare(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.store.DeleteSmbShare(name); err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "share not found", http.StatusNotFound, "sharing.share_not_found")
		} else {
			serverError(w, err)
		}
		return
	}

	if err := h.regenerateSmbConf(); err != nil {
		serverError(w, err)
		return
	}
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

func (h *SharingHandler) regenerateSmbConf() error {
	shares, err := h.store.ListSmbShares()
	if err != nil {
		return err
	}
	opts, err := h.currentSMBOptions()
	if err != nil {
		return err
	}

	var smbShares []smb.Share
	for _, s := range shares {
		sh := smb.Share{
			Name:     s.Name,
			Path:     s.Path,
			ReadOnly: s.ReadOnly,
			GuestOK:  s.GuestOK,
			Comment:  s.Comment,
		}
		if s.AllowUsers != "" {
			sh.AllowUsers = strings.Split(s.AllowUsers, ",")
		}
		smbShares = append(smbShares, sh)
	}

	return writeSMBConfig(smbShares, "smoothnas", opts)
}

// --- NFS ---

func (h *SharingHandler) routeNFS(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/nfs/exports" || path == "/api/nfs/exports/" {
		switch r.Method {
		case http.MethodGet:
			h.listNFSExports(w, r)
		case http.MethodPost:
			h.createNFSExport(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	if strings.HasPrefix(path, "/api/nfs/exports/") {
		idStr := strings.TrimPrefix(path, "/api/nfs/exports/")
		switch r.Method {
		case http.MethodDelete:
			h.deleteNFSExport(w, r, idStr)
		case http.MethodPatch:
			h.updateNFSExport(w, r, idStr)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	jsonNotFound(w)
}

func (h *SharingHandler) listNFSExports(w http.ResponseWriter, r *http.Request) {
	exports, err := h.store.ListNfsExports()
	if err != nil {
		serverError(w, err)
		return
	}
	if exports == nil {
		exports = []db.NfsExport{}
	}
	json.NewEncoder(w).Encode(exports)
}

func (h *SharingHandler) createNFSExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path       string   `json:"path"`
		Networks   []string `json:"networks"`
		Sync       bool     `json:"sync"`
		RootSquash bool     `json:"root_squash"`
		ReadOnly   bool     `json:"read_only"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	if err := nfs.ValidateExportPath(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, net := range req.Networks {
		if err := nfs.ValidateNetwork(net); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	st, err := os.Stat(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			jsonError(w, fmt.Sprintf("export path does not exist: %s", req.Path), http.StatusBadRequest)
			return
		}
		serverError(w, fmt.Errorf("stat export path: %w", err))
		return
	}
	if !st.IsDir() {
		jsonError(w, fmt.Sprintf("export path is not a directory: %s", req.Path), http.StatusBadRequest)
		return
	}

	if err := ensureNFSExportServing(); err != nil {
		serverError(w, err)
		return
	}

	exp, err := h.store.CreateNfsExport(db.NfsExport{
		Path:       req.Path,
		Networks:   strings.Join(req.Networks, ","),
		Sync:       req.Sync,
		RootSquash: req.RootSquash,
		ReadOnly:   req.ReadOnly,
	})
	if err != nil {
		serverError(w, err)
		return
	}

	if err := h.regenerateExports(); err != nil {
		serverError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(exp)
}

func ensureNFSExportServing() error {
	if err := enableNFSServiceForExports(true); err != nil {
		return err
	}
	enabled := enabledProtocolsForExports()
	enabled["nfs"] = true
	return applyFirewallForExports(enabled)
}

func (h *SharingHandler) deleteNFSExport(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonErrorCoded(w, "invalid export id", http.StatusBadRequest, "sharing.invalid_export_id")
		return
	}

	if err := h.store.DeleteNfsExport(id); err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "export not found", http.StatusNotFound, "sharing.export_not_found")
		} else {
			serverError(w, err)
		}
		return
	}

	if err := h.regenerateExports(); err != nil {
		serverError(w, err)
		return
	}
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

func (h *SharingHandler) updateNFSExport(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonErrorCoded(w, "invalid export id", http.StatusBadRequest, "sharing.invalid_export_id")
		return
	}

	var req struct {
		Sync bool `json:"sync"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	exp, err := h.store.UpdateNfsExportSync(id, req.Sync)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "export not found", http.StatusNotFound, "sharing.export_not_found")
		} else {
			serverError(w, err)
		}
		return
	}

	if err := h.regenerateExports(); err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(exp)
}

func (h *SharingHandler) regenerateExports() error {
	dbExports, err := h.store.ListNfsExports()
	if err != nil {
		return err
	}

	pools, err := h.store.ListSmoothfsPools()
	if err != nil {
		return err
	}

	exports := buildNFSExports(dbExports, pools)
	return writeNFSExports(exports)
}

func buildNFSExports(dbExports []db.NfsExport, pools []db.SmoothfsPool) []nfs.Export {
	exports := make([]nfs.Export, 0, len(dbExports))
	for _, e := range dbExports {
		exp := nfs.Export{
			Path:       e.Path,
			Networks:   strings.Split(e.Networks, ","),
			Sync:       e.Sync,
			RootSquash: e.RootSquash,
			ReadOnly:   e.ReadOnly,
		}
		if pool := smoothfsPoolForPath(e.Path, pools); pool != nil {
			if id, err := uuid.Parse(pool.UUID); err == nil {
				exp.Fsid = nfs.SmoothfsExportFsidOption(id, pool.Mountpoint, e.Path)
			}
		}
		exports = append(exports, exp)
	}
	return exports
}

// --- iSCSI ---

func (h *SharingHandler) routeISCSI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/iscsi/targets" || path == "/api/iscsi/targets/" {
		switch r.Method {
		case http.MethodGet:
			h.listISCSITargets(w, r)
		case http.MethodPost:
			h.createISCSITarget(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	if strings.HasPrefix(path, "/api/iscsi/targets/") {
		rest := strings.TrimPrefix(path, "/api/iscsi/targets/")
		parts := strings.SplitN(rest, "/", 2)
		targetIQN := parts[0]
		subpath := ""
		if len(parts) > 1 {
			subpath = parts[1]
		}

		switch subpath {
		case "":
			if r.Method == http.MethodDelete {
				h.deleteISCSITarget(w, r, targetIQN)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "acls":
			switch r.Method {
			case http.MethodGet:
				h.listISCSIACLs(w, r, targetIQN)
			case http.MethodPost:
				h.addISCSIACL(w, r, targetIQN)
			default:
				jsonMethodNotAllowed(w)
			}
		case "quiesce":
			if r.Method == http.MethodPost {
				h.setISCSIFileTargetQuiesced(w, r, targetIQN, true)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "resume":
			if r.Method == http.MethodPost {
				h.setISCSIFileTargetQuiesced(w, r, targetIQN, false)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "move-intent":
			switch r.Method {
			case http.MethodPost:
				h.createISCSIFileTargetMoveIntent(w, r, targetIQN)
			case http.MethodDelete:
				h.clearISCSIFileTargetMoveIntent(w, r, targetIQN)
			default:
				jsonMethodNotAllowed(w)
			}
		case "move-intent/execute":
			if r.Method == http.MethodPost {
				h.executeISCSIFileTargetMoveIntent(w, r, targetIQN)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "move-intent/abort":
			if r.Method == http.MethodPost {
				h.abortISCSIFileTargetMoveIntent(w, r, targetIQN)
			} else {
				jsonMethodNotAllowed(w)
			}
		default:
			if strings.HasPrefix(subpath, "acls/") {
				aclIQN := strings.TrimPrefix(subpath, "acls/")
				if r.Method == http.MethodDelete {
					h.removeISCSIACL(w, r, targetIQN, aclIQN)
				} else {
					jsonMethodNotAllowed(w)
				}
			} else {
				jsonNotFound(w)
			}
		}
		return
	}

	jsonNotFound(w)
}

func (h *SharingHandler) listISCSITargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.store.ListIscsiTargets()
	if err != nil {
		serverError(w, err)
		return
	}
	if targets == nil {
		targets = []db.IscsiTarget{}
	}

	type iscsiTargetResponse struct {
		db.IscsiTarget
		LUNPin     *iscsi.LUNPinStatus `json:"lun_pin,omitempty"`
		Quiesced   bool                `json:"quiesced"`
		MoveIntent *iscsiLUNMoveIntent `json:"move_intent,omitempty"`
	}
	resp := make([]iscsiTargetResponse, 0, len(targets))
	for _, target := range targets {
		item := iscsiTargetResponse{IscsiTarget: target}
		if target.BackingType == db.IscsiBackingFile {
			status := inspectLUNPin(target.BlockDevice)
			item.LUNPin = &status
			quiesced, err := h.store.GetBoolConfig(iscsiTargetQuiescedConfigKey(target.IQN), false)
			if err != nil {
				serverError(w, err)
				return
			}
			item.Quiesced = quiesced
			intent, err := h.getISCSIFileTargetMoveIntent(target.IQN)
			if err != nil {
				serverError(w, err)
				return
			}
			item.MoveIntent = intent
		}
		resp = append(resp, item)
	}
	json.NewEncoder(w).Encode(resp)
}

// createTargetRequest accepts either a block-backed LUN (set
// BlockDevice to /dev/...) or a file-backed LUN (set BackingFile
// to an absolute path of a regular file).  Exactly one must be
// populated; setting both is a 400.  BackingType is derived from
// which field is set and is not part of the request surface.
type createTargetRequest struct {
	IQN         string `json:"iqn"`
	BlockDevice string `json:"block_device,omitempty"`
	BackingFile string `json:"backing_file,omitempty"`
	CHAPUser    string `json:"chap_user"`
	CHAPPass    string `json:"chap_pass"`
}

func (h *SharingHandler) createISCSITarget(w http.ResponseWriter, r *http.Request) {
	var req createTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.IQN == "" {
		jsonErrorCoded(w, "iqn required", http.StatusBadRequest, "sharing.iqn_required")
		return
	}
	if (req.BlockDevice == "" && req.BackingFile == "") ||
		(req.BlockDevice != "" && req.BackingFile != "") {
		jsonErrorCoded(w, "exactly one of block_device or backing_file required", http.StatusBadRequest, "sharing.target_backing_required")
		return
	}

	if err := iscsi.ValidateIQN(req.IQN); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	backingType := db.IscsiBackingBlock
	backingPath := req.BlockDevice
	if req.BackingFile != "" {
		backingType = db.IscsiBackingFile
		backingPath = req.BackingFile
		if err := iscsi.ValidateBackingFilePath(req.BackingFile); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if err := iscsi.ValidateBlockDevice(req.BlockDevice); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Persist to SQLite.
	target, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         req.IQN,
		BlockDevice: backingPath,
		BackingType: backingType,
		CHAPUser:    req.CHAPUser,
		CHAPPass:    req.CHAPPass,
	})
	if err != nil {
		if err == db.ErrDuplicate {
			jsonErrorCoded(w, "target IQN already exists", http.StatusConflict, "sharing.target_iqn_taken")
		} else {
			serverError(w, err)
		}
		return
	}

	// Create via targetcli — dispatch on backing type. CreateFileBackedTarget
	// auto-pins PIN_LUN when the backing file is on a smoothfs mount
	// (§6.5), matching the Phase 0 §iSCSI "pinned by default" ruling.
	var cerr error
	switch backingType {
	case db.IscsiBackingFile:
		cerr = iscsi.CreateFileBackedTarget(req.IQN, backingPath)
	default:
		cerr = iscsi.CreateTarget(req.IQN, backingPath)
	}
	if cerr != nil {
		// Rollback SQLite on targetcli failure.
		h.store.DeleteIscsiTarget(req.IQN)
		jsonError(w, cerr.Error(), http.StatusBadRequest)
		return
	}

	if req.CHAPUser != "" && req.CHAPPass != "" {
		iscsi.SetCHAP(req.IQN, req.CHAPUser, req.CHAPPass)
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(target)
}

func (h *SharingHandler) deleteISCSITarget(w http.ResponseWriter, r *http.Request, iqn string) {
	// Look up the persisted row so we pick the right teardown path
	// (file-backed vs block-backed). Fall back to the block-backed
	// destructor on any lookup failure — it's what pre-7.5 targets
	// carry and the only safe default if somehow the row is gone
	// but targetcli state still exists.
	target, err := h.store.GetIscsiTarget(iqn)
	switch {
	case err == db.ErrNotFound:
		// Fall through: try to clean targetcli anyway, then 404.
		iscsi.DestroyTarget(iqn)
		jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
		return
	case err != nil:
		serverError(w, err)
		return
	}

	switch target.BackingType {
	case db.IscsiBackingFile:
		iscsi.DestroyFileBackedTarget(iqn, target.BlockDevice)
		_ = h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), false)
		_ = h.store.DeleteConfig(iscsiTargetMoveIntentConfigKey(iqn))
	default:
		iscsi.DestroyTarget(iqn)
	}

	// Remove from SQLite.
	if err := h.store.DeleteIscsiTarget(iqn); err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
		} else {
			serverError(w, err)
		}
		return
	}

	fmt.Fprintf(w, `{"status":"destroyed"}`)
}

func (h *SharingHandler) setISCSIFileTargetQuiesced(w http.ResponseWriter, _ *http.Request, iqn string, quiesce bool) {
	target, err := h.store.GetIscsiTarget(iqn)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
			return
		}
		serverError(w, err)
		return
	}
	if target.BackingType != db.IscsiBackingFile {
		jsonErrorCoded(w, "only file-backed iSCSI targets can be quiesced for smoothfs movement", http.StatusBadRequest, "sharing.target_must_be_file_backed_quiesce")
		return
	}

	pinStatus := inspectLUNPin(target.BlockDevice)
	if pinStatus.OnSmoothfs && !pinStatus.Pinned {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "smoothfs file-backed LUN is not pinned",
			"lun_pin": pinStatus,
		})
		return
	}

	action := resumeISCSITarget
	state := "resumed"
	if quiesce {
		action = quiesceISCSITarget
		state = "quiesced"
	}
	if err := action(iqn); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), quiesce); err != nil {
		serverError(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       state,
		"iqn":          target.IQN,
		"backing_file": target.BlockDevice,
		"lun_pin":      pinStatus,
		"quiesced":     quiesce,
	})
}

func iscsiTargetQuiescedConfigKey(iqn string) string {
	return "iscsi.target." + iqn + ".quiesced"
}

func (h *SharingHandler) createISCSIFileTargetMoveIntent(w http.ResponseWriter, r *http.Request, iqn string) {
	var req struct {
		DestinationTier string `json:"destination_tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	req.DestinationTier = strings.TrimSpace(req.DestinationTier)
	if req.DestinationTier == "" {
		jsonErrorCoded(w, "destination_tier required", http.StatusBadRequest, "sharing.destination_tier_required")
		return
	}

	target, pinStatus, ok := h.validateISCSIFileTargetMovePreflight(w, iqn)
	if !ok {
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	intent := iscsiLUNMoveIntent{
		IQN:             target.IQN,
		BackingFile:     target.BlockDevice,
		DestinationTier: req.DestinationTier,
		State:           iscsiLUNMoveIntentStatePlanned,
		StateUpdatedAt:  now,
		CreatedAt:       now,
	}
	if err := h.persistISCSIFileTargetMoveIntent(iqn, intent); err != nil {
		serverError(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "move intent recorded",
		"move_intent": intent,
		"lun_pin":     pinStatus,
		"quiesced":    true,
	})
}

func (h *SharingHandler) clearISCSIFileTargetMoveIntent(w http.ResponseWriter, _ *http.Request, iqn string) {
	target, err := h.store.GetIscsiTarget(iqn)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
			return
		}
		serverError(w, err)
		return
	}
	if target.BackingType != db.IscsiBackingFile {
		jsonErrorCoded(w, "only file-backed iSCSI targets can have smoothfs move intent", http.StatusBadRequest, "sharing.target_must_be_file_backed_intent")
		return
	}
	if err := h.store.DeleteConfig(iscsiTargetMoveIntentConfigKey(iqn)); err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "move intent cleared",
		"iqn":    iqn,
	})
}

// iscsiLUNMoveIntentExecutorStartedReason is the journal reason
// recorded the moment the execute endpoint flips the intent from
// `planned` to `executing`. The async executor overwrites this with
// step-specific reasons as it progresses; the value is observable
// for the brief window between journal and goroutine kickoff, plus
// permanently if the executor is mocked out (tests).
const iscsiLUNMoveIntentExecutorStartedReason = "executor started"

func (h *SharingHandler) executeISCSIFileTargetMoveIntent(w http.ResponseWriter, _ *http.Request, iqn string) {
	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil {
		serverError(w, err)
		return
	}
	if intent == nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "move intent must be recorded before execution",
			"iqn":   iqn,
		})
		return
	}
	if intent.State != iscsiLUNMoveIntentStatePlanned {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       "move intent is not in planned state; abort or clear before retrying",
			"move_intent": intent,
		})
		return
	}

	_, pinStatus, ok := h.validateISCSIFileTargetMovePreflight(w, iqn)
	if !ok {
		return
	}

	intent.State = iscsiLUNMoveIntentStateExecuting
	intent.StateUpdatedAt = time.Now().UTC().Format(time.RFC3339)
	intent.Reason = iscsiLUNMoveIntentExecutorStartedReason
	if err := h.persistISCSIFileTargetMoveIntent(iqn, *intent); err != nil {
		serverError(w, err)
		return
	}

	// Drive the rest of the state machine asynchronously so the HTTP
	// request can return promptly with 202; operators poll via
	// GET /api/iscsi/targets to see progress through the journal.
	// The executor function is overridable for tests.
	go runActiveLUNMoveImpl(context.Background(), h, iqn)

	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "move intent journaled; executor started",
		"move_intent": intent,
		"lun_pin":     pinStatus,
		"quiesced":    true,
	})
}

func (h *SharingHandler) abortISCSIFileTargetMoveIntent(w http.ResponseWriter, _ *http.Request, iqn string) {
	target, err := h.store.GetIscsiTarget(iqn)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
			return
		}
		serverError(w, err)
		return
	}
	if target.BackingType != db.IscsiBackingFile {
		jsonErrorCoded(w, "only file-backed iSCSI targets can have smoothfs move intent", http.StatusBadRequest, "sharing.target_must_be_file_backed_intent")
		return
	}
	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil {
		serverError(w, err)
		return
	}
	if intent == nil {
		jsonErrorCoded(w, "no move intent recorded", http.StatusConflict, "sharing.no_move_intent")
		return
	}
	if !iscsiLUNMoveIntentAbortable(intent.State) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       "move intent is in a terminal state; clear it instead of aborting",
			"move_intent": intent,
		})
		return
	}

	intent.State = iscsiLUNMoveIntentStatePlanned
	intent.StateUpdatedAt = time.Now().UTC().Format(time.RFC3339)
	intent.Reason = "operator abort"
	if err := h.persistISCSIFileTargetMoveIntent(iqn, *intent); err != nil {
		serverError(w, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "move intent aborted",
		"move_intent": intent,
	})
}

func (h *SharingHandler) persistISCSIFileTargetMoveIntent(iqn string, intent iscsiLUNMoveIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data))
}

func (h *SharingHandler) validateISCSIFileTargetMovePreflight(w http.ResponseWriter, iqn string) (*db.IscsiTarget, iscsi.LUNPinStatus, bool) {
	target, err := h.store.GetIscsiTarget(iqn)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "target not found", http.StatusNotFound, "sharing.target_not_found")
			return nil, iscsi.LUNPinStatus{}, false
		}
		serverError(w, err)
		return nil, iscsi.LUNPinStatus{}, false
	}
	if target.BackingType != db.IscsiBackingFile {
		jsonErrorCoded(w, "only file-backed iSCSI targets can be moved by smoothfs active-LUN movement", http.StatusBadRequest, "sharing.target_must_be_file_backed_active_lun")
		return nil, iscsi.LUNPinStatus{}, false
	}
	quiesced, err := h.store.GetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), false)
	if err != nil {
		serverError(w, err)
		return nil, iscsi.LUNPinStatus{}, false
	}
	if !quiesced {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":    "file-backed LUN must be quiesced before recording move intent",
			"quiesced": false,
		})
		return nil, iscsi.LUNPinStatus{}, false
	}
	pinStatus := inspectLUNPin(target.BlockDevice)
	if !pinStatus.OnSmoothfs || !pinStatus.Pinned {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "smoothfs file-backed LUN must be pinned before recording move intent",
			"lun_pin": pinStatus,
		})
		return nil, pinStatus, false
	}
	return target, pinStatus, true
}

func (h *SharingHandler) getISCSIFileTargetMoveIntent(iqn string) (*iscsiLUNMoveIntent, error) {
	data, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn))
	if err == db.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var intent iscsiLUNMoveIntent
	if err := json.Unmarshal([]byte(data), &intent); err != nil {
		return nil, fmt.Errorf("decode iSCSI move intent for %q: %w", iqn, err)
	}
	return &intent, nil
}

func iscsiTargetMoveIntentConfigKey(iqn string) string {
	return "iscsi.target." + iqn + ".move_intent"
}

func (h *SharingHandler) listISCSIACLs(w http.ResponseWriter, r *http.Request, targetIQN string) {
	acls, err := iscsi.ListACLs(targetIQN)
	if err != nil {
		serverError(w, err)
		return
	}
	if acls == nil {
		acls = []iscsi.ACL{}
	}
	json.NewEncoder(w).Encode(acls)
}

func (h *SharingHandler) addISCSIACL(w http.ResponseWriter, r *http.Request, targetIQN string) {
	var req struct {
		InitiatorIQN string `json:"initiator_iqn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InitiatorIQN == "" {
		jsonErrorCoded(w, "initiator_iqn required", http.StatusBadRequest, "sharing.initiator_iqn_required")
		return
	}
	if err := iscsi.AddACL(targetIQN, req.InitiatorIQN); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"status":"acl added"}`)
}

func (h *SharingHandler) removeISCSIACL(w http.ResponseWriter, r *http.Request, targetIQN, aclIQN string) {
	if err := iscsi.RemoveACL(targetIQN, aclIQN); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, `{"status":"acl removed"}`)
}

// --- Filesystem Paths ---

// isMountPoint returns true if path is an active mount point.
func isMountPoint(path string) bool {
	return exec.Command("findmnt", "-n", path).Run() == nil
}

type filesystemPath struct {
	Path   string `json:"path"`
	Source string `json:"source"` // "zfs", "mdadm", or "tier"
	Name   string `json:"name"`   // dataset, array, or tier name
}

func (h *SharingHandler) listFilesystemPaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonMethodNotAllowed(w)
		return
	}

	var paths []filesystemPath

	// ZFS datasets
	datasets, err := zfs.ListDatasets("")
	if err == nil {
		for _, ds := range datasets {
			if ds.Mounted && ds.MountPoint != "" && ds.MountPoint != "/" {
				paths = append(paths, filesystemPath{
					Path:   ds.MountPoint,
					Source: "zfs",
					Name:   ds.Name,
				})
			}
		}
	}

	// mdadm arrays — check the standard mount point directly instead of
	// resolving through device-mapper layers.
	arrays, err := mdadm.List()
	if err == nil {
		for _, a := range arrays {
			mp := "/mnt/" + a.Name
			if isMountPoint(mp) {
				paths = append(paths, filesystemPath{
					Path:   mp,
					Source: "mdadm",
					Name:   a.Name,
				})
			}
		}
	}

	// Tier pools — include any healthy or degraded tier; trust the DB state
	// rather than re-checking the mount point so that a transient findmnt
	// failure doesn't silently hide the target.
	tiers, err := h.store.ListTierInstances()
	if err == nil {
		for _, t := range tiers {
			if t.State != db.TierPoolStateHealthy && t.State != db.TierPoolStateDegraded {
				continue
			}
			paths = append(paths, filesystemPath{
				Path:   t.MountPoint,
				Source: "tier",
				Name:   t.Name,
			})
		}
	}

	if paths == nil {
		paths = []filesystemPath{}
	}
	json.NewEncoder(w).Encode(paths)
}

type fsBrowseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type fsBrowseResponse struct {
	Path    string          `json:"path"`
	Parent  string          `json:"parent,omitempty"`
	Entries []fsBrowseEntry `json:"entries"`
}

// browseFilesystem lists subdirectories of a path within /mnt.
func (h *SharingHandler) browseFilesystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonMethodNotAllowed(w)
		return
	}

	target := filepath.Clean(r.URL.Query().Get("path"))
	if target == "." || target == "" {
		target = "/mnt"
	}
	if target != "/mnt" && !strings.HasPrefix(target, "/mnt/") {
		jsonErrorCoded(w, "path must be within /mnt", http.StatusBadRequest, "sharing.path_outside_mnt")
		return
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		jsonError(w, fmt.Sprintf("cannot read directory: %v", err), http.StatusBadRequest)
		return
	}

	resp := fsBrowseResponse{Path: target, Entries: []fsBrowseEntry{}}
	if target != "/mnt" {
		resp.Parent = filepath.Dir(target)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		resp.Entries = append(resp.Entries, fsBrowseEntry{
			Name: name,
			Path: filepath.Join(target, name),
		})
	}
	json.NewEncoder(w).Encode(resp)
}
