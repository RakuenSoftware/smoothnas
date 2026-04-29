package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/iscsi"
)

// disableActiveLUNExecutor swaps runActiveLUNMoveImpl for a no-op so
// tests that exercise the execute HTTP handler don't race the async
// goroutine that the production handler kicks off. The override is
// per-test via t.Cleanup.
func disableActiveLUNExecutor(t *testing.T) {
	t.Helper()
	orig := runActiveLUNMoveImpl
	runActiveLUNMoveImpl = func(_ context.Context, _ *SharingHandler, _ string) {}
	t.Cleanup(func() { runActiveLUNMoveImpl = orig })
}

func TestListISCSITargetsIncludesFileLUNPinStatus(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{
			Path:       path,
			OnSmoothfs: true,
			Pinned:     true,
			State:      "pinned",
		}
	}

	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         "iqn.2026-01.com.smoothnas:block",
		BlockDevice: "/dev/zvol/tank/block",
		BackingType: db.IscsiBackingBlock,
	}); err != nil {
		t.Fatalf("create block target: %v", err)
	}
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         "iqn.2026-01.com.smoothnas:file",
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/iscsi/targets", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET targets status = %d, body=%s", w.Code, w.Body.String())
	}

	var got []struct {
		IQN         string              `json:"iqn"`
		BackingType string              `json:"backing_type"`
		LUNPin      *iscsi.LUNPinStatus `json:"lun_pin,omitempty"`
		Quiesced    bool                `json:"quiesced"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("targets len = %d, want 2: %#v", len(got), got)
	}
	if got[0].LUNPin != nil {
		t.Fatalf("block target lun_pin = %#v, want nil", got[0].LUNPin)
	}
	if got[1].LUNPin == nil {
		t.Fatal("file target lun_pin = nil, want status")
	}
	if !got[1].LUNPin.Pinned || got[1].LUNPin.State != "pinned" {
		t.Fatalf("file target lun_pin = %#v, want pinned status", got[1].LUNPin)
	}
	if got[1].Quiesced {
		t.Fatal("file target quiesced = true, want false by default")
	}
}

func TestListISCSITargetsIncludesQuiesceState(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/iscsi/targets", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET targets status = %d, body=%s", w.Code, w.Body.String())
	}
	var got []struct {
		IQN      string `json:"iqn"`
		Quiesced bool   `json:"quiesced"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].IQN != iqn || !got[0].Quiesced {
		t.Fatalf("targets = %#v, want quiesced target %q", got, iqn)
	}
}

func TestQuiesceISCSIFileTargetRequiresFileBacking(t *testing.T) {
	h := newTestSharingHandler(t)
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         "iqn.2026-01.com.smoothnas:block",
		BlockDevice: "/dev/zvol/tank/block",
		BackingType: db.IscsiBackingBlock,
	}); err != nil {
		t.Fatalf("create block target: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/iqn.2026-01.com.smoothnas:block/quiesce", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "file-backed") {
		t.Fatalf("body %q does not explain file-backed requirement", w.Body.String())
	}
}

func TestQuiesceISCSIFileTargetRejectsUnpinnedSmoothfsLUN(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	origQuiesce := quiesceISCSITarget
	t.Cleanup(func() {
		inspectLUNPin = origInspect
		quiesceISCSITarget = origQuiesce
	})
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{
			Path:       path,
			OnSmoothfs: true,
			Pinned:     false,
			State:      "unpinned",
			Reason:     "PIN_LUN xattr is absent",
		}
	}
	quiesceCalled := false
	quiesceISCSITarget = func(iqn string) error {
		quiesceCalled = true
		return nil
	}

	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         "iqn.2026-01.com.smoothnas:file",
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/iqn.2026-01.com.smoothnas:file/quiesce", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if quiesceCalled {
		t.Fatal("quiesce command ran despite missing PIN_LUN")
	}
}

