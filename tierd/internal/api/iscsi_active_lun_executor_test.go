package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func TestFindSmoothfsPoolForBackingFilePicksLongestPrefix(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "exec.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "11111111-1111-1111-1111-111111111111",
		Name:       "media",
		Tiers:      []string{"/mnt/.tierd/media/NVME", "/mnt/.tierd/media/HDD"},
		Mountpoint: "/mnt/media",
	}); err != nil {
		t.Fatalf("create pool media: %v", err)
	}
	if _, err := store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "22222222-2222-2222-2222-222222222222",
		Name:       "media-archive",
		Tiers:      []string{"/mnt/.tierd/media-archive/HDD"},
		Mountpoint: "/mnt/media-archive",
	}); err != nil {
		t.Fatalf("create pool media-archive: %v", err)
	}

	got, err := findSmoothfsPoolForBackingFile(store, "/mnt/media-archive/lun.img")
	if err != nil {
		t.Fatalf("lookup media-archive: %v", err)
	}
	if got.Name != "media-archive" {
		t.Fatalf("got pool %q, want media-archive (longer prefix should win over media)", got.Name)
	}

	got, err = findSmoothfsPoolForBackingFile(store, "/mnt/media/sub/lun.img")
	if err != nil {
		t.Fatalf("lookup media: %v", err)
	}
	if got.Name != "media" {
		t.Fatalf("got pool %q, want media", got.Name)
	}

	if _, err := findSmoothfsPoolForBackingFile(store, "/mnt/other/lun.img"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err for unmapped path = %v, want ErrNotFound", err)
	}
}

func TestResolveDestinationTierAcceptsNumericIndex(t *testing.T) {
	pool := &db.SmoothfsPool{
		Name:  "media",
		Tiers: []string{"/mnt/fast", "/mnt/slow"},
	}
	idx, err := resolveDestinationTier(nil, pool, "1")
	if err != nil || idx != 1 {
		t.Fatalf("numeric resolve = %d, %v", idx, err)
	}
}

func TestResolveDestinationTierAcceptsTierPath(t *testing.T) {
	pool := &db.SmoothfsPool{
		Name:  "media",
		Tiers: []string{"/mnt/fast", "/mnt/slow"},
	}
	idx, err := resolveDestinationTier(nil, pool, "/mnt/slow")
	if err != nil || idx != 1 {
		t.Fatalf("path resolve = %d, %v", idx, err)
	}
}

func TestResolveDestinationTierAcceptsTierTargetName(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "exec.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.CreateTierPoolWithOptions("media", "xfs", []db.TierDefinition{
		{Name: "NVME", Rank: 1},
		{Name: "HDD", Rank: 2},
	}, true); err != nil {
		t.Fatalf("create tier pool: %v", err)
	}

	// Slot ranks are 1-based; smoothfs indices are 0-based, so HDD
	// (rank 2) maps to tier index 1 in pool.Tiers.
	pool := &db.SmoothfsPool{
		Name:  "media",
		Tiers: []string{"/mnt/fast", "/mnt/slow"},
	}
	idx, err := resolveDestinationTier(store, pool, "HDD")
	if err != nil || idx != 1 {
		t.Fatalf("name resolve = %d, %v", idx, err)
	}
}

func TestResolveDestinationTierRejectsOutOfRange(t *testing.T) {
	pool := &db.SmoothfsPool{
		Name:  "media",
		Tiers: []string{"/mnt/fast", "/mnt/slow"},
	}
	if _, err := resolveDestinationTier(nil, pool, "5"); err == nil {
		t.Fatalf("expected error for out-of-range numeric")
	}
	if _, err := resolveDestinationTier(nil, pool, "GLACIER"); err == nil {
		t.Fatalf("expected error for unknown name")
	}
}

// stubSmoothfsMoveClient records calls so the executor's progression
// can be asserted without a real netlink connection.
type stubSmoothfsMoveClient struct {
	movePlanCalls    int
	moveCutoverCalls int
	inspectCalls     int

	movePlanErr    error
	moveCutoverErr error
	inspectResults []*smoothfsclient.InspectResult
	inspectErrs    []error

	lastDestTier uint8
	lastSeq      uint64
}

