package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

var (
	detailZFSPool  = zfs.DetailPool
	scrubZFSPool   = zfs.Scrub
	setZFSAtimeOff = zfs.SetAtimeOff
)

type rawZFSSpindownPolicyResponse struct {
	Enabled        bool                    `json:"enabled"`
	Eligible       bool                    `json:"eligible"`
	Reasons        []string                `json:"reasons"`
	HasSpecialVdev bool                    `json:"has_special_vdev"`
	ActiveWindows  []spindown.ActiveWindow `json:"active_windows"`
	ActiveNow      bool                    `json:"active_now"`
	NextActiveAt   string                  `json:"next_active_at,omitempty"`
}

type updateRawZFSSpindownRequest struct {
	Enabled       bool                     `json:"enabled"`
	ActiveWindows *[]spindown.ActiveWindow `json:"active_windows,omitempty"`
}

// ZFSHandler handles /api/pools*, /api/datasets*, /api/zvols*, /api/snapshots* endpoints.
type ZFSHandler struct {
	store           *db.Store
	afterPoolImport func(string) error
	poolsCache      *cache.Entry[[]zfs.Pool]
	datasetsCache   *cache.Entry[[]zfs.Dataset]
	zvolsCache      *cache.Entry[[]zfs.Zvol]
	snapshotsCache  *cache.Entry[[]zfs.Snapshot]
}

func (h *ZFSHandler) SetAfterPoolImport(fn func(string) error) {
	h.afterPoolImport = fn
}

func NewZFSHandler(store *db.Store) *ZFSHandler {
	return &ZFSHandler{
		store:          store,
		poolsCache:     cache.New[[]zfs.Pool](30 * time.Second),
		datasetsCache:  cache.New[[]zfs.Dataset](30 * time.Second),
		zvolsCache:     cache.New[[]zfs.Zvol](30 * time.Second),
		snapshotsCache: cache.New[[]zfs.Snapshot](30 * time.Second),
	}
}

// Route dispatches ZFS requests.
func (h *ZFSHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/pools"):
		h.routePools(w, r)
	case strings.HasPrefix(path, "/api/datasets"):
		h.routeDatasets(w, r)
	case strings.HasPrefix(path, "/api/zvols"):
		h.routeZvols(w, r)
	case strings.HasPrefix(path, "/api/snapshots"):
		h.routeSnapshots(w, r)
	default:
		jsonNotFound(w)
	}
}

// --- Pools ---

