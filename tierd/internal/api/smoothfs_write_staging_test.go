package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
)

func newSmoothfsWriteStagingTestHandler(t *testing.T) *SmoothfsHandler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "smoothfs-write-staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err = store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "11111111-1111-1111-1111-111111111111",
		Name:       "media",
		Tiers:      []string{"/mnt/fast", "/mnt/slow"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	})
	if err != nil {
		t.Fatalf("create smoothfs pool: %v", err)
	}
	origRead := readSmoothfsWriteStagingFile
	origWrite := writeSmoothfsWriteStagingFile
	origRoot := smoothfsWriteStagingRoot
	readSmoothfsWriteStagingFile = func(string) ([]byte, error) {
		return nil, errNotExist{}
	}
	writeSmoothfsWriteStagingFile = func(string, []byte, os.FileMode) error {
		return nil
	}
	smoothfsWriteStagingRoot = func(string) string { return "/sys/fs/smoothfs/test" }
	t.Cleanup(func() {
		readSmoothfsWriteStagingFile = origRead
		writeSmoothfsWriteStagingFile = origWrite
		smoothfsWriteStagingRoot = origRoot
	})
	return NewSmoothfsHandler(store)
}

type errNotExist struct{}

func (errNotExist) Error() string { return "not found" }

func smoothfsJSON(h *SmoothfsHandler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Route(rec, req)
	return rec
}

