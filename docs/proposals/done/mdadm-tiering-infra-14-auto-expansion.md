# Proposal: mdadm Tiering Infrastructure — Auto-Expansion

**Status:** Pending
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 14 of 14)
**Depends on:** mdadm-tiering-infra-12-pv-allocation, mdadm-tiering-infra-13-reconciliation

---

## Problem

When an mdadm array backing a pool tier is expanded (e.g. a disk is added to a RAID-6, increasing its usable capacity), the underlying block device grows but the pool's LV and filesystem remain at their original size. Without automatic expansion, the additional capacity is invisible to the user and `100%FREE` becomes inaccurate.

---

## Specification

### Detection

The existing mdadm array monitor polls `/proc/mdstat` and `mdadm --detail` every 30 seconds. When it detects that a managed array's reported size has increased compared to the last recorded value, it emits an internal event:

```
ArraySizeChanged { array_id: 42, old_bytes: N, new_bytes: M }
```

The pool manager subscribes to this event. On receipt, it looks up the pool and Tier slot for `array_id`. If none is found (the array is not assigned to any pool), the event is ignored.

### Expansion sequence

1. Receive the `ArraySizeChanged` event for `array_id`.
2. Look up the Tier slot: resolve `pool_name` and `pv_device`.
3. Run `pvresize {pv_device}` to inform LVM of the new PV size.
4. Run `lvextend -l +100%FREE tier-{pool_name}/data {pv_device}` — passing the specific PV so that only that tier's extents are extended. New extents append after existing segments, preserving rank order.
5. Grow the filesystem online:
   - XFS: `xfs_growfs /mnt/{pool_name}`
   - ext4: `resize2fs /dev/tier-{pool_name}/data`
6. Run segment verification (Step 12).
7. Update `updated_at` on the TierPool row.

### Failure handling

If any step fails:
- Log the error.
- Set pool `error_reason` to `'auto_expansion_failed: {step}'`.
- Do not change pool `state` (the LV and filesystem are still functional at their pre-expansion size).
- The next reconciliation pass will detect the PV/LV size mismatch and surface it.

---

## Acceptance Criteria

- [ ] Growing an mdadm array automatically triggers `pvresize`, `lvextend`, and online filesystem growth.
- [ ] `lvextend` passes only the specific resized PV device path, not all PVs.
- [ ] Segment verification runs after expansion.
- [ ] The expanded capacity is visible in `GET /api/tiers/{name}` immediately after growth completes.
- [ ] A failure at any expansion step logs the error and sets `error_reason`; the pool remains functional at its prior size.
- [ ] Arrays not assigned to any pool are ignored when `ArraySizeChanged` fires.
