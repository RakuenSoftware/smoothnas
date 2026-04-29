package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/nfs"
	"github.com/google/uuid"
)

func TestBuildNFSExportsAddsSmoothfsFsid(t *testing.T) {
	poolID := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	exports := buildNFSExports([]db.NfsExport{
		{
			Path:       "/mnt/media/storage",
			Networks:   "127.0.0.1",
			Sync:       false,
			RootSquash: false,
			ReadOnly:   false,
		},
	}, []db.SmoothfsPool{
		{
			UUID:       poolID.String(),
			Name:       "media",
			Mountpoint: "/mnt/media",
		},
	})

	if len(exports) != 1 {
		t.Fatalf("exports len = %d, want 1", len(exports))
	}
	want := nfs.SmoothfsExportFsidOption(poolID, "/mnt/media", "/mnt/media/storage")
	if exports[0].Fsid != want {
		t.Fatalf("Fsid = %q, want %q", exports[0].Fsid, want)
	}
}

func newTestSharingHandler(t *testing.T) *SharingHandler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "sharing.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewSharingHandler(store)
}

func TestCreateNFSExportRejectsMissingPathBeforeSideEffects(t *testing.T) {
	h := newTestSharingHandler(t)

	origEnable := enableNFSServiceForExports
	origApply := applyFirewallForExports
	origEnabled := enabledProtocolsForExports
	t.Cleanup(func() {
		enableNFSServiceForExports = origEnable
		applyFirewallForExports = origApply
		enabledProtocolsForExports = origEnabled
	})

	enableCalled := false
	applyCalled := false
	enableNFSServiceForExports = func(v3 bool) error {
		enableCalled = true
		return nil
	}
	enabledProtocolsForExports = func() map[string]bool {
		return map[string]bool{}
	}
	applyFirewallForExports = func(enabled map[string]bool) error {
		applyCalled = true
		return nil
	}

	path := filepath.Join(t.TempDir(), "missing")
	req := httptest.NewRequest(http.MethodPost, "/api/nfs/exports", stringBody(`{
		"path":"`+path+`",
		"networks":["127.0.0.1"],
		"sync":false,
		"root_squash":false,
		"read_only":false
	}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if enableCalled || applyCalled {
		t.Fatalf("expected no service/firewall side effects, enable=%t apply=%t", enableCalled, applyCalled)
	}
	exports, err := h.store.ListNfsExports()
	if err != nil {
		t.Fatalf("list exports: %v", err)
	}
	if len(exports) != 0 {
		t.Fatalf("expected no stored exports, got %#v", exports)
	}
}

func TestPatchNFSExportSyncRegeneratesExports(t *testing.T) {
	h := newTestSharingHandler(t)

	origWrite := writeNFSExports
	t.Cleanup(func() { writeNFSExports = origWrite })

	exp, err := h.store.CreateNfsExport(db.NfsExport{
		Path: "/mnt/data", Networks: "127.0.0.1", Sync: false, RootSquash: true,
	})
	if err != nil {
		t.Fatalf("create export: %v", err)
	}

	var generated []nfs.Export
	writeNFSExports = func(exports []nfs.Export) error {
		generated = append([]nfs.Export(nil), exports...)
		return nil
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/nfs/exports/1", stringBody(`{"sync":true}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got db.NfsExport
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != exp.ID || !got.Sync {
		t.Fatalf("patched export = %#v, want id %d sync true", got, exp.ID)
	}
	if len(generated) != 1 || !generated[0].Sync {
		t.Fatalf("generated exports = %#v, want one sync export", generated)
	}

	stored, err := h.store.ListNfsExports()
	if err != nil {
		t.Fatalf("list exports: %v", err)
	}
	if len(stored) != 1 || !stored[0].Sync {
		t.Fatalf("stored exports = %#v, want sync true", stored)
	}
}

func TestEnsureNFSExportServingEnablesFirewall(t *testing.T) {
	origEnable := enableNFSServiceForExports
	origApply := applyFirewallForExports
	origEnabled := enabledProtocolsForExports
	t.Cleanup(func() {
		enableNFSServiceForExports = origEnable
		applyFirewallForExports = origApply
		enabledProtocolsForExports = origEnabled
	})

	var enabledNFS bool
	var applied map[string]bool
	enableNFSServiceForExports = func(v3 bool) error {
		enabledNFS = v3
		return nil
	}
	enabledProtocolsForExports = func() map[string]bool {
		return map[string]bool{"smb": true}
	}
	applyFirewallForExports = func(enabled map[string]bool) error {
		applied = enabled
		return nil
	}

	if err := ensureNFSExportServing(); err != nil {
		t.Fatalf("ensureNFSExportServing: %v", err)
	}
	if !enabledNFS {
		t.Fatal("NFS service was not enabled")
	}
	want := map[string]bool{"smb": true, "nfs": true}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("firewall protocols = %#v, want %#v", applied, want)
	}
}
