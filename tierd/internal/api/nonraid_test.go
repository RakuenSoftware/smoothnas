package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/nonraid"
)

func openNonRaidTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func installNonRaidDiskStub(t *testing.T) {
	t.Helper()
	orig := listNonRaidDisks
	listNonRaidDisks = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Name: "sdb", Path: "/dev/sdb", Size: 8 << 40, Serial: "data1", Assignment: "unassigned"},
			{Name: "sdc", Path: "/dev/sdc", Size: 10 << 40, Serial: "data2", Assignment: "unassigned"},
			{Name: "sdd", Path: "/dev/sdd", Size: 10 << 40, Serial: "parity1", Assignment: "unassigned"},
			{Name: "sde", Path: "/dev/sde", Size: 12 << 40, Serial: "parity2", Assignment: "unassigned"},
			{Name: "sdf", Path: "/dev/sdf", Size: 12 << 40, Serial: "busy", Assignment: "zfs-pool"},
		}, nil
	}
	t.Cleanup(func() { listNonRaidDisks = orig })
}

type stubNonRaidActivator struct{}

func (stubNonRaidActivator) Activate(_ *http.Request, store *db.Store, row *db.NonRaidArrayRow, _ bool) error {
	for _, dev := range row.Devices {
		virtual := ""
		if dev.Role == nonraid.RoleData {
			virtual = "/dev/nbd" + strconv.Itoa(dev.Slot)
		}
		if err := store.SetNonRaidDeviceRuntime(row.ID, dev.Role, dev.Slot, virtual, nonraid.StateActive); err != nil {
			return err
		}
	}
	return store.SetNonRaidArrayState(row.Name, nonraid.StateActive, "")
}

func (stubNonRaidActivator) Deactivate(_ *http.Request, store *db.Store, row *db.NonRaidArrayRow) error {
	return store.SetNonRaidArrayState(row.Name, nonraid.StateConfigured, "")
}

func TestNonRaidCreateArray(t *testing.T) {
	store := openNonRaidTestStore(t)
	installNonRaidDiskStub(t)
	h := NewNonRaidHandler(store)
	h.activator = stubNonRaidActivator{}

	body, _ := json.Marshal(map[string]any{
		"name":         "media",
		"data_disks":   []string{"/dev/sdb", "/dev/sdc"},
		"parity_disks": []string{"/dev/sdd", "/dev/sde"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/nonraid/arrays", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp nonRaidArrayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "media" || resp.ParityCount != 2 || resp.MinParityBytes != 10<<40 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if !resp.DataPlaneReady {
		t.Fatalf("data plane should be marked ready: %+v", resp)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/nonraid/arrays/media", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestNonRaidRejectsBusyAndOversizedDisks(t *testing.T) {
	store := openNonRaidTestStore(t)
	installNonRaidDiskStub(t)
	h := NewNonRaidHandler(store)

	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "busy disk",
			body: map[string]any{
				"name":         "media",
				"data_disks":   []string{"/dev/sdf"},
				"parity_disks": []string{"/dev/sdd"},
			},
		},
		{
			name: "data larger than parity",
			body: map[string]any{
				"name":         "media",
				"data_disks":   []string{"/dev/sde"},
				"parity_disks": []string{"/dev/sdd"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/nonraid/arrays/validate", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.Route(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
			}
		})
	}
}
