# Proposal: Disk Spindown Phase 6C — SmoothNAS Metadata Mask Sync

**Status:** Done
**Split from:** [`disk-spindown-06-smoothfs-write-staging-dataplane.md`](../pending/disk-spindown-06-smoothfs-write-staging-dataplane.md)

---

## Context

Phases 6A and 6B added the smoothfs kernel-side metadata activity gate. SmoothNAS
now needs to drive that gate from observed device state so standby HDD tiers
stay masked out, while tiers whose HDDs are already externally active can be
used until the disks return to standby.

## Scope

1. Compute a recommended smoothfs `metadata_active_tier_mask` for managed
   SmoothNAS tier pools.
2. Keep bit 0, the fastest tier, active.
3. Treat non-rotational tiers as active.
4. Include rotational tiers only when `hdparm -C` reports their backing HDDs
   active or idle.
5. Leave unmanaged smoothfs pools unchanged instead of guessing.
6. Apply the recommended mask before enabling write staging when the operator
   has not supplied an explicit mask.
7. Surface the recommendation and reason in the API/UI.

## Acceptance Criteria

- [x] SmoothNAS never includes a standby or unknown HDD tier in the automatic
      metadata-active mask.
- [x] SmoothNAS includes SSD/NVMe tiers.
- [x] SmoothNAS includes HDD tiers that are already active/idle.
- [x] Manual `metadata_active_tier_mask` requests still override the automatic
      recommendation.
- [x] The smoothfs write-staging status response reports current and
      recommended metadata masks.

## Deferred

- Range-level COW staging for non-truncating writes.
- Draining staged writes only when an HDD has become active due to external
  activity, or when explicit fullness pressure policy requires it.
