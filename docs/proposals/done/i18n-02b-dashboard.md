# Proposal: SmoothNAS i18n Phase 2b — Dashboard

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02a-layout-language-picker.md`](i18n-02a-layout-language-picker.md)

---

## Context

The Dashboard is the appliance landing page. Phase 2a converted
the chrome (sidebar, top-bar, confirms); this slice converts the
Dashboard's own JSX literals so the operator's first post-login
view runs through `t()`. Other per-page conversions (Disks,
Arrays, Network, Pools, iSCSI, SMB, NFS, etc.) follow as separate
small slices.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Dashboard/Dashboard.tsx` routes through
   `t()`.
2. Composite strings use `{name}` interpolation
   (`dashboard.cpu.detail`, `dashboard.memory.detail`,
   `dashboard.disks.detail`, `dashboard.service.issues`,
   `dashboard.service.versionUptime`,
   `dashboard.sharing.activeCount`) so word-order shifts in
   non-English bundles don't have to template English.
3. IEC byte units (B / KiB / MiB / …) stay literal — they're
   abbreviations, translating them would mis-label the data.
4. Per-NIC speed strings (`Mb/s`, `↓ X/s ↑ Y/s`) stay literal:
   tiny labels next to live numbers, the symbols carry the
   meaning.
5. The "no tier usage" fallback for the busiest-tier composite
   uses `t('dashboard.tiering.noUsage')` rather than a literal
   so the empty state localises with the rest.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Each summary card label, value-detail line, and link
      anchor renders through `t()` (Service, Disks, mdadm
      Arrays, ZFS Pools, Active Migrations, Migration Backlog,
      Near Spillover, Datasets, Sharing, CPU, Memory).
- [x] Composite strings use named interpolation.
- [x] System Hardware + Recent Alerts section headers render
      through `t()`; alerts table column headers and the
      empty-state line render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Disks / Arrays / Network / Pools / iSCSI / SMB / NFS / Smart
  / Benchmarks / Backups / Settings / Volumes / Tiers / Tiering
  / smoothfs Pools / Users / Terminal / Updates page
  conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
