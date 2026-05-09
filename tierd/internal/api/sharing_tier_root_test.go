package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
)

func seedHealthyTierRoot(t *testing.T, h *SharingHandler, poolName, dataDir string) (root, dataPath string) {
	t.Helper()
	origMountRoot := db.TierMountRoot
	mountRoot := filepath.Join(t.TempDir(), "mnt")
	db.TierMountRoot = mountRoot
	t.Cleanup(func() { db.TierMountRoot = origMountRoot })

	if err := h.store.CreateTierInstance(poolName); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.TransitionTierInstanceState(poolName, db.TierPoolStateHealthy); err != nil {
		t.Fatalf("mark tier healthy: %v", err)
	}

	root = filepath.Join(mountRoot, poolName)
	for _, dir := range []string{".smoothfs", ".tierd-meta", dataDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return root, filepath.Join(root, dataDir)
}

func TestFilesystemPathsExposeTierChildrenNotTierRoot(t *testing.T) {
	h := newTestSharingHandler(t)
	root, dataPath := seedHealthyTierRoot(t, h, "media", "storage")

	req := httptest.NewRequest(http.MethodGet, "/api/filesystem/paths", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var paths []filesystemPath
	if err := json.NewDecoder(w.Body).Decode(&paths); err != nil {
		t.Fatalf("decode paths: %v", err)
	}
	for _, p := range paths {
		if p.Path == root {
			t.Fatalf("filesystem paths exposed internal tier root: %#v", paths)
		}
	}
	foundData := false
	for _, p := range paths {
		if p.Path == dataPath && p.Source == "tier" && p.Name == "media/storage" {
			foundData = true
		}
	}
	if !foundData {
		t.Fatalf("filesystem paths = %#v, want tier child %s", paths, dataPath)
	}
}

func TestSMBShareOnTierRootResolvesToDataChild(t *testing.T) {
	h := newTestSharingHandler(t)
	root, dataPath := seedHealthyTierRoot(t, h, "media", "storage")

	origWrite := writeSMBConfig
	t.Cleanup(func() { writeSMBConfig = origWrite })
	var generated []smb.Share
	writeSMBConfig = func(shares []smb.Share, hostname string, opts smb.Options) error {
		generated = append([]smb.Share(nil), shares...)
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/smb/shares", strings.NewReader(`{
		"name":"storage",
		"path":"`+root+`",
		"guest_ok":true
	}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created db.SmbShare
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created share: %v", err)
	}
	if created.Path != dataPath {
		t.Fatalf("created path = %q, want %q", created.Path, dataPath)
	}
	if len(generated) != 1 || generated[0].Path != dataPath {
		t.Fatalf("generated SMB shares = %#v, want path %q", generated, dataPath)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/smb/shares", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list shares status = %d, body=%s", w.Code, w.Body.String())
	}
	var listed []db.SmbShare
	if err := json.NewDecoder(w.Body).Decode(&listed); err != nil {
		t.Fatalf("decode shares: %v", err)
	}
	if len(listed) != 1 || listed[0].Path != dataPath {
		t.Fatalf("listed SMB shares = %#v, want effective path %q", listed, dataPath)
	}
}

func TestExistingSMBShareOnTierRootRegeneratesToDataChild(t *testing.T) {
	h := newTestSharingHandler(t)
	root, dataPath := seedHealthyTierRoot(t, h, "media", "storage")
	if _, err := h.store.CreateSmbShare(db.SmbShare{Name: "storage", Path: root, GuestOK: true}); err != nil {
		t.Fatalf("create legacy share: %v", err)
	}

	origWrite := writeSMBConfig
	t.Cleanup(func() { writeSMBConfig = origWrite })
	var generated []smb.Share
	writeSMBConfig = func(shares []smb.Share, hostname string, opts smb.Options) error {
		generated = append([]smb.Share(nil), shares...)
		return nil
	}

	if err := h.regenerateSmbConf(); err != nil {
		t.Fatalf("regenerate smb config: %v", err)
	}
	if len(generated) != 1 || generated[0].Path != dataPath {
		t.Fatalf("generated SMB shares = %#v, want path %q", generated, dataPath)
	}
}

