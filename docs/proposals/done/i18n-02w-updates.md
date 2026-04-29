# Proposal: SmoothNAS i18n Phase 2w — Updates

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02v-terminal.md`](i18n-02v-terminal.md)

---

## Context

Phase 2v converted Terminal. Updates is the SmoothNAS update
flow + Debian package-updates flow. This is the last
per-page slice in Phase 2 — once it lands every operator-
visible string in `tierd-ui` is keyed through the SmoothGUI
`I18nProvider`, ready for Phase 3 (non-English bundles).

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Updates/Updates.tsx` routes through
   `t()`.
2. Channel labels (`Main` / `Testing` / `JBailes`) are
   produced through `channelLabel(channel)` so the function
   resolves them through `t('updates.channel.*')`.
3. Apply / manual-upload native confirms and the package-
   update confirm route through `t()`. The apply confirm
   uses `{version}` interpolation.
4. The "Latest (channel):" label uses `{channel}`
   interpolation so the channel-name parenthetical can shift
   in non-English bundles.
5. Filenames (`manifest.json`, `tierd`, `tierd-ui.tar.gz`)
   stay literal — they're protocol/path examples baked into
   the validation messages, not labels.
6. Per the proposal, this is the final per-page slice in
   Phase 2; Phase 3 will add non-English bundles against
   the now-complete key set.

## Acceptance Criteria

- [x] Page header + subtitle + Refresh button render through
      `t()`.
- [x] SmoothNAS Updates section: heading, channel label,
      channel-button labels, "Running:" / "Latest:" /
      "checking…" labels, available banner, Apply Update /
      Updating buttons, "Up to date" / "Checking channel"
      lines, Manual Update label + hint render through
      `t()`.
- [x] Package Updates section: heading, intro paragraph,
      and Update Packages / Updating buttons render through
      `t()`.
- [x] Native confirm() for apply, manual-upload, and
      package-updates all route through `t()` with
      interpolation where needed.
- [x] All update / package stage strings (Starting,
      Uploading, Restarting, Reconnecting, etc.) and toast/
      error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope

- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).

## Phase 2 wrap-up

With this slice all per-page conversions enumerated by the
parent `smoothnas-i18n-en-nl.md` proposal are done.
Subsequent slices land non-English bundles against the
fully-keyed catalog.
