# Proposal: Disk Spindown Phase 6C — SmoothNAS Metadata Mask Sync

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phases 6A and 6B added the smoothfs kernel-side metadata activity gate. SmoothNAS
must keep that gate open for every recorded tier. Masking a standby tier removes
its directory entries from the mounted namespace, which makes valid files appear
as `ENOENT` to applications such as Plex. Disk power state is suitable for the
separate staged-data drain gate, but never for namespace visibility.

## Scope

1. Compute `metadata_active_tier_mask` from the pool's recorded tier count.
2. Keep every recorded tier active for metadata lookup.
3. Reject explicit masks that would hide a recorded tier.
4. Apply the namespace-safe mask before enabling write staging.
5. Surface the recommendation and reason in the API/UI.

## Acceptance Criteria

- [x] SmoothNAS includes every recorded tier in the automatic metadata-active
      mask, regardless of disk power state.
- [x] Manual `metadata_active_tier_mask` requests cannot hide recorded tiers.
- [x] The smoothfs write-staging status response reports current and
      recommended metadata masks.

## Deferred

- Range-level COW staging for non-truncating writes.
- Draining staged writes only when an HDD has become active due to external
  activity, or when explicit fullness pressure policy requires it.