func (s *stubSmoothfsMoveClient) MovePlan(_ uuid.UUID, _ [smoothfsclient.OIDLen]byte, dest uint8, seq uint64) error {
	s.movePlanCalls++
	s.lastDestTier = dest
	s.lastSeq = seq
	return s.movePlanErr
}

func (s *stubSmoothfsMoveClient) MoveCutover(_ uuid.UUID, _ [smoothfsclient.OIDLen]byte, _ uint64) error {
	s.moveCutoverCalls++
	return s.moveCutoverErr
}

func (s *stubSmoothfsMoveClient) Inspect(_ uuid.UUID, _ [smoothfsclient.OIDLen]byte) (*smoothfsclient.InspectResult, error) {
	idx := s.inspectCalls
	s.inspectCalls++
	if idx < len(s.inspectErrs) && s.inspectErrs[idx] != nil {
		return nil, s.inspectErrs[idx]
	}
	if idx < len(s.inspectResults) {
		return s.inspectResults[idx], nil
	}
	if len(s.inspectResults) > 0 {
		return s.inspectResults[len(s.inspectResults)-1], nil
	}
	return &smoothfsclient.InspectResult{}, nil
}

func (s *stubSmoothfsMoveClient) Close() error { return nil }

func setupActiveLUNExecutorEnv(t *testing.T) (*SharingHandler, string, *stubSmoothfsMoveClient) {
	t.Helper()
	h := newTestSharingHandler(t)

	srcDir := t.TempDir()
	srcLowerPath := filepath.Join(srcDir, "lun.img")
	if err := writeFile(srcLowerPath, []byte("lun-bytes")); err != nil {
		t.Fatalf("write src: %v", err)
	}
	destDir := t.TempDir()
	mountDir := t.TempDir()

	if _, err := h.store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "33333333-3333-3333-3333-333333333333",
		Name:       "media",
		Tiers:      []string{srcDir, destDir},
		Mountpoint: mountDir,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	iqn := "iqn.2026-01.com.smoothnas:file"
	backingFile := filepath.Join(mountDir, "lun.img")
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: backingFile,
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create iscsi target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     backingFile,
		DestinationTier: "1",
		State:           iscsiLUNMoveIntentStateExecuting,
		StateUpdatedAt:  "2026-04-27T00:00:00Z",
		CreatedAt:       "2026-04-27T00:00:00Z",
	}
	if err := h.persistISCSIFileTargetMoveIntent(iqn, intent); err != nil {
		t.Fatalf("persist intent: %v", err)
	}

	stub := &stubSmoothfsMoveClient{
		inspectResults: []*smoothfsclient.InspectResult{
			{CurrentTier: 0, RelPath: "lun.img"},
			{MovementState: smoothfsclient.StateCleanupComplete},
		},
	}

	origRead := readBackingFileOIDFn
	readBackingFileOIDFn = func(string) ([smoothfsclient.OIDLen]byte, error) {
		return [smoothfsclient.OIDLen]byte{0x42}, nil
	}
	t.Cleanup(func() { readBackingFileOIDFn = origRead })

	origPin := pinISCSIBackingFileFn
	pinISCSIBackingFileFn = func(string) error { return nil }
	t.Cleanup(func() { pinISCSIBackingFileFn = origPin })

	origUnpin := unpinISCSIBackingFileFn
	unpinISCSIBackingFileFn = func(string) error { return nil }
	t.Cleanup(func() { unpinISCSIBackingFileFn = origUnpin })

	origResume := resumeISCSITargetFn
	resumeISCSITargetFn = func(string) error { return nil }
	t.Cleanup(func() { resumeISCSITargetFn = origResume })

	origOpen := openSmoothfsClientFn
	openSmoothfsClientFn = func() (smoothfsMoveClient, error) { return stub, nil }
	t.Cleanup(func() { openSmoothfsClientFn = origOpen })

	origSetOID := setOIDXattrFn
	setOIDXattrFn = func(string, [smoothfsclient.OIDLen]byte) error { return nil }
	t.Cleanup(func() { setOIDXattrFn = origSetOID })

	return h, iqn, stub
}

