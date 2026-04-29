# Proposal: Disk Spindown Phase 6S — Smoothfs Range Recovery Supported

**Status:** Done
**Split from:** [`disk-spindown-06r-smoothfs-recovered-range-tier-mask.md`](disk-spindown-06r-smoothfs-recovered-range-tier-mask.md)

---

## Context

Phases 6P/6Q/6R surfaced range-staging recovery counters, the oldest
recovered-write timestamp, and the recovered-range tier mask. None of those
fields distinguishes "kernel persists range staging but nothing has crashed
yet" from "this kernel does not implement recovery at all" — both report
zero / empty.

SmoothNAS needs an explicit `range_staging_recovery_supported` boolean so
operators can interpret the absence of recovery activity correctly, and so
the UI can flag pools running on kernels without persistence.

This is the recovery-side analog of `write_staging_supported`.

## Scope

1. Read `range_staging_recovery_supported` when present.
2. Return the value from the Phase 5 write-staging status API.
3. Show a "range recovery supported" indicator in the smoothfs pools UI
   when true.

## Acceptance Criteria

- [x] `/api/smoothfs/pools/{name}/write-staging` reports
      `range_staging_recovery_supported`.
- [x] The smoothfs pools UI surfaces the indicator when the kernel
      supports range-staging recovery.
- [x] Older kernels that lack the sysfs file report false without failing.
