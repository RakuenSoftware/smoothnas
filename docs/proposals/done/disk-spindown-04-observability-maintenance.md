# Proposal: Disk Spindown Phase 4 — Observability and Opportunistic Maintenance

**Status:** Done
**Split from:** [`disk-spindown-03-active-windows.md`](../done/disk-spindown-03-active-windows.md)
**Follow-up:** [`disk-spindown-05-write-staging-status.md`](disk-spindown-05-write-staging-status.md)

---

## Context

Phases 1 through 3 now provide per-disk controls, standby-aware SMART reads, managed-pool opt-in policy, `noatime` enforcement, active-window scheduling, scrub/reconcile gating, and direct/raw ZFS eligibility.

This phase makes the spindown contract observable and tightens the maintenance rule:

SmoothNAS must not wake an HDD by itself. If a backing HDD is in standby, SmoothNAS defers its own scrub, quiesce, reconcile, placement, and cleanup work. Once the HDD is already active because of external activity, SmoothNAS may use that active period to perform its own maintenance.

## Scope

1. Add wake-event attribution and time-in-standby observability.
2. Wire smoothfs quiesce/reconcile into the spindown maintenance gate.
3. Ensure placement and cleanup walkers defer HDD backing-tree scans while disks are in standby.
4. Treat active windows as maintenance preferences, not as permission to wake a standby HDD.
5. Allow SmoothNAS maintenance to run once backing HDDs are already active due to external activity.

## Acceptance Criteria

- [x] Operators can see current disk state, observed standby percentage, and last wake attribution.
- [x] Observed wake events are retained in memory per disk and surfaced on the disk power response.
- [x] Smoothfs quiesce/reconcile is deferred while a spindown-enabled pool's HDD backing is in standby.
- [x] mdadm placement walks are deferred while a spindown-enabled pool's HDD backing is in standby.
- [x] mdadm/ZFS scrub and tiering reconcile gates allow SmoothNAS work when HDDs are already active due to external activity, even outside active windows.
- [x] mdadm/ZFS scrub and tiering reconcile gates defer SmoothNAS work while HDDs are in standby.

## Deferred

The remaining data-plane work is tracked in Phase 5:

- Metadata-only browse/stat/readdir verification for smoothfs paths.
- Optional SSD write-staging for HDD-resident files.
- Draining staged writes during external-active periods or fullness pressure.
