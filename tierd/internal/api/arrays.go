package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/cache"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tier"
)

var listMDADMArrays = mdadm.List

// ArraysHandler handles /api/arrays* and /api/tiers* endpoints.
type ArraysHandler struct {
	store       *db.Store
	arraysCache *cache.Entry[[]richArray]
	tierMapMu   sync.RWMutex
	tierMapInfo map[string]tierMapVerification
	// poolMu serialises mutating operations (assign, unassign, delete) on a
	// per-pool basis so concurrent requests cannot race on the same pool's LVM
	// state.
	poolMu sync.Map
	// provisionPerTierStorage creates an independent VG/LV per tier slot.
	// Used for new pools with per-tier FUSE routing.
	provisionPerTierStorage func(poolName, tierName string) error
	// ensureNamespace creates a FUSE-managed namespace for the pool if one
	// does not already exist. Called after successful per-tier provisioning
	// so the FUSE mount at /mnt/{pool} is set up automatically.
	ensureNamespace func(poolName string) error
	// purgeBackupsForPath cancels in-flight backup runs and deletes backup
	// configs whose LocalPath falls under the given mount path. Called at the
	// start of destroyTierPool so a backup schedule does not immediately race
	// rsync against a freshly recreated tier. Returns configs deleted.
	purgeBackupsForPath func(mountPath string) (int, error)
	// destroyPoolNamespaces stops FUSE daemons and tears down backing mounts
	// for every managed namespace belonging to the given pool. Called before
	// LVM teardown in destroyTierPool so the mount point is free and the
	// daemon cannot re-create the FUSE mount during / after destruction.
	destroyPoolNamespaces func(poolName string) error
	// asyncDone is an optional channel signalled when an async goroutine
	// (tier assign/delete) completes. Used by tests to wait for background
	// work. Nil in production.
	asyncDone chan struct{}
}

type tierMapVerification struct {
	Verified   bool
	VerifiedAt string
}

func NewArraysHandler(store *db.Store) *ArraysHandler {
	m := tier.NewManager(store)
	return &ArraysHandler{
		store:            store,
		arraysCache:      cache.New[[]richArray](30 * time.Second),
		tierMapInfo:      make(map[string]tierMapVerification),
		provisionPerTierStorage: m.ProvisionPerTierStorage,
		ensureNamespace:         func(string) error { return nil },
		purgeBackupsForPath:     func(string) (int, error) { return 0, nil },
		destroyPoolNamespaces:   func(string) error { return nil },
	}
}

// SetEnsureNamespace sets the callback used to create a FUSE-managed namespace
// for a pool after its first successful per-tier provisioning.
func (h *ArraysHandler) SetEnsureNamespace(fn func(string) error) {
	h.ensureNamespace = fn
}

// SetPurgeBackupsForPath wires the backup-config purge used during tier pool
// destruction.
func (h *ArraysHandler) SetPurgeBackupsForPath(fn func(string) (int, error)) {
	h.purgeBackupsForPath = fn
}

// SetDestroyPoolNamespaces wires the callback that stops FUSE daemons and
// tears down backing targets for a pool's managed namespaces during tier
// destruction.
func (h *ArraysHandler) SetDestroyPoolNamespaces(fn func(string) error) {
	h.destroyPoolNamespaces = fn
}

// invalidateAll clears the array cache. Volumes are read live on every
// request, so they have no separate cache to invalidate.
func (h *ArraysHandler) invalidateAll() {
	h.arraysCache.Invalidate()
}

