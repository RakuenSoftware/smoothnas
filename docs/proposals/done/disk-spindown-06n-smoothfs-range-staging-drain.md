# Proposal: Disk Spindown Phase 6N — Smoothfs Range Staging Drain

**Status:** Done
**Split from:** [`disk-spindown-06m-smoothfs-range-staging-io-gates.md`](disk-spindown-06m-smoothfs-range-staging-io-gates.md)
**Follow-up:** [`disk-spindown-06o-smoothfs-range-staging-recovery.md`](disk-spindown-06o-smoothfs-range-staging-recovery.md)

---

## Context

Phase 6L added buffered range staging and read-merge for unpinned cold-tier regular files. Phase 6M blocked direct I/O and mmap from bypassing staged ranges. This phase adds standby-safe drain execution for the in-memory staged-range path.

SmoothNAS must never wake a standby HDD solely to drain staged data. This phase therefore keeps drain execution behind `write_staging_drain_active_tier_mask`: SmoothNAS only sets a non-fast tier bit after it has observed that tier externally active.

## Scope

1. Drain in-memory range-staged writes back to source tiers when `write_staging_drain_active_tier_mask` permits that source tier.
2. Copy staged ranges from the fastest-tier sidecar into the source lower file, fsync it, clear staged-range state, and remove the sidecar.
3. Preserve the spindown invariant by never opening the source tier unless the drain-active mask permits it.
4. Extend the smoothfs range-staging smoke harness to cover range drain and counter cleanup.
5. Leave remount/crash recovery to the next phase.

## Acceptance Criteria

- [x] Range-staged writes drain to the source tier when the source tier is already externally active.
- [x] Range drain clears staged-range state and byte counters.
- [x] Smoothfs does not drain range-staged writes unless the source tier is drain-active.
- [x] Remount/crash recovery remains explicitly pending.
