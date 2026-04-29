# Proposal: smoothfs Active-LUN Movement Phase 1 — LUN Pin Status

**Status:** Done
**Split from:** [`smoothfs-stacked-tiering.md`](../done/smoothfs-stacked-tiering.md)

---

## Context

Smoothfs Phases 0-7 are complete in the standalone `smoothfs` repo and SmoothNAS now consumes smoothfs as an external module/artifact. File-backed iSCSI LUNs are supported by pinning backing files with `trusted.smoothfs.lun`, and tierd clears that pin only when the target is destroyed.

Before active-LUN movement can safely grow quiesce and cutover operations, operators need a direct signal that each file-backed iSCSI target is protected by smoothfs `PIN_LUN`. This phase adds that status surface without allowing movement of live LUNs.

## Scope

1. Add a best-effort SmoothNAS inspector for `trusted.smoothfs.lun` on fileio backing files.
2. Include LUN pin status in `GET /api/iscsi/targets` for file-backed targets.
3. Display the status in the iSCSI target UI.
4. Keep block-backed targets unchanged and avoid failing the target list when a stale file path cannot be inspected.

## Acceptance Criteria

- [x] File-backed iSCSI targets report whether their backing file is on smoothfs and whether `PIN_LUN` is installed.
- [x] Missing, invalid, non-smoothfs, and xattr-read-failure states are surfaced explicitly instead of hiding the target row.
- [x] The iSCSI target UI shows the current LUN pin state.
- [x] Live active-LUN movement remains disabled until the quiesce and re-pin protocol is implemented.
