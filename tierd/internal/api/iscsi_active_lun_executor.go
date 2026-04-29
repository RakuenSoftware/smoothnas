package api

// Phase 8b — active-LUN movement executor.
//
// Drives a journaled iscsiLUNMoveIntent from `executing` through
// `unpinned → moving → cutover → repinning → completed`, talking to
// the smoothfs kernel module via netlink at the move/cutover steps
// and to LIO via targetcli for resume. PIN_LUN is cleared exactly
// for the bounded movement window: dropped before MovePlan, restored
// before ResumeTarget.
//
// All netlink-touching helpers are package-level function variables
// so tests can replace them with mocks. The async kickoff
// (`runActiveLUNMoveImpl`) is similarly overridable; tests that only
// exercise the journal swap it for a no-op.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	smoothfsclient "github.com/RakuenSoftware/smoothfs"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/iscsi"
)

const (
	smoothfsOIDXattr      = "trusted.smoothfs.oid"
	activeLUNCopyChunkLen = 1 << 20 // 1 MiB
	activeLUNMovePollInterval = 250 * time.Millisecond
	activeLUNCutoverTimeout   = 30 * time.Minute
)

// runActiveLUNMoveImpl is overridable for tests. The HTTP handler
// kicks this off in a goroutine after journaling `executing`. The
// default is `runActiveLUNMove`.
var runActiveLUNMoveImpl = runActiveLUNMove

// readBackingFileOIDFn / pinISCSIBackingFileFn / unpinISCSIBackingFileFn /
// resumeISCSITargetFn / openSmoothfsClientFn are seams for tests.
var (
	readBackingFileOIDFn = readBackingFileOIDXattr
	pinISCSIBackingFileFn = iscsi.PinLUN
	unpinISCSIBackingFileFn = iscsi.UnpinLUN
	resumeISCSITargetFn = iscsi.ResumeTarget
	openSmoothfsClientFn = func() (smoothfsMoveClient, error) {
		c, err := smoothfsclient.Open()
		if err != nil {
			return nil, err
		}
		return realSmoothfsMoveClient{c: c}, nil
	}
	setOIDXattrFn = func(path string, oid [smoothfsclient.OIDLen]byte) error {
		return unix.Setxattr(path, smoothfsOIDXattr, oid[:], 0)
	}
)

// smoothfsMoveClient is the subset of smoothfsclient.Client the
// executor uses. Defining it here lets tests provide a stub instead
// of opening a real netlink socket.
type smoothfsMoveClient interface {
	MovePlan(poolUUID uuid.UUID, oid [smoothfsclient.OIDLen]byte, destTier uint8, seq uint64) error
	MoveCutover(poolUUID uuid.UUID, oid [smoothfsclient.OIDLen]byte, seq uint64) error
	Inspect(poolUUID uuid.UUID, oid [smoothfsclient.OIDLen]byte) (*smoothfsclient.InspectResult, error)
	Close() error
}

type realSmoothfsMoveClient struct{ c *smoothfsclient.Client }

func (r realSmoothfsMoveClient) MovePlan(p uuid.UUID, oid [smoothfsclient.OIDLen]byte, dest uint8, seq uint64) error {
	return r.c.MovePlan(p, oid, dest, seq)
}
func (r realSmoothfsMoveClient) MoveCutover(p uuid.UUID, oid [smoothfsclient.OIDLen]byte, seq uint64) error {
	return r.c.MoveCutover(p, oid, seq)
}
func (r realSmoothfsMoveClient) Inspect(p uuid.UUID, oid [smoothfsclient.OIDLen]byte) (*smoothfsclient.InspectResult, error) {
	return r.c.Inspect(p, oid)
}
func (r realSmoothfsMoveClient) Close() error { return r.c.Close() }

