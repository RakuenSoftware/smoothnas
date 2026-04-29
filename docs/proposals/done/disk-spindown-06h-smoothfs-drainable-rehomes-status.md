# Proposal: Disk Spindown Phase 6H — Smoothfs Drainable Rehomes Status

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

smoothfs now exposes `write_staging_drainable_rehomes`, the count of staged
truncate rehomes whose original tier is currently permitted by the
drain-active mask. SmoothNAS needs this status so operators can distinguish
between total staged rehomes and rehomes that are eligible to drain without
waking standby tiers.

## Scope

1. Read `write_staging_drainable_rehomes` when present.
2. Return the value from the Phase 5 write-staging status API.
3. Show the count in the smoothfs pools UI when it is non-zero.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `write_staging_drainable_rehomes`.
- [x] The smoothfs pools UI displays the count when active.
- [x] Older kernels that lack the sysfs file report zero without failing.
