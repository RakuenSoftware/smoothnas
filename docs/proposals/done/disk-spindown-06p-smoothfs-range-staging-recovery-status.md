# Proposal: Disk Spindown Phase 6P — Smoothfs Range Staging Recovery Status

**Status:** Done
**Split from:** [`disk-spindown-06o-smoothfs-range-staging-recovery.md`](disk-spindown-06o-smoothfs-range-staging-recovery.md)
**Follow-up:** [`disk-spindown-06q-smoothfs-oldest-recovered-write-status.md`](disk-spindown-06q-smoothfs-oldest-recovered-write-status.md)

---

## Context

Phase 6O persists range-staging metadata so smoothfs can recover staged
writes after remount or crash without waking standby source tiers. SmoothNAS
needs read-only visibility into recovery activity so operators can confirm
that replayed ranges were restored, and see staged bytes that recovered into
the read-merge view but cannot drain yet because the source tier is not
drain-active.

## Scope

1. Read the kernel-side range-staging recovery counters when present:
   `range_staging_recovered_bytes`, `range_staging_recovered_writes`,
   `range_staging_recovery_pending`.
2. Read the last-recovery event strings when present: `last_recovery_at`
   and `last_recovery_reason`.
3. Return the values from the Phase 5 write-staging status API.
4. Show the values in the smoothfs pools UI when active.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `range_staging_recovered_bytes`, `range_staging_recovered_writes`,
      and `range_staging_recovery_pending`.
- [x] `/api/smoothfs/pools/{name}/write-staging` reports `last_recovery_at`
      and `last_recovery_reason`.
- [x] The smoothfs pools UI displays the recovery counters and the last
      recovery event when active.
- [x] Older kernels that lack the sysfs files report zero / empty without
      failing.
