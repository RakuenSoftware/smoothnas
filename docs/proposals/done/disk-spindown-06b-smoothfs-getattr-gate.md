# Proposal: Disk Spindown Phase 6B — Smoothfs Cached Getattr Gate

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phase 6A stopped smoothfs fallback lookup and union readdir from walking tiers
SmoothNAS marks inactive. A remaining metadata-only path still refreshed
attributes from the lower filesystem for already-resolved cold-tier dentries.

## Scope

1. Teach smoothfs `getattr` to honor the metadata-active tier mask.
2. When an inode lives on an inactive tier, return smoothfs's cached inode
   attributes instead of calling into the lower filesystem.
3. Preserve smoothfs's synthetic inode identity in the returned stat.
4. Count cached inactive-tier `getattr` responses in `metadata_tier_skips`.

## Acceptance Criteria

- [x] `stat` of an already-resolved cold-tier object does not call lower
      `getattr` when the tier is inactive.
- [x] The returned stat uses cached smoothfs inode attributes.
- [x] The returned stat preserves smoothfs `dev` and synthetic `ino`.
- [x] `metadata_tier_skips` increases for inactive-tier cached getattr.
- [x] The metadata gate smoke harness covers the cached getattr path.

## Deferred

- Range-level COW staging for non-truncating writes.
- Draining staged writes only when an HDD has become active due to external
  activity, or when explicit fullness pressure policy requires it.
