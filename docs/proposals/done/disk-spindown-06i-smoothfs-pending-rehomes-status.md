# Proposal: Disk Spindown Phase 6I — Smoothfs Pending Rehomes Status

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs now exposes `staged_rehomes_pending`, the current count of staged
truncate rehomes that still have cleanup work outstanding. This is separate
from `staged_rehomes_total`, which is cumulative.

## Scope

1. Read `staged_rehomes_pending` when present.
2. Return the value from the Phase 5 write-staging status API.
3. Show the count in the smoothfs pools UI when it is non-zero.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `staged_rehomes_pending`.
- [x] The smoothfs pools UI displays the count when active.
- [x] Older kernels that lack the sysfs file report zero without failing.
