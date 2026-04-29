# Proposal: Disk Spindown Phase 5 — Write-Staging Status and Control Contract

**Status:** Done
**Split from:** [`disk-spindown-04-observability-maintenance.md`](../done/disk-spindown-04-observability-maintenance.md)
**Follow-up:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phases 1 through 4 now provide disk controls, standby-aware SMART reads, pool policy, active-window preferences, raw ZFS eligibility, opportunistic maintenance gating, smoothfs quiesce/reconcile gating, placement deferral, and operator-visible wake/standby observability.

The first smoothfs write-staging kernel slice exposes the sysfs status/control contract and supports SSD-first write admission for the safe file-level cases: new files land on the fastest tier until its full threshold, and replace-style writes to a cold-tier regular file can create the replacement on the fastest tier before opening the cold lower file. Broader range-level COW staging remains follow-up work.

## Scope

1. Persist desired write-staging state per smoothfs pool.
2. Surface whether write staging is kernel-supported and effectively enabled.
3. Surface staged-byte backlog and drain status when the kernel/sysfs support exists.
4. Preserve the invariant that SmoothNAS is not allowed to wake a standby HDD to drain staged writes.
5. Expose the status and toggle in the smoothfs pools UI.
6. Toggle the smoothfs kernel `write_staging_enabled` sysfs control when support is present.
7. Surface and apply the kernel `write_staging_full_pct` threshold so SSD tiers absorb writes until that threshold before HDD tiers are touched.

## Acceptance Criteria

- [x] Operators can enable desired write-staging state per smoothfs pool.
- [x] API reports desired vs effective write-staging state.
- [x] API reports kernel support and the kernel-enabled state separately.
- [x] API/UI expose staged bytes, oldest staged write age, last drain time, and last drain reason fields.
- [x] API explicitly reports `smoothnas_wakes_allowed=false`.
- [x] smoothfs exposes sysfs write-staging status/control and a smoke harness for truncate-for-write rehoming.
- [x] New smoothfs files and staged truncate writes use the fastest tier until `write_staging_full_pct`, then fall through to lower tiers.

## Deferred

The remaining data-plane implementation is tracked in Phase 6:

- Metadata-only browse/stat/readdir verification for spindown-enabled smoothfs pools.
- Range-level/kernel-backed SSD write-staging for non-truncating writes to HDD-resident files. This requires read-merge semantics so old HDD-resident bytes are not corrupted.
- Draining staged writes only when the HDD is externally active, or when explicit fullness pressure policy allows it.