func TestSmoothfsWriteStagingPersistsDesiredWhenUnsupported(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)

	rec := smoothfsJSON(h, http.MethodPut, "/api/smoothfs/pools/media/write-staging", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
	var got smoothfsWriteStagingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.DesiredEnabled || got.EffectiveEnabled || got.KernelSupported {
		t.Fatalf("unexpected unsupported response: %+v", got)
	}
	if got.Reason == "" {
		t.Fatalf("expected unsupported reason: %+v", got)
	}

	rec = smoothfsJSON(h, http.MethodGet, "/api/smoothfs/pools/media/write-staging", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
	got = smoothfsWriteStagingResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if !got.DesiredEnabled {
		t.Fatalf("desired setting was not persisted: %+v", got)
	}
}

func TestSmoothfsWriteStagingReadsKernelStatus(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
	kernelEnabled := false
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "write_staging_supported":
			return []byte("1\n"), nil
		case "write_staging_enabled":
			if kernelEnabled {
				return []byte("1\n"), nil
			}
			return []byte("0\n"), nil
		case "write_staging_full_pct":
			return []byte("95\n"), nil
		case "staged_bytes":
			return []byte("4096\n"), nil
		case "staged_rehome_bytes":
			return []byte("3072\n"), nil
		case "range_staged_bytes":
			return []byte("1024\n"), nil
		case "range_staged_writes":
			return []byte("4\n"), nil
		case "range_staging_recovery_supported":
			return []byte("1\n"), nil
		case "range_staging_recovered_bytes":
			return []byte("2048\n"), nil
		case "range_staging_recovered_writes":
			return []byte("5\n"), nil
		case "range_staging_recovery_pending":
			return []byte("512\n"), nil
		case "staged_rehomes_total":
			return []byte("3\n"), nil
		case "staged_rehomes_pending":
			return []byte("2\n"), nil
		case "write_staging_drain_pressure":
			return []byte("1\n"), nil
		case "write_staging_drainable_tier_mask":
			return []byte("0x2\n"), nil
		case "write_staging_drainable_rehomes":
			return []byte("2\n"), nil
		case "recovered_range_tier_mask":
			return []byte("0x2\n"), nil
		case "oldest_staged_write_at":
			return []byte("2026-04-26T17:00:00Z\n"), nil
		case "oldest_recovered_write_at":
			return []byte("2026-04-26T16:50:00Z\n"), nil
		case "last_drain_at":
			return []byte("2026-04-26T17:05:00Z\n"), nil
		case "last_drain_reason":
			return []byte("external-active\n"), nil
		case "last_recovery_at":
			return []byte("2026-04-26T16:55:00Z\n"), nil
		case "last_recovery_reason":
			return []byte("remount-replay\n"), nil
		case "metadata_active_tier_mask":
			return []byte("0x1\n"), nil
		case "write_staging_drain_active_tier_mask":
			return []byte("0x1\n"), nil
		case "metadata_tier_skips":
			return []byte("7\n"), nil
		default:
			return nil, errNotExist{}
		}
	}
	writeSmoothfsWriteStagingFile = func(path string, data []byte, _ os.FileMode) error {
		if filepath.Base(path) != "write_staging_enabled" {
			t.Fatalf("unexpected write path %s", path)
		}
		kernelEnabled = string(data) == "1\n"
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPut, "/api/smoothfs/pools/media/write-staging", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
	var got smoothfsWriteStagingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.DesiredEnabled || !got.EffectiveEnabled || !got.KernelSupported || !got.KernelEnabled {
		t.Fatalf("unexpected supported response: %+v", got)
	}
	if got.StagedBytes != 4096 || got.LastDrainReason != "external-active" {
		t.Fatalf("unexpected status fields: %+v", got)
	}
	if got.StagedRehomeBytes != 3072 || got.RangeStagedBytes != 1024 || got.RangeStagedWrites != 4 {
		t.Fatalf("unexpected staged range/rehome fields: %+v", got)
	}
	if !got.RangeStagingRecoverySupported {
		t.Fatalf("range staging recovery supported = false, want true; %+v", got)
	}
	if got.RangeStagingRecoveredBytes != 2048 || got.RangeStagingRecoveredWrites != 5 || got.RangeStagingRecoveryPending != 512 {
		t.Fatalf("unexpected range staging recovery fields: %+v", got)
	}
	if got.LastRecoveryAt != "2026-04-26T16:55:00Z" || got.LastRecoveryReason != "remount-replay" {
		t.Fatalf("unexpected last-recovery fields: %+v", got)
	}
	if got.OldestRecoveredWriteAt != "2026-04-26T16:50:00Z" {
		t.Fatalf("oldest recovered write at = %q, want 2026-04-26T16:50:00Z", got.OldestRecoveredWriteAt)
	}
	if got.StagedRehomesTotal != 3 {
		t.Fatalf("staged rehomes total = %d, want 3", got.StagedRehomesTotal)
	}
	if got.StagedRehomesPending != 2 {
		t.Fatalf("staged rehomes pending = %d, want 2", got.StagedRehomesPending)
	}
	if !got.WriteStagingDrainPressure || got.WriteStagingDrainableTierMask != 2 {
		t.Fatalf("unexpected drain state: %+v", got)
	}
	if got.WriteStagingDrainableRehomes != 2 {
		t.Fatalf("drainable rehomes = %d, want 2", got.WriteStagingDrainableRehomes)
	}
	if got.RecoveredRangeTierMask != 2 {
		t.Fatalf("recovered range tier mask = 0x%x, want 0x2", got.RecoveredRangeTierMask)
	}
	if got.FullThresholdPct != 95 {
		t.Fatalf("full threshold = %d, want 95", got.FullThresholdPct)
	}
	if got.MetadataActiveTierMask != 1 || got.MetadataTierSkips != 7 {
		t.Fatalf("unexpected metadata gate fields: %+v", got)
	}
	if got.WriteStagingDrainActiveTierMask != 1 || got.RecommendedDrainActiveTierMask != 1 {
		t.Fatalf("unexpected drain gate fields: %+v", got)
	}
	if got.SmoothNASWakesAllowed {
		t.Fatal("SmoothNAS must not be allowed to wake HDDs for staging drains")
	}
}

