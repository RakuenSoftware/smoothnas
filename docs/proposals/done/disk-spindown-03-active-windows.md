# Proposal: Disk Spindown Phase 3 — Active Windows and Direct ZFS Eligibility

**Status:** Done
**Split from:** [`disk-spindown-02-pool-policy.md`](../done/disk-spindown-02-pool-policy.md)
**Follow-up:** [`disk-spindown-04-observability-maintenance.md`](disk-spindown-04-observability-maintenance.md)

---

## Context

Phase 1 delivered per-disk hdparm controls and standby-aware SMART reads. Phase 2 delivered conservative pool opt-in policy, metadata/special-vdev eligibility, and `noatime` remounts for managed mounts.

This phase makes the pool policy enforceable for maintenance work by adding active-window scheduling and direct/raw ZFS eligibility.

## Scope

1. Add active-window scheduling for spindown-enabled managed tier pools.
2. Make mdadm and ZFS scrub endpoints respect active windows.
3. Make tierd reconcile entry points defer spindown-enabled managed pools outside active windows.
4. Add raw/direct ZFS pool spindown eligibility and require a real ZFS `special` vdev there; this is distinct from ZFS pools used as smoothfs lower-tier backings.
5. Expose active-window state in the Tiers and ZFS UI.

## Acceptance Criteria

- [x] Managed tier spindown policy stores active windows and reports whether the window is currently open.
- [x] mdadm scrub refuses to start outside the owning managed pool's active window.
- [x] ZFS scrub refuses to start outside either the raw ZFS pool's active window or any owning managed tier pool's active window.
- [x] Global tiering reconcile refuses to run when a spindown-enabled managed pool is outside its active window.
- [x] Boot/control-plane reconcile skips spindown-enabled managed pools outside active windows.
- [x] Direct/raw ZFS pools without a real `special` vdev cannot enable spindown.
- [x] Enabling direct/raw ZFS spindown applies `atime=off`.
- [x] Tiers and ZFS UI surfaces show active-window status and provide a daily maintenance-window shortcut.

## Deferred

The remaining filesystem-level pieces are tracked in Phase 4:

- Metadata-only browse/stat/readdir verification for smoothfs paths.
- Wake-event attribution and time-in-standby percentages.
- Optional SSD write-staging for HDD-resident files.
- Smoothfs snapshot and maintenance quiesce scheduling.
