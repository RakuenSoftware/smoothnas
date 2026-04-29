# Proposal: Disk Spindown Phase 6E — Smoothfs Drain Mask Sync

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs now exposes `write_staging_drain_active_tier_mask` as the data-drain
gate for staged writes. It is intentionally separate from
`metadata_active_tier_mask`: metadata browsing and staged-data drains can have
different safety decisions, even though SmoothNAS currently computes both from
the same externally observed disk power state.

## Scope

1. Read and report the smoothfs drain-active tier mask in the write-staging
   status API.
2. Recommend the drain-active mask from the same managed-pool disk-state
   observation used for metadata activity.
3. Refresh the drain-active mask alongside the metadata-active mask when the
   operator syncs current smoothfs activity gates.
4. Apply the drain-active mask while enabling write staging when the kernel
   exposes the sysfs file, without breaking older smoothfs kernels.
5. Surface current and recommended drain masks in the smoothfs pools UI.

## Acceptance Criteria

- [x] The Phase 5 API reports `write_staging_drain_active_tier_mask`.
- [x] SmoothNAS recommends the drain-active mask without waking standby HDDs.
- [x] Refreshing activity masks writes the drain-active mask when supported.
- [x] Older kernels that lack the drain mask sysfs file continue to accept
      write-staging updates.
- [x] The smoothfs pools UI shows current and recommended drain masks.
