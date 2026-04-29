# Proposal: Disk Spindown Phase 7 — SSD Warm-Fill Before Standby

**Status:** Done
**Completed slices:**
[`disk-spindown-07b-ssd-target-balance-placement.md`](disk-spindown-07b-ssd-target-balance-placement.md),
[`disk-spindown-07c-target-balance-movement-status.md`](disk-spindown-07c-target-balance-movement-status.md),
[`disk-spindown-07d-target-balance-candidate-exhaustion.md`](disk-spindown-07d-target-balance-candidate-exhaustion.md)

---

## Context

The spindown policy now reports and gates on confirmed SSD/NVMe tiers being
balanced at their effective `target_fill_pct`. Phase 7B taught the mdadm
placement planner to use `target_fill_pct` rather than `full_threshold_pct`
during spindown-active maintenance, so the existing bin-packer can promote data
onto below-target SSD tiers and demote data away from above-target SSD tiers.

SmoothNAS now reports and gates on active or pending target-balance movement,
and distinguishes candidate exhaustion from an actionable backlog.

This must never wake an HDD by itself. If an HDD is already in standby, warm-fill
waits. If the HDD is active for another reason, SmoothNAS may use that active
period to improve future standby time.

## Scope

1. Run warm-fill only when the source HDD tier is already active due to external
   activity or an explicit active window.

## Acceptance Criteria

- [x] SmoothNAS does not wake standby HDDs solely to warm-fill SSD tiers.
- [x] During an active window or externally active HDD period, eligible data is
      migrated up or down until faster SSD/NVMe tiers reach target fill.
- [x] HDD tiers may enter standby only after warm-fill movement is complete.
- [x] Pool spindown readiness distinguishes below-target, above-target, and
      candidate-exhausted SSD target-balance states.
