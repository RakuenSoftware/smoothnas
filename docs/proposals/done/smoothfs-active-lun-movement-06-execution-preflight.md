# Proposal: smoothfs Active-LUN Movement Phase 6 — Execution Preflight

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-05-move-intent-ui.md`](../done/smoothfs-active-lun-movement-05-move-intent-ui.md)

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added quiesce/resume. Phase 3 persists quiesce state. Phase 4 records move intent only after quiesce and pin preflight pass. Phase 5 added UI controls for recording and clearing move intent. This phase adds the REST execution entry point and preflight gate while deliberately refusing execution until the journaled kernel-backed executor exists.

## Scope

1. Add a REST execution endpoint for recorded active-LUN move intent.
2. Require recorded intent plus the existing file-backed, quiesced, smoothfs, and `PIN_LUN` preflight gates.
3. Return a clear not-implemented response while preserving move intent and never clearing `PIN_LUN`.
4. Leave actual cross-tier movement, unpin/re-pin cutover, and recovery disabled until the journaled execution phase.

## Acceptance Criteria

- [x] Executing without recorded move intent is refused.
- [x] Executing with recorded intent still enforces quiesce and `PIN_LUN` preflight.
- [x] The endpoint returns an explicit not-implemented response without mutating move intent.
- [x] `PIN_LUN` is never cleared by the preflight endpoint.
