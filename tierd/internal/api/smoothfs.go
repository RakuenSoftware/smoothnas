package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// SmoothfsHandler serves /api/smoothfs/pools* — Phase 7.7's
// REST surface for managing smoothfs pool lifecycle end-to-end:
// generate a systemd mount unit, enable it, persist the pool
// record, then let the mount-event auto-discovery from Phase 2.5
// register the pool with the planner.
type SmoothfsHandler struct {
	store *db.Store
}

type smoothfsWriteStagingResponse struct {
	PoolName                          string `json:"pool_name"`
	DesiredEnabled                    bool   `json:"desired_enabled"`
	EffectiveEnabled                  bool   `json:"effective_enabled"`
	KernelSupported                   bool   `json:"kernel_supported"`
	KernelEnabled                     bool   `json:"kernel_enabled"`
	FullThresholdPct                  uint64 `json:"full_threshold_pct"`
	Reason                            string `json:"reason,omitempty"`
	StagedBytes                       uint64 `json:"staged_bytes"`
	StagedRehomeBytes                 uint64 `json:"staged_rehome_bytes"`
	RangeStagedBytes                  uint64 `json:"range_staged_bytes"`
	RangeStagedWrites                 uint64 `json:"range_staged_writes"`
	RangeStagingRecoverySupported     bool   `json:"range_staging_recovery_supported"`
	RangeStagingRecoveredBytes        uint64 `json:"range_staging_recovered_bytes"`
	RangeStagingRecoveredWrites       uint64 `json:"range_staging_recovered_writes"`
	RangeStagingRecoveryPending       uint64 `json:"range_staging_recovery_pending"`
	StagedRehomesTotal                uint64 `json:"staged_rehomes_total"`
	StagedRehomesPending              uint64 `json:"staged_rehomes_pending"`
	WriteStagingDrainPressure         bool   `json:"write_staging_drain_pressure"`
	WriteStagingDrainableTierMask     uint64 `json:"write_staging_drainable_tier_mask"`
	WriteStagingDrainableRehomes      uint64 `json:"write_staging_drainable_rehomes"`
	RecoveredRangeTierMask            uint64 `json:"recovered_range_tier_mask"`
	OldestStagedWriteAt               string `json:"oldest_staged_write_at,omitempty"`
	OldestRecoveredWriteAt            string `json:"oldest_recovered_write_at,omitempty"`
	LastDrainAt                       string `json:"last_drain_at,omitempty"`
	LastDrainReason                   string `json:"last_drain_reason,omitempty"`
	LastRecoveryAt                    string `json:"last_recovery_at,omitempty"`
	LastRecoveryReason                string `json:"last_recovery_reason,omitempty"`
	MetadataActiveTierMask            uint64 `json:"metadata_active_tier_mask"`
	WriteStagingDrainActiveTierMask   uint64 `json:"write_staging_drain_active_tier_mask"`
	MetadataTierSkips                 uint64 `json:"metadata_tier_skips"`
	RecommendedMetadataActiveTierMask uint64 `json:"recommended_metadata_active_tier_mask,omitempty"`
	RecommendedDrainActiveTierMask    uint64 `json:"recommended_drain_active_tier_mask,omitempty"`
	MetadataActiveMaskReason          string `json:"metadata_active_mask_reason,omitempty"`
	DrainActiveMaskReason             string `json:"drain_active_mask_reason,omitempty"`
	SmoothNASWakesAllowed             bool   `json:"smoothnas_wakes_allowed"`
}

type updateSmoothfsWriteStagingRequest struct {
	Enabled                bool    `json:"enabled"`
	FullThresholdPct       *uint64 `json:"full_threshold_pct,omitempty"`
	MetadataActiveTierMask *uint64 `json:"metadata_active_tier_mask,omitempty"`
}

var (
	readSmoothfsWriteStagingFile  = os.ReadFile
	writeSmoothfsWriteStagingFile = os.WriteFile
	smoothfsWriteStagingRoot      = func(uuid string) string { return filepath.Join("/sys/fs/smoothfs", uuid) }
)

