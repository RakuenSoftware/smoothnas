# Proposal: Disk Spindown Phase 7B — SSD Target-Balance Placement

**Status:** Done
**Split from:** [`disk-spindown-07-ssd-warm-fill-before-standby.md`](disk-spindown-07-ssd-warm-fill-before-standby.md)

---

## Context

Phase 7A made pool spindown readiness require confirmed faster SSD/NVMe tiers
to be balanced at `target_fill_pct`. The mdadm placement planner already moves
files up and down tiers, but its normal admission policy intentionally allows
fast tiers to fill to `full_threshold_pct` before spilling. That is correct for
normal write buffering, but not for the pre-standby rebalance pass.

## Scope

1. Keep the normal mdadm placement policy unchanged: admit to
   `full_threshold_pct`, then drain back toward `target_fill_pct` only after
   crossing the hard cap.
2. When a spindown-enabled pool is in SmoothNAS maintenance because the backing
   HDDs are already active or an explicit active window allows work, plan
   placements against `target_fill_pct`.
3. Let the existing bin-packer promote data onto below-target SSD/NVMe tiers and
   demote data away from above-target SSD/NVMe tiers during that spindown-active
   maintenance mode.

## Acceptance Criteria

- [x] Normal placement still admits new data to faster tiers until
      `full_threshold_pct`.
- [x] Spindown-active maintenance uses `target_fill_pct` as the placement cap.
- [x] Below-target SSD/NVMe tiers can accept planned placement.
- [x] Above-target SSD/NVMe tiers spill planned placement to slower tiers.
