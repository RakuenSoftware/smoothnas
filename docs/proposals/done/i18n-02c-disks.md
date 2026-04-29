# Proposal: SmoothNAS i18n Phase 2c — Disks

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02b-dashboard.md`](i18n-02b-dashboard.md)

---

## Context

Phase 2b converted the Dashboard. This slice does the same for the
Disks page — table headers, the per-row power-state column, the
identify / spindown / standby / wipe action buttons, the wipe
confirm dialog, and the toast/error messages. Disks is the page
operators land on most often after the dashboard, so its labels
need to localise alongside the rest.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Disks/Disks.tsx` routes through `t()`.
2. The power-state column's composite strings use `{name}`
   interpolation:
   - `disks.power.timerMinutes` `{minutes}`
   - `disks.power.standbyPct` `{pct}`
   - `disks.power.lastWake` `{reason, when}`
   so word order can shift in non-English bundles without
   templating English.
3. The wipe confirm dialog uses
   `disks.wipe.message` `{path}` so the device path is an
   interpolated argument rather than baked into English.
4. Power-state badge values from the backend (`active`,
   `standby`, etc.) and SMART status (`PASSED`, `FAILED`) stay
   literal — they're protocol/observed values, not labels, and
   translating them would mis-label what the OS reports.
5. Temperature units (`°C`) and IEC byte units (rendered by
   `size_human`) stay literal: symbols, not words.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] Loading text and empty-state message render through
      `t()`.
- [x] Each table column header renders through `t()`.
- [x] Power-state composite lines (timer, standby pct, last
      wake) render through `t()` with named interpolation.
- [x] All action buttons (Identify, Set, Off, Standby, Wipe)
      and their `title=` tooltips render through `t()`.
- [x] Wipe confirm dialog title + message + confirm-button
      label render through `t()`; the device path is
      interpolated.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Arrays / Pools / iSCSI / SMB / NFS / Network / Smart /
  Benchmarks / Backups / Settings / Volumes / Tiers / Tiering
  / smoothfs Pools / Users / Terminal / Updates page
  conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
