package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/JBailes/SmoothNAS/tierd/internal/backup"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// BackupHandler handles /api/backup/* endpoints.
type BackupHandler struct {
	store *db.Store

	mu          sync.Mutex
	cancelFuncs map[int64]context.CancelFunc
	// liveProgress holds transient progress state for currently-running
	// backup runs. rsync emits progress ticks at high frequency
	// (hundreds per second on a fast source) and the old code persisted
	// every tick to backup_runs via UPDATE. Live pprof showed that SQL
	// write dominating the hot path (~18% cum, under the sql.DB
	// connection lock), starving HandleOpen.
	//
	// Progress is pure ephemeral UI state:
	//   - CompleteBackupRun / FailBackupRun wipe the progress column on
	//     terminal transition anyway
	//   - MarkStaleRunsFailed fails any "running" row on tierd restart,
	//     so any progress surviving a crash is discarded
	// Holding it in memory only is correct and drops the SQL write
	// entirely. Terminal state (started → completed / failed) is
	// unchanged and still persisted.
	liveProgress map[int64]liveRunProgress
}

// liveRunProgress is the in-memory view overlaid onto backup_runs reads
// for runs that are still executing.
type liveRunProgress struct {
	Msg   string
	Done  int
	Total int
}

func NewBackupHandler(store *db.Store) *BackupHandler {
	return &BackupHandler{
		store:        store,
		cancelFuncs:  make(map[int64]context.CancelFunc),
		liveProgress: make(map[int64]liveRunProgress),
	}
}

// setLiveProgress replaces the in-memory progress entry for a run.
// Latest-wins — intermediate values between reads are discarded, which
// is exactly what a coalescing progress ticker wants.
func (h *BackupHandler) setLiveProgress(runID int64, msg string, done, total int) {
	h.mu.Lock()
	h.liveProgress[runID] = liveRunProgress{Msg: msg, Done: done, Total: total}
	h.mu.Unlock()
}

// clearLiveProgress removes the in-memory entry for a run. Called on
// terminal transitions so reads after completion see the SQL state
// unmodified.
func (h *BackupHandler) clearLiveProgress(runID int64) {
	h.mu.Lock()
	delete(h.liveProgress, runID)
	h.mu.Unlock()
}