func readBackingFileOIDXattr(path string) ([smoothfsclient.OIDLen]byte, error) {
	var oid [smoothfsclient.OIDLen]byte
	buf := make([]byte, smoothfsclient.OIDLen)
	n, err := unix.Getxattr(path, smoothfsOIDXattr, buf)
	if err != nil {
		return oid, fmt.Errorf("read %s on %s: %w", smoothfsOIDXattr, path, err)
	}
	if n != smoothfsclient.OIDLen {
		return oid, fmt.Errorf("%s on %s has length %d, want %d",
			smoothfsOIDXattr, path, n, smoothfsclient.OIDLen)
	}
	copy(oid[:], buf[:n])
	return oid, nil
}

// findSmoothfsPoolForBackingFile returns the smoothfs pool whose
// mountpoint is the longest prefix of `path`. This matches by string
// prefix, which is sufficient because pool mountpoints are absolute
// paths reserved for smoothfs by the mount unit. Returns ErrNotFound
// if no pool covers the path.
func findSmoothfsPoolForBackingFile(store *db.Store, path string) (*db.SmoothfsPool, error) {
	pools, err := store.ListSmoothfsPools()
	if err != nil {
		return nil, err
	}
	var match *db.SmoothfsPool
	for i := range pools {
		mp := strings.TrimRight(pools[i].Mountpoint, "/")
		if mp == "" {
			continue
		}
		if path == mp || strings.HasPrefix(path, mp+"/") {
			if match == nil || len(mp) > len(match.Mountpoint) {
				match = &pools[i]
			}
		}
	}
	if match == nil {
		return nil, db.ErrNotFound
	}
	return match, nil
}

// resolveDestinationTier turns the operator-supplied
// intent.DestinationTier string into a u8 tier index inside
// `pool.Tiers`. Three forms are accepted, in order of precedence:
//
//  1. A numeric string ("0", "1", ...) bounded by len(pool.Tiers).
//  2. An exact match against one of pool.Tiers (the absolute lower
//     mount path).
//  3. A tier_targets.Name match — uses the row's Rank as the index.
//
// Returns an error mentioning the input verbatim so the operator can
// fix the recorded intent without guessing.
func resolveDestinationTier(store *db.Store, pool *db.SmoothfsPool, dest string) (uint8, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return 0, fmt.Errorf("destination_tier is empty")
	}
	if n, err := strconv.Atoi(dest); err == nil {
		if n < 0 || n >= len(pool.Tiers) {
			return 0, fmt.Errorf("destination_tier %q out of range for pool %q (ntiers=%d)",
				dest, pool.Name, len(pool.Tiers))
		}
		return uint8(n), nil
	}
	for i, p := range pool.Tiers {
		if p == dest {
			return uint8(i), nil
		}
	}
	if store != nil {
		slots, err := store.ListTierSlots(pool.Name)
		if err == nil {
			for _, s := range slots {
				if s.Name == dest {
					// `tiers.rank` is 1-based per ValidateTierDefinitions;
					// smoothfs tier indices are 0-based. Convert.
					idx := s.Rank - 1
					if idx < 0 || idx >= len(pool.Tiers) {
						return 0, fmt.Errorf("destination_tier %q resolves to slot rank %d, out of range for pool %q (ntiers=%d)",
							dest, s.Rank, pool.Name, len(pool.Tiers))
					}
					return uint8(idx), nil
				}
			}
		}
	}
	return 0, fmt.Errorf("destination_tier %q not recognized; expected a 0-based index, an absolute tier path, or a tier slot name on pool %q",
		dest, pool.Name)
}

// activeLUNMoveContext bundles everything the executor needs to drive
// the kernel state machine for one in-flight intent. It is built once
// at the start of runActiveLUNMove and passed by value through each
// state-transition step.
type activeLUNMoveContext struct {
	iqn        string
	intent     iscsiLUNMoveIntent
	pool       db.SmoothfsPool
	poolUUID   uuid.UUID
	oid        [smoothfsclient.OIDLen]byte
	destTier   uint8
	seq        uint64
	srcLowerPath  string
	destLowerPath string
}

