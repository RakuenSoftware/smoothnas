package api

import (
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
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

// SharingHandler handles /api/protocols*, /api/smb/*, /api/nfs/*, /api/iscsi/* endpoints.
type SharingHandler struct {
	store          *db.Store
	protocolsCache *cache.Entry[[]protocolStatus]
}

func NewSharingHandler(store *db.Store) *SharingHandler {
	return &SharingHandler{
		store:          store,
		protocolsCache: cache.New[[]protocolStatus](60 * time.Second),
	}
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
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	rest := strings.TrimPrefix(path, "/api/protocols/")
	if r.Method == http.MethodPut {
		h.toggleProtocol(w, r, rest)
	} else {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var err error
	if req.Enabled {
		switch proto {
		case "smb":
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

	if path == "/api/smb/shares" || path == "/api/smb/shares/" {
		switch r.Method {
		case http.MethodGet:
			h.listSMBShares(w, r)
		case http.MethodPost:
			h.createSMBShare(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	if strings.HasPrefix(path, "/api/smb/shares/") {
		name := strings.TrimPrefix(path, "/api/smb/shares/")
		switch r.Method {
		case http.MethodDelete:
			h.deleteSMBShare(w, r, name)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
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
			http.Error(w, `{"error":"share name already exists"}`, http.StatusConflict)
		} else {
			serverError(w, err)
		}
		return
	}

	// Regenerate smb.conf from all shares.
	h.regenerateSmbConf()

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(share)
}

func (h *SharingHandler) deleteSMBShare(w http.ResponseWriter, r *http.Request, name string) {
	if err := h.store.DeleteSmbShare(name); err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"share not found"}`, http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}

	h.regenerateSmbConf()
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

func (h *SharingHandler) regenerateSmbConf() {
	shares, err := h.store.ListSmbShares()
	if err != nil {
		return
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

	smb.WriteConfig(smbShares, "smoothnas")
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	if strings.HasPrefix(path, "/api/nfs/exports/") {
		idStr := strings.TrimPrefix(path, "/api/nfs/exports/")
		if r.Method == http.MethodDelete {
			h.deleteNFSExport(w, r, idStr)
		} else {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
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

	h.regenerateExports()

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(exp)
}

func (h *SharingHandler) deleteNFSExport(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid export id"}`, http.StatusBadRequest)
		return
	}

	if err := h.store.DeleteNfsExport(id); err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"export not found"}`, http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}

	h.regenerateExports()
	fmt.Fprintf(w, `{"status":"deleted"}`)
}

func (h *SharingHandler) regenerateExports() {
	dbExports, err := h.store.ListNfsExports()
	if err != nil {
		return
	}

	var exports []nfs.Export
	for _, e := range dbExports {
		exports = append(exports, nfs.Export{
			Path:       e.Path,
			Networks:   strings.Split(e.Networks, ","),
			Sync:       e.Sync,
			RootSquash: e.RootSquash,
			ReadOnly:   e.ReadOnly,
		})
	}

	nfs.WriteExports(exports)
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
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		case "acls":
			switch r.Method {
			case http.MethodGet:
				h.listISCSIACLs(w, r, targetIQN)
			case http.MethodPost:
				h.addISCSIACL(w, r, targetIQN)
			default:
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			}
		default:
			if strings.HasPrefix(subpath, "acls/") {
				aclIQN := strings.TrimPrefix(subpath, "acls/")
				if r.Method == http.MethodDelete {
					h.removeISCSIACL(w, r, targetIQN, aclIQN)
				} else {
					http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				}
			} else {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			}
		}
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
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
	json.NewEncoder(w).Encode(targets)
}

type createTargetRequest struct {
	IQN         string `json:"iqn"`
	BlockDevice string `json:"block_device"`
	CHAPUser    string `json:"chap_user"`
	CHAPPass    string `json:"chap_pass"`
}

func (h *SharingHandler) createISCSITarget(w http.ResponseWriter, r *http.Request) {
	var req createTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.IQN == "" || req.BlockDevice == "" {
		http.Error(w, `{"error":"iqn and block_device required"}`, http.StatusBadRequest)
		return
	}

	if err := iscsi.ValidateIQN(req.IQN); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := iscsi.ValidateBlockDevice(req.BlockDevice); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to SQLite.
	target, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         req.IQN,
		BlockDevice: req.BlockDevice,
		CHAPUser:    req.CHAPUser,
		CHAPPass:    req.CHAPPass,
	})
	if err != nil {
		if err == db.ErrDuplicate {
			http.Error(w, `{"error":"target IQN already exists"}`, http.StatusConflict)
		} else {
			serverError(w, err)
		}
		return
	}

	// Create via targetcli.
	if err := iscsi.CreateTarget(req.IQN, req.BlockDevice); err != nil {
		// Rollback SQLite on targetcli failure.
		h.store.DeleteIscsiTarget(req.IQN)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.CHAPUser != "" && req.CHAPPass != "" {
		iscsi.SetCHAP(req.IQN, req.CHAPUser, req.CHAPPass)
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(target)
}

func (h *SharingHandler) deleteISCSITarget(w http.ResponseWriter, r *http.Request, iqn string) {
	// Remove from targetcli.
	iscsi.DestroyTarget(iqn)

	// Remove from SQLite.
	if err := h.store.DeleteIscsiTarget(iqn); err != nil {
		if err == db.ErrNotFound {
			http.Error(w, `{"error":"target not found"}`, http.StatusNotFound)
		} else {
			serverError(w, err)
		}
		return
	}

	fmt.Fprintf(w, `{"status":"destroyed"}`)
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
		http.Error(w, `{"error":"initiator_iqn required"}`, http.StatusBadRequest)
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
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
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
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	target := filepath.Clean(r.URL.Query().Get("path"))
	if target == "." || target == "" {
		target = "/mnt"
	}
	if target != "/mnt" && !strings.HasPrefix(target, "/mnt/") {
		jsonError(w, "path must be within /mnt", http.StatusBadRequest)
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