// applyLiveProgress overlays in-memory progress onto a slice of backup
// runs read from SQL. Only running rows are touched; terminal rows
// passed through unchanged.
func (h *BackupHandler) applyLiveProgress(runs []db.BackupRun) {
	if len(runs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range runs {
		if runs[i].Status != "running" {
			continue
		}
		p, ok := h.liveProgress[runs[i].ID]
		if !ok {
			continue
		}
		runs[i].Progress = p.Msg
		runs[i].FilesDone = p.Done
		runs[i].FilesTotal = p.Total
		if p.Total > 0 && p.Done >= 0 {
			pct := p.Done * 100 / p.Total
			if pct > 100 {
				pct = 100
			}
			runs[i].ProgressPct = pct
		} else {
			runs[i].ProgressPct = -1
		}
	}
}

// applyLiveProgressOne is the single-row variant of applyLiveProgress.
func (h *BackupHandler) applyLiveProgressOne(run *db.BackupRun) {
	if run == nil || run.Status != "running" {
		return
	}
	h.mu.Lock()
	p, ok := h.liveProgress[run.ID]
	h.mu.Unlock()
	if !ok {
		return
	}
	run.Progress = p.Msg
	run.FilesDone = p.Done
	run.FilesTotal = p.Total
	if p.Total > 0 && p.Done >= 0 {
		pct := p.Done * 100 / p.Total
		if pct > 100 {
			pct = 100
		}
		run.ProgressPct = pct
	} else {
		run.ProgressPct = -1
	}
}

// Route dispatches backup requests.
func (h *BackupHandler) Route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/api/backup/configs" || path == "/api/backup/configs/":
		switch r.Method {
		case http.MethodGet:
			h.listConfigs(w, r)
		case http.MethodPost:
			h.createConfig(w, r)
		default:
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/backup/configs/"):
		rest := strings.TrimPrefix(path, "/api/backup/configs/")
		parts := strings.SplitN(rest, "/", 2)
		idStr := parts[0]
		subpath := ""
		if len(parts) > 1 {
			subpath = parts[1]
		}

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			jsonErrorCoded(w, "invalid config id", http.StatusBadRequest, "backup.invalid_config_id")
			return
		}

		switch subpath {
		case "":
			switch r.Method {
			case http.MethodDelete:
				h.deleteConfig(w, r, id)
			case http.MethodPut:
				h.updateConfig(w, r, id)
			default:
				jsonMethodNotAllowed(w)
			}
		case "run":
			if r.Method == http.MethodPost {
				h.runBackup(w, r, id)
			} else {
				jsonMethodNotAllowed(w)
			}
		default:
			jsonNotFound(w)
		}

	case path == "/api/backup/runs" || path == "/api/backup/runs/":
		if r.Method == http.MethodGet {
			h.listRuns(w, r)
		} else {
			jsonMethodNotAllowed(w)
		}

	case strings.HasPrefix(path, "/api/backup/runs/"):
		rest := strings.TrimPrefix(path, "/api/backup/runs/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			jsonErrorCoded(w, "invalid run id", http.StatusBadRequest, "backup.invalid_run_id")
			return
		}
		subpath := ""
		if len(parts) > 1 {
			subpath = parts[1]
		}
		switch subpath {
		case "":
			if r.Method == http.MethodGet {
				h.getRun(w, r, id)
			} else {
				jsonMethodNotAllowed(w)
			}
		case "cancel":
			if r.Method == http.MethodPost {
				h.cancelRun(w, r, id)
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

func (h *BackupHandler) listConfigs(w http.ResponseWriter, r *http.Request) {
	cfgs, err := h.store.ListBackupConfigs()
	if err != nil {
		serverError(w, err)
		return
	}
	if cfgs == nil {
		cfgs = []db.BackupConfig{}
	}
	json.NewEncoder(w).Encode(cfgs)
}

type createBackupConfigRequest struct {
	Name        string `json:"name"`
	TargetType  string `json:"target_type"`
	Host        string `json:"host"`
	Share       string `json:"share"`
	SMBUser     string `json:"smb_user"`
	SMBPass     string `json:"smb_pass"`
	SSHUser     string `json:"ssh_user"`
	SSHPass     string `json:"ssh_pass"`
	LocalPath   string `json:"local_path"`
	RemotePath  string `json:"remote_path"`
	Direction   string `json:"direction"`
	Method      string `json:"method"`
	Parallelism int    `json:"parallelism"`
	UseSSH      bool   `json:"use_ssh"`  // method=rsync only
	Compress    bool   `json:"compress"` // method=rsync only
	DeleteMode  bool   `json:"delete_mode"`
}

var spillFlagPathForUUID = func(uuid string) string {
	return filepath.Join("/sys/fs/smoothfs", uuid, "any_spill_since_mount")
}

func (h *BackupHandler) createConfig(w http.ResponseWriter, r *http.Request) {
	var req createBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}

	// Strip surrounding whitespace on path-like fields. Trailing tabs in
	// particular break silently: mount.nfs sends the literal tab to the
	// server, which rejects with "No such file or directory".
	req.Host = strings.TrimSpace(req.Host)
	req.Share = strings.TrimSpace(req.Share)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	req.RemotePath = strings.TrimSpace(req.RemotePath)
	req.Name = strings.TrimSpace(req.Name)

	if err := validateBackupConfig(req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Parallelism < 1 {
		req.Parallelism = 1
	} else if req.Parallelism > 16 {
		req.Parallelism = 16
	}
	// For rsync-over-SSH, target_type is unused (SSH transport bypasses any
	// mount). Default to "nfs" so the DB CHECK constraint is satisfied.
	targetType := req.TargetType
	if req.Method == "rsync" && req.UseSSH && targetType == "" {
		targetType = "nfs"
	}
	cfg, err := h.store.CreateBackupConfig(db.BackupConfig{
		Name:        req.Name,
		TargetType:  targetType,
		Host:        req.Host,
		Share:       req.Share,
		SMBUser:     req.SMBUser,
		SMBPass:     req.SMBPass,
		SSHUser:     req.SSHUser,
		SSHPass:     req.SSHPass,
		LocalPath:   req.LocalPath,
		RemotePath:  req.RemotePath,
		Direction:   req.Direction,
		Method:      req.Method,
		Parallelism: req.Parallelism,
		UseSSH:      req.UseSSH,
		Compress:    effectiveCompress(req.Method, req.UseSSH, req.Compress),
		DeleteMode:  effectiveDeleteMode(req.Method, req.DeleteMode),
	})
	if err != nil {
		if err == db.ErrDuplicate {
			jsonErrorCoded(w, "a backup config with that name already exists", http.StatusConflict, "backup.config_name_taken")
		} else {
			serverError(w, err)
		}
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(cfg)
}

func (h *BackupHandler) deleteConfig(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.store.DeleteBackupConfig(id); err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "backup config not found", http.StatusNotFound, "backup.config_not_found")
		} else {
			serverError(w, err)
		}
		return
	}
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

// updateConfig handles PUT /api/backup/configs/{id}.
// Accepts the same payload shape as createConfig. For sensitive fields
// (smb_pass, ssh_pass) an empty string means "leave the existing secret
// unchanged" — the UI never receives stored secrets, so a blank field on
// the edit form shouldn't silently clear them.
func (h *BackupHandler) updateConfig(w http.ResponseWriter, r *http.Request, id int64) {
	var req createBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	req.Share = strings.TrimSpace(req.Share)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	req.RemotePath = strings.TrimSpace(req.RemotePath)
	req.Name = strings.TrimSpace(req.Name)

	if err := validateBackupConfig(req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Parallelism < 1 {
		req.Parallelism = 1
	} else if req.Parallelism > 16 {
		req.Parallelism = 16
	}
	targetType := req.TargetType
	if req.Method == "rsync" && req.UseSSH && targetType == "" {
		targetType = "nfs"
	}

	old, err := h.store.GetBackupConfig(id)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "backup config not found", http.StatusNotFound, "backup.config_not_found")
		} else {
			serverError(w, err)
		}
		return
	}
	smbPass := req.SMBPass
	if smbPass == "" {
		smbPass = old.SMBPass
	}
	sshPass := req.SSHPass
	if sshPass == "" {
		sshPass = old.SSHPass
	}

	cfg, err := h.store.UpdateBackupConfig(id, db.BackupConfig{
		Name:        req.Name,
		TargetType:  targetType,
		Host:        req.Host,
		Share:       req.Share,
		SMBUser:     req.SMBUser,
		SMBPass:     smbPass,
		SSHUser:     req.SSHUser,
		SSHPass:     sshPass,
		LocalPath:   req.LocalPath,
		RemotePath:  req.RemotePath,
		Direction:   req.Direction,
		Method:      req.Method,
		Parallelism: req.Parallelism,
		UseSSH:      req.UseSSH,
		Compress:    effectiveCompress(req.Method, req.UseSSH, req.Compress),
		DeleteMode:  effectiveDeleteMode(req.Method, req.DeleteMode),
	})
	if err != nil {
		switch err {
		case db.ErrDuplicate:
			jsonErrorCoded(w, "a backup config with that name already exists", http.StatusConflict, "backup.config_name_taken")
		case db.ErrNotFound:
			jsonErrorCoded(w, "backup config not found", http.StatusNotFound, "backup.config_not_found")
		default:
			serverError(w, err)
		}
		return
	}

	// GetBackupConfig leaks stored passwords — strip them before responding.
	cfg.SMBPass = ""
	cfg.SSHPass = ""
	json.NewEncoder(w).Encode(cfg)
}

