# Proposal: Disk Spindown Phase 7A — SSD Target-Fill Gate

**Status:** Done
**Follow-up:** [`disk-spindown-07-ssd-warm-fill-before-standby.md`](disk-spindown-07-ssd-warm-fill-before-standby.md)

---

## Context

Before HDD tiers enter standby, every SSD/NVMe tier faster than an HDD tier
should already be balanced at its configured `target_fill_pct` where possible.
This maximizes the hot and warm working set available from SSD while still
preserving the normal gap between `target_fill_pct` and `full_threshold_pct` for
write staging.

## Scope

1. Evaluate confirmed non-rotational tier backings that are faster than a
   rotational tier in a spindown-enabled pool.
2. Compare each confirmed SSD/NVMe tier's current usage with its effective
   `target_fill_pct`.
3. Report the warm-fill status in the pool spindown API and UI.
4. Mark the pool not spindown-ready while confirmed SSD/NVMe tiers are not at
   target fill.

## Acceptance Criteria

- [x] `/api/tiers/{pool}/spindown` reports SSD target-fill status.
- [x] Confirmed SSD/NVMe tiers below or above `target_fill_pct` make pool
      spindown ineligible.
- [x] Confirmed SSD/NVMe tiers exactly at `target_fill_pct` satisfy the gate.
- [x] Unknown/unconfirmed backing types do not create false blockers.
- [x] The Tiers UI surfaces whether SSD balance is ready or not at target.