// runActiveLUNMove is the default executor. It blocks until the move
// is complete (or has failed), persisting state at each transition.
// The HTTP handler runs this in a goroutine after journaling
// `executing`; ctx is wired through so a future cancellation surface
// can interrupt the polling loop.
func runActiveLUNMove(ctx context.Context, h *SharingHandler, iqn string) {
	mc, err := h.prepareActiveLUNMove(iqn)
	if err != nil {
		h.markActiveLUNMoveFailed(iqn, fmt.Sprintf("prepare: %v", err))
		return
	}
	if err := h.driveActiveLUNMove(ctx, mc); err != nil {
		// driveActiveLUNMove already journals `failed` on its own
		// errors so it can record the most-specific reason. Logging
		// here is purely diagnostic.
		_ = err
	}
}

func (h *SharingHandler) prepareActiveLUNMove(iqn string) (activeLUNMoveContext, error) {
	var mc activeLUNMoveContext
	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil {
		return mc, fmt.Errorf("read intent: %w", err)
	}
	if intent == nil {
		return mc, errors.New("intent missing")
	}
	if intent.State != iscsiLUNMoveIntentStateExecuting {
		return mc, fmt.Errorf("intent is in state %q, want executing", intent.State)
	}
	mc.iqn = iqn
	mc.intent = *intent

	pool, err := findSmoothfsPoolForBackingFile(h.store, intent.BackingFile)
	if err != nil {
		return mc, fmt.Errorf("locate smoothfs pool for %s: %w", intent.BackingFile, err)
	}
	mc.pool = *pool
	mc.poolUUID, err = uuid.Parse(pool.UUID)
	if err != nil {
		return mc, fmt.Errorf("parse pool uuid %q: %w", pool.UUID, err)
	}

	mc.oid, err = readBackingFileOIDFn(intent.BackingFile)
	if err != nil {
		return mc, fmt.Errorf("read backing file oid: %w", err)
	}

	mc.destTier, err = resolveDestinationTier(h.store, pool, intent.DestinationTier)
	if err != nil {
		return mc, err
	}

	relPath, err := filepath.Rel(pool.Mountpoint, intent.BackingFile)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return mc, fmt.Errorf("backing file %s is not under pool mountpoint %s", intent.BackingFile, pool.Mountpoint)
	}
	if int(mc.destTier) >= len(pool.Tiers) {
		return mc, fmt.Errorf("destination tier index %d out of range for pool %s", mc.destTier, pool.Name)
	}
	mc.destLowerPath = filepath.Join(pool.Tiers[mc.destTier], relPath)

	// seq is monotonic per pool. UnixNano is unique within a tierd
	// instance and the kernel just needs uniqueness against any prior
	// in-flight move on the same OID, which is enforced separately
	// via the intent state machine.
	mc.seq = uint64(time.Now().UnixNano())
	return mc, nil
}

