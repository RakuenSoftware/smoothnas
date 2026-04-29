# Proposal: Disk Spindown Phase 6A — Smoothfs Metadata Tier Activity Gate

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phase 5 added the write-staging control/status contract and safe file-level
staging paths. The next safety requirement is that smoothfs metadata-only
namespace operations must not wake an HDD tier that SmoothNAS believes is in
standby.

## Scope

1. Add smoothfs sysfs control for the metadata-active tier mask.
2. Keep the fastest tier forced active.
3. Skip inactive tiers during smoothfs fallback lookup and union readdir.
4. Expose skipped metadata probes for operator visibility.
5. Surface the new fields through the SmoothNAS write-staging status API/UI.
6. Ensure staging-enabled creates stay on the fastest tier until
   `write_staging_full_pct`.

## Acceptance Criteria

- [x] smoothfs exposes `/sys/fs/smoothfs/<uuid>/metadata_active_tier_mask`.
- [x] smoothfs exposes `/sys/fs/smoothfs/<uuid>/metadata_tier_skips`.
- [x] Metadata fallback lookup skips inactive tiers.
- [x] Union readdir skips inactive tiers.
- [x] The fastest tier remains active even if the written mask omits it.
- [x] SmoothNAS reports the metadata active mask and skip counter.
- [x] With write staging enabled, new files prefer the fastest tier until the
      full threshold, independent of lower-tier idle write load.

## Deferred

- Stat/getattr no-wake behavior for already-resolved cold-tier dentries.
- Range-level COW staging for non-truncating writes.
- Draining staged writes only when an HDD has become active due to external
  activity, or when explicit fullness pressure policy requires it.