func (h *ZFSHandler) routePools(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/pools" || path == "/api/pools/" {
		switch r.Method {
		case http.MethodGet:
			h.listPools(w, r)
		case http.MethodPost:
			h.createPool(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}
	if path == "/api/pools/importable" {
		if r.Method == http.MethodGet {
			h.listImportablePools(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
		return
	}
	if path == "/api/pools/import" {
		if r.Method == http.MethodPost {
			h.importPool(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
		return
	}
	if path == "/api/pools/wipe-members" {
		if r.Method == http.MethodPost {
			h.wipeZFSMemberDisks(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}
		return
	}

	// /api/pools/{name}/...
	rest := strings.TrimPrefix(path, "/api/pools/")
	parts := strings.SplitN(rest, "/", 2)
	poolName := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	switch subpath {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.getPool(w, r, poolName)
		case http.MethodDelete:
			h.deletePool(w, r, poolName)
		default:
			jsonMethodNotAllowed(w)
		}
	case "vdevs":
		if r.Method == http.MethodPost {
			h.addVdev(w, r, poolName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "slog":
		switch r.Method {
		case http.MethodPost:
			h.addSLOG(w, r, poolName)
		case http.MethodDelete:
			h.removeSLOG(w, r, poolName)
		default:
			jsonMethodNotAllowed(w)
		}
	case "l2arc":
		switch r.Method {
		case http.MethodPost:
			h.addL2ARC(w, r, poolName)
		case http.MethodDelete:
			h.removeL2ARC(w, r, poolName)
		default:
			jsonMethodNotAllowed(w)
		}
	case "scrub":
		if r.Method == http.MethodPost {
			h.scrubPool(w, r, poolName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "spindown":
		switch r.Method {
		case http.MethodGet:
			h.getRawZFSSpindown(w, r, poolName)
		case http.MethodPut:
			h.updateRawZFSSpindown(w, r, poolName)
		default:
			jsonMethodNotAllowed(w)
		}
	default:
		// /api/pools/{name}/disks/{disk}/replace
		if strings.HasPrefix(subpath, "disks/") {
			diskRest := strings.TrimPrefix(subpath, "disks/")
			diskParts := strings.SplitN(diskRest, "/", 2)
			diskName := diskParts[0]
			action := ""
			if len(diskParts) > 1 {
				action = diskParts[1]
			}
			if action == "replace" && r.Method == http.MethodPost {
				h.replaceDisk(w, r, poolName, "/dev/"+diskName)
			} else {
				jsonNotFound(w)
			}
		} else {
			jsonNotFound(w)
		}
	}
}

func (h *ZFSHandler) listPools(w http.ResponseWriter, r *http.Request) {
	pools, err := h.poolsCache.GetOrFetch(zfs.ListPools)
	if err != nil {
		serverError(w, err)
		return
	}
	if pools == nil {
		pools = []zfs.Pool{}
	}
	json.NewEncoder(w).Encode(pools)
}

func (h *ZFSHandler) listImportablePools(w http.ResponseWriter, r *http.Request) {
	pools, err := zfs.ListImportablePools()
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(pools)
}

func (h *ZFSHandler) importPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		jsonErrorCoded(w, "name required", http.StatusBadRequest, "zfs.name_required")
		return
	}
	if err := zfs.ImportPool(req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	h.datasetsCache.Invalidate()
	h.zvolsCache.Invalidate()
	h.snapshotsCache.Invalidate()
	if h.afterPoolImport != nil {
		if err := h.afterPoolImport(req.Name); err != nil {
			log.Printf("zfs import %s: post-import reconcile: %v", req.Name, err)
		}
	}
	fmt.Fprintf(w, `{"status":"imported","pool":%q}`, req.Name)
}

func (h *ZFSHandler) wipeZFSMemberDisks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disks []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Wiping ZFS member disks...")
		if err := zfs.WipeZFSMemberDisks(req.Disks); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.poolsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "wiped"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

type createPoolRequest struct {
	Name       string   `json:"name"`
	VdevType   string   `json:"vdev_type"`
	DataDisks  []string `json:"data_disks"`
	SlogDisks  []string `json:"slog_disks"`
	L2arcDisks []string `json:"l2arc_disks"`
}

func (h *ZFSHandler) createPool(w http.ResponseWriter, r *http.Request) {
	var req createPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Name == "" || len(req.DataDisks) == 0 {
		jsonErrorCoded(w, "name and data_disks required", http.StatusBadRequest, "zfs.name_and_data_disks_required")
		return
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Creating ZFS pool...")
		if err := zfs.CreatePool(req.Name, req.VdevType, req.DataDisks, req.SlogDisks, req.L2arcDisks); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.poolsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "created", "pool": req.Name})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) getPool(w http.ResponseWriter, r *http.Request, name string) {
	pool, err := zfs.DetailPool(name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(pool)
}

func (h *ZFSHandler) deletePool(w http.ResponseWriter, r *http.Request, name string) {
	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Destroying ZFS pool...")
		if err := zfs.DestroyPool(name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.poolsCache.Invalidate()
		h.datasetsCache.Invalidate()
		h.zvolsCache.Invalidate()
		h.snapshotsCache.Invalidate()
		h.clearDestroyedPoolTierAssignments(name)
		jobs.Complete(jobID, map[string]string{"status": "destroyed"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) clearDestroyedPoolTierAssignments(poolName string) {
	if h.store == nil {
		return
	}
	pools, err := h.store.ListTierInstances()
	if err != nil {
		log.Printf("zfs destroy %s: list tier pools: %v", poolName, err)
		return
	}
	for _, tierPool := range pools {
		slots, err := h.store.ListTierSlots(tierPool.Name)
		if err != nil {
			log.Printf("zfs destroy %s: list slots for %s: %v", poolName, tierPool.Name, err)
			continue
		}
		for _, slot := range slots {
			if slot.BackingKind != "zfs" || slot.BackingRef != poolName {
				continue
			}
			if err := h.store.ClearTierAssignment(tierPool.Name, slot.Name); err != nil {
				log.Printf("zfs destroy %s: clear tier assignment %s/%s: %v", poolName, tierPool.Name, slot.Name, err)
				continue
			}
			err := h.store.SetTierInstanceError(
				tierPool.Name,
				fmt.Sprintf("ZFS backing pool %s was destroyed; tier %s was unassigned", poolName, slot.Name),
			)
			if err != nil {
				log.Printf("zfs destroy %s: mark tier pool %s error: %v", poolName, tierPool.Name, err)
			}
		}
	}
}

func (h *ZFSHandler) addVdev(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		VdevType string   `json:"vdev_type"`
		Disks    []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}
	if err := zfs.AddVdev(poolName, req.VdevType, req.Disks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"vdev added"}`)
}

func (h *ZFSHandler) addSLOG(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		Disks []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}
	if err := zfs.AddSLOG(poolName, req.Disks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"slog added"}`)
}

func (h *ZFSHandler) removeSLOG(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		Disks []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}
	if err := zfs.RemoveSLOG(poolName, req.Disks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"slog removed"}`)
}

func (h *ZFSHandler) addL2ARC(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		Disks []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}
	if err := zfs.AddL2ARC(poolName, req.Disks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"l2arc added"}`)
}

func (h *ZFSHandler) removeL2ARC(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		Disks []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		jsonErrorCoded(w, "disks required", http.StatusBadRequest, "zfs.disks_required")
		return
	}
	if err := zfs.RemoveL2ARC(poolName, req.Disks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"l2arc removed"}`)
}

func (h *ZFSHandler) replaceDisk(w http.ResponseWriter, r *http.Request, poolName, oldDisk string) {
	var req struct {
		NewDisk string `json:"new_disk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewDisk == "" {
		jsonErrorCoded(w, "new_disk required", http.StatusBadRequest, "zfs.new_disk_required")
		return
	}
	if err := zfs.ReplaceDisk(poolName, oldDisk, req.NewDisk); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"disk replaced, resilver started"}`)
}

func (h *ZFSHandler) scrubPool(w http.ResponseWriter, r *http.Request, name string) {
	if h.store != nil {
		decision, err := zfsMaintenanceDecision(h.store, name, spindownNow())
		if err != nil {
			serverError(w, err)
			return
		}
		if rejectBlockedMaintenance(w, name, "ZFS scrub", decision) {
			return
		}
		owners, err := zfsTierOwners(h.store, name)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, owner := range owners {
			decision, err := poolMaintenanceDecision(h.store, owner, spindownNow())
			if err != nil {
				serverError(w, err)
				return
			}
			if rejectBlockedMaintenance(w, owner, "ZFS backing scrub", decision) {
				return
			}
		}
	}
	if err := scrubZFSPool(name); err != nil {
		serverError(w, err)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"scrub started"}`)
}

func (h *ZFSHandler) getRawZFSSpindown(w http.ResponseWriter, r *http.Request, name string) {
	resp, err := h.rawZFSSpindownPolicy(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *ZFSHandler) updateRawZFSSpindown(w http.ResponseWriter, r *http.Request, name string) {
	var req updateRawZFSSpindownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	resp, err := h.rawZFSSpindownPolicy(name)
	if err != nil {
		serverError(w, err)
		return
	}
	if req.Enabled && !resp.Eligible {
		jsonError(w, "ZFS pool is not spindown eligible: "+strings.Join(resp.Reasons, "; "), http.StatusBadRequest)
		return
	}
	if req.ActiveWindows != nil {
		if _, err := spindown.StoreWindows(h.store, spindown.ZFSWindowsKey(name), *req.ActiveWindows); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Enabled {
		if err := setZFSAtimeOff(name); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := h.store.SetBoolConfig(spindown.ZFSEnabledKey(name), req.Enabled); err != nil {
		serverError(w, err)
		return
	}
	resp, err = h.rawZFSSpindownPolicy(name)
	if err != nil {
		serverError(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *ZFSHandler) rawZFSSpindownPolicy(name string) (*rawZFSSpindownPolicyResponse, error) {
	pool, err := detailZFSPool(name)
	if err != nil {
		return nil, err
	}
	enabled, err := spindown.Enabled(h.store, spindown.ZFSEnabledKey(name))
	if err != nil {
		return nil, err
	}
	decision, windows, err := spindown.DecisionFor(h.store, spindown.ZFSEnabledKey(name), spindown.ZFSWindowsKey(name), spindownNow())
	if err != nil {
		return nil, err
	}
	hasSpecial := zfs.HasSpecialVdevInLayout(pool.VdevLayout)
	reasons := []string{}
	if !hasSpecial {
		reasons = append(reasons, "raw ZFS pools require a real special metadata vdev")
	}
	return &rawZFSSpindownPolicyResponse{
		Enabled:        enabled,
		Eligible:       len(reasons) == 0,
		Reasons:        reasons,
		HasSpecialVdev: hasSpecial,
		ActiveWindows:  windows,
		ActiveNow:      decision.ActiveNow,
		NextActiveAt:   decision.NextActiveAt,
	}, nil
}

// --- Datasets ---

func (h *ZFSHandler) routeDatasets(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/datasets" || path == "/api/datasets/" {
		switch r.Method {
		case http.MethodGet:
			h.listDatasets(w, r)
		case http.MethodPost:
			h.createDataset(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	// /api/datasets/{name}/...  (name can contain slashes, encoded as pool--dataset)
	rest := strings.TrimPrefix(path, "/api/datasets/")
	parts := strings.SplitN(rest, "/", 2)
	dsID := parts[0] // URL-encoded dataset identifier
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	// Convert URL path separators: use -- as separator in URL, convert to /
	dsName := strings.ReplaceAll(dsID, "--", "/")

	switch subpath {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.getDataset(w, r, dsName)
		case http.MethodPut:
			h.updateDataset(w, r, dsName)
		case http.MethodDelete:
			h.deleteDataset(w, r, dsName)
		default:
			jsonMethodNotAllowed(w)
		}
	case "mount":
		if r.Method == http.MethodPost {
			h.mountDataset(w, r, dsName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "unmount":
		if r.Method == http.MethodPost {
			h.unmountDataset(w, r, dsName)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *ZFSHandler) listDatasets(w http.ResponseWriter, r *http.Request) {
	pool := r.URL.Query().Get("pool")
	var datasets []zfs.Dataset
	var err error
	if pool == "" {
		datasets, err = h.datasetsCache.GetOrFetch(func() ([]zfs.Dataset, error) {
			return zfs.ListDatasets("")
		})
	} else {
		datasets, err = zfs.ListDatasets(pool)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if datasets == nil {
		datasets = []zfs.Dataset{}
	}
	json.NewEncoder(w).Encode(datasets)
}

type createDatasetRequest struct {
	Name        string `json:"name"`
	MountPoint  string `json:"mount_point"`
	Compression string `json:"compression"`
	Quota       uint64 `json:"quota"`
	Reservation uint64 `json:"reservation"`
}

func (h *ZFSHandler) createDataset(w http.ResponseWriter, r *http.Request) {
	var req createDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Name == "" {
		jsonErrorCoded(w, "name required", http.StatusBadRequest, "zfs.name_required")
		return
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Creating dataset...")
		if err := zfs.CreateDataset(req.Name, req.MountPoint, req.Compression, req.Quota, req.Reservation); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.datasetsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "created", "dataset": req.Name})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) getDataset(w http.ResponseWriter, r *http.Request, name string) {
	datasets, err := zfs.ListDatasets("")
	if err != nil {
		serverError(w, err)
		return
	}
	for _, ds := range datasets {
		if ds.Name == name {
			json.NewEncoder(w).Encode(ds)
			return
		}
	}
	jsonErrorCoded(w, "dataset not found", http.StatusNotFound, "zfs.dataset_not_found")
}

func (h *ZFSHandler) updateDataset(w http.ResponseWriter, r *http.Request, name string) {
	var props map[string]string
	if err := json.NewDecoder(r.Body).Decode(&props); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if err := zfs.UpdateDataset(name, props); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.datasetsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"updated"}`)
}

func (h *ZFSHandler) deleteDataset(w http.ResponseWriter, r *http.Request, name string) {
	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Destroying dataset...")
		if err := zfs.DestroyDataset(name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.datasetsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "destroyed"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) mountDataset(w http.ResponseWriter, r *http.Request, name string) {
	if err := zfs.MountDataset(name); err != nil {
		serverError(w, err)
		return
	}
	h.datasetsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"mounted"}`)
}

func (h *ZFSHandler) unmountDataset(w http.ResponseWriter, r *http.Request, name string) {
	if err := zfs.UnmountDataset(name); err != nil {
		serverError(w, err)
		return
	}
	h.datasetsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"unmounted"}`)
}

// --- Zvols ---

func (h *ZFSHandler) routeZvols(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/zvols" || path == "/api/zvols/" {
		switch r.Method {
		case http.MethodGet:
			h.listZvols(w, r)
		case http.MethodPost:
			h.createZvol(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	rest := strings.TrimPrefix(path, "/api/zvols/")
	parts := strings.SplitN(rest, "/", 2)
	zvolID := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	zvolName := strings.ReplaceAll(zvolID, "--", "/")

	switch subpath {
	case "":
		if r.Method == http.MethodDelete {
			h.deleteZvol(w, r, zvolName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "resize":
		if r.Method == http.MethodPut {
			h.resizeZvol(w, r, zvolName)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *ZFSHandler) listZvols(w http.ResponseWriter, r *http.Request) {
	pool := r.URL.Query().Get("pool")
	var zvols []zfs.Zvol
	var err error
	if pool == "" {
		zvols, err = h.zvolsCache.GetOrFetch(func() ([]zfs.Zvol, error) {
			return zfs.ListZvols("")
		})
	} else {
		zvols, err = zfs.ListZvols(pool)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if zvols == nil {
		zvols = []zfs.Zvol{}
	}
	json.NewEncoder(w).Encode(zvols)
}

type createZvolRequest struct {
	Name      string `json:"name"`
	Size      string `json:"size"`
	BlockSize string `json:"block_size"`
}

func (h *ZFSHandler) createZvol(w http.ResponseWriter, r *http.Request) {
	var req createZvolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Name == "" || req.Size == "" {
		jsonErrorCoded(w, "name and size required", http.StatusBadRequest, "zfs.name_and_size_required")
		return
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Creating zvol...")
		if err := zfs.CreateZvol(req.Name, req.Size, req.BlockSize); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.zvolsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "created", "zvol": req.Name})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) deleteZvol(w http.ResponseWriter, r *http.Request, name string) {
	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Destroying zvol...")
		if err := zfs.DestroyZvol(name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.zvolsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "destroyed"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) resizeZvol(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Size string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Size == "" {
		jsonErrorCoded(w, "size required", http.StatusBadRequest, "zfs.size_required")
		return
	}
	if err := zfs.ResizeZvol(name, req.Size); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.zvolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"resized"}`)
}

// --- Snapshots ---

func (h *ZFSHandler) routeSnapshots(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/api/snapshots" || path == "/api/snapshots/" {
		switch r.Method {
		case http.MethodGet:
			h.listSnapshots(w, r)
		case http.MethodPost:
			h.createSnapshot(w, r)
		default:
			jsonMethodNotAllowed(w)
		}
		return
	}

	// /api/snapshots/{id}/...
	rest := strings.TrimPrefix(path, "/api/snapshots/")
	parts := strings.SplitN(rest, "/", 2)
	snapID := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}

	// Snapshot IDs in URL: dataset--snapname (@ is not URL-safe)
	snapName := strings.ReplaceAll(snapID, "--", "/")
	// Reconstruct the @ separator: last / becomes @
	if idx := strings.LastIndex(snapName, "/"); idx > 0 {
		// Only if there's a dataset prefix already containing /
		// For simple pool@snap, the -- gives pool/snap, need pool@snap
	}
	// Actually use a different approach: snap IDs use ~ as @ replacement in URL
	snapName = strings.ReplaceAll(snapID, "~", "@")
	snapName = strings.ReplaceAll(snapName, "--", "/")

	switch subpath {
	case "":
		if r.Method == http.MethodDelete {
			h.deleteSnapshot(w, r, snapName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "rollback":
		if r.Method == http.MethodPost {
			h.rollbackSnapshot(w, r, snapName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "clone":
		if r.Method == http.MethodPost {
			h.cloneSnapshot(w, r, snapName)
		} else {
			jsonMethodNotAllowed(w)
		}
	case "send":
		if r.Method == http.MethodPost {
			h.sendSnapshot(w, r, snapName)
		} else {
			jsonMethodNotAllowed(w)
		}
	default:
		jsonNotFound(w)
	}
}

func (h *ZFSHandler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	dataset := r.URL.Query().Get("dataset")
	var snaps []zfs.Snapshot
	var err error
	if dataset == "" {
		snaps, err = h.snapshotsCache.GetOrFetch(func() ([]zfs.Snapshot, error) {
			return zfs.ListSnapshots("")
		})
	} else {
		snaps, err = zfs.ListSnapshots(dataset)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if snaps == nil {
		snaps = []zfs.Snapshot{}
	}
	json.NewEncoder(w).Encode(snaps)
}

type createSnapshotRequest struct {
	Dataset string `json:"dataset"`
	Name    string `json:"name"`
}

func (h *ZFSHandler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req createSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.Dataset == "" || req.Name == "" {
		jsonErrorCoded(w, "dataset and name required", http.StatusBadRequest, "zfs.dataset_and_name_required")
		return
	}

	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Creating snapshot...")
		if err := zfs.CreateSnapshot(req.Dataset, req.Name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.snapshotsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "created", "snapshot": req.Dataset + "@" + req.Name})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) deleteSnapshot(w http.ResponseWriter, r *http.Request, name string) {
	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Destroying snapshot...")
		if err := zfs.DestroySnapshot(name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.snapshotsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "destroyed"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) rollbackSnapshot(w http.ResponseWriter, r *http.Request, name string) {
	jobID := jobs.Start()
	go func() {
		jobs.UpdateProgress(jobID, "Rolling back snapshot...")
		if err := zfs.RollbackSnapshot(name); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		h.snapshotsCache.Invalidate()
		h.datasetsCache.Invalidate()
		jobs.Complete(jobID, map[string]string{"status": "rolled back"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) cloneSnapshot(w http.ResponseWriter, r *http.Request, snapName string) {
	var req struct {
		NewDataset string `json:"new_dataset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewDataset == "" {
		jsonErrorCoded(w, "new_dataset required", http.StatusBadRequest, "zfs.new_dataset_required")
		return
	}
	if err := zfs.CloneSnapshot(snapName, req.NewDataset); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.datasetsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"cloned","dataset":"%s"}`, req.NewDataset)
}

func (h *ZFSHandler) sendSnapshot(w http.ResponseWriter, r *http.Request, snapName string) {
	var req struct {
		OutputPath string `json:"output_path"`
		BaseSnap   string `json:"base_snap"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	if req.OutputPath == "" {
		req.OutputPath = "/tmp/zfs-send-" + strings.ReplaceAll(snapName, "/", "-") + ".zfs"
	}

	jobID := jobs.Start()
	outputPath := req.OutputPath
	go func() {
		jobs.UpdateProgress(jobID, "Sending snapshot...")
		if err := zfs.SendSnapshot(snapName, outputPath, req.BaseSnap); err != nil {
			jobs.Fail(jobID, err)
			return
		}
		jobs.Complete(jobID, map[string]string{"status": "sent", "path": outputPath})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}
