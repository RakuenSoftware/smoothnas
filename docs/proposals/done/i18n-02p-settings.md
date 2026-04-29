# Proposal: SmoothNAS i18n Phase 2p — Settings

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02o-backups.md`](i18n-02o-backups.md)

---

## Context

Phase 2o converted Backups. Settings is the small system-config
page (hostname + change-password). This slice routes its
labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Settings/Settings.tsx` routes through
   `t()`.
2. Reuses `common.{save,refresh}` for the shared button
   labels.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Hostname section: heading + Save button render through
      `t()`.
- [x] Change Password section: heading, both field labels,
      and Change button render through `t()`.
- [x] Success and error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Volumes / Tiers / Tiering / smoothfs Pools / Users /
  Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