func (h *SharingHandler) driveActiveLUNMove(ctx context.Context, mc activeLUNMoveContext) error {
	client, err := openSmoothfsClientFn()
	if err != nil {
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("open smoothfs netlink: %v", err))
	}
	defer client.Close()

	if err := unpinISCSIBackingFileFn(mc.intent.BackingFile); err != nil {
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("unpin lun: %v", err))
	}
	mc.intent = h.transitionActiveLUNMove(mc.iqn, iscsiLUNMoveIntentStateUnpinned, "lun unpinned")

	// Capture the source lower path now so we can re-pin and copy
	// from it even after MovePlan flips the kernel state. We resolve
	// via Inspect post-plan rather than guessing at the lower path
	// from the smoothfs path because any prior cutover may have
	// already moved the inode away from pool.Tiers[0].
	if err := client.MovePlan(mc.poolUUID, mc.oid, mc.destTier, mc.seq); err != nil {
		// Best-effort re-pin so the LUN doesn't sit unpinned with
		// no journaled forward path.
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("move plan: %v", err))
	}

	insp, err := client.Inspect(mc.poolUUID, mc.oid)
	if err != nil {
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("inspect after plan: %v", err))
	}
	if int(insp.CurrentTier) >= len(mc.pool.Tiers) {
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn,
			fmt.Sprintf("inspect reports current_tier=%d out of range", insp.CurrentTier))
	}
	mc.srcLowerPath = filepath.Join(mc.pool.Tiers[insp.CurrentTier], insp.RelPath)
	mc.intent = h.transitionActiveLUNMove(mc.iqn, iscsiLUNMoveIntentStateMoving,
		fmt.Sprintf("plan accepted, copying %s -> %s", mc.srcLowerPath, mc.destLowerPath))

	if err := copyLowerFileForActiveLUNMove(mc.srcLowerPath, mc.destLowerPath, mc.oid); err != nil {
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("copy lower file: %v", err))
	}

	if err := client.MoveCutover(mc.poolUUID, mc.oid, mc.seq); err != nil {
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("move cutover: %v", err))
	}
	mc.intent = h.transitionActiveLUNMove(mc.iqn, iscsiLUNMoveIntentStateCutover, "kernel cutover committed")

	if err := waitActiveLUNMoveCleanup(ctx, client, mc); err != nil {
		_ = pinISCSIBackingFileFn(mc.intent.BackingFile)
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("wait cleanup: %v", err))
	}

	if err := pinISCSIBackingFileFn(mc.intent.BackingFile); err != nil {
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("repin lun: %v", err))
	}
	mc.intent = h.transitionActiveLUNMove(mc.iqn, iscsiLUNMoveIntentStateRepinning, "lun re-pinned on destination")

	if err := resumeISCSITargetFn(mc.iqn); err != nil {
		// LUN is pinned and on the new tier; the operator can resume
		// manually. Record the state but don't roll back.
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("resume target: %v", err))
	}
	if err := h.store.SetBoolConfig(iscsiTargetQuiescedConfigKey(mc.iqn), false); err != nil {
		return h.markActiveLUNMoveFailedReturning(mc.iqn, fmt.Sprintf("clear quiesced flag: %v", err))
	}
	h.transitionActiveLUNMove(mc.iqn, iscsiLUNMoveIntentStateCompleted, "active-lun move complete")
	return nil
}

func waitActiveLUNMoveCleanup(ctx context.Context, client smoothfsMoveClient, mc activeLUNMoveContext) error {
	deadline := time.Now().Add(activeLUNCutoverTimeout)
	for {
		insp, err := client.Inspect(mc.poolUUID, mc.oid)
		if err != nil {
			return err
		}
		switch insp.MovementState {
		case smoothfsclient.StateCleanupComplete, smoothfsclient.StatePlaced:
			return nil
		case smoothfsclient.StateFailed, smoothfsclient.StateStale:
			return fmt.Errorf("kernel reports movement state %q", insp.MovementState)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cleanup did not complete within %s; last state %q",
				activeLUNCutoverTimeout, insp.MovementState)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(activeLUNMovePollInterval):
		}
	}
}

// copyLowerFileForActiveLUNMove copies bytes and sets the smoothfs
// OID xattr on the destination so the kernel cutover can resolve the
// dest dentry. The PIN_LUN xattr is intentionally NOT propagated;
// the executor reinstalls it through smoothfs after cutover.
func copyLowerFileForActiveLUNMove(src, dst string, oid [smoothfsclient.OIDLen]byte) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer srcF.Close()
	st, err := srcF.Stat()
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir dst parent: %w", err)
	}
	dstF, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(dstF, srcF); err != nil {
		dstF.Close()
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := dstF.Sync(); err != nil {
		dstF.Close()
		return fmt.Errorf("fsync dst: %w", err)
	}
	if err := dstF.Close(); err != nil {
		return fmt.Errorf("close dst: %w", err)
	}
	if err := setOIDXattrFn(dst, oid); err != nil {
		return fmt.Errorf("set oid xattr on dst: %w", err)
	}
	return nil
}

