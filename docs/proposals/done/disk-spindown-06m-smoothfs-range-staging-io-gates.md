# Proposal: Disk Spindown Phase 6M — Smoothfs Range Staging I/O Gates

**Status:** Done
**Split from:** [`disk-spindown-06l-smoothfs-range-staging-read-merge.md`](disk-spindown-06l-smoothfs-range-staging-read-merge.md)
**Follow-up:** [`disk-spindown-06n-smoothfs-range-staging-drain-recovery.md`](../pending/disk-spindown-06n-smoothfs-range-staging-drain-recovery.md)

---

## Context

Phase 6L added the first smoothfs range-level write-staging data path: buffered non-truncating writes to unpinned cold-tier regular files can land on the fastest tier, and smoothfs read-merge overlays staged ranges on top of the original lower bytes.

This phase closes the next correctness gap: direct I/O and mmap must not bypass that merge layer once a file has staged ranges. Otherwise an application could observe stale bytes from the original HDD lower file or write around staged data.

## Scope

1. Refuse direct reads and writes on files that currently have staged ranges.
2. Refuse mmap on files that currently have staged ranges.
3. Keep direct I/O and mmap unchanged for files without staged ranges.
4. Extend the smoothfs range-staging smoke harness to cover direct I/O refusal.
5. Leave persistence replay and drain-back to the next phase.

## Acceptance Criteria

- [x] Direct I/O cannot bypass staged ranges.
- [x] mmap cannot bypass staged ranges.
- [x] Existing passthrough behavior remains available for files without staged ranges.
- [x] Range-staging drain/recovery remains explicitly pending.
