# Proposal: SmoothNAS i18n Phase 2a — Layout + Language Picker

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-01-framework.md`](i18n-01-framework.md)

---

## Context

Phase 1 wired SmoothGUI's `I18nProvider` and the SmoothNAS catalog
scaffold without converting any existing JSX literals. The
proposal's Phase 2 calls for "one PR per feature page" plus the
header language picker. This slice picks the chrome up first —
sidebar nav labels, top-bar user menu, the reboot/shutdown
confirm dialogs — because those are seen on every page and
because the header is where the picker lives.

Per-page conversions (Dashboard, Disks, Arrays, Network, etc.)
land as separate small slices.

## Scope

1. **`tierd-ui/src/components/LanguagePicker/LanguagePicker.tsx`** —
   new component. Renders a `<select>` driving `setLanguage`
   from `useI18n()` when `SUPPORTED_LANGUAGES.length >= 2`.
   Hidden until a second language ships (Phase 3) so a single-
   option picker doesn't clutter the header.
2. **`Layout.tsx` rewritten through `useI18n()`**. The static
   `NAV_ITEMS` array becomes `buildNavItems(t)`, recomputed on
   each render so language changes propagate without component
   remount. Section names (`Overview`, `Hardware`, `Storage`,
   `Sharing`, `System`) and per-row labels go through
   `nav.section.*` and `nav.*` keys.
3. **`TopBar` user-menu and confirm dialogs** keyed through
   `topbar.user.*`, `topbar.reboot.*`, `topbar.shutdown.*`.
   The picker mounts before AlertsButton / UserDropdown.
4. **`tierd-ui/src/i18n/locales/en.ts`** extended with the
   new keys plus a per-row scaffold for the still-unconverted
   pages (`nav.tiers`, `nav.tiering`, `nav.volumes`,
   `nav.smoothfsPools`, `nav.users`, `nav.terminal`,
   `nav.updates`).

## Acceptance Criteria

- [x] Sidebar nav labels + section names render through
      `t()`; switching language re-renders the labels live.
- [x] Top-bar user-menu (Account Settings, Reboot, Shutdown,
      Logout) renders through `t()`.
- [x] Reboot and Shutdown confirm dialogs (title, message,
      confirm-button label) render through `t()`.
- [x] LanguagePicker renders a working `<select>` when a
      second supported language is registered, and renders
      nothing today (Phase 1 / 2a state with English only).
- [x] `make test-frontend` (`tsc -b`), `make test-backend`,
      and `make lint` are clean.

## Out of scope (later slices)

- Per-page conversions (Dashboard, Disks, Arrays, Pools,
  iSCSI, SMB, NFS, Network, Smart, Benchmarks, Backups,
  Settings, Volumes, Tiers, Tiering, smoothfs Pools, Users,
  Terminal, Updates).
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
