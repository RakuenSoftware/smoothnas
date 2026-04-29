# Proposal: smoothfs Active-LUN Movement Phase 4 — Move Intent

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-03-quiesce-state.md`](../done/smoothfs-active-lun-movement-03-quiesce-state.md)

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added operator quiesce/resume surfaces. Phase 3 persists and displays quiesce state. This phase adds the durable REST move-intent/preflight surface that records the requested destination only after SmoothNAS knows the LUN is quiesced and still protected by `PIN_LUN`.

## Scope

1. Add a REST endpoint to record move intent for a file-backed iSCSI target.
2. Require the target to be file-backed, quiesced, on smoothfs, and still `PIN_LUN` protected before recording intent.
3. Include move intent in the target list so operators can observe planned movement.
4. Add a REST endpoint to clear move intent.
5. Leave actual cross-tier movement, unpin/re-pin cutover, and recovery disabled until the journaled execution phase.

## Acceptance Criteria

- [x] SmoothNAS records move intent only for quiesced, pinned smoothfs file-backed LUNs.
- [x] Move intent is visible from the iSCSI target list.
- [x] Operators can clear move intent before execution.
- [x] Cross-tier active-LUN execution remains pending until journaled movement, cutover, and recovery are implemented.
