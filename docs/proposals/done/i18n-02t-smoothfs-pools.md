# Proposal: SmoothNAS i18n Phase 2t — smoothfs Pools

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02s-tiering.md`](i18n-02s-tiering.md)

---

## Context

Phase 2s converted Tiering. The smoothfs Pools page (Phase 7
of the tiering work) drives the kernel-side pool lifecycle —
create form, write-staging dashboard with twenty-plus metric
lines, and the movement-log table. This slice routes its
labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/SmoothfsPools/SmoothfsPools.tsx`
   routes through `t()`.
2. The 20+ write-staging metric strings (staged bytes,
   rehome bytes, range bytes/writes, recovered ranges,
   masks, recommended masks, etc.) each get a key with
   named interpolation so a non-English bundle can flip the
   suffix without templating.
3. Hex-mask values (`0x{mask}`) keep their `0x` prefix in
   the key string so the protocol-style hex output stays
   recognisable.
4. The destroy native `confirm()` and the reconcile
   `prompt()` route through `t()` with `{name}`
   interpolation.
5. Reuses cross-page keys where strings match exactly:
   `iscsi.action.quiesce`, `arrays.action.destroy`,
   `tiers.button.creating`, `arrays.button.create`,
   `datasets.col.name`, `common.{refresh,cancel,loading}`.

## Acceptance Criteria

- [x] Page heading + Create Pool / Refresh buttons render
      through `t()`.
- [x] Create form (heading, every field label and
      placeholder, hint paragraph, Cancel and Create buttons)
      renders through `t()`.
- [x] Pool table column headers, the empty state, and the
      per-row state badge (active / waiting / off) render
      through `t()`.
- [x] All write-staging metric lines render through `t()`
      with named interpolation.
- [x] Per-pool action buttons (Enable / Disable Staging,
      Refresh Mask, Quiesce, Reconcile, Destroy) render
      through `t()`.
- [x] Movement log heading + the long hint paragraph +
      empty state + table column headers render through
      `t()`.
- [x] Destroy / reconcile native dialogs route through
      `t()` with `{name}` interpolation.
- [x] All error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