func TestNFSExportOnTierRootResolvesToDataChild(t *testing.T) {
	h := newTestSharingHandler(t)
	root, dataPath := seedHealthyTierRoot(t, h, "media", "storage")

	origEnable := enableNFSServiceForExports
	origApply := applyFirewallForExports
	origEnabled := enabledProtocolsForExports
	origWrite := writeNFSExports
	t.Cleanup(func() {
		enableNFSServiceForExports = origEnable
		applyFirewallForExports = origApply
		enabledProtocolsForExports = origEnabled
		writeNFSExports = origWrite
	})
	enableNFSServiceForExports = func(bool) error { return nil }
	applyFirewallForExports = func(map[string]bool) error { return nil }
	enabledProtocolsForExports = func() map[string]bool { return map[string]bool{} }
	var generated []nfs.Export
	writeNFSExports = func(exports []nfs.Export) error {
		generated = append([]nfs.Export(nil), exports...)
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/nfs/exports", strings.NewReader(`{
		"path":"`+root+`",
		"networks":["127.0.0.1"],
		"sync":false,
		"root_squash":true,
		"read_only":false
	}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created db.NfsExport
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created export: %v", err)
	}
	if created.Path != dataPath {
		t.Fatalf("created export path = %q, want %q", created.Path, dataPath)
	}
	if len(generated) != 1 || generated[0].Path != dataPath {
		t.Fatalf("generated NFS exports = %#v, want path %q", generated, dataPath)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/nfs/exports", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list exports status = %d, body=%s", w.Code, w.Body.String())
	}
	var listed []db.NfsExport
	if err := json.NewDecoder(w.Body).Decode(&listed); err != nil {
		t.Fatalf("decode exports: %v", err)
	}
	if len(listed) != 1 || listed[0].Path != dataPath {
		t.Fatalf("listed NFS exports = %#v, want effective path %q", listed, dataPath)
	}
}

func TestExistingNFSExportOnTierRootRegeneratesToDataChild(t *testing.T) {
	h := newTestSharingHandler(t)
	root, dataPath := seedHealthyTierRoot(t, h, "media", "storage")
	if _, err := h.store.CreateNfsExport(db.NfsExport{
		Path: root, Networks: "127.0.0.1", RootSquash: true,
	}); err != nil {
		t.Fatalf("create legacy export: %v", err)
	}

	origWrite := writeNFSExports
	t.Cleanup(func() { writeNFSExports = origWrite })
	var generated []nfs.Export
	writeNFSExports = func(exports []nfs.Export) error {
		generated = append([]nfs.Export(nil), exports...)
		return nil
	}

	if err := h.regenerateExports(); err != nil {
		t.Fatalf("regenerate nfs exports: %v", err)
	}
	if len(generated) != 1 || generated[0].Path != dataPath {
		t.Fatalf("generated NFS exports = %#v, want path %q", generated, dataPath)
	}
}

func TestISCSIFileBackingDirectlyUnderTierRootRejected(t *testing.T) {
	h := newTestSharingHandler(t)
	root, _ := seedHealthyTierRoot(t, h, "media", "storage")
	backing := filepath.Join(root, "lun.img")
	if err := os.WriteFile(backing, []byte("lun"), 0o644); err != nil {
		t.Fatalf("write backing file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets", strings.NewReader(`{
		"iqn":"iqn.2026-01.com.smoothnas:rootlun",
		"backing_file":"`+backing+`"
	}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "internal tier root") {
		t.Fatalf("response %q does not explain tier root policy", w.Body.String())
	}
	targets, err := h.store.ListIscsiTargets()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("stored targets = %#v, want none", targets)
	}
}
