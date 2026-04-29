package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/smb"
)

func TestGetSMBConfigDefaultsToPerformanceMode(t *testing.T) {
	h := newTestSharingHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/smb/config", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got smbConfigResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.CompatibilityMode || !got.PerformanceMode {
		t.Fatalf("config = %#v, want performance mode by default", got)
	}
}

func TestUpdateSMBConfigPersistsAndRegenerates(t *testing.T) {
	h := newTestSharingHandler(t)
	if _, err := h.store.CreateSmbShare(db.SmbShare{Name: "media", Path: "/mnt/media"}); err != nil {
		t.Fatalf("create smb share: %v", err)
	}

	origWrite := writeSMBConfig
	t.Cleanup(func() { writeSMBConfig = origWrite })

	var generatedShares []smb.Share
	var generatedOptions smb.Options
	writeSMBConfig = func(shares []smb.Share, hostname string, opts smb.Options) error {
		generatedShares = append([]smb.Share(nil), shares...)
		generatedOptions = opts
		return nil
	}

	req := httptest.NewRequest(http.MethodPut, "/api/smb/config", strings.NewReader(`{"compatibility_mode":true}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !generatedOptions.CompatibilityMode {
		t.Fatalf("generated options = %#v, want compatibility mode", generatedOptions)
	}
	if len(generatedShares) != 1 || generatedShares[0].Name != "media" {
		t.Fatalf("generated shares = %#v, want media share", generatedShares)
	}

	stored, err := h.store.GetBoolConfig(smbCompatibilityModeConfigKey, false)
	if err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	if !stored {
		t.Fatal("expected compatibility mode to be persisted")
	}

	var got smbConfigResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.CompatibilityMode || got.PerformanceMode {
		t.Fatalf("response = %#v, want compatibility mode", got)
	}
}
