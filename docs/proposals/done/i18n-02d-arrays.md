# Proposal: SmoothNAS i18n Phase 2d — Arrays

**Status:** Done
**Split from:** [`smoothnas-i18n-en-nl.md`](../pending/smoothnas-i18n-en-nl.md)
**Predecessor:** [`i18n-02c-disks.md`](i18n-02c-disks.md)

---

## Context

Phase 2c converted the Disks page. This slice does the same for
the Arrays page — both the mdadm and ZFS halves, the create form
with its tab switcher, the importable-pool list, and the wipe /
destroy confirm dialogs. Arrays is where operators provision
storage, so its labels need to localise alongside the rest.

## Scope

1. Every operator-visible JSX literal in
   `tierd-ui/src/pages/Arrays/Arrays.tsx` (including the inline
   `ZfsDiskPicker` and `ZfsMemberDiskPicker` helpers) routes
   through `t()`.
2. Composite strings use `{name}` interpolation:
   - `arrays.disks.selected` `{count}`
   - `arrays.zfs.dataSelected` `{count}`
   - `arrays.summary.disks` `{active, total}`
   - `arrays.summary.usedWithPct` `{used, pct}`
   - `arrays.summary.free` `{free}`
   - `arrays.detail.disksValue` `{active, total}`
   - `arrays.confirm.destroyArrayMessage` `{name}`
   - `arrays.confirm.destroyPoolMessage` `{name}`
   - `arrays.confirm.wipeMembersMessage` `{disks}`
   - `arrays.toast.scrubStarted` / `arrays.toast.poolImported` `{name}`
   - `arrays.warn.minDisks` `{level, min, selected}`
   - `arrays.error.*Prefix` `{err}` for the few cases that
     surface a job-error string inline.
3. Backend-reported state values (mdadm `active`/`degraded`/
   `inactive`, ZFS `ONLINE`/`DEGRADED`/`UNAVAIL`) and protocol
   identifiers (`raid5`, `raidz1`, mount points, `/dev/...`
   paths, `tank`, `md0` placeholders) stay literal — they're
   protocol values, not labels.
4. IEC byte units (rendered server-side into `size_human`,
   `lv_size`, `alloc_human`, `free_human`) stay literal: the
   server already formatted them; the UI just composes a
   sentence around the value.
5. `ZfsDiskPicker` gains an `emptyMessage` prop so the empty-
   state line stays inside the parent component's `t()` scope
   rather than hard-coding English in a leaf component.

## Acceptance Criteria

- [x] Page header + subtitle + Create / Refresh buttons render
      through `t()`.
- [x] Create form (mdadm + ZFS tabs) renders every label,
      option, status line, and button through `t()`.
- [x] Both empty states render through `t()`.
- [x] Per-array and per-pool summary rows render their
      composite values through `t()` with named interpolation.
- [x] Expanded array details (Path / RAID Level / State / Size
      / Disks / Mount / Filesystem / Tier Slot / Member Disks)
      render through `t()`.
- [x] Destroy / Scrub / Import / Wipe buttons + their confirm
      dialogs render through `t()`.
- [x] Toast and error messages render through `t()`.
- [x] `make test-frontend` (`tsc -b`) and `make lint` are
      clean.

## Out of scope (later slices)

- Pools / iSCSI / SMB / NFS / Network / Smart / Benchmarks /
  Backups / Settings / Volumes / Tiers / Tiering / smoothfs
  Pools / Users / Terminal / Updates page conversions.
- Non-English bundles (Phase 3).
- Literal-string lint (proposal §8.3).
- REST error-code stabilisation (Phase 6).
