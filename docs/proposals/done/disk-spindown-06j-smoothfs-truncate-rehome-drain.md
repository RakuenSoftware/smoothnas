# Proposal: Disk Spindown Phase 6J — Smoothfs Truncate Rehome Drain

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs can stage replace-style writes by rehoming a cold-tier regular file to
the fastest tier before the lower file is opened. The stale original lower file
should be cleaned up only after SmoothNAS has observed that original tier active
due to external activity and has written the matching drain-active mask bit.

## Scope

1. Use `write_staging_drain_active_tier_mask` as the gate for truncate-rehome
   drain cleanup.
2. Remove stale original lower files for staged truncate rehomes on drain-active
   tiers.
3. Clear per-inode staged state and update drain timestamps/reasons after
   cleanup.

## Acceptance Criteria

- [x] smoothfs does not touch a cold source tier until SmoothNAS marks that tier
      drain-active.
- [x] Once the source tier is drain-active, stale original lower files for
      staged truncate rehomes are removed.
- [x] Pending/drainable staged rehome status clears after cleanup.
- [x] The remaining pending proposal is narrowed to range-level staging and
      range-level drain execution.