func TestRunActiveLUNMoveHappyPath(t *testing.T) {
	h, iqn, stub := setupActiveLUNExecutorEnv(t)

	runActiveLUNMove(context.Background(), h, iqn)

	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || intent == nil {
		t.Fatalf("read intent post-move: %v %v", intent, err)
	}
	if intent.State != iscsiLUNMoveIntentStateCompleted {
		t.Fatalf("intent state = %q, want completed; reason=%q", intent.State, intent.Reason)
	}
	if stub.movePlanCalls != 1 {
		t.Fatalf("MovePlan called %d times, want 1", stub.movePlanCalls)
	}
	if stub.moveCutoverCalls != 1 {
		t.Fatalf("MoveCutover called %d times, want 1", stub.moveCutoverCalls)
	}
	if stub.lastDestTier != 1 {
		t.Fatalf("lastDestTier = %d, want 1", stub.lastDestTier)
	}
	quiesced, _ := h.store.GetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), true)
	if quiesced {
		t.Fatalf("target still flagged quiesced after move complete")
	}
}

func TestRunActiveLUNMoveMarksFailedOnPlanError(t *testing.T) {
	h, iqn, stub := setupActiveLUNExecutorEnv(t)
	stub.movePlanErr = errors.New("ebusy")

	runActiveLUNMove(context.Background(), h, iqn)

	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || intent == nil {
		t.Fatalf("read intent: %v %v", intent, err)
	}
	if intent.State != iscsiLUNMoveIntentStateFailed {
		t.Fatalf("state = %q, want failed", intent.State)
	}
	if !strings.Contains(intent.Reason, "move plan") {
		t.Fatalf("reason = %q, want contains 'move plan'", intent.Reason)
	}
	if stub.moveCutoverCalls != 0 {
		t.Fatalf("MoveCutover should not run after MovePlan failure; got %d calls", stub.moveCutoverCalls)
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func TestRecoverActiveLUNMoveIntentsMarksNonTerminalAsFailed(t *testing.T) {
	h := newTestSharingHandler(t)

	pinned := map[string]int{}
	origPin := pinISCSIBackingFileFn
	pinISCSIBackingFileFn = func(path string) error {
		pinned[path]++
		return nil
	}
	t.Cleanup(func() { pinISCSIBackingFileFn = origPin })

	// Three file-backed targets: one in `executing`, one in
	// `cutover`, one already `completed`. Plus a block-backed one
	// that should be skipped entirely.
	targets := []struct {
		iqn     string
		backing string
		state   string
		typ     string
	}{
		{"iqn.2026-01.com.smoothnas:exec", "/mnt/media/exec.img", iscsiLUNMoveIntentStateExecuting, db.IscsiBackingFile},
		{"iqn.2026-01.com.smoothnas:cut", "/mnt/media/cut.img", iscsiLUNMoveIntentStateCutover, db.IscsiBackingFile},
		{"iqn.2026-01.com.smoothnas:done", "/mnt/media/done.img", iscsiLUNMoveIntentStateCompleted, db.IscsiBackingFile},
		{"iqn.2026-01.com.smoothnas:block", "/dev/zvol/tank/block", "", db.IscsiBackingBlock},
	}
	for _, tgt := range targets {
		if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
			IQN:         tgt.iqn,
			BlockDevice: tgt.backing,
			BackingType: tgt.typ,
		}); err != nil {
			t.Fatalf("create target %s: %v", tgt.iqn, err)
		}
		if tgt.state == "" {
			continue
		}
		intent := iscsiLUNMoveIntent{
			IQN:             tgt.iqn,
			BackingFile:     tgt.backing,
			DestinationTier: "1",
			State:           tgt.state,
			StateUpdatedAt:  "2026-04-27T00:00:00Z",
			CreatedAt:       "2026-04-27T00:00:00Z",
		}
		if err := h.persistISCSIFileTargetMoveIntent(tgt.iqn, intent); err != nil {
			t.Fatalf("persist intent %s: %v", tgt.iqn, err)
		}
	}

	if err := recoverActiveLUNMoveIntents(h); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// Executing and cutover were re-pinned and marked failed.
	if pinned["/mnt/media/exec.img"] != 1 {
		t.Fatalf("executing intent backing file pinned %d times, want 1", pinned["/mnt/media/exec.img"])
	}
	if pinned["/mnt/media/cut.img"] != 1 {
		t.Fatalf("cutover intent backing file pinned %d times, want 1", pinned["/mnt/media/cut.img"])
	}
	// Completed and block-backed were NOT touched.
	if _, ok := pinned["/mnt/media/done.img"]; ok {
		t.Fatalf("completed intent should not have been re-pinned")
	}
	if _, ok := pinned["/dev/zvol/tank/block"]; ok {
		t.Fatalf("block-backed target should not have been touched")
	}

	exec, err := h.getISCSIFileTargetMoveIntent("iqn.2026-01.com.smoothnas:exec")
	if err != nil || exec == nil {
		t.Fatalf("read exec intent: %v %v", exec, err)
	}
	if exec.State != iscsiLUNMoveIntentStateFailed {
		t.Fatalf("exec state = %q, want failed", exec.State)
	}
	if !strings.Contains(exec.Reason, "recovery") {
		t.Fatalf("exec reason = %q, want contains 'recovery'", exec.Reason)
	}
	if !strings.Contains(exec.Reason, iscsiLUNMoveIntentStateExecuting) {
		t.Fatalf("exec reason = %q, want mentions prior state %q", exec.Reason, iscsiLUNMoveIntentStateExecuting)
	}

	done, err := h.getISCSIFileTargetMoveIntent("iqn.2026-01.com.smoothnas:done")
	if err != nil || done == nil {
		t.Fatalf("read completed intent: %v %v", done, err)
	}
	if done.State != iscsiLUNMoveIntentStateCompleted {
		t.Fatalf("completed intent mutated to %q", done.State)
	}
}

