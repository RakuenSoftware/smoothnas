# Proposal: Disk Spindown Phase 7D — Target-Balance Candidate Exhaustion

**Status:** Done
**Split from:** [`disk-spindown-07-ssd-warm-fill-before-standby.md`](disk-spindown-07-ssd-warm-fill-before-standby.md)

---

## Context

Phase 7C made spindown readiness wait for active or pending SSD target-balance
movement. One final status case remained: after a target-balance pass completes,
the pool may still be below or above exact `target_fill_pct` because there are no
eligible files left to move. That state should be visible separately from an
actionable movement backlog.

## Scope

1. Record candidate exhaustion in the target-balance movement status.
2. Keep exact target-fill status visible when SSD/NVMe tiers remain below or
   above target.
3. Suppress the actionable target-fill blocker when the latest target-balance
   pass completed with candidate exhaustion and no active or pending moves.
4. Surface the exhausted state in the Tiers UI as best-effort SSD balance.

## Acceptance Criteria

- [x] Target-balance movement status reports candidate exhaustion.
- [x] Candidate exhaustion is distinct from active or pending movement.
- [x] Exact target-fill status still reports below-target or above-target tiers.
- [x] Pool spindown readiness can proceed after movement completes with no
      eligible candidates left.
