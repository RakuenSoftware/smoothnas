# Proposal: Disk Spindown Phase 6O — Smoothfs Range Staging Recovery

**Status:** Done
**Split from:** [`disk-spindown-06n-smoothfs-range-staging-drain.md`](disk-spindown-06n-smoothfs-range-staging-drain.md)
**Tierd-side follow-ups:**
- [`disk-spindown-06p-smoothfs-range-staging-recovery-status.md`](disk-spindown-06p-smoothfs-range-staging-recovery-status.md) — recovered counters, recovery-pending bytes, last-recovery event
- [`disk-spindown-06q-smoothfs-oldest-recovered-write-status.md`](disk-spindown-06q-smoothfs-oldest-recovered-write-status.md) — `oldest_recovered_write_at`
- [`disk-spindown-06r-smoothfs-recovered-range-tier-mask.md`](disk-spindown-06r-smoothfs-recovered-range-tier-mask.md) — `recovered_range_tier_mask`
- [`disk-spindown-06s-smoothfs-range-recovery-supported.md`](disk-spindown-06s-smoothfs-range-recovery-supported.md) — `range_staging_recovery_supported`
**Implementation:** [RakuenSoftware/smoothfs#18](https://github.com/RakuenSoftware/smoothfs/pull/18)

---

## Context

Phase 6L added range staging/read-merge. Phase 6M blocked direct I/O and mmap from bypassing staged ranges. Phase 6N added standby-safe in-memory range drain once SmoothNAS marks the source tier drain-active.

The remaining range-staging work is durability across remount or crash. Smoothfs currently holds staged-range metadata in memory, so a crash before drain can strand fastest-tier sidecar bytes without reconstructing the merge view.

## Scope

1. Persist enough range-staging metadata to recover staged writes after remount or crash.
2. Replay staged-range state on mount without touching standby source tiers.
3. Make recovery either restore the read-merge view or complete a safe drain only when spindown policy permits the source tier.
4. Extend live smoke/fault tests to cover remount and crash recovery.

## Acceptance Criteria

- [x] Range-staged writes survive remount/crash and are recovered without data loss.
- [x] Recovery does not wake standby HDDs solely to inspect or drain staged data.
- [x] Recovered staged ranges can later drain when the source tier becomes drain-active.
- [x] Fault tests cover crash before drain and crash during drain.