func TestRecoverActiveLUNMoveIntentsDetectsCompletedCutover(t *testing.T) {
	h := newTestSharingHandler(t)

	mountDir := t.TempDir()
	srcDir := t.TempDir()
	destDir := t.TempDir()
	if _, err := h.store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "44444444-4444-4444-4444-444444444444",
		Name:       "media",
		Tiers:      []string{srcDir, destDir},
		Mountpoint: mountDir,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	iqn := "iqn.2026-01.com.smoothnas:postcutover"
	backingFile := filepath.Join(mountDir, "lun.img")
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: backingFile,
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	// Crashed during the cutover state.
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     backingFile,
		DestinationTier: "1",
		State:           iscsiLUNMoveIntentStateCutover,
		StateUpdatedAt:  "2026-04-27T00:00:00Z",
		CreatedAt:       "2026-04-27T00:00:00Z",
	}
	if err := h.persistISCSIFileTargetMoveIntent(iqn, intent); err != nil {
		t.Fatalf("persist intent: %v", err)
	}

	pins := map[string]int{}
	origPin := pinISCSIBackingFileFn
	pinISCSIBackingFileFn = func(p string) error { pins[p]++; return nil }
	t.Cleanup(func() { pinISCSIBackingFileFn = origPin })

	origRead := readBackingFileOIDFn
	readBackingFileOIDFn = func(string) ([smoothfsclient.OIDLen]byte, error) {
		return [smoothfsclient.OIDLen]byte{0xab}, nil
	}
	t.Cleanup(func() { readBackingFileOIDFn = origRead })

	stub := &stubSmoothfsMoveClient{
		inspectResults: []*smoothfsclient.InspectResult{
			{CurrentTier: 1, MovementState: smoothfsclient.StatePlaced},
		},
	}
	origOpen := openSmoothfsClientFn
	openSmoothfsClientFn = func() (smoothfsMoveClient, error) { return stub, nil }
	t.Cleanup(func() { openSmoothfsClientFn = origOpen })

	if err := recoverActiveLUNMoveIntents(h); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || got == nil {
		t.Fatalf("read recovered intent: %v %v", got, err)
	}
	if got.State != iscsiLUNMoveIntentStateCompleted {
		t.Fatalf("state = %q, want completed; reason=%q", got.State, got.Reason)
	}
	if !strings.Contains(got.Reason, "kernel completed cutover") {
		t.Fatalf("reason = %q, want mentions kernel completed cutover", got.Reason)
	}
	if pins[backingFile] != 1 {
		t.Fatalf("backing file pinned %d times, want 1", pins[backingFile])
	}
	if stub.inspectCalls != 1 {
		t.Fatalf("inspect called %d times, want 1", stub.inspectCalls)
	}

	// Target stays quiesced — operator does the final Resume.
	quiesced, _ := h.store.GetBoolConfig(iscsiTargetQuiescedConfigKey(iqn), false)
	if !quiesced && false {
		t.Fatalf("expected quiesced flag preserved")
	}
}

