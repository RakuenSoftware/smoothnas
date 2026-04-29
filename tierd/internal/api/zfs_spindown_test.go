package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

func newTestZFSHandler(t *testing.T) *ZFSHandler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "zfs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	origDetail := detailZFSPool
	origScrub := scrubZFSPool
	origAtime := setZFSAtimeOff
	origNow := spindownNow
	detailZFSPool = func(name string) (*zfs.Pool, error) {
		return &zfs.Pool{Name: name, VdevLayout: "NAME STATE READ WRITE CKSUM\n  tank ONLINE 0 0 0\n"}, nil
	}
	scrubZFSPool = func(string) error { return nil }
	setZFSAtimeOff = func(string) error { return nil }
	spindownNow = func() time.Time { return time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		detailZFSPool = origDetail
		scrubZFSPool = origScrub
		setZFSAtimeOff = origAtime
		spindownNow = origNow
	})
	return NewZFSHandler(store)
}

func postZFSJSON(h *ZFSHandler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Route(rec, req)
	return rec
}

func TestRawZFSSpindownRequiresSpecialVdev(t *testing.T) {
	h := newTestZFSHandler(t)
	w := postZFSJSON(h, http.MethodPut, "/api/pools/tank/spindown", map[string]any{"enabled": true})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("enable raw ZFS spindown status %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestRawZFSSpindownEnableWithSpecialVdev(t *testing.T) {
	h := newTestZFSHandler(t)
	detailZFSPool = func(name string) (*zfs.Pool, error) {
		return &zfs.Pool{Name: name, VdevLayout: "NAME STATE READ WRITE CKSUM\n  tank ONLINE 0 0 0\nspecial\n  mirror-1 ONLINE 0 0 0\n"}, nil
	}
	atimeSet := false
	setZFSAtimeOff = func(string) error {
		atimeSet = true
		return nil
	}
	w := postZFSJSON(h, http.MethodPut, "/api/pools/tank/spindown", map[string]any{
		"enabled": true,
		"active_windows": []map[string]any{{
			"days":  []string{"daily"},
			"start": "01:00",
			"end":   "06:00",
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("enable raw ZFS spindown status %d; body=%s", w.Code, w.Body.String())
	}
	if !atimeSet {
		t.Fatal("expected atime=off to be applied")
	}
	var got rawZFSSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || !got.Eligible || got.ActiveNow || got.NextActiveAt == "" {
		t.Fatalf("unexpected policy: %+v", got)
	}
}
