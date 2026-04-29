# Proposal: Disk Spindown Phase 6R — Smoothfs Recovered Range Tier Mask

**Status:** Done
**Split from:** [`disk-spindown-06q-smoothfs-oldest-recovered-write-status.md`](disk-spindown-06q-smoothfs-oldest-recovered-write-status.md)
**Follow-up:** [`disk-spindown-06s-smoothfs-range-recovery-supported.md`](disk-spindown-06s-smoothfs-range-recovery-supported.md)

---

## Context

Phase 6P/6Q surfaced range-staging recovery counters and the oldest
recovered-write timestamp. After a remount/crash replay, recovered ranges
may be pinned to one or more source tiers waiting to drain. SmoothNAS needs
the per-tier breakdown so the spindown decision logic can see which tiers
have recovered ranges that would drain once the tier becomes drain-active.

This is the recovery-side analog of `write_staging_drainable_tier_mask`.

## Scope

1. Read `recovered_range_tier_mask` when present.
2. Return the value from the Phase 5 write-staging status API.
3. Show the bitmask in the smoothfs pools UI when non-zero.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `recovered_range_tier_mask`.
- [x] The smoothfs pools UI displays the bitmask when non-zero.
- [x] Older kernels that lack the sysfs file report zero without failing.
