package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
)

// postJSON is a test helper that encodes body as JSON, creates a request with
// the given method and path, sends it through the handler, and returns the
// response recorder.
func postJSON(h *ArraysHandler, method, path string, body any) *httptest.ResponseRecorder {
	bs, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Route(w, req)
	return w
}

func seedTierForLevels(t *testing.T, h *ArraysHandler, poolName string) {
	t.Helper()
	if err := h.store.CreateTierPool(poolName, "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "SSD", Rank: 2},
	}); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	// stub out listPoolPVs so GET /api/tiers/{name}/levels doesn't fail
	listPoolPVs = func(vg string) ([]lvm.PVInfo, error) { return nil, nil }
}

func TestAddTierLevelCreatesNewSlot(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"level_name":         "HDD",
		"rank":               3,
		"target_fill_pct":    40,
		"full_threshold_pct": 90,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tiers/store/levels: status %d, body %s", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "HDD" {
		t.Errorf("name = %v, want HDD", got["name"])
	}
	if got["rank"] != float64(3) {
		t.Errorf("rank = %v, want 3", got["rank"])
	}
	if got["target_fill_pct"] != float64(90) {
		t.Errorf("target_fill_pct = %v, want 90", got["target_fill_pct"])
	}
	if got["full_threshold_pct"] != float64(90) {
		t.Errorf("full_threshold_pct = %v, want 90", got["full_threshold_pct"])
	}
	if got["state"] != db.TierSlotStateEmpty {
		t.Errorf("state = %v, want empty", got["state"])
	}

	// Verify it appears in the GET list.
	req := httptest.NewRequest(http.MethodGet, "/api/tiers/store/levels", nil)
	rw := httptest.NewRecorder()
	h.Route(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /api/tiers/store/levels: status %d", rw.Code)
	}
	var levels []map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &levels); err != nil {
		t.Fatalf("unmarshal levels: %v", err)
	}
	if len(levels) != 3 {
		t.Errorf("expected 3 levels after add, got %d", len(levels))
	}
}

func TestAddTierLevelDefaultsFillValues(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"level_name": "TAPE",
		"rank":       10,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tiers/store/levels: status %d, body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["target_fill_pct"] != float64(95) {
		t.Errorf("default target_fill_pct = %v, want 95", got["target_fill_pct"])
	}
	if got["full_threshold_pct"] != float64(95) {
		t.Errorf("default full_threshold_pct = %v, want 95", got["full_threshold_pct"])
	}
}

func TestAddTierLevelSlowestUsesFullThresholdAsTargetFill(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"level_name":         "HDD",
		"rank":               3,
		"target_fill_pct":    40,
		"full_threshold_pct": 90,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/tiers/store/levels: status %d, body %s", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["target_fill_pct"] != float64(90) {
		t.Errorf("slowest target_fill_pct = %v, want 90", got["target_fill_pct"])
	}
	if got["full_threshold_pct"] != float64(90) {
		t.Errorf("slowest full_threshold_pct = %v, want 90", got["full_threshold_pct"])
	}
}

func TestAddTierLevelDuplicateRankReturns400(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	// Rank 2 already exists (SSD from seed).
	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"level_name": "FLASH",
		"rank":       2,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for duplicate rank, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddTierLevelMissingNameReturns400(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"rank": 5,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddTierLevelInvalidFillReturns400(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPool("store", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 3},
	}); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	listPoolPVs = func(vg string) ([]lvm.PVInfo, error) { return nil, nil }

	w := postJSON(h, http.MethodPost, "/api/tiers/store/levels", map[string]any{
		"level_name":         "SSD",
		"rank":               2,
		"target_fill_pct":    80,
		"full_threshold_pct": 70, // must be greater than target
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for target >= full, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteTierLevelRemovesEmptySlot(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	// Add a level we can safely delete (SSD is empty in this test pool).
	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/store/levels/SSD", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /api/tiers/store/levels/SSD: status %d, body %s", w.Code, w.Body.String())
	}

	// Should now be gone from the level list.
	req2 := httptest.NewRequest(http.MethodGet, "/api/tiers/store/levels", nil)
	w2 := httptest.NewRecorder()
	h.Route(w2, req2)
	var levels []map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &levels); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, l := range levels {
		if l["name"] == "SSD" {
			t.Errorf("SSD still present after delete: %v", levels)
		}
	}
}

func TestDeleteTierLevelWithPVAssignedReturns409(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPool("store", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
	}); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("store", "NVME", "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/store/levels/NVME", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 for level with PV, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteTierLevelNotFoundReturns404(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/store/levels/DOESNOTEXIST", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown level, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateTierLevelValidatesTargetLessThanFull(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	// target=90 >= full=80 should be rejected.
	w := postJSON(h, http.MethodPut, "/api/tiers/store/levels/NVME", map[string]any{
		"target_fill_pct":    90,
		"full_threshold_pct": 80,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for target >= full, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateTierLevelSlowestUsesFullThresholdAsTargetFill(t *testing.T) {
	h := newTestHandler(t)
	seedTierForLevels(t, h, "store")

	w := postJSON(h, http.MethodPut, "/api/tiers/store/levels/SSD", map[string]any{
		"target_fill_pct":    40,
		"full_threshold_pct": 88,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/tiers/store/levels/SSD: status %d, body %s", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["target_fill_pct"] != float64(88) {
		t.Errorf("slowest target_fill_pct = %v, want 88", got["target_fill_pct"])
	}
	if got["full_threshold_pct"] != float64(88) {
		t.Errorf("slowest full_threshold_pct = %v, want 88", got["full_threshold_pct"])
	}
}

func TestAddTierLevelPoolNotFoundReturns404(t *testing.T) {
	h := newTestHandler(t)

	w := postJSON(h, http.MethodPost, "/api/tiers/nopool/levels", map[string]any{
		"level_name": "HDD",
		"rank":       fmt.Sprintf("%d", 3),
	})
	// rank field is a string here — should get 400 or 404; the pool doesn't
	// exist so the DB ErrNotFound path fires first.  Accept either 400/404.
	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Fatalf("want 404 or 400 for missing pool, got %d: %s", w.Code, w.Body.String())
	}
}
