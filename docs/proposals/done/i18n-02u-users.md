# Proposal: SmoothNAS i18n Phase 2u — Users

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02t-smoothfs-pools.md`](i18n-02t-smoothfs-pools.md)

---

## Context

Phase 2t converted smoothfs Pools. Users is the small local-
account management page (create + delete). This slice routes
its labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Users/Users.tsx` routes through
   `t()`.
2. Delete confirm dialog uses `{username}` interpolation.

## Acceptance Criteria

- [x] Page header + Create User / Refresh buttons render
      through `t()`.
- [x] Create form labels and buttons render through `t()`.
- [x] Empty state and table column headers render through
      `t()`.
- [x] Delete confirm dialog and validation/toast/error
      messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
