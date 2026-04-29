# Proposal: SmoothNAS i18n Phase 2i — iSCSI Targets

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02h-snapshots.md`](i18n-02h-snapshots.md)

---

## Context

Phase 2h finished the Pools tab strip. iSCSI Targets is the
sharing-side page for block-LUN export and the home of the
Phase 8 active-LUN move state machine. This slice converts
its labels — protocol toggle, create form (block + file
backstore variants), per-row badges, and the seven move-intent
action buttons.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/IscsiTargets/IscsiTargets.tsx` routes
   through `t()`.
2. The state-machine badge labels (`Planned`, `Executing`,
   `Unpinned`, `Moving`, `Cutover`, `Re-pinning`, `Completed`,
   `Failed`) route through `iscsi.move.state.*` keys; the
   underlying state values from the backend (`planned`,
   `executing`, …) stay protocol-literal — they're sent to
   `title=` for forensic tooltips and used by the abort/retry
   gates.
3. The badge tooltip composes through `t()` with named
   interpolation (`iscsi.move.tooltip.{state,destination,
   reason,updated}`) so each line localises while still
   carrying the raw protocol values.
4. The `LUN Pin` / `Quiesce` badges route their labels
   through `iscsi.lunPin.*` / `iscsi.quiesce.*`. The `N/A`
   badge for both shares one key (`iscsi.lunPin.na`).
5. The native `confirm()` and `prompt()` strings (destroy
   target, destination tier) route through `t()` even though
   the modal chrome itself is browser-default — the message
   is the only operator-visible text.
6. IQN format examples, `/dev/zvol/...` paths, and
   `/mnt/smoothfs/...` paths in input placeholders stay
   literal — they're protocol/path examples, not labels.
7. The `targets.map((t: any) => …)` parameter is renamed to
   `row` to avoid shadowing the `t` function from
   `useI18n()`.

## Acceptance Criteria

- [x] Protocol toggle label + Enable/Disable button render
      through `t()`.
- [x] `Add Target` button + create form (radio-group, IQN,
      block/file fields, CHAP fields, the file-mode hint, and
      both action buttons) render through `t()`.
- [x] Empty-state line and table column headers render
      through `t()`.
- [x] Per-row backing badge (Block/File), LUN-pin badge,
      quiesce badge, and move-intent badge labels all render
      through `t()`. Move-intent tooltip lines compose through
      `t()` with named interpolation.
- [x] All seven move-intent action buttons + Quiesce / Resume
      / Destroy buttons render through `t()`.
- [x] Native confirm/prompt strings (destroy target,
      destination tier) route through `t()`.
- [x] All error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- SMB / NFS / Network / Smart / Benchmarks / Backups /
  Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
