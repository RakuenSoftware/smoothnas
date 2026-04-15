package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier"
)

type createTierDefinitionRequest struct {
	Name string `json:"name"`
	Rank int    `json:"rank"`
}

type createTierRequest struct {
	Name       string                         `json:"name"`
	Filesystem string                         `json:"filesystem"`
	Tiers      *[]createTierDefinitionRequest `json:"tiers,omitempty"`
}

type assignTierArrayRequest struct {
	ArrayID int64 `json:"array_id"`
}

type deleteTierRequest struct {
	ConfirmPoolName string `json:"confirm_pool_name"`
}

type createTierDefinitionResponse struct {
	Name             string `json:"name"`
	Rank             int    `json:"rank"`
	State            string `json:"state"`
	ArrayID          any    `json:"array_id"`
	PVDevice         any    `json:"pv_device"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	UsedBytes        uint64 `json:"used_bytes"`
	FreeBytes        uint64 `json:"free_bytes"`
	TargetFillPct    int    `json:"target_fill_pct"`
	FullThresholdPct int    `json:"full_threshold_pct"`
}

type createTierResponse struct {
	Name             string                         `json:"name"`
	Filesystem       string                         `json:"filesystem"`
	State            string                         `json:"state"`
	MountPoint       string                         `json:"mount_point"`
	CapacityBytes    uint64                         `json:"capacity_bytes"`
	UsedBytes        uint64                         `json:"used_bytes"`
	Tiers            []createTierDefinitionResponse `json:"tiers"`
	CreatedAt        string                         `json:"created_at"`
	UpdatedAt        string                         `json:"updated_at"`
	LastReconciledAt any                            `json:"last_reconciled_at"`
}

type tierDetailResponse struct {
	Name             string `json:"name"`
	Rank             int    `json:"rank"`
	State            string `json:"state"`
	ArrayID          any    `json:"array_id"`
	PVDevice         any    `json:"pv_device"`
	CapacityBytes    uint64 `json:"capacity_bytes"`
	UsedBytes        uint64 `json:"used_bytes"`
	FreeBytes        uint64 `json:"free_bytes"`
	TargetFillPct    int    `json:"target_fill_pct"`
	FullThresholdPct int    `json:"full_threshold_pct"`
}

type poolDetailResponse struct {
	Name             string               `json:"name"`
	Filesystem       string               `json:"filesystem"`
	State            string               `json:"state"`
	MountPoint       string               `json:"mount_point"`
	CapacityBytes    uint64               `json:"capacity_bytes"`
	UsedBytes        uint64               `json:"used_bytes"`
	ErrorReason      any                  `json:"error_reason"`
	Tiers            []tierDetailResponse `json:"tiers"`
	CreatedAt        string               `json:"created_at"`
	UpdatedAt        string               `json:"updated_at"`
	LastReconciledAt any                  `json:"last_reconciled_at"`
}

type poolMapSegmentResponse struct {
	Rank     int    `json:"rank"`
	Tier     string `json:"tier"`
	PVDevice string `json:"pv_device"`
	PEStart  uint64 `json:"pe_start"`
	PEEnd    uint64 `json:"pe_end"`
}

type poolMapResponse struct {
	Pool       string                   `json:"pool"`
	LV         string                   `json:"lv"`
	Segments   []poolMapSegmentResponse `json:"segments"`
	Verified   bool                     `json:"verified"`
	VerifiedAt string                   `json:"verified_at"`
}

var (
	createPoolVG            = lvm.VGCreateEmpty
	removePoolVG            = lvm.VGRemove
	removePoolVGPlaceholder = lvm.VGRemovePlaceholder
	vgExists                = lvm.VGExists
	isMountPathBusy         = lvm.IsMounted
	removePVLabel           = lvm.RemovePV
	listPoolPVs             = lvm.ListPVsInVG
	poolUsageBytes          = mountedPathUsageBytes
	tierDataLVExists        = lvm.LVExists
	listTierSegments        = lvm.ListLVSegments
	tierMapNow              = time.Now
	unmountTierPath         = lvm.Unmount
	lazyUnmountPath         = lvm.LazyUnmount
	removeTierFSTab         = lvm.RemoveFSTabEntry
	removeTierLV            = lvm.RemoveLV
	deactivateTierLV        = lvm.DeactivateLV
	listManagedPVs          = lvm.ListManagedPVs
)

func validateTierNameRequest(w http.ResponseWriter, tierName string) bool {
	if err := db.ValidateTierInstanceName(tierName); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func isCreateTierConflict(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed: tier_pools.name")
}

func tierConflict(w http.ResponseWriter, action, state string) {
	jsonError(w, fmt.Sprintf("%s blocked while tier is in state %s", action, state), http.StatusConflict)
}

func createTierDefinitions(req *[]createTierDefinitionRequest) ([]db.TierDefinition, error) {
	if req == nil {
		return db.DefaultTierDefinitions(), nil
	}
	if len(*req) == 0 {
		return nil, fmt.Errorf("tiers must contain at least one entry")
	}
	defs := make([]db.TierDefinition, 0, len(*req))
	for _, tier := range *req {
		defs = append(defs, db.TierDefinition{
			Name: strings.TrimSpace(tier.Name),
			Rank: tier.Rank,
		})
	}
	if err := db.ValidateTierDefinitions(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func (h *ArraysHandler) recoverStaleEmptyTier(tierName string) error {
	t, err := h.store.GetTierInstance(tierName)
	if err != nil {
		if err == db.ErrNotFound {
			return nil
		}
		return err
	}
	if t.State != db.TierPoolStateProvisioning && t.State != db.TierPoolStateError {
		return nil
	}

	assignments, err := h.store.GetTierAssignments(tierName)
	if err != nil {
		return err
	}
	if len(assignments) != 0 {
		return nil
	}

	// Guard against slots that are stuck in a non-empty state (e.g. degraded
	// or missing) even though no array_id is present — those represent a
	// partially-cleaned assignment and should not be auto-deleted.
	slots, err := h.store.ListTierSlots(tierName)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		if slot.State != db.TierSlotStateEmpty {
			return nil
		}
	}

	// Clean up the loopback placeholder before removing the VG so the loop
	// device and backing file are released even if vgremove is a no-op.
	_ = removePoolVGPlaceholder("tier-" + tierName)
	_ = removePoolVG("tier-" + tierName)
	if err := h.store.DeleteTierInstance(tierName); err != nil {
		return fmt.Errorf("delete stale tier instance: %w", err)
	}
	_ = os.Remove(t.MountPoint)
	return nil
}

// routeTiers handles named tier instances:
//   - GET/POST /api/tiers
//   - DELETE /api/tiers/{name}
//   - PUT/DELETE /api/tiers/{name}/tiers/{tier_name}
func (h *ArraysHandler) routeTiers(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/api/tiers" || path == "/api/tiers/" {
		switch r.Method {
		case http.MethodGet:
			h.listTiers(w, r)
		case http.MethodPost:
			h.createTier(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	rest := strings.TrimPrefix(path, "/api/tiers/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	tierName := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	switch subpath {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.getTier(w, r, tierName)
		case http.MethodDelete:
			h.deleteTier(w, r, tierName)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "map":
		if r.Method == http.MethodGet {
			h.getTierMap(w, r, tierName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		switch {
		case strings.HasPrefix(subpath, "levels"):
			h.routeTierLevels(w, r, tierName, strings.TrimPrefix(subpath, "levels"))
		case strings.HasPrefix(subpath, "tiers/"):
			tierSlotName := strings.TrimPrefix(subpath, "tiers/")
			if tierSlotName == "" {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			switch r.Method {
			case http.MethodPut:
				h.assignTierArray(w, r, tierName, tierSlotName)
			case http.MethodDelete:
				h.unassignTierArray(w, r, tierName, tierSlotName)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}
}

func (h *ArraysHandler) getTier(w http.ResponseWriter, r *http.Request, poolName string) {
	if !validateTierNameRequest(w, poolName) {
		return
	}
	resp, err := poolDetailFromStore(h, poolName)
	if err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"pool not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *ArraysHandler) getTierMap(w http.ResponseWriter, r *http.Request, poolName string) {
	if !validateTierNameRequest(w, poolName) {
		return
	}
	if _, err := h.store.GetTierInstance(poolName); err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"pool not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}

	exists, err := tierDataLVExists("tier-"+poolName, "data")
	if err != nil {
		serverError(w, fmt.Errorf("check tier lv: %w", err))
		return
	}
	if !exists {
		http.Error(w, `{"error":"LV does not exist yet; assign an array to a tier slot first"}`, http.StatusServiceUnavailable)
		return
	}

	resp, err := h.refreshTierMap(poolName)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *ArraysHandler) refreshTierMap(poolName string) (*poolMapResponse, error) {
	slots, err := h.store.ListTierSlots(poolName)
	if err != nil {
		return nil, fmt.Errorf("list tier slots: %w", err)
	}

	slotByDevice := make(map[string]db.TierSlot, len(slots))
	for _, slot := range slots {
		if slot.PVDevice == nil {
			continue
		}
		slotByDevice[*slot.PVDevice] = slot
	}

	segments, err := listTierSegments("tier-"+poolName, "data")
	if err != nil {
		return nil, fmt.Errorf("list lv segments: %w", err)
	}

	resp := &poolMapResponse{
		Pool:     poolName,
		LV:       "data",
		Segments: make([]poolMapSegmentResponse, 0, len(segments)),
		Verified: true,
	}

	prevRank := 0
	for i, segment := range segments {
		slot, ok := slotByDevice[segment.PVPath]
		if !ok {
			return nil, fmt.Errorf("segment device %s is not assigned to pool %s", segment.PVPath, poolName)
		}
		if i > 0 && slot.Rank < prevRank {
			resp.Verified = false
		}
		prevRank = slot.Rank
		resp.Segments = append(resp.Segments, poolMapSegmentResponse{
			Rank:     slot.Rank,
			Tier:     slot.Name,
			PVDevice: segment.PVPath,
			PEStart:  segment.PEStart,
			PEEnd:    segment.PEEnd,
		})
	}

	resp.VerifiedAt = h.setTierMapVerification(poolName, resp.Verified, tierMapNow())
	if !resp.Verified {
		if err := h.store.SetTierInstanceError(poolName, "segment_order_violation"); err != nil {
			return nil, fmt.Errorf("set tier pool error: %w", err)
		}
	}

	return resp, nil
}

func poolDetailFromStore(h *ArraysHandler, poolName string) (*poolDetailResponse, error) {
	pool, err := h.store.GetTierInstance(poolName)
	if err != nil {
		return nil, err
	}
	slots, err := h.store.ListTierSlots(poolName)
	if err != nil {
		return nil, err
	}
	// Try legacy monolithic VG first, then fall back to per-tier VGs.
	pvs, _ := listPoolPVs("tier-" + poolName)
	pvByDevice := make(map[string]lvm.PVInfo, len(pvs))
	var capacityBytes uint64
	for _, pv := range pvs {
		pvByDevice[pv.Device] = pv
		capacityBytes += pv.SizeBytes
	}

	// Per-tier VG lookup: for new pools, each tier has its own VG.
	perTierPVs := make(map[string][]lvm.PVInfo) // tierName → PVs
	if len(pvs) == 0 {
		for _, slot := range slots {
			if slot.State == db.TierSlotStateEmpty {
				continue
			}
			vg := tier.PerTierVGName(poolName, slot.Name)
			tierPVs, _ := listPoolPVs(vg)
			if len(tierPVs) > 0 {
				perTierPVs[slot.Name] = tierPVs
				for _, pv := range tierPVs {
					capacityBytes += pv.SizeBytes
				}
			}
		}
	}

	resp := &poolDetailResponse{
		Name:          pool.Name,
		Filesystem:    pool.Filesystem,
		State:         pool.State,
		MountPoint:    pool.MountPoint,
		CapacityBytes: capacityBytes,
		UsedBytes:     poolUsageBytes(pool.MountPoint),
		ErrorReason:   nil,
		Tiers:         make([]tierDetailResponse, 0, len(slots)),
		CreatedAt:     pool.CreatedAt,
		UpdatedAt:     pool.UpdatedAt,
	}
	if pool.ErrorReason != "" {
		resp.ErrorReason = pool.ErrorReason
	}
	if pool.LastReconciledAt != "" {
		resp.LastReconciledAt = pool.LastReconciledAt
	}
	for _, slot := range slots {
		var arrayID any
		if slot.ArrayID != nil {
			arrayID = *slot.ArrayID
		}
		var pvDevice any
		if slot.PVDevice != nil {
			pvDevice = *slot.PVDevice
		}
		var capacity, usedBytes, freeBytes uint64
		// Legacy: PV stats from monolithic VG.
		if slot.PVDevice != nil {
			if pv, ok := pvByDevice[*slot.PVDevice]; ok {
				capacity = pv.SizeBytes
				usedBytes = pv.UsedBytes
				freeBytes = pv.FreeBytes
			}
		}
		// Per-tier: PV stats from per-tier VG give the VG capacity split, but
		// UsedBytes there just means "allocated to an LV" (i.e. the LV exists
		// and occupies the VG) — it does not reflect how much of the tier's
		// filesystem is actually holding data. Prefer a statfs on the backing
		// mount so the UI shows real used/free. Fall back to PV stats when the
		// tier is not currently mounted.
		if tierPVs, ok := perTierPVs[slot.Name]; ok && len(tierPVs) > 0 {
			capacity = 0
			for _, pv := range tierPVs {
				capacity += pv.SizeBytes
			}
			usedBytes = 0
			freeBytes = 0
			for _, pv := range tierPVs {
				usedBytes += pv.UsedBytes
				freeBytes += pv.FreeBytes
			}
		}
		if fsCap, fsUsed, fsFree, ok := h.backingFSUsage(pool.Name, slot.Name); ok {
			if fsCap > 0 {
				capacity = fsCap
			}
			usedBytes = fsUsed
			freeBytes = fsFree
		}
		resp.Tiers = append(resp.Tiers, tierDetailResponse{
			Name:             slot.Name,
			Rank:             slot.Rank,
			State:            slot.State,
			ArrayID:          arrayID,
			PVDevice:         pvDevice,
			CapacityBytes:    capacity,
			UsedBytes:        usedBytes,
			FreeBytes:        freeBytes,
			TargetFillPct:    slot.TargetFillPct,
			FullThresholdPct: slot.FullThresholdPct,
		})
	}
	return resp, nil
}

func mountedPathUsageBytes(mountPoint string) uint64 {
	if mountPoint == "" || !isMountPathBusy(mountPoint) {
		return 0
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountPoint, &stat); err != nil {
		return 0
	}
	return (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
}

// killMountHolders SIGKILLs every process that has any file open under
// mountPath. Called from destroyTierPool when normal umount fails
// because the filesystem is "in use". Uses `fuser -km` from the psmisc
// package — listed in updater.requiredPackages so it gets installed by
// EnsureSystemPackages on every appliance, and shipped in the ISO base
// install. If fuser is somehow missing (manually-built host, broken
// install) we log loudly so the failure mode is visible.
//
// A short pause afterwards gives the kernel time to tear down the
// file-descriptor tables so the subsequent umount actually frees the
// backing filesystem.
func killMountHolders(mountPath string) error {
	if _, err := exec.LookPath("fuser"); err != nil {
		log.Printf("destroy: fuser not on PATH; tier destroy cannot reclaim a busy mount. Install psmisc.")
		return fmt.Errorf("fuser missing: %w", err)
	}
	cmd := exec.Command("fuser", "-km", mountPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// fuser exits 1 when no processes were found — that's not an
		// error in our context. Anything else is a real problem worth
		// surfacing because the caller is about to lvremove and that
		// will fail if the kill didn't land.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// no holders — that's fine
		} else {
			log.Printf("destroy: fuser -km %s: %v (out=%q)",
				mountPath, err, strings.TrimSpace(string(out)))
			return err
		}
	} else {
		log.Printf("destroy: SIGKILLed mount holders on %s: %s",
			mountPath, strings.TrimSpace(string(out)))
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

// statfsUsedBytes returns (total - available) for a mount point, used to
// capture the empty-FS metadata baseline right after mkfs.
func statfsUsedBytes(mountPath string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountPath, &st); err != nil {
		return 0, false
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, true
}

// backingFSUsage returns statvfs-derived capacity / used / free bytes for a
// tier's backing mount, with the empty-filesystem metadata baseline
// subtracted so "used" reflects user data, not XFS's per-AG reservation
// pool. Returns ok=false when the mount is missing or statfs fails.
//
// Baseline discovery: on first read the current "used" is assumed to be the
// metadata baseline and persisted under control_plane_config. Subsequent
// reads subtract it. If a tier is reformatted the baseline must be cleared
// (the create path does this).
func (h *ArraysHandler) backingFSUsage(poolName, tierName string) (capacity, used, free uint64, ok bool) {
	mountPath := tier.PerTierBackingMount(poolName, tierName)
	if !isMountPathBusy(mountPath) {
		return 0, 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountPath, &st); err != nil {
		return 0, 0, 0, false
	}
	bs := uint64(st.Bsize)
	capacity = st.Blocks * bs
	free = st.Bavail * bs
	used = (st.Blocks - st.Bfree) * bs

	baseline := h.tierBaselineBytes(poolName, tierName, used)
	if baseline > 0 {
		if capacity > baseline {
			capacity -= baseline
		}
		if used > baseline {
			used -= baseline
		} else {
			used = 0
		}
	}
	return capacity, used, free, true
}

// tierBaselineBytes returns the recorded empty-FS metadata baseline for a
// tier. If no baseline is recorded, currentUsed is treated as the baseline
// (since the caller is computing it right after mkfs on a fresh tier) and
// persisted. This self-heal keeps behavior correct across tierd restarts
// without requiring explicit seeding.
func (h *ArraysHandler) tierBaselineBytes(poolName, tierName string, currentUsed uint64) uint64 {
	key := "tier_baseline." + poolName + "." + tierName
	val, err := h.store.GetControlPlaneConfig(key)
	if err == nil && val != "" {
		if n, perr := strconv.ParseUint(val, 10, 64); perr == nil {
			return n
		}
	}
	// First sighting — record currentUsed as baseline.
	_ = h.store.SetControlPlaneConfig(key, strconv.FormatUint(currentUsed, 10))
	return currentUsed
}

func (h *ArraysHandler) resolveArrayByID(arrayID int64) (*richArray, error) {
	if arrayID <= 0 {
		return nil, fmt.Errorf("array_id must be positive")
	}

	arrays, err := listMDADMArrays()
	if err != nil {
		return nil, err
	}
	for _, array := range arrays {
		registeredID, err := h.store.EnsureMDADMArray(array.Path)
		if err != nil {
			return nil, err
		}
		if registeredID == arrayID {
			resolved := richArray{ID: registeredID, Array: array}
			return &resolved, nil
		}
	}
	return nil, db.ErrNotFound
}

func (h *ArraysHandler) listTiers(w http.ResponseWriter, r *http.Request) {
	tiers, err := h.store.ListTierInstances()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]poolDetailResponse, 0, len(tiers))
	for _, t := range tiers {
		detail, err := poolDetailFromStore(h, t.Name)
		if err != nil {
			serverError(w, fmt.Errorf("load pool %s detail: %w", t.Name, err))
			return
		}
		out = append(out, *detail)
	}
	json.NewEncoder(w).Encode(out)
}

func (h *ArraysHandler) createTier(w http.ResponseWriter, r *http.Request) {
	var req createTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if !validateTierNameRequest(w, req.Name) {
		return
	}
	filesystem := strings.TrimSpace(req.Filesystem)
	if filesystem == "" {
		filesystem = "xfs"
	}
	if err := lvm.ValidateFilesystem(filesystem); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	tierDefs, err := createTierDefinitions(req.Tiers)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	mountPoint := db.TierMountPoint(req.Name)
	if info, err := os.Stat(mountPoint); err == nil && !info.IsDir() {
		jsonError(w, fmt.Sprintf("mount point %s already exists as a file", mountPoint), http.StatusConflict)
		return
	}
	if isMountPathBusy(mountPoint) {
		jsonError(w, fmt.Sprintf("mount point %s is already mounted", mountPoint), http.StatusConflict)
		return
	}
	if err := h.recoverStaleEmptyTier(req.Name); err != nil {
		serverError(w, fmt.Errorf("recover stale tier %s: %w", req.Name, err))
		return
	}
	if _, err := h.store.GetTierInstance(req.Name); err == nil {
		jsonError(w, fmt.Sprintf("tier %s already exists", req.Name), http.StatusConflict)
		return
	} else if err != db.ErrNotFound {
		serverError(w, err)
		return
	}
	if err := h.store.CreateTierPool(req.Name, filesystem, tierDefs); err != nil {
		if isCreateTierConflict(err) {
			jsonError(w, fmt.Sprintf("tier %s already exists", req.Name), http.StatusConflict)
			return
		}
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	// Create the empty VG immediately so the pool VG exists as soon as the DB
	// record does. A loopback-backed placeholder PV is used until the first
	// real array is assigned; ProvisionStorage removes it at that point.
	if err := createPoolVG("tier-" + req.Name); err != nil {
		_ = h.store.DeleteTierInstance(req.Name)
		serverError(w, fmt.Errorf("create tier vg: %w", err))
		return
	}
	created, err := h.store.GetTierInstance(req.Name)
	if err != nil {
		serverError(w, fmt.Errorf("reload created tier: %w", err))
		return
	}

	h.invalidateAll()
	w.WriteHeader(http.StatusCreated)
	resp := createTierResponse{
		Name:          created.Name,
		Filesystem:    created.Filesystem,
		State:         created.State,
		MountPoint:    created.MountPoint,
		CapacityBytes: 0,
		UsedBytes:        0,
		CreatedAt:        created.CreatedAt,
		UpdatedAt:        created.UpdatedAt,
		LastReconciledAt: nil,
		Tiers:            make([]createTierDefinitionResponse, 0, len(tierDefs)),
	}
	for _, tier := range tierDefs {
		resp.Tiers = append(resp.Tiers, createTierDefinitionResponse{
			Name:             tier.Name,
			Rank:             tier.Rank,
			State:            db.TierSlotStateEmpty,
			ArrayID:          nil,
			PVDevice:         nil,
			CapacityBytes:    0,
			TargetFillPct:    50,
			FullThresholdPct: 95,
		})
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *ArraysHandler) deleteTier(w http.ResponseWriter, r *http.Request, tierName string) {
	if !validateTierNameRequest(w, tierName) {
		return
	}

	unlock := h.lockPool(tierName)

	var req deleteTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		unlock()
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.ConfirmPoolName != tierName {
		unlock()
		jsonError(w, "confirm_pool_name must exactly match the pool name", http.StatusBadRequest)
		return
	}

	t, err := h.store.GetTierInstance(tierName)
	if err != nil {
		unlock()
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"tier not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}
	consumers, err := h.tierConsumers(tierName)
	if err != nil {
		unlock()
		serverError(w, fmt.Errorf("list tier consumers: %w", err))
		return
	}
	if len(consumers) > 0 {
		unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":     "pool has active consumers; remove them before deleting",
			"consumers": consumers,
		})
		return
	}

	if t.State != db.TierPoolStateDestroying {
		if err := h.store.TransitionTierInstanceState(tierName, db.TierPoolStateDestroying); err != nil {
			unlock()
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
	}

	// Release the lock and return immediately so the UI can poll the
	// "destroying" state. The goroutine re-acquires the lock for teardown.
	unlock()
	h.invalidateAll()
	_ = json.NewEncoder(w).Encode(map[string]string{"state": "destroying"})

	go func() {
		defer func() {
			if h.asyncDone != nil {
				h.asyncDone <- struct{}{}
			}
		}()
		unlock := h.lockPool(tierName)
		defer unlock()
		if err := h.destroyTierPool(t); err != nil {
			_ = h.store.SetTierInstanceDestroyingReason(tierName, err.Error())
			h.invalidateAll()
			return
		}
		h.clearTierMapVerification(tierName)
		h.invalidateAll()
	}()
}

func (h *ArraysHandler) tierConsumers(poolName string) ([]string, error) {
	mountPoint := db.TierMountPoint(poolName)
	var consumers []string

	smbShares, err := h.store.ListSmbShares()
	if err != nil {
		return nil, err
	}
	for _, share := range smbShares {
		if share.Path == mountPoint || strings.HasPrefix(share.Path, mountPoint+"/") {
			consumers = append(consumers, "smb:"+share.Name)
		}
	}

	nfsExports, err := h.store.ListNfsExports()
	if err != nil {
		return nil, err
	}
	for _, exp := range nfsExports {
		if exp.Path == mountPoint || strings.HasPrefix(exp.Path, mountPoint+"/") {
			consumers = append(consumers, "nfs:"+exp.Path)
		}
	}

	iscsiTargets, err := h.store.ListIscsiTargets()
	if err != nil {
		return nil, err
	}
	lvPath := "/dev/tier-" + poolName + "/data"
	for _, target := range iscsiTargets {
		if target.BlockDevice == lvPath {
			consumers = append(consumers, "iscsi:"+target.IQN)
		}
	}

	return consumers, nil
}

func (h *ArraysHandler) destroyTierPool(pool *db.TierInstance) error {
	const lvName = "data"
	vg := "tier-" + pool.Name

	// Cancel and remove any backup_configs pointing at this pool's mount
	// before tearing down the filesystem. Otherwise a running rsync will
	// keep the FUSE mount busy (EBUSY on umount) and any backup scheduled
	// against this path will immediately recreate files as soon as the pool
	// is re-provisioned.
	if n, err := h.purgeBackupsForPath(pool.MountPoint); err != nil {
		log.Printf("destroy pool %s: purge backups under %s: %v", pool.Name, pool.MountPoint, err)
	} else if n > 0 {
		log.Printf("destroy pool %s: purged %d backup config(s) under %s", pool.Name, n, pool.MountPoint)
	}

	// Stop FUSE daemons and tear down backing mounts for any managed
	// namespaces on this pool. Without this, the FUSE daemon keeps the
	// mount point busy and the next create attempt fails with "already
	// mounted".
	if err := h.destroyPoolNamespaces(pool.Name); err != nil {
		log.Printf("destroy pool %s: destroy namespaces: %v", pool.Name, err)
	}

	if isMountPathBusy(pool.MountPoint) {
		if err := unmountTierPath(pool.MountPoint); err != nil {
			if lazyErr := lazyUnmountPath(pool.MountPoint); lazyErr != nil {
				return fmt.Errorf("unmount %s: %w", pool.MountPoint, err)
			}
		}
	}

	if err := removeTierFSTab(vg, lvName, pool.MountPoint); err != nil {
		return fmt.Errorf("remove fstab entry: %w", err)
	}
	if err := os.Remove(pool.MountPoint); err != nil && !os.IsNotExist(err) {
		if !strings.Contains(err.Error(), "directory not empty") {
			return fmt.Errorf("remove mount point: %w", err)
		}
	}

	exists, err := tierDataLVExists(vg, lvName)
	if err != nil {
		return fmt.Errorf("check lv: %w", err)
	}
	if exists {
		if err := removeTierLV(vg, lvName); err != nil {
			// lvremove can fail when a stale mount from a previous tierd
			// instance persists in a different mount namespace (e.g. after a
			// restart under systemd PrivateTmp), keeping the dm device busy.
			// Deactivate the LV to force-release the device, then retry.
			log.Printf("destroy pool %s: lvremove failed, attempting deactivate+retry: %v", pool.Name, err)
			if deactErr := deactivateTierLV(vg, lvName); deactErr != nil {
				return fmt.Errorf("remove lv: %w (deactivate also failed: %v)", err, deactErr)
			}
			if err := removeTierLV(vg, lvName); err != nil {
				return fmt.Errorf("remove lv after deactivate: %w", err)
			}
		}
	}

	slots, err := h.store.ListTierSlots(pool.Name)
	if err != nil {
		return fmt.Errorf("list tier slots: %w", err)
	}

	// Remove the loopback placeholder PV (if any) before destroying the VG
	// so the loop device and its backing image file are released cleanly.
	_ = removePoolVGPlaceholder(vg)

	// Remove the legacy per-pool VG (old monolithic-LV architecture).
	// This is a no-op for pools using the new per-tier-LV architecture.
	if exists, err := vgExists(vg); err != nil {
		return fmt.Errorf("check vg: %w", err)
	} else if exists {
		if err := removePoolVG(vg); err != nil {
			return fmt.Errorf("remove vg: %w", err)
		}
	}

	// Tear down per-tier VGs (new per-tier-LV architecture: each tier slot
	// has its own VG named tier-{pool}-{slot}, e.g. tier-media-NVME).
	for _, slot := range slots {
		perTierVG := tier.PerTierVGName(pool.Name, slot.Name)
		backingMount := tier.PerTierBackingMount(pool.Name, slot.Name)

		// Clear the stored empty-FS baseline so a future tier with the same
		// name re-discovers its own.
		_ = h.store.SetControlPlaneConfig("tier_baseline."+pool.Name+"."+slot.Name, "")

		// Unmount the per-tier backing mount if active. If normal umount
		// fails because processes (rsync, orphan FUSE fds) still hold
		// files on the mount, SIGKILL everything touching it and retry.
		// This is aggressive but correct: the user asked to destroy the
		// tier, anything still using it is orphan work that must yield.
		if isMountPathBusy(backingMount) {
			if err := unmountTierPath(backingMount); err != nil {
				log.Printf("destroy pool %s: umount %s failed (%v); killing holders",
					pool.Name, backingMount, err)
				_ = killMountHolders(backingMount)
				if err2 := unmountTierPath(backingMount); err2 != nil {
					// Still failing — detach the mount namespace entry so
					// lvremove's "in use" check has a chance even if an fd
					// keeper is stuck.
					_ = lazyUnmountPath(backingMount)
				}
			}
		}
		_ = removeTierFSTab(perTierVG, lvName, backingMount)
		_ = os.Remove(backingMount)

		// Collect PVs now, before the VG is removed.
		perTierPVs, _ := listPoolPVs(perTierVG)

		// Remove the per-tier LV, with deactivate-retry if the device is busy.
		// If the LV somehow survives all attempts (device pinned by an
		// orphan process etc.), wipe the filesystem signature so a future
		// create can't silently remount the old data. ProvisionPerTierStorage's
		// idempotent "LV exists → just mount" branch relies on the FS being
		// intact; a blank signature forces a clean reformat downstream.
		if lvOK, _ := tierDataLVExists(perTierVG, lvName); lvOK {
			if err := removeTierLV(perTierVG, lvName); err != nil {
				log.Printf("destroy pool %s: lvremove %s failed, deactivating: %v", pool.Name, perTierVG, err)
				_ = deactivateTierLV(perTierVG, lvName)
				if err := removeTierLV(perTierVG, lvName); err != nil {
					log.Printf("destroy pool %s: lvremove %s still failed after deactivate; wiping FS signature instead: %v",
						pool.Name, perTierVG, err)
					_ = lvm.WipeSignatures("/dev/" + perTierVG + "/" + lvName)
				}
			}
		}

		// Remove the per-tier VG.
		if vgOK, _ := vgExists(perTierVG); vgOK {
			_ = removePoolVGPlaceholder(perTierVG)
			_ = removePoolVG(perTierVG)
		}

		// Wipe PV labels from any devices that were in this VG but are not
		// tracked in the DB (e.g. an orphaned device from a partial provision).
		for _, pv := range perTierPVs {
			_ = removePVLabel(pv.Device)
		}

		if slot.PVDevice == nil {
			continue
		}
		// Wipe the DB-tracked PV label (best-effort; may already be gone if
		// it was caught by the perTierPVs sweep above).
		_ = removePVLabel(*slot.PVDevice)
		if err := h.store.ClearTierAssignment(pool.Name, slot.Name); err != nil {
			return fmt.Errorf("clear tier slot %s: %w", slot.Name, err)
		}
	}
	// Clean up the backing directory and the pool mount point.
	_ = os.Remove("/mnt/.tierd-backing/" + pool.Name)
	_ = os.Remove(pool.MountPoint)

	// Sweep any orphaned PVs tagged with this pool that were not captured by
	// the slot loop (e.g. if the DB slot was cleared before LVM was cleaned up).
	if managedPVs, err := listManagedPVs(); err == nil {
		for _, pv := range managedPVs {
			if pv.PoolName != pool.Name {
				continue
			}
			if vgOK, _ := vgExists(pv.VGName); vgOK {
				_ = removePoolVG(pv.VGName)
			}
			_ = removePVLabel(pv.Device)
		}
	}
	// Clean up unified-tiering rows that reference this pool: managed
	// namespaces, tier targets, and the placement domain itself.
	if err := h.store.DeleteManagedNamespacesByPlacementDomain(pool.Name); err != nil {
		return fmt.Errorf("delete managed namespaces for pool %s: %w", pool.Name, err)
	}
	if err := h.store.DeleteTierTargetsByPlacementDomain(pool.Name); err != nil {
		return fmt.Errorf("delete tier targets for pool %s: %w", pool.Name, err)
	}
	if err := h.store.DeleteTierInstance(pool.Name); err != nil {
		return fmt.Errorf("delete tier pool row: %w", err)
	}
	return nil
}

// ResumeDestroyingPools retries destruction for any tier pool left in the
// "destroying" state after a restart. Each pool is torn down in its own
// goroutine under the per-pool lock so it does not block startup.
func (h *ArraysHandler) ResumeDestroyingPools() {
	pools, err := h.store.ListTierInstances()
	if err != nil {
		log.Printf("resume destroying: list instances: %v", err)
		return
	}
	for i := range pools {
		p := pools[i]
		if p.State != db.TierPoolStateDestroying {
			continue
		}
		log.Printf("resume destroying: retrying teardown for pool %q", p.Name)
		go func() {
			unlock := h.lockPool(p.Name)
			defer unlock()
			if err := h.destroyTierPool(&p); err != nil {
				log.Printf("resume destroying: pool %q: %v", p.Name, err)
				_ = h.store.SetTierInstanceDestroyingReason(p.Name, err.Error())
				h.invalidateAll()
				return
			}
			h.clearTierMapVerification(p.Name)
			h.invalidateAll()
			log.Printf("resume destroying: pool %q successfully destroyed", p.Name)
		}()
	}
}

func (h *ArraysHandler) assignTierArray(w http.ResponseWriter, r *http.Request, poolName, tierName string) {
	if !validateTierNameRequest(w, poolName) {
		return
	}

	unlock := h.lockPool(poolName)

	pool, err := h.store.GetTierInstance(poolName)
	if err != nil {
		unlock()
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"tier not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}
	if pool.State == db.TierPoolStateDestroying {
		unlock()
		tierConflict(w, "array assignment", pool.State)
		return
	}
	var req assignTierArrayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		unlock()
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	array, err := h.resolveArrayByID(req.ArrayID)
	if err != nil {
		unlock()
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"array not found"}`, http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch array.State {
	case "active", "degraded", "clean":
	default:
		unlock()
		jsonError(w, fmt.Sprintf("array %d is in state %s", req.ArrayID, array.State), http.StatusUnprocessableEntity)
		return
	}

	if _, err := h.store.GetTierSlot(poolName, tierName); err != nil {
		unlock()
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"tier slot not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}
	if err := h.store.AssignArrayToTier(poolName, tierName, req.ArrayID, array.Path); err != nil {
		unlock()
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"tier slot not found"}`, http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	// Eagerly mark the pool healthy so the UI is not blocked while LVM
	// provisioning runs in the background. If provisioning fails the
	// goroutine will transition to the error state instead.
	if pool.State == db.TierPoolStateProvisioning {
		_ = h.store.TransitionTierInstanceState(poolName, db.TierPoolStateHealthy)
	}
	unlock()
	h.invalidateAll()
	_ = json.NewEncoder(w).Encode(map[string]string{"state": db.TierPoolStateHealthy})

	go func() {
		defer func() {
			if h.asyncDone != nil {
				h.asyncDone <- struct{}{}
			}
		}()
		// Per-tier path creates a slot-scoped VG (tier-{pool}-{slot}).
		// No pool lock needed — concurrent assignments to other slots run
		// fully in parallel without interfering.
		provErr := h.provisionPerTierStorage(poolName, tierName)
		if provErr != nil {
			_ = h.store.SetTierInstanceError(poolName, provErr.Error())
			h.invalidateAll()
			return
		}
		// Record the empty-FS baseline right now, while the tier is
		// guaranteed fresh. Lazy baseline capture on first UI read
		// races with rsync writing data before the UI polls and bakes
		// that data into the baseline. Capturing at provision time
		// pins the baseline to the real XFS metadata overhead.
		if used, ok := statfsUsedBytes(tier.PerTierBackingMount(poolName, tierName)); ok {
			_ = h.store.SetControlPlaneConfig(
				"tier_baseline."+poolName+"."+tierName,
				strconv.FormatUint(used, 10),
			)
		}
		// Ensure a FUSE-managed namespace exists so writes to /mnt/{pool}
		// are routed through the tiering daemon to the backing stores.
		if err := h.ensureNamespace(poolName); err != nil {
			log.Printf("ensure namespace for pool %q: %v", poolName, err)
		}
		if err := h.refreshTierMapVerificationIfLVExists(poolName); err != nil {
			_ = h.store.SetTierInstanceError(poolName, err.Error())
			h.invalidateAll()
			return
		}
		h.invalidateAll()
	}()
}

func (h *ArraysHandler) unassignTierArray(w http.ResponseWriter, r *http.Request, poolName, tierName string) {
	jsonError(w, "tier downsize is not supported", http.StatusMethodNotAllowed)
}

// createTierLevelRequest is the body for POST /api/tiers/{name}/levels.
type createTierLevelRequest struct {
	LevelName        string `json:"level_name"`
	Rank             int    `json:"rank"`
	TargetFillPct    *int   `json:"target_fill_pct,omitempty"`
	FullThresholdPct *int   `json:"full_threshold_pct,omitempty"`
}

// updateTierLevelRequest is the body for PUT /api/tiers/{name}/levels/{level}.
type updateTierLevelRequest struct {
	TargetFillPct    *int `json:"target_fill_pct,omitempty"`
	FullThresholdPct *int `json:"full_threshold_pct,omitempty"`
}

// routeTierLevels handles /api/tiers/{name}/levels and
// /api/tiers/{name}/levels/{level}.
//
// Supported operations:
//
//	GET    /api/tiers/{name}/levels              — list all tier levels for this pool
//	POST   /api/tiers/{name}/levels              — add a new tier level
//	PUT    /api/tiers/{name}/levels/{level}      — update target_fill_pct / full_threshold_pct
//	DELETE /api/tiers/{name}/levels/{level}      — remove an empty tier level (409 if PV assigned)
func (h *ArraysHandler) routeTierLevels(w http.ResponseWriter, r *http.Request, poolName, subpath string) {
	if !validateTierNameRequest(w, poolName) {
		return
	}

	levelName := strings.TrimPrefix(subpath, "/")

	if levelName == "" {
		switch r.Method {
		case http.MethodGet:
			resp, err := poolDetailFromStore(h, poolName)
			if err != nil {
				if err == db.ErrNotFound {
					http.Error(w, `{"error":"pool not found"}`, http.StatusNotFound)
					return
				}
				serverError(w, err)
				return
			}
			_ = json.NewEncoder(w).Encode(resp.Tiers)
		case http.MethodPost:
			h.addTierLevel(w, r, poolName)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodPut:
		h.updateTierLevel(w, r, poolName, levelName)
	case http.MethodDelete:
		h.deleteTierLevel(w, r, poolName, levelName)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (h *ArraysHandler) addTierLevel(w http.ResponseWriter, r *http.Request, poolName string) {
	var req createTierLevelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.LevelName = strings.TrimSpace(req.LevelName)
	if req.LevelName == "" {
		jsonError(w, "level_name is required", http.StatusBadRequest)
		return
	}
	if req.Rank <= 0 {
		jsonError(w, "rank must be a positive integer", http.StatusBadRequest)
		return
	}

	targetFill := 50
	if req.TargetFillPct != nil {
		targetFill = *req.TargetFillPct
	}
	fullThreshold := 95
	if req.FullThresholdPct != nil {
		fullThreshold = *req.FullThresholdPct
	}

	if err := h.store.AddTierSlot(poolName, req.LevelName, req.Rank, targetFill, fullThreshold); err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"pool not found"}`, http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	slot, err := h.store.GetTierSlot(poolName, req.LevelName)
	if err != nil {
		serverError(w, fmt.Errorf("reload tier slot: %w", err))
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(tierDetailResponse{
		Name:             slot.Name,
		Rank:             slot.Rank,
		State:            slot.State,
		ArrayID:          nil,
		PVDevice:         nil,
		CapacityBytes:    0,
		UsedBytes:        0,
		FreeBytes:        0,
		TargetFillPct:    slot.TargetFillPct,
		FullThresholdPct: slot.FullThresholdPct,
	})
}

func (h *ArraysHandler) updateTierLevel(w http.ResponseWriter, r *http.Request, poolName, levelName string) {
	var req updateTierLevelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	slot, err := h.store.GetTierSlot(poolName, levelName)
	if err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"tier level not found"}`, http.StatusNotFound)
			return
		}
		serverError(w, err)
		return
	}

	targetFill := slot.TargetFillPct
	fullThreshold := slot.FullThresholdPct
	if req.TargetFillPct != nil {
		targetFill = *req.TargetFillPct
	}
	if req.FullThresholdPct != nil {
		fullThreshold = *req.FullThresholdPct
	}
	if targetFill >= fullThreshold {
		jsonError(w, "target_fill_pct must be less than full_threshold_pct", http.StatusBadRequest)
		return
	}

	if err := h.store.SetTierSlotFill(poolName, levelName, targetFill, fullThreshold); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	slot, err = h.store.GetTierSlot(poolName, levelName)
	if err != nil {
		serverError(w, fmt.Errorf("reload tier level: %w", err))
		return
	}
	var arrayID any
	if slot.ArrayID != nil {
		arrayID = *slot.ArrayID
	}
	var pvDevice any
	if slot.PVDevice != nil {
		pvDevice = *slot.PVDevice
	}
	_ = json.NewEncoder(w).Encode(tierDetailResponse{
		Name:             slot.Name,
		Rank:             slot.Rank,
		State:            slot.State,
		ArrayID:          arrayID,
		PVDevice:         pvDevice,
		CapacityBytes:    0,
		UsedBytes:        0,
		FreeBytes:        0,
		TargetFillPct:    slot.TargetFillPct,
		FullThresholdPct: slot.FullThresholdPct,
	})
}

func (h *ArraysHandler) deleteTierLevel(w http.ResponseWriter, r *http.Request, poolName, levelName string) {
	if err := h.store.DeleteTierSlot(poolName, levelName); err != nil {
		switch err {
		case db.ErrNotFound:
			http.Error(w, `{"error":"tier level not found"}`, http.StatusNotFound)
		case db.ErrTierSlotInUse:
			jsonError(w, "tier level has an assigned PV; unassign the array before deleting", http.StatusConflict)
		default:
			serverError(w, err)
		}
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"deleted": levelName})
}
