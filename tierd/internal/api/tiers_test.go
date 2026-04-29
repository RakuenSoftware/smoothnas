package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/lvm"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/spindown"
)

func newTestHandler(t *testing.T) *ArraysHandler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "tiers.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := NewArraysHandler(store)
	h.asyncDone = make(chan struct{}, 4)
	// Tests run without root; replace LVM calls with no-ops.
	origCreatePoolVG := createPoolVG
	origRemovePoolVG := removePoolVG
	origRemovePoolVGPlaceholder := removePoolVGPlaceholder
	origVGExists := vgExists
	origIsMountPathBusy := isMountPathBusy
	origListMDADMArrays := listMDADMArrays
	origRemovePVLabel := removePVLabel
	origListPoolPVs := listPoolPVs
	origPoolUsageBytes := poolUsageBytes
	origTierDataLVExists := tierDataLVExists
	origListTierSegments := listTierSegments
	origTierMapNow := tierMapNow
	origUnmountTierPath := unmountTierPath
	origLazyUnmountPath := lazyUnmountPath
	origRemoveTierFSTab := removeTierFSTab
	origRemoveTierLV := removeTierLV
	origStatfsPath := statfsPath
	origRemountNoatime := remountNoatime
	origReadMountInfo := readMountInfo
	origScrubMDADMArray := scrubMDADMArray
	origSpindownNow := spindownNow
	origListDisksForSpindown := listDisksForSpindown
	origQueryPowerStateForSpindown := queryPowerStateForSpindown
	origZFSMemberDevicesForSpindown := zfsMemberDevicesForSpindown
	createPoolVG = func(string) error { return nil }
	removePoolVG = func(string) error { return nil }
	removePoolVGPlaceholder = func(string) error { return nil }
	vgExists = func(string) (bool, error) { return true, nil }
	isMountPathBusy = func(string) bool { return false }
	listMDADMArrays = func() ([]mdadm.Array, error) { return nil, nil }
	removePVLabel = func(string) error { return nil }
	listPoolPVs = func(string) ([]lvm.PVInfo, error) { return nil, nil }
	poolUsageBytes = func(string) uint64 { return 0 }
	tierDataLVExists = func(string, string) (bool, error) { return false, nil }
	listTierSegments = func(string, string) ([]lvm.Segment, error) { return nil, nil }
	tierMapNow = func() time.Time { return time.Date(2026, 4, 9, 2, 0, 0, 0, time.UTC) }
	unmountTierPath = func(string) error { return nil }
	lazyUnmountPath = func(string) error { return nil }
	removeTierFSTab = func(string, string, string) error { return nil }
	removeTierLV = func(string, string) error { return nil }
	statfsPath = origStatfsPath
	remountNoatime = func(string) error { return nil }
	readMountInfo = origReadMountInfo
	scrubMDADMArray = func(string) error { return nil }
	spindownNow = func() time.Time { return time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC) }
	listDisksForSpindown = func() ([]disk.Disk, error) { return nil, nil }
	queryPowerStateForSpindown = func(string) (string, error) { return "active", nil }
	zfsMemberDevicesForSpindown = func(string) []string { return nil }
	t.Cleanup(func() {
		createPoolVG = origCreatePoolVG
		removePoolVG = origRemovePoolVG
		removePoolVGPlaceholder = origRemovePoolVGPlaceholder
		vgExists = origVGExists
		isMountPathBusy = origIsMountPathBusy
		listMDADMArrays = origListMDADMArrays
		removePVLabel = origRemovePVLabel
		listPoolPVs = origListPoolPVs
		poolUsageBytes = origPoolUsageBytes
		tierDataLVExists = origTierDataLVExists
		listTierSegments = origListTierSegments
		tierMapNow = origTierMapNow
		unmountTierPath = origUnmountTierPath
		lazyUnmountPath = origLazyUnmountPath
		removeTierFSTab = origRemoveTierFSTab
		removeTierLV = origRemoveTierLV
		statfsPath = origStatfsPath
		remountNoatime = origRemountNoatime
		readMountInfo = origReadMountInfo
		scrubMDADMArray = origScrubMDADMArray
		spindownNow = origSpindownNow
		listDisksForSpindown = origListDisksForSpindown
		queryPowerStateForSpindown = origQueryPowerStateForSpindown
		zfsMemberDevicesForSpindown = origZFSMemberDevicesForSpindown
	})
	return h
}

func TestListTiersEndpoint(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tiers", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tiers: status %d, body %s", w.Code, w.Body.String())
	}

	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 tiers on fresh DB, got %d: %s", len(got), w.Body.String())
	}
}

