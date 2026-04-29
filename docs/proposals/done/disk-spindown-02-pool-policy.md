# Proposal: Disk Spindown Phase 2 — Pool Policy and Eligibility

**Status:** Done
**Split from:** [`disk-spindown.md`](../done/disk-spindown.md)
**Follow-up:** [`disk-spindown-03-active-windows.md`](disk-spindown-03-active-windows.md)

---

## Context

Phase 1 delivered operator-facing per-disk controls: hdparm standby timers, manual standby, standby-aware status, SMART `-n standby` polling, and UI/API surfaces.

Phase 2 adds the pool-level guardrail needed before the later scheduler work can safely run: spindown is opt-in per pool, eligibility is conservative, the fastest SmoothNAS tier owns the smoothfs metadata role for managed tier pools, and enabling a pool remounts its managed data-plane and backing mounts with `noatime`.

## Scope

1. Add pool-scoped spindown policy.
2. Enforce `noatime` on every spindown-enabled pool-managed mount.
3. Gate eligibility on the metadata-on-fastest invariant.
4. Surface eligibility and policy state in the UI.

## Acceptance Criteria

- [x] Spindown is opt-in per pool.
- [x] Pool-managed mounts use `noatime` when spindown is enabled.
- [x] ZFS backings used under smoothfs can rely on the fastest SmoothNAS tier for namespace metadata.
- [x] Pools without metadata pinned to the fastest tier cannot enable spindown.
- [x] Operators can view and toggle pool spindown eligibility from the Tiers page.

## Completion Notes

- `/api/tiers/{pool}/spindown` exposes and updates the pool policy.
- Enabling a pool remounts `/mnt/{pool}` and `/mnt/.tierd-backing/{pool}/{tier}` mounts with `noatime`.
- Eligibility blocks pools without `meta_on_fastest`.
- A ZFS HDD pool added as a smoothfs lower tier is not modified with `zpool add special`; the fastest SmoothNAS tier supplies smoothfs namespace metadata instead. Direct/raw ZFS exports still need their own real ZFS `special` vdev before they can claim the same metadata-on-SSD invariant.
- The remaining scheduler, attribution, and write-staging work is split into Phase 3.
