# Proposal: smoothfs Active-LUN Movement Phase 8d — UI Status & Abort

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-08-journaled-execution.md`](smoothfs-active-lun-movement-08-journaled-execution.md)
**Predecessor:** [`smoothfs-active-lun-movement-08c-startup-recovery.md`](smoothfs-active-lun-movement-08c-startup-recovery.md)

---

## Context

The active-LUN execute endpoint now journals state through
`planned → executing → unpinned → moving → cutover → repinning →
completed | failed`, with an `abort` endpoint that rolls any
non-terminal state back to `planned` and a tierd-startup recovery
sweep that re-pins backing files for stranded intents. The iSCSI
target list already returned `move_intent.state` and (since 8a)
`state_updated_at` and `reason`. The UI rendered them as a
`Move to X` info badge with the state in a tooltip.

This phase makes the UI state-aware: a per-state badge color +
label, the reason and last update timestamp surfaced in the
tooltip, and an Abort button that drives `POST .../move-intent/abort`
exactly when the state is non-terminal-non-planned.

## Scope

1. Add `abortIscsiMoveIntent` to the frontend API helper.
2. Replace the single-color `Move to X` badge with a state-aware
   badge:
   - `planned` → info
   - `executing` / `unpinned` / `moving` / `cutover` / `repinning`
     / `completed` → active
   - `failed` → inactive
3. Surface the reason and `state_updated_at` in the badge tooltip
   so an operator can see why an intent landed in `failed`
   (recovery, kernel error, pin failure) without needing the API.
4. Add an "Abort Move" button visible whenever a move intent is
   present, and enabled only on non-terminal-non-planned states.
5. Disable "Execute Move" unless the intent is in `planned` so the
   UI can no longer try to re-execute an in-flight or terminated
   intent.

## Non-goals

- A `tierd-cli` mirror of the active-LUN move-intent REST endpoints
  is left for a follow-up. The endpoints are already accessible via
  curl; the CLI add introduces an HTTP-client pattern that
  `tierd-cli` does not currently use, so it earns its own slice if
  operator demand justifies it.

## Acceptance Criteria

- [x] The iSCSI target row badge shows the journaled move-intent
      state with a state-appropriate color.
- [x] Hovering the badge surfaces the reason and last update
      timestamp.
- [x] Abort Move is enabled exactly when the intent is in a
      non-terminal-non-planned state and disabled otherwise.
- [x] Execute Move is disabled when the intent is not in `planned`.
- [x] `tsc -b` passes.

## Pending in 08

- 08 AC #4 (CLI surface) is partially satisfied by the REST surface
  + an upcoming `tierd-cli` mirror. The follow-up doc, if one
  lands, will be 8e.
- 08 AC #5: named VFS reviewer sign-off (process gate).