func TestPoolSpindownRequiresMetadataOnFastest(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPool("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AssignBackingToTier("media", "NVME", "zfs", "fast"); err != nil {
		t.Fatalf("assign fast: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Eligible {
		t.Fatalf("expected pool without meta_on_fastest to be ineligible: %+v", got)
	}
	if len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "metadata") {
		t.Fatalf("expected metadata reason, got %+v", got.Reasons)
	}
}

func TestPoolSpindownEnableRemountsNoatimeAndPersists(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AssignBackingToTier("media", "NVME", "zfs", "fast"); err != nil {
		t.Fatalf("assign fast: %v", err)
	}
	if err := h.store.AssignBackingToTier("media", "HDD", "zfs", "slow"); err != nil {
		t.Fatalf("assign slow: %v", err)
	}
	readMountInfo = func() ([]byte, error) {
		return []byte(`1 0 8:1 / /mnt/media rw,relatime - smoothfs none rw
2 0 8:2 / /mnt/.tierd-backing/media/NVME rw,relatime - xfs /dev/test rw
3 0 8:3 / /mnt/.tierd-backing/media/HDD rw,relatime - xfs /dev/test rw
`), nil
	}
	var remounted []string
	remountNoatime = func(path string) error {
		remounted = append(remounted, path)
		return nil
	}

	w := postJSON(h, http.MethodPut, "/api/tiers/media/spindown", map[string]any{"enabled": true})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT spindown: status %d body %s", w.Code, w.Body.String())
	}
	if len(remounted) != 3 {
		t.Fatalf("expected three noatime remounts, got %#v", remounted)
	}
	enabled, err := h.store.GetBoolConfig(poolSpindownConfigKey("media"), false)
	if err != nil || !enabled {
		t.Fatalf("stored spindown enabled = %v err=%v", enabled, err)
	}
}

func TestPoolSpindownRequiresConfirmedSSDTiersAtTargetFill(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "NVME", "md0"); err != nil {
		t.Fatalf("assign NVME tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md1"); err != nil {
		t.Fatalf("assign HDD tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{
			{Path: "/dev/md0", MemberDisks: []string{"/dev/nvme0n1"}},
			{Path: "/dev/md1", MemberDisks: []string{"/dev/sda"}},
		}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Path: "/dev/nvme0n1", Rotational: false},
			{Path: "/dev/sda", Rotational: true},
		}, nil
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if !strings.HasSuffix(path, "/NVME") {
			return syscall.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 100
		st.Bfree = 80
		st.Bavail = 80
		return nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Eligible {
		t.Fatalf("expected SSD below target fill to block spindown: %+v", got)
	}
	if !got.SSDTargetFill.Required || got.SSDTargetFill.Satisfied {
		t.Fatalf("unexpected SSD warm-fill status: %+v", got.SSDTargetFill)
	}
	if len(got.SSDTargetFill.Tiers) != 1 || got.SSDTargetFill.Tiers[0].Name != "NVME" {
		t.Fatalf("unexpected warm-fill tiers: %+v", got.SSDTargetFill.Tiers)
	}
	tierStatus := got.SSDTargetFill.Tiers[0]
	if tierStatus.Reason != "below target_fill_pct" || tierStatus.Direction != "promote_to_ssd" || tierStatus.DeltaBytes != 30 {
		t.Fatalf("unexpected below-target tier status: %+v", tierStatus)
	}
	if !strings.Contains(strings.Join(got.Reasons, "; "), "target_fill_pct") {
		t.Fatalf("expected target_fill_pct reason, got %+v", got.Reasons)
	}
}

func TestPoolSpindownBlocksConfirmedSSDTiersAboveTargetFill(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "NVME", "md0"); err != nil {
		t.Fatalf("assign NVME tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md1"); err != nil {
		t.Fatalf("assign HDD tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{
			{Path: "/dev/md0", MemberDisks: []string{"/dev/nvme0n1"}},
			{Path: "/dev/md1", MemberDisks: []string{"/dev/sda"}},
		}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Path: "/dev/nvme0n1", Rotational: false},
			{Path: "/dev/sda", Rotational: true},
		}, nil
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if !strings.HasSuffix(path, "/NVME") {
			return syscall.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 100
		st.Bfree = 40
		st.Bavail = 40
		return nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Eligible {
		t.Fatalf("expected SSD above target fill to block spindown: %+v", got)
	}
	if !got.SSDTargetFill.Required || got.SSDTargetFill.Satisfied {
		t.Fatalf("unexpected SSD target-balance status: %+v", got.SSDTargetFill)
	}
	if len(got.SSDTargetFill.Tiers) != 1 {
		t.Fatalf("unexpected target-balance tiers: %+v", got.SSDTargetFill.Tiers)
	}
	tierStatus := got.SSDTargetFill.Tiers[0]
	if tierStatus.Reason != "above target_fill_pct" || tierStatus.Direction != "demote_from_ssd" || tierStatus.DeltaBytes != -10 {
		t.Fatalf("unexpected above-target tier status: %+v", tierStatus)
	}
	if !strings.Contains(strings.Join(got.Reasons, "; "), "not at target_fill_pct") {
		t.Fatalf("expected not-at-target reason, got %+v", got.Reasons)
	}
}