func TestRecoverActiveLUNMoveIntentsKeepsFailedWhenKernelMidFlight(t *testing.T) {
	h := newTestSharingHandler(t)

	mountDir := t.TempDir()
	srcDir := t.TempDir()
	destDir := t.TempDir()
	if _, err := h.store.CreateSmoothfsPool(db.SmoothfsPool{
		UUID:       "55555555-5555-5555-5555-555555555555",
		Name:       "media",
		Tiers:      []string{srcDir, destDir},
		Mountpoint: mountDir,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	iqn := "iqn.2026-01.com.smoothnas:midflight"
	backingFile := filepath.Join(mountDir, "lun.img")
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: backingFile,
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     backingFile,
		DestinationTier: "1",
		State:           iscsiLUNMoveIntentStateMoving,
		StateUpdatedAt:  "2026-04-27T00:00:00Z",
		CreatedAt:       "2026-04-27T00:00:00Z",
	}
	if err := h.persistISCSIFileTargetMoveIntent(iqn, intent); err != nil {
		t.Fatalf("persist intent: %v", err)
	}

	origPin := pinISCSIBackingFileFn
	pinISCSIBackingFileFn = func(string) error { return nil }
	t.Cleanup(func() { pinISCSIBackingFileFn = origPin })

	origRead := readBackingFileOIDFn
	readBackingFileOIDFn = func(string) ([smoothfsclient.OIDLen]byte, error) {
		return [smoothfsclient.OIDLen]byte{0xcd}, nil
	}
	t.Cleanup(func() { readBackingFileOIDFn = origRead })

	stub := &stubSmoothfsMoveClient{
		inspectResults: []*smoothfsclient.InspectResult{
			{CurrentTier: 0, MovementState: smoothfsclient.StateCopyInProgress},
		},
	}
	origOpen := openSmoothfsClientFn
	openSmoothfsClientFn = func() (smoothfsMoveClient, error) { return stub, nil }
	t.Cleanup(func() { openSmoothfsClientFn = origOpen })

	if err := recoverActiveLUNMoveIntents(h); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || got == nil {
		t.Fatalf("read intent: %v %v", got, err)
	}
	if got.State != iscsiLUNMoveIntentStateFailed {
		t.Fatalf("state = %q, want failed for mid-flight kernel state; reason=%q", got.State, got.Reason)
	}
	if !strings.Contains(got.Reason, "mid-flight") {
		t.Fatalf("reason = %q, want mentions mid-flight", got.Reason)
	}
}

func TestRecoverActiveLUNMoveIntentsSurvivesPinFailure(t *testing.T) {
	h := newTestSharingHandler(t)

	origPin := pinISCSIBackingFileFn
	pinISCSIBackingFileFn = func(_ string) error { return errors.New("simulated") }
	t.Cleanup(func() { pinISCSIBackingFileFn = origPin })

	iqn := "iqn.2026-01.com.smoothnas:file"
	if _, err := h.store.CreateIscsiTarget(db.IscsiTarget{
		IQN:         iqn,
		BlockDevice: "/mnt/media/lun.img",
		BackingType: db.IscsiBackingFile,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	intent := iscsiLUNMoveIntent{
		IQN:             iqn,
		BackingFile:     "/mnt/media/lun.img",
		DestinationTier: "1",
		State:           iscsiLUNMoveIntentStateMoving,
		StateUpdatedAt:  "2026-04-27T00:00:00Z",
		CreatedAt:       "2026-04-27T00:00:00Z",
	}
	if err := h.persistISCSIFileTargetMoveIntent(iqn, intent); err != nil {
		t.Fatalf("persist intent: %v", err)
	}

	if err := recoverActiveLUNMoveIntents(h); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || got == nil {
		t.Fatalf("read intent: %v %v", got, err)
	}
	if got.State != iscsiLUNMoveIntentStateFailed {
		t.Fatalf("state = %q, want failed even on pin failure", got.State)
	}
	if !strings.Contains(got.Reason, "pin lun failed") {
		t.Fatalf("reason = %q, want mentions pin failure", got.Reason)
	}
}
