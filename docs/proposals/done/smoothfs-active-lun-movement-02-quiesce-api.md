# Proposal: smoothfs Active-LUN Movement Phase 2 — Quiesce API

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-01-lun-pin-status.md`](../done/smoothfs-active-lun-movement-01-lun-pin-status.md)

---

## Context

Phase 1 exposed whether SmoothNAS file-backed iSCSI targets are currently protected by smoothfs `PIN_LUN`. This phase adds the operator-facing quiesce/resume surface that later active-LUN movement will require before any pin is cleared.

## Scope

1. Add targetcli-backed SmoothNAS helpers to disable and re-enable an iSCSI target portal group.
2. Add REST endpoints to quiesce and resume file-backed iSCSI targets.
3. Reject quiesce on block-backed targets and on smoothfs file-backed targets whose `PIN_LUN` status is not pinned.
4. Add UI actions for file-backed target quiesce and resume.
5. Keep the backing file pinned during quiesce; movement and unpin/re-pin cutover remain disabled.

## Acceptance Criteria

- [x] Operators can quiesce and resume a file-backed iSCSI target through REST and UI surfaces.
- [x] SmoothNAS refuses quiesce when the target is block-backed.
- [x] SmoothNAS refuses quiesce when a smoothfs-backed LUN is not protected by `PIN_LUN`.
- [x] The target remains pinned throughout the quiesce/resume operation.
- [x] Cross-tier active-LUN movement remains pending until journaled movement, cutover, and recovery are implemented.
