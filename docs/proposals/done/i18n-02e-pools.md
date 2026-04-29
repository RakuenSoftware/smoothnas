# Proposal: SmoothNAS i18n Phase 2e — Pools

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02d-arrays.md`](i18n-02d-arrays.md)

---

## Context

Phase 2d converted the Arrays page. Pools is the dedicated ZFS
page (`Pools / Datasets / Zvols / Snapshots` tab strip);
Datasets / Zvols / Snapshots are separate child components and
will land in their own slices. This slice converts the parent
`Pools.tsx` — the page chrome, the pools tab, and the create
form — through `t()`.

Most of the strings on this page already have keys: the create
form, importable-pool table, and ZFS-member-wipe section
overlap with what Phase 2d added under `arrays.*`. Reusing the
same key for an identical string is the right call so a bundle
author only translates each phrase once.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Pools/Pools.tsx` routes through `t()`.
2. Strings shared with Phase 2d (the importable-pool table,
   ZFS member-disk wipe, vdev option labels, data/SLOG/L2ARC
   row labels, the `noUnassigned` empty line, the pool
   confirm/destroy dialogs and toasts, error messages) reuse
   the existing `arrays.*` keys.
3. New `pools.*` keys cover what's specific to this page:
   the page header (`pools.title` = `ZFS`), subtitle, tab
   labels, the `Create Pool` button, the create-form heading,
   the loading line, the pools-table column headers (Size /
   Used / Free), the `{used} ({pct}%)` composite, and the
   `Pool created` toast.
4. Backend-reported state (`ONLINE`, `DEGRADED`, etc.) and IEC
   byte values rendered server-side stay literal.
5. The Datasets / Zvols / Snapshots child components are not
   modified in this slice — they'll be converted in Phases
   2f / 2g / 2h.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Tab strip (Pools / Datasets / Zvols / Snapshots) renders
      through `t()`.
- [x] `Create Pool` button + create-form heading + every form
      label/option/button render through `t()`.
- [x] Importable-pool table headers, body cells (state,
      status), and `Import` button render through `t()`
      (reusing `arrays.*` keys).
- [x] ZFS member-disk wipe heading + button render through
      `t()` (reusing `arrays.*` keys).
- [x] Pools table headers, the `{used} ({pct}%)` composite,
      and Scrub / Destroy buttons render through `t()`.
- [x] Destroy / wipe confirm dialogs render through `t()`.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Datasets / Zvols / Snapshots child page conversions.
- iSCSI / SMB / NFS / Network / Smart / Benchmarks / Backups
  / Settings / Volumes / Tiers / Tiering / smoothfs Pools /
  Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