func TestQuiesceAndResumeISCSIFileTarget(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	origQuiesce := quiesceISCSITarget
	origResume := resumeISCSITarget
	t.Cleanup(func() {
		inspectLUNPin = origInspect
		quiesceISCSITarget = origQuiesce
		resumeISCSITarget = origResume
	})
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	var actions []string
	quiesceISCSITarget = func(iqn string) error {
		actions = append(actions, "quiesce:"+iqn)
		return nil
	}
	resumeISCSITarget = func(iqn string) error {
		actions = append(actions, "resume:"+iqn)
		return nil
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/iscsi/targets/" + iqn + "/quiesce", "quiesced"},
		{"/api/iscsi/targets/" + iqn + "/resume", "resumed"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		w := httptest.NewRecorder()
		h.Route(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", tc.path, w.Code, w.Body.String())
		}
		var got struct {
			Status   string `json:"status"`
			IQN      string `json:"iqn"`
			Quiesced bool   `json:"quiesced"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s response: %v", tc.path, err)
		}
		if got.Status != tc.want || got.IQN != iqn {
			t.Fatalf("%s response = %#v, want status %q iqn %q", tc.path, got, tc.want, iqn)
		}
		wantQuiesced := tc.want == "quiesced"
		if got.Quiesced != wantQuiesced {
			t.Fatalf("%s quiesced = %t, want %t", tc.path, got.Quiesced, wantQuiesced)
		}
		stored, err := h.store.GetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), !wantQuiesced)
		if err != nil {
			t.Fatalf("read stored quiesce state: %v", err)
		}
		if stored != wantQuiesced {
			t.Fatalf("%s stored quiesced = %t, want %t", tc.path, stored, wantQuiesced)
		}
	}

	wantActions := []string{"quiesce:" + iqn, "resume:" + iqn}
	if strings.Join(actions, ",") != strings.Join(wantActions, ",") {
		t.Fatalf("actions = %#v, want %#v", actions, wantActions)
	}
}

func TestCreateISCSIMoveIntentRequiresQuiescedPinnedFileLUN(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent", strings.NewReader(`{"destination_tier":"FAST"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("not-quiesced status = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: false, State: "unpinned"}
	}

	req = httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent", strings.NewReader(`{"destination_tier":"FAST"}`))
	w = httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("unpinned status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateListAndClearISCSIMoveIntent(t *testing.T) {
	h := newTestSharingHandler(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent", strings.NewReader(`{"destination_tier":"FAST"}`))
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("create move intent status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Status     string             `json:"status"`
		MoveIntent iscsiLUNMoveIntent `json:"move_intent"`
		Quiesced   bool               `json:"quiesced"`
	}
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.MoveIntent.IQN != iqn || created.MoveIntent.DestinationTier != "FAST" ||
		created.MoveIntent.State != "planned" || !created.Quiesced {
		t.Fatalf("created intent = %#v, want planned FAST intent for %q", created, iqn)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/iscsi/targets", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list targets status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var listed []struct {
		IQN        string              `json:"iqn"`
		MoveIntent *iscsiLUNMoveIntent `json:"move_intent,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed) != 1 || listed[0].MoveIntent == nil || listed[0].MoveIntent.DestinationTier != "FAST" {
		t.Fatalf("listed targets = %#v, want move intent destination FAST", listed)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/iscsi/targets/"+iqn+"/move-intent", nil)
	w = httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("clear move intent status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn)); err != db.ErrNotFound {
		t.Fatalf("move intent config after clear error = %v, want ErrNotFound", err)
	}
}

func TestExecuteISCSIMoveIntentRequiresRecordedIntent(t *testing.T) {
	h := newTestSharingHandler(t)
	disableActiveLUNExecutor(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/execute", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestExecuteISCSIMoveIntentJournalsExecutingState(t *testing.T) {
	h := newTestSharingHandler(t)
	disableActiveLUNExecutor(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStatePlanned,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/execute", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Status     string             `json:"status"`
		MoveIntent iscsiLUNMoveIntent `json:"move_intent"`
		Quiesced   bool               `json:"quiesced"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Quiesced || got.MoveIntent.State != iscsiLUNMoveIntentStateExecuting {
		t.Fatalf("execute response = %#v, want executing FAST intent and quiesced=true", got)
	}
	if got.MoveIntent.Reason != iscsiLUNMoveIntentExecutorStartedReason {
		t.Fatalf("execute reason = %q, want stub reason", got.MoveIntent.Reason)
	}
	if got.MoveIntent.StateUpdatedAt == intent.StateUpdatedAt {
		t.Fatalf("state_updated_at not advanced after execute: %q", got.MoveIntent.StateUpdatedAt)
	}

	stored, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn))
	if err != nil {
		t.Fatalf("read stored intent: %v", err)
	}
	var persisted iscsiLUNMoveIntent
	if err := json.Unmarshal([]byte(stored), &persisted); err != nil {
		t.Fatalf("decode stored intent: %v", err)
	}
	if persisted.State != iscsiLUNMoveIntentStateExecuting {
		t.Fatalf("stored state = %q, want executing", persisted.State)
	}
}

func TestExecuteISCSIMoveIntentRejectsNonPlannedState(t *testing.T) {
	h := newTestSharingHandler(t)
	disableActiveLUNExecutor(t)

	origInspect := inspectLUNPin
	t.Cleanup(func() { inspectLUNPin = origInspect })
	inspectLUNPin = func(path string) iscsi.LUNPinStatus {
		return iscsi.LUNPinStatus{Path: path, OnSmoothfs: true, Pinned: true, State: "pinned"}
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true); err != nil {
		t.Fatalf("set quiesce state: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStateExecuting,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/execute", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	stored, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn))
	if err != nil {
		t.Fatalf("read stored intent: %v", err)
	}
	if stored != string(data) {
		t.Fatalf("stored intent mutated on rejected re-execute: got %q want %q", stored, string(data))
	}
}

func TestAbortISCSIMoveIntentTransitionsBackToPlanned(t *testing.T) {
	h := newTestSharingHandler(t)

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStateExecuting,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		Reason:          iscsiLUNMoveIntentExecutorStartedReason,
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/abort", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	stored, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn))
	if err != nil {
		t.Fatalf("read stored intent: %v", err)
	}
	var persisted iscsiLUNMoveIntent
	if err := json.Unmarshal([]byte(stored), &persisted); err != nil {
		t.Fatalf("decode stored intent: %v", err)
	}
	if persisted.State != iscsiLUNMoveIntentStatePlanned {
		t.Fatalf("aborted state = %q, want planned", persisted.State)
	}
	if persisted.Reason != "operator abort" {
		t.Fatalf("aborted reason = %q, want operator abort", persisted.Reason)
	}
}

func TestAbortISCSIMoveIntentRejectsPlannedState(t *testing.T) {
	h := newTestSharingHandler(t)

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStatePlanned,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/abort", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestAbortISCSIMoveIntentTransitionsFailedBackToPlanned(t *testing.T) {
	h := newTestSharingHandler(t)

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStateFailed,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		Reason:          "move plan: ebusy",
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/abort", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	stored, err := h.store.GetConfig(iscsiTargetMoveIntentConfigKey(iqn))
	if err != nil {
		t.Fatalf("read stored intent: %v", err)
	}
	var persisted iscsiLUNMoveIntent
	if err := json.Unmarshal([]byte(stored), &persisted); err != nil {
		t.Fatalf("decode stored intent: %v", err)
	}
	if persisted.State != iscsiLUNMoveIntentStatePlanned {
		t.Fatalf("aborted state = %q, want planned", persisted.State)
	}
	if persisted.Reason != "operator abort" {
		t.Fatalf("aborted reason = %q, want 'operator abort'", persisted.Reason)
	}
}

func TestAbortISCSIMoveIntentRejectsCompletedState(t *testing.T) {
	h := newTestSharingHandler(t)

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "FAST",
		State:           iscsiLUNMoveIntentStateCompleted,
		StateUpdatedAt:  "2026-04-26T20:00:00Z",
		CreatedAt:       "2026-04-26T20:00:00Z",
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := h.store.SetConfig(iscsiTargetMoveIntentConfigKey(iqn), string(data)); err != nil {
		t.Fatalf("store intent: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/abort", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestAbortISCSIMoveIntentRequiresIntent(t *testing.T) {
	h := newTestSharingHandler(t)

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create file target: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/iscsi/targets/"+iqn+"/move-intent/abort", nil)
	w := httptest.NewRecorder()
	h.Route(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}
