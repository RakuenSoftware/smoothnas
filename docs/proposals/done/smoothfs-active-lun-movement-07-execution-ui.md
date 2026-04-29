# Proposal: smoothfs Active-LUN Movement Phase 7 — Execution UI

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-06-execution-preflight.md`](../done/smoothfs-active-lun-movement-06-execution-preflight.md)

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added quiesce/resume. Phase 3 persists quiesce state. Phase 4 records move intent only after quiesce and pin preflight pass. Phase 5 added UI controls for recording and clearing move intent. Phase 6 added the execution REST preflight gate. This phase adds the UI action for that execution entry point while the backend still returns a deliberate not-implemented response.

## Scope

1. Add a frontend API helper for the active-LUN execution preflight endpoint.
2. Add an iSCSI UI action to execute recorded move intent.
3. Disable the action unless the target is file-backed, quiesced, pinned, and has recorded move intent.
4. Keep actual journaled movement disabled until the executor/recovery phase is implemented.

## Acceptance Criteria

- [x] Operators can trigger the active-LUN execution preflight from the iSCSI target UI.
- [x] The UI disables execution until quiesce, `PIN_LUN`, and move-intent preconditions are visible.
- [x] Backend execution remains intentionally blocked until journaled movement is implemented.
