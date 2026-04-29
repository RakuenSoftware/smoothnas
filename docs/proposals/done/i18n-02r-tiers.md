# Proposal: SmoothNAS i18n Phase 2r — Tiers

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02q-volumes.md`](i18n-02q-volumes.md)

---

## Context

Phase 2q converted Volumes. Tiers is the page where operators
provision named storage tiers, assign mdadm/ZFS backings to
their slots, edit fill targets, and tune the pool-spindown
policy. This slice routes every operator-visible string
through `t()`.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Tiers/Tiers.tsx` routes through `t()`.
2. The map parameter `tiers.map((t: any) => …)` is renamed
   to `tier` so it doesn't shadow the `t` from `useI18n()`.
3. Composite strings use `{name}` interpolation:
   `tiers.toast.created` `{name}`,
   `tiers.toast.levelUpdated` `{name}`,
   `tiers.confirm.deleteMessage` `{name}`,
   `tiers.confirm.assignMessage` `{kind, label, slot, tier}`,
   `tiers.error.backingNotFound` `{key}`,
   `tiers.spindown.next` `{when}`,
   `tiers.spindown.ssdBalance` `{state}`,
   `tiers.storage.usedFree` `{used, free}`,
   `tiers.storage.capacity` `{cap, pct}`,
   `tiers.storage.usedPctTooltip` `{pct}`.
4. The long meta-on-fastest warning lives as a single key
   so the Warning prefix and the body translate together
   (the `<strong>Warning:</strong>` tag wraps a `t('tiers.warning.label')`,
   followed by the rest in `t('tiers.warning.metaOnFastest')`).
5. Backend-reported state values (`provisioning`,
   `destroying`, `healthy`, level `rank`, the `mdadm` /
   `zfs` backing kinds, `pv_device` paths, mount points)
   stay literal — protocol values, not labels.

## Acceptance Criteria

- [x] Page header + Refresh button render through `t()`.
- [x] Create Tier card (heading, Tier Name field, mount-
      point hint, Advanced toggle, the long warning, Create
      Tier button) renders through `t()`.
- [x] Each tier card renders the head row, Mount Point /
      Filesystem / Spindown / Storage rows, the Tier Levels
      table headers, the per-level edit/save/cancel
      controls, and the Assigning / Manage pins blocks
      through `t()`.
- [x] Spindown row's `enabled / eligible / blocked` state
      pill, `window open / deferred` pill, the "next" stamp,
      the SSD-balance sub-status, and the three buttons
      (Disable/Enable, Nightly, Anytime) plus their tooltips
      render through `t()`.
- [x] All confirm dialogs (Delete Tier, Assign Backing) and
      toast/error messages route through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Tiering / smoothfs Pools / Users / Terminal / Updates
  page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