// lockPool acquires the per-pool mutex for poolName and returns an unlock
// function. Callers must defer the returned function to release the lock.
func (h *ArraysHandler) lockPool(poolName string) func() {
	actual, _ := h.poolMu.LoadOrStore(poolName, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (h *ArraysHandler) setTierMapVerification(poolName string, verified bool, at time.Time) string {
	recordedAt := at.UTC().Format(time.RFC3339)
	h.tierMapMu.Lock()
	h.tierMapInfo[poolName] = tierMapVerification{
		Verified:   verified,
		VerifiedAt: recordedAt,
	}
	h.tierMapMu.Unlock()
	return recordedAt
}

func (h *ArraysHandler) clearTierMapVerification(poolName string) {
	h.tierMapMu.Lock()
	delete(h.tierMapInfo, poolName)
	h.tierMapMu.Unlock()
}

func (h *ArraysHandler) refreshTierMapVerificationIfLVExists(poolName string) error {
	exists, err := tierDataLVExists("tier-"+poolName, "data")
	if err != nil || !exists {
		return err
	}
	_, err = h.refreshTierMap(poolName)
	return err
}

// Route dispatches array, volume, and tier requests.
func (h *ArraysHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case strings.HasPrefix(path, "/api/arrays"):
		h.routeArrays(w, r)
	case strings.HasPrefix(path, "/api/tiers"):
		h.routeTiers(w, r)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// --- Arrays ---

func (h *ArraysHandler) routeArrays(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// GET/POST /api/arrays
	if path == "/api/arrays" || path == "/api/arrays/" {
		switch r.Method {
		case http.MethodGet:
			h.listArrays(w, r)
		case http.MethodPost:
			h.createArray(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	// /api/arrays/{id}/...
	rest := strings.TrimPrefix(path, "/api/arrays/")
	parts := strings.SplitN(rest, "/", 2)
	arrayName := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	arrayPath := "/dev/" + arrayName

	switch subpath {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.getArray(w, r, arrayPath)
		case http.MethodDelete:
			h.deleteArray(w, r, arrayPath)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "disks":
		if r.Method == http.MethodPost {
			h.addDisk(w, r, arrayPath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "scrub":
		if r.Method == http.MethodPost {
			h.scrubArray(w, r, arrayPath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		// /api/arrays/{id}/disks/{disk} or /api/arrays/{id}/disks/{disk}/replace
		if strings.HasPrefix(subpath, "disks/") {
			h.routeArrayDisk(w, r, arrayPath, strings.TrimPrefix(subpath, "disks/"))
		} else {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}
}

func (h *ArraysHandler) routeArrayDisk(w http.ResponseWriter, r *http.Request, arrayPath, diskSubpath string) {
	parts := strings.SplitN(diskSubpath, "/", 2)
	diskName := parts[0]
	diskPath := "/dev/" + diskName
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method == http.MethodDelete {
			h.removeDisk(w, r, arrayPath, diskPath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	case "replace":
		if r.Method == http.MethodPost {
			h.replaceDisk(w, r, arrayPath, diskPath)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// richArray extends mdadm.Array with tier assignment.
// Tier is populated from the database.
type richArray struct {
	ID int64 `json:"id"`
	mdadm.Array
	Tier       string `json:"tier,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`
	LVSize     string `json:"lv_size,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	Mounted    bool   `json:"mounted"`
}

func (h *ArraysHandler) fetchRichArrays() ([]richArray, error) {
	arrays, err := listMDADMArrays()
	if err != nil {
		return nil, err
	}
	if arrays == nil {
		arrays = []mdadm.Array{}
	}

	// Tier assignments by array path (named-tier model).
	tierByArray := map[string]string{}
	if assignments, err := h.store.ListTierArrayAssignments(); err == nil {
		for _, a := range assignments {
			tierByArray[a.ArrayPath] = fmt.Sprintf("%s/%s", a.TierName, a.Slot)
		}
	}

	rich := []richArray{}
	for _, a := range arrays {
		r := richArray{Array: a}
		arrayID, err := h.store.EnsureMDADMArray(a.Path)
		if err != nil {
			return nil, err
		}
		r.ID = arrayID
		if tierName, ok := tierByArray[a.Path]; ok {
			r.Tier = tierName
		}
		rich = append(rich, r)
	}
	return rich, nil
}

func (h *ArraysHandler) listArrays(w http.ResponseWriter, r *http.Request) {
	rich, err := h.arraysCache.GetOrFetch(h.fetchRichArrays)
	if err != nil {
		serverError(w, err)
		return
	}
	json.NewEncoder(w).Encode(rich)
}

type createArrayRequest struct {
	Name  string   `json:"name"`
	Level string   `json:"level"`
	Disks []string `json:"disks"`
}

func (h *ArraysHandler) createArray(w http.ResponseWriter, r *http.Request) {
	var req createArrayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.Level == "" || len(req.Disks) == 0 {
		http.Error(w, `{"error":"name, level, and disks required"}`, http.StatusBadRequest)
		return
	}

	jobID := jobs.StartTagged("array-create")
	go h.runCreateArray(jobID, req)

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ArraysHandler) runCreateArray(jobID string, req createArrayRequest) {
	progress := func(msg string) { jobs.UpdateProgress(jobID, msg) }
	arrayDev := "/dev/" + req.Name

	// Validate inputs before starting slow work.
	if err := mdadm.ValidateRAIDLevel(req.Level); err != nil {
		jobs.Fail(jobID, err)
		return
	}
	for _, d := range req.Disks {
		if err := mdadm.ValidateDiskPath(d); err != nil {
			jobs.Fail(jobID, err)
			return
		}
	}

	progress("Preparing disks...")
	if err := mdadm.PrepareDisks(req.Disks); err != nil {
		jobs.Fail(jobID, fmt.Errorf("prepare disks: %w", err))
		return
	}

	progress("Creating RAID array...")
	if err := mdadm.Assemble(req.Name, req.Level, req.Disks); err != nil {
		jobs.Fail(jobID, err)
		return
	}

	// Tune stripe_cache_size for parity RAID levels. Best-effort: failure
	// here only costs performance, not correctness.
	if mdadm.IsParityRAID(req.Level) {
		mdadm.SetStripeCacheSize(arrayDev, mdadm.DefaultStripeCachePages)
	}

	progress("Saving mdadm configuration...")
	if err := mdadm.SaveConf(); err != nil {
		jobs.Fail(jobID, err)
		return
	}

	h.invalidateAll()
	jobs.Complete(jobID, map[string]string{"status": "created", "path": arrayDev})
}

func (h *ArraysHandler) getArray(w http.ResponseWriter, r *http.Request, path string) {
	a, err := mdadm.Detail(path)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(a)
}

func (h *ArraysHandler) deleteArray(w http.ResponseWriter, r *http.Request, path string) {
	if a, err := h.store.GetTierAssignmentByArrayPath(path); err == nil {
		http.Error(w, fmt.Sprintf(`{"error":"array is backing tier %s slot %s; delete the tier before destroying the array"}`, a.TierName, a.Slot), http.StatusConflict)
		return
	}

	a, _ := mdadm.Detail(path)

	name := strings.TrimPrefix(path, "/dev/")
	var memberDisks []string
	if a != nil {
		name = a.Name
		memberDisks = a.MemberDisks
	}

	jobID := jobs.StartTagged("array-destroy")
	go h.runDeleteArray(jobID, path, name, memberDisks)

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"job_id":"%s"}`, jobID)
}

func (h *ArraysHandler) runDeleteArray(jobID, path, name string, memberDisks []string) {
	progress := func(msg string) { jobs.UpdateProgress(jobID, msg) }

	progress("Stopping RAID array...")
	if err := mdadm.Stop(path); err != nil {
		jobs.Fail(jobID, err)
		return
	}

	progress("Zeroing superblocks...")
	mdadm.ZeroSuperblocks(memberDisks)

	// Wipe any filesystem or LVM PV signatures left on the array device itself.
	// This prevents stale VG metadata from blocking future tier provisioning,
	// including metadata from a different machine's install.
	progress("Wiping array device signatures...")
	_ = lvm.WipeSignatures(path)
	_ = lvm.RemovePV(path)

	progress("Saving mdadm configuration...")
	mdadm.SaveConf()

	h.invalidateAll()
	jobs.Complete(jobID, map[string]string{"status": "destroyed"})
}

func (h *ArraysHandler) addDisk(w http.ResponseWriter, r *http.Request, arrayPath string) {
	var req struct {
		Disk string `json:"disk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Disk == "" {
		http.Error(w, `{"error":"disk path required"}`, http.StatusBadRequest)
		return
	}

	if err := mdadm.AddDisk(arrayPath, req.Disk); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.invalidateAll()
	fmt.Fprintf(w, `{"status":"disk added"}`)
}

func (h *ArraysHandler) removeDisk(w http.ResponseWriter, r *http.Request, arrayPath, diskPath string) {
	if err := mdadm.FailDisk(arrayPath, diskPath); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := mdadm.RemoveDisk(arrayPath, diskPath); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.invalidateAll()
	fmt.Fprintf(w, `{"status":"disk removed"}`)
}

func (h *ArraysHandler) replaceDisk(w http.ResponseWriter, r *http.Request, arrayPath, oldDisk string) {
	var req struct {
		NewDisk string `json:"new_disk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewDisk == "" {
		http.Error(w, `{"error":"new_disk path required"}`, http.StatusBadRequest)
		return
	}

	if err := mdadm.ReplaceDisk(arrayPath, oldDisk, req.NewDisk); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.invalidateAll()
	fmt.Fprintf(w, `{"status":"disk replaced, rebuild started"}`)
}

func (h *ArraysHandler) scrubArray(w http.ResponseWriter, r *http.Request, path string) {
	if err := mdadm.Scrub(path); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.arraysCache.Invalidate()
	fmt.Fprintf(w, `{"status":"scrub started"}`)
}
