# Proposal: SmoothNAS i18n Phase 2m — SMART

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02l-network.md`](i18n-02l-network.md)

---

## Context

Phase 2l converted Network. SMART is a small page (alarm rules
+ alarm history); this slice routes its labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Smart/Smart.tsx` routes through `t()`.
2. Reuses Dashboard's `dashboard.alerts.*` keys for the
   alarm-history column headers — they're identical strings
   already keyed in Phase 2b.
3. SMART attribute names (`Reallocated_Sector_Ct`, etc.),
   operator strings (`>`, `<`, `==`), and severity values
   (`info` / `warning` / `critical`) stay literal — they're
   protocol/observed values from the backend.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Alarm Rules section: heading, empty state, table
      headers, and Delete button render through `t()`.
- [x] Alarm History section: heading, empty state, and table
      headers render through `t()` (reusing
      `dashboard.alerts.*`).
- [x] Error message routes through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Benchmarks / Backups / Settings / Volumes / Tiers /
  Tiering / smoothfs Pools / Users / Terminal / Updates
  page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
