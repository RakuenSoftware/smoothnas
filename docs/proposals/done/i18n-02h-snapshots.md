# Proposal: SmoothNAS i18n Phase 2h — Snapshots

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02g-zvols.md`](i18n-02g-zvols.md)

---

## Context

Phase 2g converted the Zvols child component. This slice
finishes the Pools tab strip by converting the Snapshots
child — create form, table, the dual destroy/rollback confirm
dialogs, and toast/error messages.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Snapshots/Snapshots.tsx` routes
   through `t()`.
2. Both confirm dialogs (destroy + rollback) use `{name}`
   interpolation so the snapshot name is interpolated.
3. ZFS snapshot names (`tank/data@backup-2026`) and IEC byte
   sizes rendered server-side stay literal — they're
   protocol values, not labels.
4. The "Created" timestamp string from `s.created` is left
   literal — it's an ISO-style timestamp from the server
   (locale-formatting can be a later refinement once the
   bundle covers more pages).
5. Reuses cross-page keys where strings match exactly:
   `arrays.col.actions`, `arrays.action.destroy`,
   `arrays.button.create`, `arrays.creating`,
   `arrays.error.{lostConnection,jobFailed,destroyArrayPrefix}`,
   `arrays.confirm.confirm`, `datasets.col.{name,used}`,
   `common.cancel`, `common.refresh`.

## Acceptance Criteria

- [x] `Create Snapshot` and `Refresh` buttons render through
      `t()`.
- [x] Create form heading + every field label and button
      render through `t()`.
- [x] Loading text and empty-state message render through
      `t()`.
- [x] Table column headers (Name / Used / Created / Actions)
      render through `t()`.
- [x] Both confirm dialogs (Destroy Snapshot + Rollback
      Snapshot) render their title and message through `t()`
      with `{name}` interpolation.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- iSCSI / SMB / NFS / Network / Smart / Benchmarks / Backups
  / Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