// transitionActiveLUNMove journals a new state on the intent. On
// store error it logs and returns the intent unchanged — the
// executor's outer error path will eventually catch a stuck state on
// the next operator-driven action.
func (h *SharingHandler) transitionActiveLUNMove(iqn, state, reason string) iscsiLUNMoveIntent {
	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || intent == nil {
		return iscsiLUNMoveIntent{}
	}
	intent.State = state
	intent.StateUpdatedAt = time.Now().UTC().Format(time.RFC3339)
	intent.Reason = reason
	if err := h.persistISCSIFileTargetMoveIntent(iqn, *intent); err != nil {
		return *intent
	}
	return *intent
}

func (h *SharingHandler) markActiveLUNMoveFailed(iqn, reason string) {
	intent, err := h.getISCSIFileTargetMoveIntent(iqn)
	if err != nil || intent == nil {
		return
	}
	intent.State = iscsiLUNMoveIntentStateFailed
	intent.StateUpdatedAt = time.Now().UTC().Format(time.RFC3339)
	intent.Reason = reason
	_ = h.persistISCSIFileTargetMoveIntent(iqn, *intent)
}

func (h *SharingHandler) markActiveLUNMoveFailedReturning(iqn, reason string) error {
	h.markActiveLUNMoveFailed(iqn, reason)
	return errors.New(reason)
}

// activeLUNMoveSeqFromTime is exposed for tests that want a
// deterministic seq. It is not used internally; the production path
// reads time.Now().UnixNano() directly to avoid an extra allocation
// on the hot path.
func activeLUNMoveSeqFromTime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	// Use the unsigned reading of the monotonic ns count.
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(t.UnixNano()))
	return binary.LittleEndian.Uint64(b[:])
}

// recoverActiveLUNMoveIntents is the active-LUN crash-recovery sweep.
// Called once from ReconcileSharingConfig at tierd startup. For
// every file-backed iSCSI target with an intent stuck in a
// non-terminal state, we:
//
//  1. Best-effort re-pin the backing file via PIN_LUN. The pin call
//     is idempotent on smoothfs (setxattr "1" on an already-pinned
//     inode is a no-op), so this is safe regardless of which step
//     of the executor was interrupted; mid-cutover the kernel may
//     have already swapped the lower path, in which case the pin
//     lands on the new lower (still pinned, still safe).
//  2. Probe smoothfs `Inspect` to see whether the kernel state
//     machine actually finished cutover before tierd died. If
//     `CurrentTier == DestinationTier` and the movement state is
//     settled (`placed` or `cleanup_complete`), the move really did
//     complete — mark the intent `completed` instead of `failed`
//     so the operator only has to Resume, not abort + re-execute
//     a move that already happened.
//  3. Otherwise mark the intent `failed` with a recovery-specific
//     reason that names the prior journaled state so the operator
//     can see in the UI / API exactly which targets are stuck.
//  4. Leave the target quiesced in either case. The safety AC says
//     a crashed move must never leave a live LIO target on an
//     unpinned backing file; we satisfy that by not auto-resuming.
//     The operator runs `abort` (drops back to `planned`) and
//     re-executes (after deciding whether the destination tier
//     still makes sense), `clear` to cancel, or Resume to bring a
//     `completed` recovered LUN back online.
//
// We still do NOT drive the kernel state machine forward from a
// partial state — only the post-cutover case is fast-forwarded by
// the Inspect probe. Mid-flight states (copy_in_progress,
// cutover_in_progress, switched, cleanup_in_progress) become
// `failed`; the operator's abort + retry path handles those.
func recoverActiveLUNMoveIntents(h *SharingHandler) error {
	targets, err := h.store.ListIscsiTargets()
	if err != nil {
		return fmt.Errorf("list iscsi targets: %w", err)
	}
	for _, t := range targets {
		if t.BackingType != db.IscsiBackingFile {
			continue
		}
		intent, err := h.getISCSIFileTargetMoveIntent(t.IQN)
		if err != nil {
			// A single corrupted intent shouldn't fail the whole
			// startup reconcile. Log via the failed-state journal
			// so the operator notices on next list.
			fmt.Fprintf(os.Stderr, "tierd: read iscsi move intent for %s: %v\n", t.IQN, err)
			continue
		}
		if intent == nil {
			continue
		}
		if !iscsiLUNMoveIntentNonTerminal(intent.State) {
			continue
		}

		priorState := intent.State
		pinErr := pinISCSIBackingFileFn(intent.BackingFile)

		newState, reason := decideRecoveredActiveLUNState(h, intent, priorState, pinErr)
		intent.State = newState
		intent.StateUpdatedAt = time.Now().UTC().Format(time.RFC3339)
		intent.Reason = reason
		if err := h.persistISCSIFileTargetMoveIntent(t.IQN, *intent); err != nil {
			fmt.Fprintf(os.Stderr, "tierd: persist recovered intent for %s: %v\n", t.IQN, err)
			continue
		}
	}
	return nil
}