func TestPoolSpindownAllowsConfirmedSSDTiersAtTargetFill(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "NVME", "md0"); err != nil {
		t.Fatalf("assign NVME tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md1"); err != nil {
		t.Fatalf("assign HDD tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{
			{Path: "/dev/md0", MemberDisks: []string{"/dev/nvme0n1"}},
			{Path: "/dev/md1", MemberDisks: []string{"/dev/sda"}},
		}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Path: "/dev/nvme0n1", Rotational: false},
			{Path: "/dev/sda", Rotational: true},
		}, nil
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if !strings.HasSuffix(path, "/NVME") {
			return syscall.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 100
		st.Bfree = 50
		st.Bavail = 50
		return nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Eligible {
		t.Fatalf("expected SSD at target fill to allow spindown: %+v", got)
	}
	if !got.SSDTargetFill.Required || !got.SSDTargetFill.Satisfied {
		t.Fatalf("unexpected SSD warm-fill status: %+v", got.SSDTargetFill)
	}
	if len(got.SSDTargetFill.Tiers) != 1 || got.SSDTargetFill.Tiers[0].CurrentFillPct != 50 || got.SSDTargetFill.Tiers[0].DeltaBytes != 0 {
		t.Fatalf("unexpected target-balance tier status: %+v", got.SSDTargetFill.Tiers)
	}
}

func TestPoolSpindownBlocksWhileTargetBalanceMovementActive(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "NVME", "md0"); err != nil {
		t.Fatalf("assign NVME tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md1"); err != nil {
		t.Fatalf("assign HDD tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{
			{Path: "/dev/md0", MemberDisks: []string{"/dev/nvme0n1"}},
			{Path: "/dev/md1", MemberDisks: []string{"/dev/sda"}},
		}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Path: "/dev/nvme0n1", Rotational: false},
			{Path: "/dev/sda", Rotational: true},
		}, nil
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if !strings.HasSuffix(path, "/NVME") {
			return syscall.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 100
		st.Bfree = 50
		st.Bavail = 50
		return nil
	}
	if err := spindown.StoreTargetBalanceStatus(h.store, "media", spindown.TargetBalanceStatus{
		Active:       true,
		StartedAt:    "2026-04-26T19:50:00Z",
		CheckedAt:    "2026-04-26T19:50:00Z",
		PlannedMoves: 3,
		PendingMoves: 3,
		Reason:       "target-balance placement running",
	}); err != nil {
		t.Fatalf("store target balance status: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Eligible {
		t.Fatalf("expected active target-balance movement to block spindown: %+v", got)
	}
	if !got.SSDTargetFill.Satisfied {
		t.Fatalf("target fill should be satisfied; movement alone should block: %+v", got.SSDTargetFill)
	}
	if !got.SSDTargetFill.Movement.Active || got.SSDTargetFill.Movement.PendingMoves != 3 {
		t.Fatalf("unexpected movement status: %+v", got.SSDTargetFill.Movement)
	}
	if !strings.Contains(strings.Join(got.Reasons, "; "), "target-balance placement running") {
		t.Fatalf("expected movement reason, got %+v", got.Reasons)
	}
}

