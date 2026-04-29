# Proposal: smoothfs Active-LUN Movement Phase 8c — Startup Recovery

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-08-journaled-execution.md`](smoothfs-active-lun-movement-08-journaled-execution.md)
**Predecessor:** [`smoothfs-active-lun-movement-08b-kernel-wiring.md`](smoothfs-active-lun-movement-08b-kernel-wiring.md)

---

## Context

Phase 8b runs the active-LUN move executor in a goroutine after
journaling `executing`. If tierd is killed mid-execution, intents
can be left in any of `executing`, `unpinned`, `moving`, `cutover`,
`repinning` with no goroutine alive to drive them forward. The
safety AC for Phase 8 requires that a tierd or host crash never
leaves a live LIO target on an unpinned backing file.

This phase adds the startup recovery sweep that satisfies the safety
AC by re-pinning every backing file with a non-terminal intent and
parking the intent in `failed` so an operator must consciously
decide whether to retry.

## Scope

1. Add `recoverActiveLUNMoveIntents` that runs once at tierd start
   (from `ReconcileSharingConfig`).
2. For each file-backed iSCSI target with an intent in any of
   `executing`, `unpinned`, `moving`, `cutover`, `repinning`:
   - Best-effort `iscsi.PinLUN(intent.BackingFile)` (idempotent on
     smoothfs; setxattr "1" on an already-pinned inode is a no-op,
     so this is safe regardless of which step was interrupted).
   - Mark the intent `failed` with reason
     `"recovery: tierd restarted in state %q; lun re-pinned, abort to retry"`,
     or `"... pin lun failed: %v"` if the re-pin call errored.
   - Leave the iSCSI target quiesced. Resume requires deliberate
     operator action via `abort` + re-execute (or `clear` to drop
     the intent entirely).
3. Per-target failures (one corrupt intent, one missing backing
   file) do not fail the whole reconcile.

## Non-goals

- **Forward recovery is out of scope.** This phase does not try to
  drive the kernel state machine forward from a partial state
  (e.g., resume a half-finished copy or detect that cutover already
  completed). That would require coordinating tierd journal state,
  kernel `Inspect` results, and LIO state, with non-resumable
  cutover semantics in the middle. Forward recovery can land later
  if operator pain warrants it; the safety AC does not require it.

## Acceptance Criteria

- [x] `ReconcileSharingConfig` runs the recovery sweep at startup.
- [x] Intents in `executing`, `unpinned`, `moving`, `cutover`, or
      `repinning` are re-pinned and transitioned to `failed`.
- [x] Intents in terminal states (`planned`, `completed`, `failed`)
      are left untouched.
- [x] Block-backed iSCSI targets are skipped.
- [x] A pin failure on one intent records the failure in the
      reason but does not abort the whole sweep, so subsequent
      intents are still recovered.
- [x] An operator can `abort` (drops back to `planned`) and
      re-execute, or `clear` to cancel, after recovery has marked
      an intent `failed`.

## Pending in 8d / 08

- 8d: UI status badge, abort button, and a `tierd-cli` active-LUN
  command set.
- AC #5 of 08: named VFS reviewer sign-off (process gate).
