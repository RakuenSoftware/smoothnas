# Proposal: SmoothNAS i18n Phase 2g — Zvols

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02f-datasets.md`](i18n-02f-datasets.md)

---

## Context

Phase 2f converted the Datasets child component. This slice
does the same for the Zvols child component (the second tab on
the Pools page) — create form, table, destroy confirm, and
toast/error messages.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Zvols/Zvols.tsx` routes through `t()`.
2. The destroy confirm message uses `{name}` interpolation.
3. ZFS zvol names (`tank/vol0`), block-size values (`4K`),
   and IEC byte sizes (`100G`, `size_human`) stay literal —
   they're protocol identifiers / unit suffixes, not labels.
4. Reuses cross-page keys where strings match exactly:
   `arrays.col.actions`, `arrays.action.destroy`,
   `arrays.button.create`, `arrays.creating`,
   `arrays.error.{lostConnection,jobFailed,destroyArrayPrefix}`,
   `datasets.col.name`, `common.cancel`, `common.refresh`.

## Acceptance Criteria

- [x] `Create Zvol` and `Refresh` buttons render through
      `t()`.
- [x] Create form heading + every field label and button
      render through `t()`.
- [x] Loading text and empty-state message render through
      `t()`.
- [x] Table column headers render through `t()`.
- [x] Destroy confirm dialog title + message render through
      `t()`; zvol name is interpolated.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Snapshots child component (Phase 2h).
- iSCSI / SMB / NFS / Network / Smart / Benchmarks / Backups
  / Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
