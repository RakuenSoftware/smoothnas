# Proposal: Disk Spindown Phase 6D — Smoothfs Metadata Mask Refresh

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phase 6C taught SmoothNAS to compute a namespace-safe
`metadata_active_tier_mask` from the recorded tiers and apply it while write
staging is toggled. Operators also need a direct refresh action so a pool can
restore that invariant without changing the write-staging enabled state.

## Scope

1. Add a REST action that recomputes the smoothfs metadata-active tier mask and
   writes `/sys/fs/smoothfs/{uuid}/metadata_active_tier_mask`.
2. Return the refreshed write-staging status after the mask is applied.
3. Reject kernels without smoothfs write-staging support without writing sysfs.
4. Surface the refresh action in the smoothfs pools UI.

## Acceptance Criteria

- [x] `POST /api/smoothfs/pools/{name}/metadata-active-mask/refresh` applies the
      recommended mask for managed pools.
- [x] The refresh action does not toggle `write_staging_enabled`.
- [x] Pools without managed disk-state data still receive the namespace-safe
      all-recorded-tier metadata mask.
- [x] The smoothfs pools UI can refresh the metadata-active mask independently
      from the write-staging toggle.
