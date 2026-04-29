# Proposal: smoothfs Active-LUN Movement Phase 3 — Quiesce State

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-02-quiesce-api.md`](../done/smoothfs-active-lun-movement-02-quiesce-api.md)

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added operator quiesce/resume surfaces for file-backed targets while keeping the backing file pinned. This phase persists and exposes quiesce state so the later journaled move path can require a known SmoothNAS-owned quiesce before any movement cutover is attempted.

## Scope

1. Persist file-backed iSCSI target quiesce state after successful quiesce or resume actions.
2. Include quiesce state in `GET /api/iscsi/targets`.
3. Return quiesce state in quiesce and resume responses.
4. Display quiesce state in the iSCSI UI and disable invalid quiesce/resume actions.
5. Leave cross-tier movement disabled until the journaled move/recovery phase is implemented.

## Acceptance Criteria

- [x] SmoothNAS records whether a file-backed iSCSI target is quiesced.
- [x] Operators can observe quiesce state through the REST target list and UI.
- [x] Successful quiesce sets state to quiesced; successful resume clears it.
- [x] Cross-tier active-LUN movement remains pending until journaled movement, cutover, and recovery are implemented.
