# Proposal: smoothfs Active-LUN Movement Phase 8b — Kernel Wiring

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-08-journaled-execution.md`](smoothfs-active-lun-movement-08-journaled-execution.md)
**Predecessor:** [`smoothfs-active-lun-movement-08a-journaled-state.md`](smoothfs-active-lun-movement-08a-journaled-state.md)

---

## Context

Phase 8a journaled the `planned → executing` transition but stopped
there with a stub reason. This phase extends the journal with the
forward states `unpinned`, `moving`, `cutover`, `repinning`,
`completed`, plus the terminal `failed`, and wires the executor that
drives the kernel through them.

The execute HTTP handler now spawns the executor in a goroutine after
journaling `executing`, returning 202 immediately so operators can
poll target state via the existing list endpoint.

## Scope

1. Move-state constants for the full forward path and the `failed`
   terminal; helper to recognize non-terminal states (used by
   `abort`).
2. Helpers for resolving the in-flight intent into kernel-actionable
   inputs:
   - read `trusted.smoothfs.oid` xattr on the backing file
   - find the smoothfs pool whose mountpoint covers the backing file
     (longest-prefix match)
   - resolve the operator-supplied `destination_tier` string to a
     0-based tier index, accepting numeric, absolute path, or tier
     slot name
3. Async executor that:
   - Unpins LUN
   - `MovePlan(uuid, oid, destTier, seq)`
   - Copies source lower file to destination lower path (preserving
     mode, propagating only the smoothfs OID xattr)
   - `MoveCutover(uuid, oid, seq)`
   - Polls `Inspect` until movement reaches `cleanup_complete` or
     fails
   - Re-pins LUN through smoothfs (lands on the new lower)
   - Resumes the iSCSI target and clears the quiesced flag
4. Failure handling: every step that errors marks the intent
   `failed` with a step-specific reason, and best-effort re-pins the
   source so the LUN never sits unpinned with no journaled forward
   path. The operator can `abort` to drop back to `planned` and
   retry.
5. Test seams: `runActiveLUNMoveImpl`, `openSmoothfsClientFn`,
   `readBackingFileOIDFn`, pin/unpin/resume function variables,
   `setOIDXattrFn` — so tests cover the executor and helpers without
   a real netlink connection or root privileges.

## Acceptance Criteria

- [x] `POST .../move-intent/execute` returns 202 and kicks off the
      executor goroutine.
- [x] On the happy path, the executor drives the intent through
      `unpinned → moving → cutover → repinning → completed`, calls
      `MovePlan` once with the resolved destination tier, calls
      `MoveCutover` once, and clears the quiesced flag at the end.
- [x] On any kernel/IO/pin error, the executor marks the intent
      `failed` with a step-specific reason and best-effort re-pins
      the source LUN.
- [x] `abort` accepts any non-terminal state and drops the intent
      back to `planned`; refused on `planned`, `completed`, `failed`.
- [x] Helpers for OID xattr read, pool prefix match, and tier-name
      resolution are unit-tested.

## Pending in 8c–8d / 08

- Crash recovery on tierd startup (8c).
- UI surfaces for status badge, abort button, and a `tierd-cli`
  active-LUN command (8d).
- Named VFS reviewer sign-off (08 AC #5; process gate).
- End-to-end smoke against a real smoothfs pool with an iSCSI
  file-backed LUN — no test rig is wired up here, so the executor
  has been exercised only via mocked netlink + xattr seams.