func (h *BackupHandler) runBackup(w http.ResponseWriter, r *http.Request, id int64) {
	cfg, err := h.store.GetBackupConfig(id)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "backup config not found", http.StatusNotFound, "backup.config_not_found")
		} else {
			serverError(w, err)
		}
		return
	}
	if err := h.guardDeleteMode(cfg); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}

	runID, err := h.store.CreateBackupRun(id)
	if err != nil {
		serverError(w, err)
		return
	}

	log.Printf("backup run %d (config %d) started: %s %s→%s via %s", runID, id, cfg.Direction, cfg.Host, cfg.LocalPath, cfg.Method)

	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.cancelFuncs[runID] = cancel
	h.mu.Unlock()

	go func() {
		defer func() {
			cancel() // always release context resources
			h.mu.Lock()
			delete(h.cancelFuncs, runID)
			delete(h.liveProgress, runID)
			h.mu.Unlock()
		}()

		h.setLiveProgress(runID, "Starting backup...", -1, -1)
		runCfg := backupConfigFromDB(cfg)
		if routedPath, routed, err := h.smoothfsBulkIngestPath(cfg); err != nil {
			log.Printf("backup run %d (config %d): smoothfs bulk route skipped: %v", runID, id, err)
		} else if routed {
			log.Printf("backup run %d (config %d): routing SmoothFS ingest %s -> %s", runID, id, cfg.LocalPath, routedPath)
			h.setLiveProgress(runID, "Writing directly to SmoothFS bulk tier...", -1, -1)
			runCfg.LocalPath = routedPath
		}
		summary, err := backup.Run(ctx, runCfg, func(msg string, done, total int) {
			h.setLiveProgress(runID, msg, done, total)
		})
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("backup run %d (config %d): cancelled", runID, id)
				_ = h.store.FailBackupRun(runID, "Cancelled")
			} else {
				log.Printf("backup run %d (config %d) failed: %v", runID, id, err)
				_ = h.store.FailBackupRun(runID, err.Error())
			}
			return
		}
		log.Printf("backup run %d (config %d) completed", runID, id)
		_ = h.store.CompleteBackupRun(runID, summary)
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"run_id":%d}`, runID)
}

func backupConfigFromDB(cfg *db.BackupConfig) backup.Config {
	return backup.Config{
		TargetType:  cfg.TargetType,
		Host:        cfg.Host,
		Share:       cfg.Share,
		SMBUser:     cfg.SMBUser,
		SMBPass:     cfg.SMBPass,
		SSHUser:     cfg.SSHUser,
		SSHPass:     cfg.SSHPass,
		LocalPath:   cfg.LocalPath,
		RemotePath:  cfg.RemotePath,
		Direction:   cfg.Direction,
		Method:      cfg.Method,
		Parallelism: cfg.Parallelism,
		UseSSH:      cfg.UseSSH,
		Compress:    cfg.Compress,
		DeleteMode:  cfg.DeleteMode,
	}
}

// listRuns handles GET /api/backup/runs.
// Query params: config_id (optional), active=true (optional).
// Without config_id: returns all active runs across all configs.
func (h *BackupHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	configIDStr := r.URL.Query().Get("config_id")
	activeOnly := r.URL.Query().Get("active") == "true"

	if configIDStr == "" {
		runs, err := h.store.ListActiveBackupRuns()
		if err != nil {
			serverError(w, err)
			return
		}
		if runs == nil {
			runs = []db.BackupRun{}
		}
		h.applyLiveProgress(runs)
		json.NewEncoder(w).Encode(runs)
		return
	}

	configID, err := strconv.ParseInt(configIDStr, 10, 64)
	if err != nil {
		jsonErrorCoded(w, "invalid config_id", http.StatusBadRequest, "backup.invalid_config_id")
		return
	}

	runs, err := h.store.ListBackupRunsByConfig(configID, activeOnly)
	if err != nil {
		serverError(w, err)
		return
	}
	if runs == nil {
		runs = []db.BackupRun{}
	}
	h.applyLiveProgress(runs)
	json.NewEncoder(w).Encode(runs)
}

// getRun handles GET /api/backup/runs/{id}.
func (h *BackupHandler) getRun(w http.ResponseWriter, r *http.Request, id int64) {
	run, err := h.store.GetBackupRun(id)
	if err != nil {
		if err == db.ErrNotFound {
			jsonErrorCoded(w, "backup run not found", http.StatusNotFound, "backup.run_not_found")
		} else {
			serverError(w, err)
		}
		return
	}
	h.applyLiveProgressOne(run)
	json.NewEncoder(w).Encode(run)
}

// cancelRun handles POST /api/backup/runs/{id}/cancel.
func (h *BackupHandler) cancelRun(w http.ResponseWriter, r *http.Request, id int64) {
	h.mu.Lock()
	cancel, ok := h.cancelFuncs[id]
	h.mu.Unlock()

	if !ok {
		jsonErrorCoded(w, "run not found or already finished", http.StatusNotFound, "backup.run_not_found_or_finished")
		return
	}
	cancel()
	fmt.Fprintf(w, `{"status":"cancelling"}`)
}

// PurgeBackupsUnderPath cancels any running backups and deletes any backup
// configs whose LocalPath falls under mountPath. Called when a tier pool is
// being destroyed so the associated backup schedule does not resurrect the
// pool's mount by racing rsync against a freshly recreated tier.
//
// Returns the number of configs deleted.
func (h *BackupHandler) PurgeBackupsUnderPath(mountPath string) (int, error) {
	if mountPath == "" {
		return 0, nil
	}
	cfgs, err := h.store.ListBackupConfigs()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, c := range cfgs {
		if c.LocalPath != mountPath && !strings.HasPrefix(c.LocalPath, mountPath+"/") {
			continue
		}
		runs, err := h.store.ListBackupRunsByConfig(c.ID, true)
		if err == nil {
			h.mu.Lock()
			for _, r := range runs {
				if cancel, ok := h.cancelFuncs[r.ID]; ok {
					cancel()
				}
			}
			h.mu.Unlock()
		}
		if err := h.store.DeleteBackupConfig(c.ID); err != nil {
			return deleted, fmt.Errorf("delete backup config %d: %w", c.ID, err)
		}
		deleted++
	}
	return deleted, nil
}

func validateBackupConfig(req createBackupConfigRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if req.Method != "cp" && req.Method != "rsync" {
		return fmt.Errorf("method must be 'cp' or 'rsync'")
	}
	// target_type is required unless this is rsync-over-SSH (which bypasses
	// any mount). Mount-based paths (cp and rsync-without-SSH) need it.
	needsMount := req.Method == "cp" || (req.Method == "rsync" && !req.UseSSH)
	if needsMount && req.TargetType != "nfs" && req.TargetType != "smb" {
		return fmt.Errorf("target_type must be 'nfs' or 'smb' when using a mount transport")
	}
	if strings.TrimSpace(req.Host) == "" {
		return fmt.Errorf("host is required")
	}
	if strings.TrimSpace(req.Share) == "" {
		return fmt.Errorf("share is required")
	}
	if strings.TrimSpace(req.LocalPath) == "" {
		return fmt.Errorf("local_path is required")
	}
	if req.Direction != "push" && req.Direction != "pull" {
		return fmt.Errorf("direction must be 'push' or 'pull'")
	}
	if needsMount && req.TargetType == "smb" && req.SMBUser == "" {
		return fmt.Errorf("smb_user is required for SMB targets")
	}
	return nil
}

func (h *BackupHandler) guardDeleteMode(cfg *db.BackupConfig) error {
	if cfg == nil || !cfg.DeleteMode || cfg.Method != "rsync" || cfg.Direction != "pull" {
		return nil
	}

	dst := filepath.Clean(cfg.LocalPath)
	pools, err := h.store.ListSmoothfsPools()
	if err != nil {
		return fmt.Errorf("delete guard: list smoothfs pools: %w", err)
	}
	pool := smoothfsPoolForPath(dst, pools)
	if pool == nil {
		return nil
	}
	spilled, err := smoothfsPoolHasAnySpill(pool.UUID)
	if err != nil {
		return fmt.Errorf("refusing rsync --delete for smoothfs destination %s: cannot read spill status for pool %s: %w",
			dst, pool.Name, err)
	}
	if spilled {
		return fmt.Errorf("refusing rsync --delete for spill-active smoothfs destination %s (pool %s)", dst, pool.Name)
	}
	return nil
}

func (h *BackupHandler) smoothfsBulkIngestPath(cfg *db.BackupConfig) (string, bool, error) {
	if cfg == nil || cfg.Method != "rsync" || cfg.Direction != "pull" || cfg.DeleteMode {
		return "", false, nil
	}

	dst := filepath.Clean(cfg.LocalPath)
	empty, err := pathAbsentOrEmpty(dst)
	if err != nil {
		return "", false, fmt.Errorf("inspect destination %s: %w", dst, err)
	}
	if !empty {
		return "", false, nil
	}

	pools, err := h.store.ListSmoothfsPools()
	if err != nil {
		return "", false, fmt.Errorf("list smoothfs pools: %w", err)
	}
	pool := smoothfsPoolForPath(dst, pools)
	if pool == nil || len(pool.Tiers) < 2 {
		return "", false, nil
	}

	fastPath := filepath.Clean(strings.TrimSpace(pool.Tiers[0]))
	bulkPath := filepath.Clean(strings.TrimSpace(pool.Tiers[len(pool.Tiers)-1]))
	if fastPath == "." || bulkPath == "." || fastPath == bulkPath {
		return "", false, nil
	}

	rel, err := filepath.Rel(filepath.Clean(pool.Mountpoint), dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false, nil
	}
	if rel == "." {
		rel = ""
	}
	target := filepath.Join(bulkPath, rel)
	log.Printf("backup: SmoothFS pool %s bulk ingest: writing rsync pull directly to backing %s instead of mount %s",
		pool.Name, target, dst)
	return target, true, nil
}

func pathAbsentOrEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	defer f.Close()

	_, err = f.ReadDir(1)
	if err != nil {
		if err == os.ErrNotExist {
			return true, nil
		}
		if err == io.EOF {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func smoothfsPoolForPath(path string, pools []db.SmoothfsPool) *db.SmoothfsPool {
	cleaned := filepath.Clean(path)
	var best *db.SmoothfsPool
	bestLen := -1

	for i := range pools {
		mnt := filepath.Clean(pools[i].Mountpoint)
		if cleaned != mnt && !strings.HasPrefix(cleaned, mnt+string(os.PathSeparator)) {
			continue
		}
		if len(mnt) > bestLen {
			best = &pools[i]
			bestLen = len(mnt)
		}
	}
	return best
}

func smoothfsPoolHasAnySpill(uuid string) (bool, error) {
	data, err := os.ReadFile(spillFlagPathForUUID(uuid))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) == "1", nil
}

// effectiveCompress returns cfg.Compress only when the run-time path actually
// uses it — i.e. rsync over direct SSH. In mount mode --compress compresses
// the in-process rsync stream, not the NFS/SMB wire, so it's pointless.
// Call this right before INSERT/UPDATE so the stored flag matches behavior.
func effectiveCompress(method string, useSSH, compress bool) bool {
	return compress && method == "rsync" && useSSH
}

func effectiveDeleteMode(method string, deleteMode bool) bool {
	return deleteMode && method == "rsync"
}