// decideRecoveredActiveLUNState picks the terminal state for a
// crash-recovered intent. If the kernel reports the move already
// finished cutover (current_tier == destination, state placed or
// cleanup_complete), the intent is marked `completed` so the
// operator just needs to Resume the target. Any kernel error,
// state mismatch, or unknown destination keeps the safety-first
// `failed` outcome of the original Phase 8c sweep.
//
// pinErr is the result of the best-effort pre-Inspect re-pin; if it
// failed we still try to detect a completed move (the pin failure
// becomes part of the recorded reason).
func decideRecoveredActiveLUNState(h *SharingHandler, intent *iscsiLUNMoveIntent,
	priorState string, pinErr error) (string, string) {

	failedReason := func(detail string) string {
		base := fmt.Sprintf("recovery: tierd restarted in state %q; lun re-pinned, abort to retry",
			priorState)
		if pinErr != nil {
			base = fmt.Sprintf("recovery: tierd restarted in state %q; pin lun failed: %v",
				priorState, pinErr)
		}
		if detail != "" {
			base += "; " + detail
		}
		return base
	}

	pool, err := findSmoothfsPoolForBackingFile(h.store, intent.BackingFile)
	if err != nil {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("locate pool: %v", err))
	}
	poolUUID, err := uuid.Parse(pool.UUID)
	if err != nil {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("parse pool uuid: %v", err))
	}
	destTier, err := resolveDestinationTier(h.store, pool, intent.DestinationTier)
	if err != nil {
		return iscsiLUNMoveIntentStateFailed, failedReason(err.Error())
	}
	oid, err := readBackingFileOIDFn(intent.BackingFile)
	if err != nil {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("read oid xattr: %v", err))
	}

	client, err := openSmoothfsClientFn()
	if err != nil {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("open smoothfs netlink: %v", err))
	}
	defer client.Close()

	insp, err := client.Inspect(poolUUID, oid)
	if err != nil {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("inspect: %v", err))
	}

	switch insp.MovementState {
	case smoothfsclient.StatePlaced, smoothfsclient.StateCleanupComplete:
	default:
		// Mid-flight kernel states (plan_accepted, copy_*, cutover,
		// switched, cleanup_in_progress, failed, stale): keep the
		// safety-first `failed` outcome.
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("kernel state %q is mid-flight", insp.MovementState))
	}
	if insp.CurrentTier != destTier {
		return iscsiLUNMoveIntentStateFailed,
			failedReason(fmt.Sprintf("kernel current_tier=%d does not match destination tier %d",
				insp.CurrentTier, destTier))
	}

	// Move actually finished kernel-side before tierd died. Pin
	// already landed on the destination via the idempotent re-pin
	// at the start of recovery. Operator just needs to Resume.
	completedReason := "recovery: kernel completed cutover before tierd restart; resume to bring online"
	if pinErr != nil {
		completedReason = fmt.Sprintf("recovery: kernel completed cutover before tierd restart; pin lun failed: %v",
			pinErr)
	}
	return iscsiLUNMoveIntentStateCompleted, completedReason
}
