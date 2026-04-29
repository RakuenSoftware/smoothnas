# Proposal: Disk Spindown Phase 6G — Smoothfs Drain State Status

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs now exposes read-only drain-state signals without performing any drain:
`write_staging_drain_pressure` reports fastest-tier fullness pressure when
staged work exists, and `write_staging_drainable_tier_mask` reports non-fast
tiers that have staged work and SmoothNAS drain permission.

## Scope

1. Read both smoothfs drain-state sysfs files when present.
2. Return the values from the Phase 5 write-staging status API.
3. Show drain pressure and drainable tier mask in the smoothfs pools UI.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `write_staging_drain_pressure`.
- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `write_staging_drainable_tier_mask`.
- [x] The smoothfs pools UI displays both values when active.
- [x] Older kernels that lack the sysfs files report zero values without
      failing.
