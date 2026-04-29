# Proposal: Disk Spindown Phase 6L — Smoothfs Range Staging Read-Merge

**Status:** Done
**Split from:** [`disk-spindown-06k-smoothfs-range-status-fields.md`](disk-spindown-06k-smoothfs-range-status-fields.md)
**Follow-up:** [`disk-spindown-06m-smoothfs-range-staging-drain-recovery.md`](../pending/disk-spindown-06m-smoothfs-range-staging-drain-recovery.md)

---

## Context

Phase 5 added the SmoothNAS write-staging control/status contract and the first smoothfs kernel data-plane slice: sysfs status/control, SSD-first new-file admission until the configured full threshold, and truncate-for-write rehoming of cold-tier regular files onto the fastest tier. Phases 6A through 6K added standby-aware metadata masks, drain-active masks, drain/rehome status, truncate-rehome cleanup, and separate byte counters for truncate-rehome and range-level staging.

This phase adds the first range-level data-plane implementation in smoothfs while keeping the high-risk drain/recovery work pending.

## Scope

1. Stage buffered non-truncating writes for unpinned cold-tier regular files into the fastest tier while the original cold-tier file remains untouched.
2. Add read-merge for range-staged files so reads through smoothfs return staged bytes overlaid onto the original lower bytes.
3. Keep direct I/O and pinned LUN files on the existing passthrough path.
4. Update smoothfs operator docs/support matrix and add a live mount smoke harness for range staging.
5. Leave persistence replay, mmap semantics, and drain-back to the next phase.

## Acceptance Criteria

- [x] Buffered non-truncating writes to unpinned HDD-resident files can land on the fastest tier without rewriting or truncating the HDD lower file.
- [x] Reads through smoothfs merge staged ranges over old lower-file bytes.
- [x] `range_staged_bytes`, `range_staged_writes`, and `staged_bytes` report range-staging activity.
- [x] Direct I/O and pinned LUN files bypass range staging.
- [x] Range drain/recovery remains explicitly pending rather than hidden behind the new read-merge path.
