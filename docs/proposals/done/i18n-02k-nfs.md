# Proposal: SmoothNAS i18n Phase 2k — NFS Exports

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02j-smb.md`](i18n-02j-smb.md)

---

## Context

Phase 2j converted SMB Shares. NFS Exports is the third
sharing-side page; this slice converts its protocol toggle,
create form, and table.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/NfsExports/NfsExports.tsx` routes
   through `t()`.
2. The Sync/Async write-mode label flips dynamically; both
   labels route through `nfs.writeMode.{sync,async}`.
3. CIDR placeholders (`10.0.0.0/24, 192.168.1.0/24`) and
   the comma-joined networks string from the server stay
   literal — they're protocol values.
4. Reuses cross-page keys where strings match exactly:
   `iscsi.protocol.{enabled,disabled}`,
   `iscsi.button.{enable,disable}`, `iscsi.col.path`,
   `arrays.button.create`, `smb.field.{path,pathPlaceholder,readOnly}`,
   `common.{cancel,refresh,yes,no}`.

## Acceptance Criteria

- [x] Protocol toggle label + Enable/Disable button render
      through `t()`.
- [x] Add Export / Refresh buttons render through `t()`.
- [x] Create form heading + every field label, placeholder,
      checkbox label, and button render through `t()`.
- [x] Empty-state line and table column headers render
      through `t()`.
- [x] Per-row Sync/Async write-mode label and Yes/No
      root-squash cell render through `t()`.
- [x] All error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Network / Smart / Benchmarks / Backups / Settings /
  Volumes / Tiers / Tiering / smoothfs Pools / Users /
  Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
