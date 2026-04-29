# Proposal: Disk Spindown Phase 6Q — Smoothfs Oldest Recovered Write Status

**Status:** Done
**Split from:** [`disk-spindown-06p-smoothfs-range-staging-recovery-status.md`](disk-spindown-06p-smoothfs-range-staging-recovery-status.md)
**Follow-up:** [`disk-spindown-06r-smoothfs-recovered-range-tier-mask.md`](disk-spindown-06r-smoothfs-recovered-range-tier-mask.md)

---

## Context

Phase 6P surfaced range-staging recovery counters and the last-recovery
event. Once a remount/crash replay completes, recovered ranges may sit
pinned in the read-merge view until the source tier becomes drain-active.
SmoothNAS needs the timestamp of the oldest recovered range so operators
can see how long staged-then-recovered bytes have been waiting.

This is the recovery-side analog of `oldest_staged_write_at`.

## Scope

1. Read `oldest_recovered_write_at` when present.
2. Return the value from the Phase 5 write-staging status API.
3. Show the timestamp in the smoothfs pools UI when active.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `oldest_recovered_write_at`.
- [x] The smoothfs pools UI displays the timestamp when active.
- [x] Older kernels that lack the sysfs file report empty without failing.
