# Proposal: SmoothNAS i18n Phase 2s — Tiering

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02r-tiers.md`](i18n-02r-tiers.md)

---

## Context

Phase 2r converted Tiers. Tiering is the namespace inventory
view (left list of managed namespaces, right detail panel
with the coordinated-snapshot table). This slice routes its
labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Tiering/Tiering.tsx` routes through
   `t()`.
2. The "Showing 50 most recent" notice and the long
   cross-pool snapshot info-note each live as a single key.
3. Reuses cross-page keys where strings match exactly:
   `volumes.col.{backend,health}`, `volumes.detail.{placement,snapshotMode}`,
   `pools.tab.snapshots`, `snapshots.col.created`,
   `tiers.button.creating`, `common.{refresh,delete}`.

## Acceptance Criteria

- [x] Page header + Refresh button render through `t()`.
- [x] Namespace list section heading and empty state render
      through `t()`.
- [x] Detail panel rows (ID / Backend / Kind / Health /
      Placement / Pin State / Exposed Path / Snapshot Mode)
      render through `t()`.
- [x] Snapshot section: Take Snapshot button, "No snapshots
      yet" empty state, the 50-row notice, the table column
      headers, and the per-row Atomic / Inconsistent badges
      and Delete button render through `t()`.
- [x] Cross-pool snapshot info-note routes through `t()`.
- [x] Confirm dialog and toast/error messages route through
      `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- smoothfs Pools / Users / Terminal / Updates page
  conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
