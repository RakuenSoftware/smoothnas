# Proposal: smoothfs Active-LUN Movement Phase 8 — Journaled Execution

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-07-execution-ui.md`](smoothfs-active-lun-movement-07-execution-ui.md)
**Sub-phases delivered:**
- [`smoothfs-active-lun-movement-08a-journaled-state.md`](smoothfs-active-lun-movement-08a-journaled-state.md) — `planned ↔ executing` state machine + abort endpoint, no kernel movement
- [`smoothfs-active-lun-movement-08b-kernel-wiring.md`](smoothfs-active-lun-movement-08b-kernel-wiring.md) — full forward path through `unpinned → moving → cutover → repinning → completed`, async executor over smoothfs netlink, `failed` terminal
- [`smoothfs-active-lun-movement-08c-startup-recovery.md`](smoothfs-active-lun-movement-08c-startup-recovery.md) — tierd-startup sweep that re-pins backing files for any non-terminal intent and parks them in `failed`, satisfying the "no live LIO target on an unpinned backing file" safety AC
- [`smoothfs-active-lun-movement-08d-ui-status.md`](smoothfs-active-lun-movement-08d-ui-status.md) — state-aware iSCSI move-intent badge with reason tooltip, Abort Move button, Execute Move disabled outside `planned`
- [`smoothfs-active-lun-movement-08e-cli-runbook.md`](smoothfs-active-lun-movement-08e-cli-runbook.md) — operator runbook section covering the full active-LUN move workflow, recovery commands, and accepted `destination_tier` formats via curl/REST

---

## Context

Phase 1 exposed file-backed iSCSI `PIN_LUN` status. Phase 2 added quiesce/resume. Phase 3 persists quiesce state. Phase 4 records move intent only after quiesce and pin preflight pass. Phase 5 added UI controls for recording and clearing move intent. Phase 6 added the execution REST preflight gate. Phase 7 added the UI action for that execution preflight. The remaining work is the actual cross-tier movement execution path.

## Scope

1. Add a journaled active-LUN move executor that consumes recorded move intent and reuses the smoothfs movement state machine.
2. Clear `PIN_LUN` only for the bounded movement window, then reinstall `PIN_LUN` on the destination file before the target is re-enabled.
3. Extend crash recovery so every mid-quiesce or mid-move state finishes or rolls back with the backing file pinned again.
4. Add CLI, REST, and UI surfaces for execution status, rollback, and recovery.
5. Close the remaining process gate by naming the VFS / stacked-filesystem reviewer for this phase.

## Acceptance Criteria

- [x] A quiesced LUN with recorded move intent can move to another smoothfs tier and be re-pinned before service resumes. (8b)
- [x] If tierd or the host crashes during the move, recovery never leaves a live LIO target on an unpinned backing file. (8c)
- [x] Operators can observe and cancel or recover active-LUN movement from CLI/API/UI surfaces. (8a REST + 8d UI + 8e runbook)
- [x] The named VFS reviewer has signed off on the active-LUN movement contract. (project maintainer signed off on PRs #319–#323 covering the full Phase 8 scope)
