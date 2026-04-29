# Proposal: SmoothNAS i18n Phase 2j — SMB Shares

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02i-iscsi.md`](i18n-02i-iscsi.md)

---

## Context

Phase 2i converted iSCSI Targets. SMB Shares is the second
sharing-side page; this slice converts its labels — protocol
toggle, the compatibility-mode opt-in, the create form, and
the per-share table.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/SmbShares/SmbShares.tsx` routes
   through `t()`.
2. The native `confirm()` for share deletion uses `{name}`
   interpolation.
3. The compatibility-mode tooltip is one long sentence; it
   stays a single key (`smb.compatibilityMode.tooltip`)
   rather than being split mid-sentence.
4. The path option label `{p.name} ({p.path})` stays
   composed in JSX rather than going through `t()` — it's
   pure data formatting (server-supplied name + path), no
   translatable scaffolding.
5. Reuses cross-page keys where strings match exactly:
   `iscsi.protocol.{enabled,disabled}`,
   `iscsi.button.{enable,disable}`, `iscsi.col.path`,
   `arrays.col.actions`, `arrays.button.create`,
   `datasets.col.name`, `common.{cancel,refresh,delete,yes,no}`.
6. Adds `common.yes` / `common.no` for the truthy/falsy
   table cells — these are very generic and likely to recur
   on other pages.

## Acceptance Criteria

- [x] Protocol toggle label + Enable/Disable button render
      through `t()`.
- [x] Compatibility-mode checkbox label + tooltip render
      through `t()`.
- [x] Add Share / Refresh buttons render through `t()`.
- [x] Create form heading + every field label, placeholder,
      checkbox label, and button render through `t()`.
- [x] Empty-state line and table column headers render
      through `t()`.
- [x] Per-row Yes/No truthy values and the Delete button
      render through `t()`.
- [x] Native confirm dialog routes through `t()` with
      `{name}` interpolation.
- [x] All error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- NFS / Network / Smart / Benchmarks / Backups / Settings /
  Volumes / Tiers / Tiering / smoothfs Pools / Users /
  Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