func TestSmoothfsWriteStagingWritesMetadataActiveMaskBeforeEnable(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
	var writes []string
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "write_staging_supported", "write_staging_enabled":
			return []byte("1\n"), nil
		case "write_staging_full_pct":
			return []byte("98\n"), nil
		case "metadata_active_tier_mask":
			return []byte("0x3\n"), nil
		default:
			return nil, errNotExist{}
		}
	}
	writeSmoothfsWriteStagingFile = func(path string, data []byte, _ os.FileMode) error {
		writes = append(writes, filepath.Base(path)+"="+string(data))
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPut, "/api/smoothfs/pools/media/write-staging", map[string]any{
		"enabled":                   true,
		"full_threshold_pct":        97,
		"metadata_active_tier_mask": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
	want := []string{
		"write_staging_full_pct=97\n",
		"metadata_active_tier_mask=0x1\n",
		"write_staging_enabled=1\n",
	}
	if len(writes) != len(want) {
		t.Fatalf("writes = %#v, want %#v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Fatalf("writes[%d] = %q, want %q; all writes=%#v", i, writes[i], want[i], writes)
		}
	}
}

func TestSmoothfsWriteStagingAutoWritesMetadataMaskFromObservedDiskState(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
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
	if err := h.store.UpdateSmoothfsPool(db.SmoothfsPool{
		UUID:       "11111111-1111-1111-1111-111111111111",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	}); err != nil {
		t.Fatalf("update smoothfs pool: %v", err)
	}

	origListMDADMArrays := listMDADMArrays
	origListDisks := listDisksForSpindown
	origQueryPower := queryPowerStateForSpindown
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
	queryPowerStateForSpindown = func(path string) (string, error) {
		if path != "/dev/sda" {
			t.Fatalf("unexpected power query path %q", path)
		}
		return "standby", nil
	}
	t.Cleanup(func() {
		listMDADMArrays = origListMDADMArrays
		listDisksForSpindown = origListDisks
		queryPowerStateForSpindown = origQueryPower
	})

	kernelEnabled := false
	kernelMask := uint64(3)
	var writes []string
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "write_staging_supported":
			return []byte("1\n"), nil
		case "write_staging_enabled":
			if kernelEnabled {
				return []byte("1\n"), nil
			}
			return []byte("0\n"), nil
		case "metadata_active_tier_mask":
			return []byte("0x" + strconv.FormatUint(kernelMask, 16) + "\n"), nil
		case "write_staging_drain_active_tier_mask":
			return []byte("0x" + strconv.FormatUint(kernelMask, 16) + "\n"), nil
		case "write_staging_full_pct":
			return []byte("98\n"), nil
		default:
			return nil, errNotExist{}
		}
	}
	writeSmoothfsWriteStagingFile = func(path string, data []byte, _ os.FileMode) error {
		writes = append(writes, filepath.Base(path)+"="+string(data))
		switch filepath.Base(path) {
		case "metadata_active_tier_mask", "write_staging_drain_active_tier_mask":
			trimmed := strings.TrimSpace(string(data))
			parsed, err := strconv.ParseUint(trimmed, 0, 64)
			if err != nil {
				t.Fatalf("parse metadata mask %q: %v", trimmed, err)
			}
			kernelMask = parsed
		case "write_staging_enabled":
			kernelEnabled = string(data) == "1\n"
		}
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPut, "/api/smoothfs/pools/media/write-staging", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
	var got smoothfsWriteStagingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetadataActiveTierMask != 1 || got.RecommendedMetadataActiveTierMask != 1 {
		t.Fatalf("metadata masks = current %x recommended %x, want 1", got.MetadataActiveTierMask, got.RecommendedMetadataActiveTierMask)
	}
	if got.WriteStagingDrainActiveTierMask != 1 || got.RecommendedDrainActiveTierMask != 1 {
		t.Fatalf("drain masks = current %x recommended %x, want 1", got.WriteStagingDrainActiveTierMask, got.RecommendedDrainActiveTierMask)
	}
	if !strings.Contains(got.MetadataActiveMaskReason, "HDD inactive") {
		t.Fatalf("metadata reason = %q", got.MetadataActiveMaskReason)
	}
	wantPrefix := []string{
		"metadata_active_tier_mask=0x1\n",
		"write_staging_drain_active_tier_mask=0x1\n",
		"write_staging_enabled=1\n",
	}
	if len(writes) < len(wantPrefix) {
		t.Fatalf("writes = %#v, want at least %#v", writes, wantPrefix)
	}
	for i := range wantPrefix {
		if writes[i] != wantPrefix[i] {
			t.Fatalf("writes[%d] = %q, want %q; all writes=%#v", i, writes[i], wantPrefix[i], writes)
		}
	}
}

