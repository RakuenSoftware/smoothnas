# Proposal: mdadm Heat Engine — Region Inventory

**Status:** Pending
**Part of:** mdadm-complete-heat-engine (Step 3 of 9)
**Depends on:** mdadm-heat-engine-02-managed-volumes

---

## Problem

The heat engine operates on regions — fixed-size slices of each managed volume. Without a region inventory, the system cannot track where data physically lives, cannot measure heat per region, and cannot schedule targeted migrations. Regions must be created deterministically from LV geometry and kept current as volumes grow or as physical placement shifts after pvmove.

---

## Specification

### Region sizing

Each managed volume is divided into regions of equal size. The region size comes from the owning tier instance's `region_size_mb` value. Every volume in the same tier instance uses the same region size.

Region size must be a multiple of the LVM PE size. The default is 256 MiB (64 PEs at 4 MiB each).

The final region of a volume may be smaller than `region_size_mb` if the LV size is not evenly divisible.

### Region row fields

Each region row in `managed_volume_regions` stores:

- `region_index` — zero-based sequential index within the volume
- `region_offset_bytes` — byte offset from the start of the LV
- `region_size_bytes` — actual byte size of this region (may be smaller for the last region)
- `current_tier` — resolved from `seg_pe_ranges` (see below)
- All other columns default on insert

### Computing region count

```
region_count = ceil(lv_size_bytes / region_size_bytes)
```

where `region_size_bytes = region_size_mb * 1024 * 1024`.

### Resolving current tier from `seg_pe_ranges`

After inserting region rows, resolve `current_tier` for each region by calling:

```
lvs -o seg_pe_ranges,devices --noheadings --units b --nosuffix {vg}/{lv}
```

Parse each segment as `(start_pe, end_pe, pv_device)`. For each region:

1. Compute the region's PE range: `pe_start = region_offset_bytes / pe_size_bytes`, `pe_end = (region_offset_bytes + region_size_bytes) / pe_size_bytes - 1`.
2. Find the segment whose PE range contains the region's starting PE.
3. Look up the segment's PV device in `tier_levels` to find the matching tier name and rank.
4. Set `current_tier` to that tier name.

If a region spans a segment boundary (its PEs span two segments on different PVs), set `current_tier` to the tier that owns the majority of the region's PEs.

If no matching tier is found for a segment's PV, log an ERROR and set `current_tier` to `unknown`. This is a data integrity alert.

### When to create or reconcile regions

**Volume create:** After `lvcreate` completes and the `managed_volumes` row is inserted, create region rows for the new volume and resolve `current_tier`.

**Volume extend:** After `lvextend` completes and `size_bytes` is updated, append new region rows for the added bytes only. The existing rows are not touched. Re-resolve `current_tier` for all regions (segment boundaries shift after extend).

**Startup reconciliation:** For each managed volume, compute the expected region count from the live LV size and `region_size_mb`. If the count in the DB differs, add missing rows or remove excess rows, then re-resolve `current_tier` for all regions.

**After pvmove:** The migration engine (Step 5) calls region reconciliation after each completed migration to update `current_tier` for the moved regions.

### Placement summary on `managed_volumes`

After resolving regions, compute a placement summary and store it in memory (not in a DB column) for use by the API:

- `bytes_by_tier` — a map of tier name → total bytes of regions currently on that tier
- `spilled_bytes` — total bytes where `spilled = true`

The API layer reads this summary from an in-memory cache refreshed after each reconciliation.

### Inventory package

Add a `tierd/internal/inventory` package with:

```go
func CreateRegions(ctx context.Context, db *Store, lvm *LVMClient, vol ManagedVolume, peSizeBytes int64) error
func ReconcileRegions(ctx context.Context, db *Store, lvm *LVMClient, vol ManagedVolume, peSizeBytes int64) error
func ResolveCurrentTiers(ctx context.Context, db *Store, lvm *LVMClient, vol ManagedVolume) error
func PlacementSummary(regions []ManagedVolumeRegion) PlacementSummary
```

`CreateRegions` is called by the volume create path.
`ReconcileRegions` is called by the volume extend path and on startup.
`ResolveCurrentTiers` is called by the migration engine after pvmove completes.

### PE size discovery

Obtain the PE size from:

```
vgs -o vg_extent_size --noheadings --units b --nosuffix {vg_name}
```

Cache the PE size per VG in memory at startup. Refresh if a new array is added to the VG.

---

## Acceptance Criteria

- [ ] Creating a managed volume immediately creates region rows at the tier instance's configured `region_size_mb`.
- [ ] Each region row has a resolved `current_tier` derived from `seg_pe_ranges` and `tier_levels`.
- [ ] The last region of a volume may be smaller than `region_size_mb` and is recorded with the correct `region_size_bytes`.
- [ ] Extending a volume appends new region rows and re-resolves `current_tier` for all regions.
- [ ] Startup reconciliation adds missing regions and removes stale regions for each managed volume.
- [ ] After a pvmove, `ResolveCurrentTiers` updates `current_tier` for the affected regions.
- [ ] If a segment's PV device cannot be matched to a `tier_levels` row, `current_tier` is set to `unknown` and an ERROR is logged.
- [ ] PE size is read from `vgs` and cached; it is not hardcoded.
- [ ] The placement summary correctly aggregates bytes by tier across all regions.
