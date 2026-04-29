# Proposal: Disk Spindown Phase 6F — Smoothfs Staged Rehome Status

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs now records truncate-write staging rehomes separately from total staged
bytes. `staged_rehomes_total` is useful because truncate rehome is the first
supported data-plane staging path, while range-level staging and drain execution
remain pending.

## Scope

1. Read `staged_rehomes_total` from smoothfs sysfs.
2. Return the counter from the Phase 5 write-staging status API.
3. Show the counter in the smoothfs pools UI.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `staged_rehomes_total`.
- [x] The smoothfs pools UI shows the rehome count when non-zero.
- [x] Older kernels that lack the sysfs file report zero without failing.