func TestSmoothfsMetadataMaskRefreshWritesRecommendation(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
	configureManagedSmoothfsMetadataMaskTest(t, h, "standby")

	kernelMask := uint64(3)
	var writes []string
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		switch filepath.Base(path) {
		case "write_staging_supported":
			return []byte("1\n"), nil
		case "write_staging_enabled":
			return []byte("1\n"), nil
		case "metadata_active_tier_mask":
			return []byte("0x" + strconv.FormatUint(kernelMask, 16) + "\n"), nil
		case "write_staging_drain_active_tier_mask":
			return []byte("0x" + strconv.FormatUint(kernelMask, 16) + "\n"), nil
		case "write_staging_full_pct":
			return []byte("98\n"), nil
		default:
			return nil, errNotExist{}
		}
	}
	writeSmoothfsWriteStagingFile = func(path string, data []byte, _ os.FileMode) error {
		writes = append(writes, filepath.Base(path)+"="+string(data))
		switch filepath.Base(path) {
		case "metadata_active_tier_mask", "write_staging_drain_active_tier_mask":
		default:
			t.Fatalf("unexpected sysfs write to %s", path)
		}
		trimmed := strings.TrimSpace(string(data))
		parsed, err := strconv.ParseUint(trimmed, 0, 64)
		if err != nil {
			t.Fatalf("parse metadata mask %q: %v", trimmed, err)
		}
		kernelMask = parsed
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPost, "/api/smoothfs/pools/media/metadata-active-mask/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST metadata-active-mask refresh status %d body=%s", rec.Code, rec.Body.String())
	}
	var got smoothfsWriteStagingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetadataActiveTierMask != 1 || got.RecommendedMetadataActiveTierMask != 1 {
		t.Fatalf("metadata masks = current %x recommended %x, want 1", got.MetadataActiveTierMask, got.RecommendedMetadataActiveTierMask)
	}
	if got.WriteStagingDrainActiveTierMask != 1 || got.RecommendedDrainActiveTierMask != 1 {
		t.Fatalf("drain masks = current %x recommended %x, want 1", got.WriteStagingDrainActiveTierMask, got.RecommendedDrainActiveTierMask)
	}
	if !strings.Contains(got.MetadataActiveMaskReason, "HDD inactive") {
		t.Fatalf("metadata reason = %q", got.MetadataActiveMaskReason)
	}
	want := []string{
		"metadata_active_tier_mask=0x1\n",
		"write_staging_drain_active_tier_mask=0x1\n",
	}
	if len(writes) != len(want) {
		t.Fatalf("writes = %#v, want %#v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Fatalf("writes[%d] = %q, want %q; all writes=%#v", i, writes[i], want[i], writes)
		}
	}
}

func TestSmoothfsMetadataMaskRefreshRejectsUnmanagedPool(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		if filepath.Base(path) == "write_staging_supported" {
			return []byte("1\n"), nil
		}
		return nil, errNotExist{}
	}
	writeSmoothfsWriteStagingFile = func(path string, _ []byte, _ os.FileMode) error {
		t.Fatalf("unexpected sysfs write to %s", path)
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPost, "/api/smoothfs/pools/media/metadata-active-mask/refresh", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST metadata-active-mask refresh status %d body=%s", rec.Code, rec.Body.String())
	}
}

func configureManagedSmoothfsMetadataMaskTest(t *testing.T, h *SmoothfsHandler, hddState string) {
	t.Helper()
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
	if err := h.store.UpdateSmoothfsPool(db.SmoothfsPool{
		UUID:       "11111111-1111-1111-1111-111111111111",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd-backing/media/NVME", "/mnt/.tierd-backing/media/HDD"},
		Mountpoint: "/mnt/media",
		UnitPath:   "/etc/systemd/system/mnt-media.mount",
	}); err != nil {
		t.Fatalf("update smoothfs pool: %v", err)
	}

	origListMDADMArrays := listMDADMArrays
	origListDisks := listDisksForSpindown
	origQueryPower := queryPowerStateForSpindown
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
	queryPowerStateForSpindown = func(path string) (string, error) {
		if path != "/dev/sda" {
			t.Fatalf("unexpected power query path %q", path)
		}
		return hddState, nil
	}
	t.Cleanup(func() {
		listMDADMArrays = origListMDADMArrays
		listDisksForSpindown = origListDisks
		queryPowerStateForSpindown = origQueryPower
	})
}

func TestSmoothfsWriteStagingRejectsInvalidFullThresholdBeforeSysfsWrites(t *testing.T) {
	h := newSmoothfsWriteStagingTestHandler(t)
	readSmoothfsWriteStagingFile = func(path string) ([]byte, error) {
		if filepath.Base(path) == "write_staging_supported" {
			return []byte("1\n"), nil
		}
		return nil, errNotExist{}
	}
	writeSmoothfsWriteStagingFile = func(path string, _ []byte, _ os.FileMode) error {
		t.Fatalf("unexpected sysfs write to %s", path)
		return nil
	}

	rec := smoothfsJSON(h, http.MethodPut, "/api/smoothfs/pools/media/write-staging", map[string]any{
		"enabled":            true,
		"full_threshold_pct": 0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT write-staging status %d body=%s", rec.Code, rec.Body.String())
	}
}