func TestPoolSpindownAllowsCandidateExhaustedTargetBalance(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "NVME", "md0"); err != nil {
		t.Fatalf("assign NVME tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md1"); err != nil {
		t.Fatalf("assign HDD tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{
			{Path: "/dev/md0", MemberDisks: []string{"/dev/nvme0n1"}},
			{Path: "/dev/md1", MemberDisks: []string{"/dev/sda"}},
		}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{
			{Path: "/dev/nvme0n1", Rotational: false},
			{Path: "/dev/sda", Rotational: true},
		}, nil
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if !strings.HasSuffix(path, "/NVME") {
			return syscall.ENOENT
		}
		st.Bsize = 1
		st.Blocks = 100
		st.Bfree = 80
		st.Bavail = 80
		return nil
	}
	if err := spindown.StoreTargetBalanceStatus(h.store, "media", spindown.TargetBalanceStatus{
		FinishedAt:         "2026-04-26T19:55:00Z",
		CheckedAt:          "2026-04-26T19:55:00Z",
		CandidateCount:     0,
		CandidateExhausted: true,
		Reason:             "target-balance placement exhausted candidates",
	}); err != nil {
		t.Fatalf("store target balance status: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/spindown", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET spindown: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Eligible {
		t.Fatalf("candidate-exhausted target balance should not block spindown: %+v", got)
	}
	if got.SSDTargetFill.Satisfied {
		t.Fatalf("SSD target fill should still report exact target unsatisfied: %+v", got.SSDTargetFill)
	}
	if !got.SSDTargetFill.Movement.CandidateExhausted {
		t.Fatalf("expected candidate-exhausted movement status: %+v", got.SSDTargetFill.Movement)
	}
	if strings.Contains(strings.Join(got.Reasons, "; "), "not at target_fill_pct") {
		t.Fatalf("candidate-exhausted target balance should suppress actionable backlog reason, got %+v", got.Reasons)
	}
}

func TestPoolSpindownActiveWindowsPersist(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AssignBackingToTier("media", "HDD", "zfs", "slow"); err != nil {
		t.Fatalf("assign slow: %v", err)
	}

	w := postJSON(h, http.MethodPut, "/api/tiers/media/spindown", map[string]any{
		"enabled": true,
		"active_windows": []map[string]any{{
			"days":  []string{"daily"},
			"start": "01:00",
			"end":   "06:00",
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT spindown windows: status %d body %s", w.Code, w.Body.String())
	}
	var got poolSpindownPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ActiveNow {
		t.Fatalf("expected noon to be outside 01:00-06:00 active window: %+v", got)
	}
	if got.NextActiveAt == "" {
		t.Fatalf("expected next active time: %+v", got)
	}
	if len(got.ActiveWindows) != 1 || got.ActiveWindows[0].Start != "01:00" {
		t.Fatalf("unexpected windows: %+v", got.ActiveWindows)
	}
}

func TestMdadmScrubBlockedOutsidePoolActiveWindow(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	w := postJSON(h, http.MethodPut, "/api/tiers/media/spindown", map[string]any{
		"enabled": true,
		"active_windows": []map[string]any{{
			"days":  []string{"daily"},
			"start": "01:00",
			"end":   "06:00",
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT spindown windows: status %d body %s", w.Code, w.Body.String())
	}
	scrubCalled := false
	scrubMDADMArray = func(string) error {
		scrubCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/arrays/md0/scrub", nil)
	rec := httptest.NewRecorder()
	h.Route(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("scrub status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if scrubCalled {
		t.Fatal("scrub command ran outside active window")
	}
}

func TestMdadmScrubAllowedOutsideWindowWhenHDDAlreadyActive(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", "HDD", "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	w := postJSON(h, http.MethodPut, "/api/tiers/media/spindown", map[string]any{
		"enabled": true,
		"active_windows": []map[string]any{{
			"days":  []string{"daily"},
			"start": "01:00",
			"end":   "06:00",
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT spindown windows: status %d body %s", w.Code, w.Body.String())
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{{Path: "/dev/md0", MemberDisks: []string{"/dev/sda"}}}, nil
	}
	listDisksForSpindown = func() ([]disk.Disk, error) {
		return []disk.Disk{{Path: "/dev/sda", Rotational: true}}, nil
	}
	queryPowerStateForSpindown = func(path string) (string, error) {
		if path != "/dev/sda" {
			t.Fatalf("unexpected power query path %q", path)
		}
		return "active", nil
	}
	scrubCalled := false
	scrubMDADMArray = func(string) error {
		scrubCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/arrays/md0/scrub", nil)
	rec := httptest.NewRecorder()
	h.Route(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrub status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !scrubCalled {
		t.Fatal("expected scrub command to run once HDD was externally active")
	}
}

func TestListTiersIncludesTierDetailsAndLiveCapacity(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotSSD, "md1"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	listPoolPVs = func(vg string) ([]lvm.PVInfo, error) {
		// poolDetailFromStore now queries per-tier VGs first and falls
		// back to the legacy monolithic VG only when none exist.
		// Simulate a legacy-layout pool: per-tier VGs return empty, the
		// legacy "tier-media" VG holds all PVs.
		switch vg {
		case "tier-media":
			return []lvm.PVInfo{
				{Device: "/dev/md0", SizeBytes: 100},
				{Device: "/dev/md1", SizeBytes: 300},
			}, nil
		case "tier-media-NVME", "tier-media-SSD", "tier-media-HDD":
			return nil, nil
		default:
			t.Fatalf("unexpected vg %q", vg)
			return nil, nil
		}
	}
	poolUsageBytes = func(mountPoint string) uint64 {
		if mountPoint != "/mnt/media" {
			t.Fatalf("unexpected mount point %q", mountPoint)
		}
		return 40
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tiers: status %d, body %s", w.Code, w.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(got))
	}
	if got[0]["capacity_bytes"] != float64(400) || got[0]["used_bytes"] != float64(40) {
		t.Fatalf("unexpected pool capacity/usage: %#v", got[0])
	}
	tiers, ok := got[0]["tiers"].([]any)
	if !ok || len(tiers) != 3 {
		t.Fatalf("expected tier detail array, got %#v", got[0]["tiers"])
	}
	first := tiers[0].(map[string]any)
	if first["name"] != "NVME" || first["capacity_bytes"] != float64(100) || first["array_id"] != float64(1) {
		t.Fatalf("unexpected first tier detail: %#v", first)
	}
	last := tiers[2].(map[string]any)
	if last["state"] != db.TierSlotStateEmpty || last["array_id"] != nil || last["pv_device"] != nil || last["capacity_bytes"] != float64(0) {
		t.Fatalf("unexpected empty tier detail: %#v", last)
	}
}

func TestListTiersUsesMountedBackingTotalCapacity(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	listPoolPVs = func(vg string) ([]lvm.PVInfo, error) {
		switch vg {
		case "tier-media-NVME":
			return []lvm.PVInfo{{Device: "/dev/md0", SizeBytes: 1000}}, nil
		case "tier-media-SSD", "tier-media-HDD", "tier-media":
			return nil, nil
		default:
			t.Fatalf("unexpected vg %q", vg)
			return nil, nil
		}
	}
	isMountPathBusy = func(path string) bool {
		return path == "/mnt/.tierd-backing/media/NVME"
	}
	statfsPath = func(path string, st *syscall.Statfs_t) error {
		if path != "/mnt/.tierd-backing/media/NVME" {
			t.Fatalf("unexpected statfs path %q", path)
		}
		st.Blocks = 100
		st.Bfree = 40
		st.Bavail = 35
		st.Bsize = 10
		return nil
	}
	if err := h.store.SetControlPlaneConfig("tier_baseline.media.NVME", "900"); err != nil {
		t.Fatalf("seed polluted baseline: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tiers: status %d, body %s", w.Code, w.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	tiers := got[0]["tiers"].([]any)
	first := tiers[0].(map[string]any)
	if first["capacity_bytes"] != float64(1000) {
		t.Fatalf("capacity should be total blocks, not free bytes: %#v", first)
	}
	if first["free_bytes"] != float64(350) || first["used_bytes"] != float64(600) {
		t.Fatalf("unexpected mounted usage: %#v", first)
	}
	if val, err := h.store.GetControlPlaneConfig("tier_baseline.media.NVME"); err != nil || val != "" {
		t.Fatalf("polluted baseline was not cleared, val=%q err=%v", val, err)
	}
}

func TestGetTierReturnsDetailedPoolObject(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	listPoolPVs = func(vg string) ([]lvm.PVInfo, error) {
		// Simulate legacy-only layout: PVs live in the monolithic VG,
		// per-tier VGs don't exist yet.
		if vg == "tier-media" {
			return []lvm.PVInfo{{Device: "/dev/md0", SizeBytes: 512}}, nil
		}
		return nil, nil
	}
	poolUsageBytes = func(string) uint64 { return 128 }

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "media" || got["capacity_bytes"] != float64(512) || got["used_bytes"] != float64(128) {
		t.Fatalf("unexpected pool detail: %#v", got)
	}
}

func TestGetTierReturnsNotFoundForUnknownPool(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/missing", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pool not found") {
		t.Fatalf("expected pool-not-found error, got %s", w.Body.String())
	}
}

func TestGetTierMapReturnsOrderedSegmentsAndVerificationTimestamp(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign nvme: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotSSD, "md1"); err != nil {
		t.Fatalf("assign ssd: %v", err)
	}
	tierDataLVExists = func(vg, lv string) (bool, error) {
		if vg != "tier-media" || lv != "data" {
			t.Fatalf("unexpected lv lookup %s/%s", vg, lv)
		}
		return true, nil
	}
	listTierSegments = func(vg, lv string) ([]lvm.Segment, error) {
		if vg != "tier-media" || lv != "data" {
			t.Fatalf("unexpected segment lookup %s/%s", vg, lv)
		}
		return []lvm.Segment{
			{VGName: vg, LVName: lv, PVPath: "/dev/md0", PEStart: 0, PEEnd: 2559},
			{VGName: vg, LVName: lv, PVPath: "/dev/md1", PEStart: 2560, PEEnd: 10239},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/map", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["pool"] != "media" || got["lv"] != "data" || got["verified"] != true {
		t.Fatalf("unexpected map response: %#v", got)
	}
	if got["verified_at"] != "2026-04-09T02:00:00Z" {
		t.Fatalf("verified_at = %v", got["verified_at"])
	}
	segments, ok := got["segments"].([]any)
	if !ok || len(segments) != 2 {
		t.Fatalf("segments = %#v", got["segments"])
	}
	first := segments[0].(map[string]any)
	if first["rank"] != float64(1) || first["tier"] != db.TierSlotNVME || first["pv_device"] != "/dev/md0" {
		t.Fatalf("unexpected first segment: %#v", first)
	}
}

func TestGetTierMapReturnsNotFoundForUnknownPool(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/missing/map", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pool not found") {
		t.Fatalf("expected pool-not-found error, got %s", w.Body.String())
	}
}

func TestGetTierMapReturnsServiceUnavailableWithoutLV(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/map", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LV does not exist yet") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestGetTierMapMarksPoolErrorWhenSegmentsAreOutOfOrder(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign nvme: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotSSD, "md1"); err != nil {
		t.Fatalf("assign ssd: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	tierDataLVExists = func(string, string) (bool, error) { return true, nil }
	listTierSegments = func(vg, lv string) ([]lvm.Segment, error) {
		return []lvm.Segment{
			{VGName: vg, LVName: lv, PVPath: "/dev/md1", PEStart: 0, PEEnd: 2559},
			{VGName: vg, LVName: lv, PVPath: "/dev/md0", PEStart: 2560, PEEnd: 10239},
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tiers/media/map", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["verified"] != false {
		t.Fatalf("expected verified=false, got %#v", got)
	}
	pool, err := h.store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("reload pool: %v", err)
	}
	if pool.State != db.TierPoolStateError || pool.ErrorReason != "segment_order_violation" {
		t.Fatalf("pool after verification = %+v", pool)
	}
}

func TestListTiersMethodNotAllowed(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPut, "/api/tiers", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestCreateTierRejectsEmptyCustomTierList(t *testing.T) {
	h := newTestHandler(t)
	body := `{"name":"media","tiers":[]}`
	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(body))
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty tier list, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateTierRejectsReservedNameBeforeSideEffects(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"root"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for reserved tier name, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reserved") {
		t.Fatalf("expected reserved-name error, got %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(db.TierMountRoot, "root")); !os.IsNotExist(err) {
		t.Fatalf("expected no mount point side effect, got err=%v", err)
	}
}

func TestCreateListAndDeleteTier(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)

	createBody := `{"name":"media"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(createBody))
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on create, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/tiers", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on list, got %d: %s", w.Code, w.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 tier, got %d", len(got))
	}
	if got[0]["name"] != "media" {
		t.Fatalf("unexpected tier name: %v", got[0]["name"])
	}
	if got[0]["state"] != db.TierPoolStateProvisioning {
		t.Fatalf("unexpected tier state: %v", got[0]["state"])
	}
	tiers, ok := got[0]["tiers"].([]any)
	if !ok {
		t.Fatalf("expected tiers in response, got %T", got[0]["tiers"])
	}
	if len(tiers) != 3 {
		t.Fatalf("expected 3 tiers in response, got %d", len(tiers))
	}
	hdd := tiers[2].(map[string]any)
	if hdd["state"] != db.TierSlotStateEmpty {
		t.Fatalf("unexpected hdd tier state: %v", hdd["state"])
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"media"}`))
	w = httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(db.TierMountRoot, "media")); !os.IsNotExist(err) {
		t.Fatalf("expected tier mount point to be removed after delete, got err=%v", err)
	}
}

func TestCreateTierAcceptsCustomTierList(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"media","tiers":[{"name":"FAST","rank":1},{"name":"CAPACITY","rank":3}]}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on custom create, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got["filesystem"] != "xfs" {
		t.Fatalf("filesystem = %v, want xfs", got["filesystem"])
	}
	tiers, ok := got["tiers"].([]any)
	if !ok || len(tiers) != 2 {
		t.Fatalf("expected 2 custom tiers, got %#v", got["tiers"])
	}
}

func TestCreateTierRecoversStaleEmptyTierInstance(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("seed stale tier: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(db.TierMountRoot, "media"), 0755); err != nil {
		t.Fatalf("seed stale mount point: %v", err)
	}
	if err := h.store.SetTierInstanceError("media", "boom"); err != nil {
		t.Fatalf("seed stale tier error state: %v", err)
	}

	var removedVG string
	removePoolVG = func(vg string) error {
		removedVG = vg
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 after stale tier recovery, got %d: %s", w.Code, w.Body.String())
	}
	if removedVG != "tier-media" {
		t.Fatalf("expected stale vg cleanup for tier-media, got %q", removedVG)
	}
	assignments, err := h.store.GetTierAssignments("media")
	if err != nil {
		t.Fatalf("get tier assignments: %v", err)
	}
	if len(assignments) != 0 {
		t.Fatalf("unexpected assignments after recreate: %+v", assignments)
	}
}

func TestCreateTierRejectsExistingAssignedTier(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("seed tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotHDD, "md0"); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for existing tier, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tier media already exists") {
		t.Fatalf("expected stable duplicate-tier error, got %s", w.Body.String())
	}
}

func TestDeleteTierRejectsInvalidNameBeforeSideEffects(t *testing.T) {
	h := newTestHandler(t)
	removePoolVGCalled := false
	removePoolVG = func(string) error {
		removePoolVGCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/-media", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid tier name, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "must start with a lowercase letter or digit") {
		t.Fatalf("expected specific validation error, got %s", w.Body.String())
	}
	if removePoolVGCalled {
		t.Fatal("expected validation to run before delete side effects")
	}
}

func TestDeleteTierRejectsMismatchedConfirmPoolNameBeforeLVMCommands(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}

	unmountCalled := false
	unmountTierPath = func(string) error {
		unmountCalled = true
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"backup"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if unmountCalled {
		t.Fatal("expected no LVM commands for mismatched confirm_pool_name")
	}
}

func TestDeleteTierRejectsActiveConsumers(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if _, err := h.store.CreateSmbShare(db.SmbShare{Name: "media-share", Path: "/mnt/media/shared"}); err != nil {
		t.Fatalf("create smb share: %v", err)
	}
	if _, err := h.store.CreateNfsExport(db.NfsExport{Path: "/mnt/media/exports"}); err != nil {
		t.Fatalf("create nfs export: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	consumers, ok := got["consumers"].([]any)
	if !ok || len(consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %#v", got["consumers"])
	}
}

func TestDeleteTierDestroysPoolStorageAndDBRows(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotNVME, "md0"); err != nil {
		t.Fatalf("assign nvme: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotSSD, "md1"); err != nil {
		t.Fatalf("assign ssd: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(db.TierMountRoot, "media"), 0755); err != nil {
		t.Fatalf("create mount dir: %v", err)
	}

	isMountPathBusy = func(string) bool { return true }
	var unmountedPaths []string
	unmountTierPath = func(mountPoint string) error {
		unmountedPaths = append(unmountedPaths, mountPoint)
		return nil
	}
	var pvRemoved []string
	removePVLabel = func(pv string) error {
		pvRemoved = append(pvRemoved, pv)
		return nil
	}
	var removedVGs []string
	removePoolVG = func(vg string) error {
		removedVGs = append(removedVGs, vg)
		return nil
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone // wait for async destroy goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	poolMount := filepath.Join(db.TierMountRoot, "media")
	foundPoolMount := false
	for _, p := range unmountedPaths {
		if p == poolMount {
			foundPoolMount = true
			break
		}
	}
	if !foundPoolMount {
		t.Fatalf("pool mount %q not in unmounted paths: %v", poolMount, unmountedPaths)
	}
	if len(pvRemoved) != 2 {
		t.Fatalf("pvRemoved = %v, want 2 entries", pvRemoved)
	}
	foundLegacyVG := false
	for _, vg := range removedVGs {
		if vg == "tier-media" {
			foundLegacyVG = true
			break
		}
	}
	if !foundLegacyVG {
		t.Fatalf("tier-media not in removed VGs: %v", removedVGs)
	}
	if _, err := h.store.GetTierInstance("media"); err != db.ErrNotFound {
		t.Fatalf("expected pool row to be deleted, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(db.TierMountRoot, "media")); !os.IsNotExist(err) {
		t.Fatalf("expected mount dir to be removed, got err=%v", err)
	}
}

func TestDeleteTierLeavesDestroyingStateWithErrorReasonOnFailure(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	isMountPathBusy = func(string) bool { return true }
	unmountTierPath = func(string) error { return assertErr("busy") }
	lazyUnmountPath = func(string) error { return assertErr("still busy") }

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone // wait for async destroy goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	pool, err := h.store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("reload pool: %v", err)
	}
	if pool.State != db.TierPoolStateDestroying {
		t.Fatalf("state = %q, want destroying", pool.State)
	}
	if pool.ErrorReason == "" || !strings.Contains(pool.ErrorReason, "unmount /mnt/media") {
		t.Fatalf("error_reason = %q", pool.ErrorReason)
	}
}

func TestAssignTierArrayRejectsInvalidPoolNameBeforeProvision(t *testing.T) {
	h := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPut, "/api/tiers/Root/tiers/HDD", stringBody(`{"array_id":1}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid tier name, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "must start with a lowercase letter or digit") {
		t.Fatalf("expected specific validation error, got %s", w.Body.String())
	}
}

func TestAssignTierArrayToCustomTierTransitionsPoolHealthy(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	h.provisionPerTierStorage = func(string, string) error { return nil }
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{{
			Name:  "md0",
			Path:  "/dev/md0",
			State: "active",
		}}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"media","tiers":[{"name":"FAST","rank":1},{"name":"CAPACITY","rank":2}]}`))
	w := httptest.NewRecorder()
	h.Route(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create tier: got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/tiers/media/tiers/FAST", stringBody(`{"array_id":1}`))
	w = httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone // wait for async provision goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("assign array: got %d: %s", w.Code, w.Body.String())
	}
	// After async provisioning, verify the pool transitioned to healthy.
	pool, err := h.store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("reload pool: %v", err)
	}
	if pool.State != db.TierPoolStateHealthy {
		t.Fatalf("pool state = %v, want healthy", pool.State)
	}
	slots, err := h.store.ListTierSlots("media")
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(slots))
	}
	if slots[0].Name != "FAST" || slots[0].State != db.TierSlotStateAssigned {
		t.Fatalf("first slot = %+v", slots[0])
	}
}

func TestAssignTierArrayRejectsInactiveArray(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{{
			Name:  "md0",
			Path:  "/dev/md0",
			State: "inactive",
		}}, nil
	}

	req := httptest.NewRequest(http.MethodPut, "/api/tiers/media/tiers/HDD", stringBody(`{"array_id":1}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for inactive array, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAssignTierArraySetsErrorStateOnProvisionFailure(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	// Provision fails for the per-tier path.
	h.provisionPerTierStorage = func(string, string) error { return assertErr("boom") }
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{{
			Name:  "md0",
			Path:  "/dev/md0",
			State: "active",
		}}, nil
	}

	req := httptest.NewRequest(http.MethodPut, "/api/tiers/media/tiers/HDD", stringBody(`{"array_id":1}`))
	w := httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone // wait for async provision goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (accepted eagerly), got %d: %s", w.Code, w.Body.String())
	}
	// The response must already reflect the eagerly-applied healthy state.
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["state"] != db.TierPoolStateHealthy {
		t.Fatalf("response state = %q, want %q", resp["state"], db.TierPoolStateHealthy)
	}
	// Background provisioning failure must leave the tier in error state
	// (not deleted) so the user can see what went wrong.
	pool, err := h.store.GetTierInstance("media")
	if err != nil {
		t.Fatalf("tier must still exist after provision failure, got err=%v", err)
	}
	if pool.State != db.TierPoolStateError {
		t.Fatalf("pool state = %q, want %q", pool.State, db.TierPoolStateError)
	}
}

func TestCreateTierRejectsMountPointFile(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := os.WriteFile(filepath.Join(db.TierMountRoot, "media"), []byte("x"), 0644); err != nil {
		t.Fatalf("seed mount point file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tiers", stringBody(`{"name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for mount-point file, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteTierRetrySucceedsFromDestroyingState(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateDestroying); err != nil {
		t.Fatalf("transition destroying: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media", stringBody(`{"confirm_pool_name":"media"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)
	<-h.asyncDone // wait for async destroy goroutine

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for retry from destroying state, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := h.store.GetTierInstance("media"); err != db.ErrNotFound {
		t.Fatalf("expected pool row to be deleted after retry, got err=%v", err)
	}
}

func TestAssignTierArrayRejectsDestroyingState(t *testing.T) {
	oldMountRoot := db.TierMountRoot
	db.TierMountRoot = t.TempDir()
	t.Cleanup(func() { db.TierMountRoot = oldMountRoot })

	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateDestroying); err != nil {
		t.Fatalf("transition destroying: %v", err)
	}
	listMDADMArrays = func() ([]mdadm.Array, error) {
		return []mdadm.Array{{
			Name:  "md0",
			Path:  "/dev/md0",
			State: "active",
		}}, nil
	}

	req := httptest.NewRequest(http.MethodPut, "/api/tiers/media/tiers/HDD", stringBody(`{"array_id":1}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for destroying tier, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), db.TierPoolStateDestroying) {
		t.Fatalf("expected current state in error body, got %s", w.Body.String())
	}
}

func TestUnassignTierArrayNotSupported(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotHDD, "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media/tiers/HDD", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for tier downsize, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnassignTierArrayAlwaysReturns405(t *testing.T) {
	h := newTestHandler(t)
	if err := h.store.CreateTierInstance("media"); err != nil {
		t.Fatalf("create tier: %v", err)
	}
	if err := h.store.AddArrayToTierSlot("media", db.TierSlotHDD, "md0"); err != nil {
		t.Fatalf("assign array: %v", err)
	}
	if err := h.store.TransitionTierInstanceState("media", db.TierPoolStateHealthy); err != nil {
		t.Fatalf("transition healthy: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/tiers/media/tiers/HDD", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for tier downsize, got %d: %s", w.Code, w.Body.String())
	}
	// Slot and pool state must be unchanged.
	slot, err := h.store.GetTierSlot("media", "HDD")
	if err != nil {
		t.Fatalf("get tier slot: %v", err)
	}
	if slot.State != db.TierSlotStateAssigned {
		t.Fatalf("slot state changed after rejected unassign: %s", slot.State)
	}
}

func assertErr(msg string) error { return &staticErr{msg: msg} }

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }

func stringBody(s string) *strings.Reader { return strings.NewReader(s) }
