package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
}

func NewBackupHandler(store *db.Store) *BackupHandler {
	return &BackupHandler{
		store:       store,
		cancelFuncs: make(map[int64]context.CancelFunc),
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
			jsonError(w, "invalid config id", http.StatusBadRequest)
			return
		}

		switch subpath {
		case "":
			if r.Method == http.MethodDelete {
				h.deleteConfig(w, r, id)
			} else {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		case "run":
			if r.Method == http.MethodPost {
				h.runBackup(w, r, id)
			} else {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}

	case path == "/api/backup/runs" || path == "/api/backup/runs/":
		if r.Method == http.MethodGet {
			h.listRuns(w, r)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}

	case strings.HasPrefix(path, "/api/backup/runs/"):
		rest := strings.TrimPrefix(path, "/api/backup/runs/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			jsonError(w, "invalid run id", http.StatusBadRequest)
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
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		case "cancel":
			if r.Method == http.MethodPost {
				h.cancelRun(w, r, id)
			} else {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}

	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
	LocalPath   string `json:"local_path"`
	RemotePath  string `json:"remote_path"`
	Direction   string `json:"direction"`
	Method      string `json:"method"`
	Parallelism int    `json:"parallelism"`
}

func (h *BackupHandler) createConfig(w http.ResponseWriter, r *http.Request) {
	var req createBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
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
	cfg, err := h.store.CreateBackupConfig(db.BackupConfig{
		Name:        req.Name,
		TargetType:  req.TargetType,
		Host:        req.Host,
		Share:       req.Share,
		SMBUser:     req.SMBUser,
		SMBPass:     req.SMBPass,
		LocalPath:   req.LocalPath,
		RemotePath:  req.RemotePath,
		Direction:   req.Direction,
		Method:      req.Method,
		Parallelism: req.Parallelism,
	})
	if err != nil {
		if err == db.ErrDuplicate {
			jsonError(w, "a backup config with that name already exists", http.StatusConflict)
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
			jsonError(w, "backup config not found", http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

func (h *BackupHandler) runBackup(w http.ResponseWriter, r *http.Request, id int64) {
	cfg, err := h.store.GetBackupConfig(id)
	if err != nil {
		if err == db.ErrNotFound {
			jsonError(w, "backup config not found", http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}

	runID, err := h.store.CreateBackupRun(id)
	if err != nil {
		serverError(w, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.cancelFuncs[runID] = cancel
	h.mu.Unlock()

	go func() {
		defer func() {
			cancel() // always release context resources
			h.mu.Lock()
			delete(h.cancelFuncs, runID)
			h.mu.Unlock()
		}()

		_ = h.store.UpdateBackupRunProgress(runID, "Starting backup...", -1, -1)
		summary, err := backup.Run(ctx, backup.Config{
			TargetType:  cfg.TargetType,
			Host:        cfg.Host,
			Share:       cfg.Share,
			SMBUser:     cfg.SMBUser,
			SMBPass:     cfg.SMBPass,
			LocalPath:   cfg.LocalPath,
			RemotePath:  cfg.RemotePath,
			Direction:   cfg.Direction,
			Method:      cfg.Method,
			Parallelism: cfg.Parallelism,
		}, func(msg string, done, total int) {
			_ = h.store.UpdateBackupRunProgress(runID, msg, done, total)
		})
		if err != nil {
			if ctx.Err() != nil {
				_ = h.store.FailBackupRun(runID, "Cancelled")
			} else {
				_ = h.store.FailBackupRun(runID, err.Error())
			}
			return
		}
		_ = h.store.CompleteBackupRun(runID, summary)
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"run_id":%d}`, runID)
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
		json.NewEncoder(w).Encode(runs)
		return
	}

	configID, err := strconv.ParseInt(configIDStr, 10, 64)
	if err != nil {
		jsonError(w, "invalid config_id", http.StatusBadRequest)
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
	json.NewEncoder(w).Encode(runs)
}

// getRun handles GET /api/backup/runs/{id}.
func (h *BackupHandler) getRun(w http.ResponseWriter, r *http.Request, id int64) {
	run, err := h.store.GetBackupRun(id)
	if err != nil {
		if err == db.ErrNotFound {
			jsonError(w, "backup run not found", http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}
	json.NewEncoder(w).Encode(run)
}

// cancelRun handles POST /api/backup/runs/{id}/cancel.
func (h *BackupHandler) cancelRun(w http.ResponseWriter, r *http.Request, id int64) {
	h.mu.Lock()
	cancel, ok := h.cancelFuncs[id]
	h.mu.Unlock()

	if !ok {
		jsonError(w, "run not found or already finished", http.StatusNotFound)
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
	if req.TargetType != "nfs" && req.TargetType != "smb" {
		return fmt.Errorf("target_type must be 'nfs' or 'smb'")
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
	if req.Method != "cp" && req.Method != "rsync" {
		return fmt.Errorf("method must be 'cp' or 'rsync'")
	}
	if req.TargetType == "smb" && req.SMBUser == "" {
		return fmt.Errorf("smb_user is required for SMB targets")
	}
	return nil
}
