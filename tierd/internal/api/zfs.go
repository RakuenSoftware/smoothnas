package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

// ZFSHandler handles /api/pools*, /api/datasets*, /api/zvols*, /api/snapshots* endpoints.
type ZFSHandler struct {
	poolsCache     *cache.Entry[[]zfs.Pool]
	datasetsCache  *cache.Entry[[]zfs.Dataset]
	zvolsCache     *cache.Entry[[]zfs.Zvol]
	snapshotsCache *cache.Entry[[]zfs.Snapshot]
}

func NewZFSHandler() *ZFSHandler {
	return &ZFSHandler{
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
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "vdevs":
		if r.Method == http.MethodPost {
			h.addVdev(w, r, poolName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "slog":
		switch r.Method {
		case http.MethodPost:
			h.addSLOG(w, r, poolName)
		case http.MethodDelete:
			h.removeSLOG(w, r, poolName)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "l2arc":
		switch r.Method {
		case http.MethodPost:
			h.addL2ARC(w, r, poolName)
		case http.MethodDelete:
			h.removeL2ARC(w, r, poolName)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "scrub":
		if r.Method == http.MethodPost {
			h.scrubPool(w, r, poolName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			}
		} else {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" || len(req.DataDisks) == 0 {
		http.Error(w, `{"error":"name and data_disks required"}`, http.StatusBadRequest)
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
		jobs.Complete(jobID, map[string]string{"status": "destroyed"})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ZFSHandler) addVdev(w http.ResponseWriter, r *http.Request, poolName string) {
	var req struct {
		VdevType string   `json:"vdev_type"`
		Disks    []string `json:"disks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Disks) == 0 {
		http.Error(w, `{"error":"disks required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"disks required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"disks required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"disks required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"disks required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"new_disk required"}`, http.StatusBadRequest)
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
	if err := zfs.Scrub(name); err != nil {
		serverError(w, err)
		return
	}
	h.poolsCache.Invalidate()
	fmt.Fprintf(w, `{"status":"scrub started"}`)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "mount":
		if r.Method == http.MethodPost {
			h.mountDataset(w, r, dsName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "unmount":
		if r.Method == http.MethodPost {
			h.unmountDataset(w, r, dsName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
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
	http.Error(w, `{"error":"dataset not found"}`, http.StatusNotFound)
}

func (h *ZFSHandler) updateDataset(w http.ResponseWriter, r *http.Request, name string) {
	var props map[string]string
	if err := json.NewDecoder(r.Body).Decode(&props); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "resize":
		if r.Method == http.MethodPut {
			h.resizeZvol(w, r, zvolName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Size == "" {
		http.Error(w, `{"error":"name and size required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"size required"}`, http.StatusBadRequest)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "rollback":
		if r.Method == http.MethodPost {
			h.rollbackSnapshot(w, r, snapName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "clone":
		if r.Method == http.MethodPost {
			h.cloneSnapshot(w, r, snapName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "send":
		if r.Method == http.MethodPost {
			h.sendSnapshot(w, r, snapName)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Dataset == "" || req.Name == "" {
		http.Error(w, `{"error":"dataset and name required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"new_dataset required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
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