func NewSmoothfsHandler(store *db.Store) *SmoothfsHandler {
	return &SmoothfsHandler{store: store}
}

func (h *SmoothfsHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/smoothfs/pools" || path == "/api/smoothfs/pools/":
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
	case path == "/api/smoothfs/movement-log" || path == "/api/smoothfs/movement-log/":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		h.movementLog(w, r)
	case strings.HasPrefix(path, "/api/smoothfs/pools/"):
		rest := strings.TrimPrefix(path, "/api/smoothfs/pools/")
		rest = strings.TrimSuffix(rest, "/")
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			h.get(w, r, name)
		case sub == "" && r.Method == http.MethodDelete:
			h.destroy(w, r, name)
		case sub == "quiesce" && r.Method == http.MethodPost:
			h.quiesce(w, r, name)
		case sub == "reconcile" && r.Method == http.MethodPost:
			h.reconcile(w, r, name)
		case sub == "write-staging" && r.Method == http.MethodGet:
			h.getWriteStaging(w, r, name)
		case sub == "write-staging" && r.Method == http.MethodPut:
			h.updateWriteStaging(w, r, name)
		case sub == "metadata-active-mask/refresh" && r.Method == http.MethodPost:
			h.refreshMetadataActiveMask(w, r, name)
		default:
			jsonNotFound(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *SmoothfsHandler) getWriteStaging(w http.ResponseWriter, _ *http.Request, name string) {
	resp, err := h.writeStagingStatus(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *SmoothfsHandler) updateWriteStaging(w http.ResponseWriter, r *http.Request, name string) {
	var req updateSmoothfsWriteStagingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	if req.FullThresholdPct != nil && (*req.FullThresholdPct < 1 || *req.FullThresholdPct > 100) {
		jsonErrorCoded(w, "full_threshold_pct must be between 1 and 100", http.StatusBadRequest, "smoothfs.full_threshold_out_of_range")
		return
	}
	root := smoothfsWriteStagingRoot(pool.UUID)
	supported := sysfsBool(filepath.Join(root, "write_staging_supported"))
	if supported {
		metadataMask := req.MetadataActiveTierMask
		if metadataMask == nil {
			recommendation := h.recommendMetadataActiveTierMask(*pool)
			if recommendation.OK {
				metadataMask = &recommendation.Mask
			}
		}
		if req.FullThresholdPct != nil {
			data := []byte(strconv.FormatUint(*req.FullThresholdPct, 10) + "\n")
			if err := writeSmoothfsWriteStagingFile(filepath.Join(root, "write_staging_full_pct"), data, 0o644); err != nil {
				serverError(w, err)
				return
			}
		}
		if metadataMask != nil {
			if err := writeSmoothfsMetadataActiveMask(root, *metadataMask); err != nil {
				serverError(w, err)
				return
			}
			if err := writeSmoothfsDrainActiveMaskIfPresent(root, *metadataMask); err != nil {
				serverError(w, err)
				return
			}
		}
		value := []byte("0\n")
		if req.Enabled {
			value = []byte("1\n")
		}
		if err := writeSmoothfsWriteStagingFile(filepath.Join(root, "write_staging_enabled"), value, 0o644); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := h.store.SetBoolConfig(smoothfsWriteStagingConfigKey(name), req.Enabled); err != nil {
		serverError(w, err)
		return
	}
	resp, err := h.writeStagingStatus(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *SmoothfsHandler) refreshMetadataActiveMask(w http.ResponseWriter, _ *http.Request, name string) {
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	root := smoothfsWriteStagingRoot(pool.UUID)
	if !sysfsBool(filepath.Join(root, "write_staging_supported")) {
		jsonErrorCoded(w, "smoothfs write-staging support is not available on this kernel", http.StatusBadRequest, "smoothfs.staging_unsupported")
		return
	}
	recommendation := h.recommendMetadataActiveTierMask(*pool)
	if !recommendation.OK {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":                                 "metadata active mask could not be computed",
			"reason":                                recommendation.Reason,
			"recommended_metadata_active_tier_mask": recommendation.Mask,
		})
		return
	}
	if err := writeSmoothfsMetadataActiveMask(root, recommendation.Mask); err != nil {
		serverError(w, err)
		return
	}
	if err := writeSmoothfsDrainActiveMaskIfPresent(root, recommendation.Mask); err != nil {
		serverError(w, err)
		return
	}
	resp, err := h.writeStagingStatus(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *SmoothfsHandler) writeStagingStatus(name string) (*smoothfsWriteStagingResponse, error) {
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		return nil, err
	}
	desired, err := h.store.GetBoolConfig(smoothfsWriteStagingConfigKey(name), false)
	if err != nil {
		return nil, err
	}
	root := smoothfsWriteStagingRoot(pool.UUID)
	supported := sysfsBool(filepath.Join(root, "write_staging_supported"))
	kernelEnabled := sysfsBool(filepath.Join(root, "write_staging_enabled"))
	recommendation := h.recommendMetadataActiveTierMask(*pool)
	resp := &smoothfsWriteStagingResponse{
		PoolName:                          name,
		DesiredEnabled:                    desired,
		KernelSupported:                   supported,
		KernelEnabled:                     kernelEnabled,
		FullThresholdPct:                  sysfsUint(filepath.Join(root, "write_staging_full_pct")),
		EffectiveEnabled:                  desired && supported && kernelEnabled,
		SmoothNASWakesAllowed:             false,
		StagedBytes:                       sysfsUint(filepath.Join(root, "staged_bytes")),
		StagedRehomeBytes:                 sysfsUint(filepath.Join(root, "staged_rehome_bytes")),
		RangeStagedBytes:                  sysfsUint(filepath.Join(root, "range_staged_bytes")),
		RangeStagedWrites:                 sysfsUint(filepath.Join(root, "range_staged_writes")),
		RangeStagingRecoverySupported:     sysfsBool(filepath.Join(root, "range_staging_recovery_supported")),
		RangeStagingRecoveredBytes:        sysfsUint(filepath.Join(root, "range_staging_recovered_bytes")),
		RangeStagingRecoveredWrites:       sysfsUint(filepath.Join(root, "range_staging_recovered_writes")),
		RangeStagingRecoveryPending:       sysfsUint(filepath.Join(root, "range_staging_recovery_pending")),
		StagedRehomesTotal:                sysfsUint(filepath.Join(root, "staged_rehomes_total")),
		StagedRehomesPending:              sysfsUint(filepath.Join(root, "staged_rehomes_pending")),
		WriteStagingDrainPressure:         sysfsBool(filepath.Join(root, "write_staging_drain_pressure")),
		WriteStagingDrainableTierMask:     sysfsUint(filepath.Join(root, "write_staging_drainable_tier_mask")),
		WriteStagingDrainableRehomes:      sysfsUint(filepath.Join(root, "write_staging_drainable_rehomes")),
		RecoveredRangeTierMask:            sysfsUint(filepath.Join(root, "recovered_range_tier_mask")),
		OldestStagedWriteAt:               sysfsString(filepath.Join(root, "oldest_staged_write_at")),
		OldestRecoveredWriteAt:            sysfsString(filepath.Join(root, "oldest_recovered_write_at")),
		LastDrainAt:                       sysfsString(filepath.Join(root, "last_drain_at")),
		LastDrainReason:                   sysfsString(filepath.Join(root, "last_drain_reason")),
		LastRecoveryAt:                    sysfsString(filepath.Join(root, "last_recovery_at")),
		LastRecoveryReason:                sysfsString(filepath.Join(root, "last_recovery_reason")),
		MetadataActiveTierMask:            sysfsUint(filepath.Join(root, "metadata_active_tier_mask")),
		WriteStagingDrainActiveTierMask:   sysfsUint(filepath.Join(root, "write_staging_drain_active_tier_mask")),
		MetadataTierSkips:                 sysfsUint(filepath.Join(root, "metadata_tier_skips")),
		RecommendedMetadataActiveTierMask: recommendation.Mask,
		RecommendedDrainActiveTierMask:    recommendation.Mask,
		MetadataActiveMaskReason:          recommendation.Reason,
		DrainActiveMaskReason:             recommendation.Reason,
	}
	if desired && !supported {
		resp.Reason = "smoothfs write-staging support is not available on this kernel"
	}
	if desired && supported && !kernelEnabled {
		resp.Reason = "smoothfs write staging is not enabled in the kernel"
	}
	if !desired {
		resp.Reason = "write staging is disabled"
	}
	return resp, nil
}

func smoothfsWriteStagingConfigKey(name string) string {
	return "smoothfs.write_staging." + name + ".enabled"
}

func writeSmoothfsMetadataActiveMask(root string, mask uint64) error {
	data := []byte("0x" + strconv.FormatUint(mask, 16) + "\n")
	return writeSmoothfsWriteStagingFile(filepath.Join(root, "metadata_active_tier_mask"), data, 0o644)
}

func writeSmoothfsDrainActiveMaskIfPresent(root string, mask uint64) error {
	path := filepath.Join(root, "write_staging_drain_active_tier_mask")
	if sysfsString(path) == "" {
		return nil
	}
	data := []byte("0x" + strconv.FormatUint(mask, 16) + "\n")
	return writeSmoothfsWriteStagingFile(path, data, 0o644)
}

func sysfsBool(path string) bool {
	switch strings.ToLower(sysfsString(path)) {
	case "1", "true", "yes", "on", "supported":
		return true
	default:
		return false
	}
}

func sysfsUint(path string) uint64 {
	val := sysfsString(path)
	if val == "" {
		return 0
	}
	n, err := strconv.ParseUint(val, 0, 64)
	if err != nil {
		return 0
	}
	return n
}

func sysfsString(path string) string {
	data, err := readSmoothfsWriteStagingFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// movementLog exposes the smoothfs_movement_log table, newest
// rows first. Supports ?limit=N&offset=M query-string paging; the
// store caps limit at 500 to keep a runaway UI from pinning tierd
// on a giant response.
func (h *SmoothfsHandler) movementLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)
	offset := atoiDefault(q.Get("offset"), 0)
	entries, err := h.store.ListSmoothfsMovementLog(limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	if entries == nil {
		entries = []db.SmoothfsMovementLogEntry{}
	}
	_ = json.NewEncoder(w).Encode(entries)
}

// quiesce pauses in-flight cutovers + refuses new MOVE_PLAN on
// the pool. Implemented by sending the SMOOTHFS_CMD_QUIESCE
// netlink to the kernel via smoothfs.Client. Per Phase 2 semantics,
// quiesce returns once the kernel has drained writers; tierd just
// forwards the ack.
func (h *SmoothfsHandler) quiesce(w http.ResponseWriter, _ *http.Request, name string) {
	if blocked, err := h.rejectSmoothfsMaintenanceOutsideWindow(w, name, "smoothfs quiesce"); err != nil || blocked {
		if err != nil {
			serverError(w, err)
		}
		return
	}
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	poolUUID, err := uuid.Parse(pool.UUID)
	if err != nil {
		serverError(w, err)
		return
	}
	client, err := smoothfsclient.Open()
	if err != nil {
		jsonError(w, "smoothfs netlink: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer client.Close()
	if err := client.Quiesce(poolUUID); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reconcile lifts the quiesce + re-arms heat drain + planner.
type reconcileRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (h *SmoothfsHandler) reconcile(w http.ResponseWriter, r *http.Request, name string) {
	var req reconcileRequest
	// Body is optional; ignore decode errors on empty body.
	_ = json.NewDecoder(r.Body).Decode(&req)
	if blocked, err := h.rejectSmoothfsMaintenanceOutsideWindow(w, name, "smoothfs reconcile"); err != nil || blocked {
		if err != nil {
			serverError(w, err)
		}
		return
	}

	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	poolUUID, err := uuid.Parse(pool.UUID)
	if err != nil {
		serverError(w, err)
		return
	}
	client, err := smoothfsclient.Open()
	if err != nil {
		jsonError(w, "smoothfs netlink: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer client.Close()
	if err := client.Reconcile(poolUUID, req.Reason); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *SmoothfsHandler) rejectSmoothfsMaintenanceOutsideWindow(w http.ResponseWriter, name, action string) (bool, error) {
	if h.store == nil {
		return false, nil
	}
	if _, err := h.store.GetTierInstance(name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	decision, err := poolMaintenanceDecision(h.store, name, spindownNow())
	if err != nil {
		return false, err
	}
	return rejectBlockedMaintenance(w, name, action, decision), nil
}

func (h *SmoothfsHandler) list(w http.ResponseWriter, _ *http.Request) {
	pools, err := h.store.ListSmoothfsPools()
	if err != nil {
		serverError(w, err)
		return
	}
	if pools == nil {
		pools = []db.SmoothfsPool{}
	}
	_ = json.NewEncoder(w).Encode(pools)
}

func (h *SmoothfsHandler) get(w http.ResponseWriter, _ *http.Request, name string) {
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(pool)
}

type createSmoothfsPoolRequest struct {
	Name      string   `json:"name"`
	UUID      string   `json:"uuid,omitempty"`
	Tiers     []string `json:"tiers"`
	MountBase string   `json:"mount_base,omitempty"`
}

func (h *SmoothfsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createSmoothfsPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	cr := smoothfsclient.CreateManagedPoolRequest{
		Name:      req.Name,
		Tiers:     req.Tiers,
		MountBase: req.MountBase,
	}
	if req.UUID != "" {
		parsed, err := uuid.Parse(req.UUID)
		if err != nil {
			jsonError(w, "invalid uuid: "+err.Error(), http.StatusBadRequest)
			return
		}
		cr.UUID = parsed
	}

	// Persist the row FIRST so a racing list request doesn't see
	// a systemd unit with no DB backing. Mountpoint + UnitPath are
	// filled in after CreateManagedPool returns the canonical
	// shape; we patch the row post-hoc rather than derive them
	// twice (once to pre-compute, once for the systemd write).
	//
	// Actually — the systemd unit write is the side-effecting step
	// and the thing most likely to fail (permissions, daemon-
	// reload), so we do it FIRST and only persist on success. An
	// orphan mount unit on disk with no DB row is easier to repair
	// than the reverse.
	mp, err := createManagedPool(cr)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	row := db.SmoothfsPool{
		UUID:       mp.UUID.String(),
		Name:       mp.Name,
		Tiers:      mp.Tiers,
		Mountpoint: mp.Mountpoint,
		UnitPath:   mp.UnitPath,
	}
	persisted, err := h.store.CreateSmoothfsPool(row)
	if err != nil {
		// Roll back the systemd unit so we don't leak state.
		_ = destroyManagedPool(*mp)
		if errors.Is(err, db.ErrDuplicate) {
			jsonErrorCoded(w, "pool name already exists", http.StatusConflict, "smoothfs.pool_name_taken")
			return
		}
		serverError(w, err)
		return
	}

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(persisted)
}

func (h *SmoothfsHandler) destroy(w http.ResponseWriter, _ *http.Request, name string) {
	pool, err := h.store.GetSmoothfsPool(name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			jsonErrorCoded(w, "pool not found", http.StatusNotFound, "smoothfs.pool_not_found")
			return
		}
		serverError(w, err)
		return
	}

	parsed, err := uuid.Parse(pool.UUID)
	if err != nil {
		serverError(w, err)
		return
	}
	mp := smoothfsclient.ManagedPool{
		Name:       pool.Name,
		UUID:       parsed,
		Tiers:      pool.Tiers,
		Mountpoint: pool.Mountpoint,
		UnitPath:   pool.UnitPath,
	}

	// Tear down the mount unit first; if that fails we still
	// remove the DB row so the operator can retry from a clean
	// slate. The systemd side is idempotent — `systemctl disable
	// --now` on a missing unit succeeds silently.
	destroyErr := destroyManagedPool(mp)

	if err := h.store.DeleteSmoothfsPool(name); err != nil {
		serverError(w, err)
		return
	}

	if destroyErr != nil {
		jsonError(w, destroyErr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
