# Proposal: smoothfs Active-LUN Movement Phase 8e — CLI Runbook

**Status:** Done
**Split from:** [`smoothfs-active-lun-movement-08-journaled-execution.md`](smoothfs-active-lun-movement-08-journaled-execution.md)
**Predecessor:** [`smoothfs-active-lun-movement-08d-ui-status.md`](smoothfs-active-lun-movement-08d-ui-status.md)

---

## Context

Phase 8d shipped the operator-visible UI surface for active-LUN
movement. AC #4 of Phase 08 also calls for a CLI surface. The
existing `tierd-cli` binary uses direct library calls (netlink for
smoothfs, targetcli wrappers for iSCSI) to stay outside tierd's
HTTP auth surface. Adding a CLI for active-LUN moves through that
pattern would re-implement the journal, executor goroutine, and
crash-recovery sweep — duplicating tierd's own logic and creating a
second source of truth.

The right CLI for active-LUN moves is the REST surface itself. All
endpoints are reachable on the same host via curl with a session
cookie obtained from `POST /api/auth/login`. This phase makes that
surface durable for operators by documenting the full workflow,
recovery commands, and accepted `destination_tier` formats in the
operator runbook, alongside the existing iSCSI section.

## Scope

1. Add an "active-LUN movement" subsection to
   `docs/smoothfs-operator-runbook.md` covering:
   - The `planned → executing → unpinned → moving → cutover →
     repinning → completed | failed` journal state machine
   - The UI workflow (Quiesce, Plan Move, Execute Move, Abort Move,
     Clear Move)
   - Worked curl examples for the same-host REST surface
   - Recovery commands (`abort`, `clear`, post-restart Resume)
   - The three accepted `destination_tier` formats (numeric index,
     absolute path, tier slot name)

## Non-goals

- A standalone `tierd-cli iscsi-move` HTTP-client subcommand.
  Adding the auth + session-cookie plumbing to `tierd-cli` for one
  feature is more surface area than it earns, and a separate Go
  CLI would need to track REST contract changes. If operator
  demand calls for it later it can land as its own slice without
  re-doing the runbook documentation.

## Acceptance Criteria

- [x] The runbook documents the full Phase 8 active-LUN journal
      state machine.
- [x] Worked curl examples cover Quiesce → Plan → Execute → poll →
      Abort/Clear → Resume.
- [x] The recovery scenario after a tierd-restart is documented,
      including the expected `failed` state with the recovery
      reason.
- [x] All three `destination_tier` formats are documented.

## Pending in 08

- 08 AC #5: named VFS reviewer sign-off (process gate). Cannot be
  satisfied by code or docs; requires an operator to designate the
  reviewer.
