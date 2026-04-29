# Proposal: Disk Spindown Phase 7C — Target-Balance Movement Status

**Status:** Done
**Split from:** [`disk-spindown-07-ssd-warm-fill-before-standby.md`](disk-spindown-07-ssd-warm-fill-before-standby.md)

---

## Context

Phase 7B made the mdadm placement planner rebalance to `target_fill_pct` during
spindown-active maintenance. The spindown API still needed to know when that
movement pass was active or had unfinished moves so HDD standby would not be
allowed halfway through a warm-fill rebalance.

## Scope

1. Persist target-balance movement status for spindown-active mdadm placement
   passes.
2. Report candidate count, planned moves, pending moves, completed moves, and
   skipped moves in `/api/tiers/{pool}/spindown`.
3. Treat active target-balance placement or pending moves as a pool spindown
   blocker.
4. Surface target-balance movement in the Tiers UI as an SSD balance state.

## Acceptance Criteria

- [x] The placement pass records active, completed, and pending movement state.
- [x] `/api/tiers/{pool}/spindown` includes target-balance movement status.
- [x] Pool spindown readiness is blocked while target-balance movement is active
      or pending.
- [x] The Tiers UI distinguishes SSD balance movement from a ready state.
