# Proposal: smoothfs Active-LUN Movement Phase 8a — Journaled State

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-08-journaled-execution.md`](smoothfs-active-lun-movement-08-journaled-execution.md)

---

## Context

Phase 7 added the iSCSI UI action that posts to the active-LUN execute
endpoint while the backend deliberately returned 501. The backing
`iscsiLUNMoveIntent` carried a `state` field that was always
`"planned"` — the UI rendered it as a tooltip, but no transitions
existed.

This phase is the first slice of the Phase 8 journaled executor: it
formalizes the state-machine values that future slices will drive
through, journals the `executing` transition on the execute endpoint,
and adds an `abort` endpoint so an operator can drop a stuck intent
back to `planned` without clearing it. The actual kernel-side
movement, unpin/re-pin cutover, and crash recovery are still pending
in 8b–8d.

## Scope

1. Extend `iscsiLUNMoveIntent` with `state_updated_at` and `reason`
   fields.
2. Replace the 501 in `executeISCSIFileTargetMoveIntent` with a real
   `planned → executing` transition; persist via the existing config
   store; return 202 Accepted with the new intent. Keep the existing
   preflight (file-backed, quiesced, smoothfs, `PIN_LUN`) and refuse
   re-execute when state is not `planned`.
3. Add `POST /api/iscsi/targets/<iqn>/move-intent/abort` that
   transitions any non-`planned` intent back to `planned` with
   `reason="operator abort"`. Refuse 409 on a `planned` intent
   (nothing to abort) or when no intent is recorded.
4. Stamp `executing` with a stable `reason` of
   `"executor stub: kernel-side movement not yet wired"` so the next
   slice can identify journaled-but-not-executed intents during
   recovery.

## Acceptance Criteria

- [x] `POST .../move-intent/execute` transitions `planned → executing`
      and returns 202 with the journaled intent.
- [x] `POST .../move-intent/execute` is refused 409 when state is not
      `planned`.
- [x] `POST .../move-intent/abort` transitions any non-`planned`
      intent back to `planned` and records `reason="operator abort"`.
- [x] `POST .../move-intent/abort` is refused 409 on a `planned`
      intent or when no intent is recorded.
- [x] Existing `move-intent` create / clear / list / preflight
      surfaces are unchanged.
