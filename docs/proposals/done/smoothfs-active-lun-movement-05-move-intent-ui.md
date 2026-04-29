# Proposal: smoothfs Active-LUN Movement Phase 5 — Move Intent UI

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-04-move-intent.md`](../done/smoothfs-active-lun-movement-04-move-intent.md)

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added quiesce/resume. Phase 3 persists quiesce state. Phase 4 records move intent only after quiesce and pin preflight pass. This phase adds the UI controls for operators to record or clear that move intent from the iSCSI target table.

## Scope

1. Add frontend API helpers for the move-intent REST endpoints.
2. Add an iSCSI UI action to record move intent when the target is file-backed, quiesced, pinned, and has no active intent.
3. Add an iSCSI UI action to clear recorded move intent.
4. Keep execution disabled until the journaled movement/recovery phase is implemented.

## Acceptance Criteria

- [x] Operators can record move intent from the iSCSI target UI.
- [x] Operators can clear recorded move intent from the iSCSI target UI.
- [x] The UI disables invalid move-intent actions before they reach the API.
- [x] Cross-tier active-LUN execution remains pending until journaled movement, cutover, and recovery are implemented.
