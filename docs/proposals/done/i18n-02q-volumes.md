# Proposal: SmoothNAS i18n Phase 2q — Volumes

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02p-settings.md`](i18n-02p-settings.md)

---

## Context

Phase 2p converted Settings. Volumes shows the smoothfs-backed
namespace list, the per-volume detail panel (placement /
capacity / snapshot mode / file placement / movement state /
policy targets). This slice routes its labels through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Volumes/Volumes.tsx` routes through
   `t()`.
2. The four summary cards (Volumes / Pinned / Movement /
   Lifecycle) and their detail lines render through `t()`.
3. Detail-panel composites use named interpolation:
   `volumes.detail.totalSuffix` `{total}`,
   `volumes.summary.tierRank` `{rank}`,
   `volumes.summary.tierCount` `{count, bytes}`,
   `volumes.movement.fromTo` `{from, to}`,
   `volumes.toast.{pinUpdated,unpinned}` `{name}`.
4. Backend-reported state values (placement_state, health,
   pin_state, snapshot_mode, backend_kind, activity_band)
   stay literal — protocol values, not labels. Where the
   server returns an empty value the fallback routes through
   `common.{none,unknown,na}` (lowercased to match the
   surrounding state-value cells).
5. Adds `common.na` to the shared block — it recurs in
   percent-table cells.

## Acceptance Criteria

- [x] Page header + summary cards render through `t()`.
- [x] Empty state line and Volume List header render through
      `t()`.
- [x] Per-row column headers + Details / Unpin / Pin hot
      action buttons render through `t()`.
- [x] Detail panel cards (Placement / Capacity / Snapshot
      Mode), the File Placement / Movement State / Policy
      Targets sections, their empty states, and the
      `from {a} to {b}` movement composite render through
      `t()`.
- [x] All toast and error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Tiers / Tiering / smoothfs Pools / Users / Terminal /
  Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
