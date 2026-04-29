# Proposal: Disk Spindown Phase 6K — Smoothfs Range Status Fields

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)
**Requires smoothfs:** RakuenSoftware/smoothfs#14

---

## Context

Phase 6 still needs the full range-level write-staging data plane. Before the
range COW path can drain safely, SmoothNAS needs separate status fields for
truncate-rehome staged bytes and range-staged bytes. The aggregate
`staged_bytes` field remains the high-level staged-work total.

## Scope

1. Read smoothfs `staged_rehome_bytes`, `range_staged_bytes`, and
   `range_staged_writes` sysfs fields.
2. Include those fields in `/api/smoothfs/pools/{pool}/write-staging`.
3. Surface non-zero range/rehome counters in the SmoothFS Pools UI.

## Acceptance Criteria

- [x] SmoothNAS reports truncate-rehome staged bytes separately from aggregate
      staged bytes.
- [x] SmoothNAS reports range-staged bytes and range-staged write counts when
      the kernel exposes them.
- [x] Existing kernels that lack the new sysfs files continue reporting zeroes
      without breaking write-staging status.
