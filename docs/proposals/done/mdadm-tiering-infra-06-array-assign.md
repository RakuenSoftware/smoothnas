# Proposal: mdadm Tiering Infrastructure — PUT /api/tiers/{name}/tiers/{tier_name} (Array Assignment)

**Status:** Done
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 6 of 14)
**Depends on:** mdadm-tiering-infra-05-pool-create, mdadm-tiering-infra-02-pv-tags, mdadm-tiering-infra-12-pv-allocation

---

## Problem

After a pool is created its tier slots are empty. Array assignment is the primary operational action that attaches real storage to a slot, triggers LV creation on first assignment, and extends the LV on subsequent assignments. Without this endpoint the pool cannot move past `provisioning` state.

---

## Specification

### Request

`PUT /api/tiers/{name}/tiers/{tier_name}`

```json
{
  "array_id": 42
}
```

### Validation

1. Pool must exist and not be in `destroying` state.
2. Tier slot `{tier_name}` must exist within the pool and be in `empty` state (already-`assigned` slots are rejected `409`).
3. The array (`array_id`) must not already be assigned to any tier in any pool (rejected `409`).
4. The mdadm array must be in `active` or `degraded` state — not `failed`, `inactive`, or unknown (rejected `422`).

### Assignment sequence

1. Resolve the array's block device path (e.g. `/dev/md0`).
2. Run `wipefs -a {device}` to clear any stale partition or filesystem signatures. This is required to prevent `pvcreate` from failing interactively. (Addresses failure mode from `b56483e`.)
3. Run `pvcreate {device}`.
4. Apply PV tags per Step 2: `pvchange --addtag smoothnas-pool:{name} --addtag smoothnas-tier:{tier_name} {device}`.
5. Run `vgextend tier-{name} {device}`.
6. Update the Tier row: `array_id`, `pv_device`, `state = 'assigned'`.
7. **If this is the first assigned slot (pool `state = 'provisioning'`)** — create the LV:
   a. Collect all `assigned` PV device paths sorted by `rank` ascending (fastest first).
   b. Run `lvcreate -l 100%FREE -n data --type linear tier-{name} {pv1} {pv2} ...` (see Step 12).
   c. Format: `mkfs.xfs /dev/tier-{name}/data` or `mkfs.ext4 /dev/tier-{name}/data` per pool `filesystem`.
   d. Create `/mnt/{name}` if it does not exist.
   e. Mount: `mount /dev/tier-{name}/data /mnt/{name}`.
   f. Add fstab entry.
   g. Run segment verification (Step 12).
   h. Transition pool to `state = 'healthy'`.
8. **If the LV already exists (subsequent slot assignment)** — extend it:
   a. Run `lvextend -l +100%FREE tier-{name}/data {new_pv}` targeting the specific new PV.
   b. Grow the filesystem: `xfs_growfs /mnt/{name}` or `resize2fs /dev/tier-{name}/data`.
   c. Re-run segment verification (Step 12).

### Rollback

If any step from 3 onwards fails, attempt to reverse completed steps in reverse order:
- If `vgextend` succeeded but later steps failed: `vgreduce tier-{name} {device}`, `pvremove {device}`.
- Pool state is not changed if the LV already existed prior to this call.
- Pool remains `provisioning` if LV creation failed.

### Response

**200 OK** — returns the updated pool object (same schema as `GET /api/tiers/{name}`, Step 9).

---

## Acceptance Criteria

- [ ] `wipefs` runs on the device before `pvcreate`.
- [ ] `pvcreate` and PV tagging run before `vgextend`.
- [ ] Assigning the first array triggers LV creation, formatting, mount, and fstab entry; pool transitions to `healthy`.
- [ ] Assigning a subsequent array runs `lvextend` targeting the specific new PV, then grows the filesystem.
- [ ] Segment verification runs after every LV create or extend.
- [ ] Assigning to an already-`assigned` slot is rejected with `409 Conflict`.
- [ ] Assigning an array already used elsewhere is rejected with `409 Conflict`.
- [ ] Assigning a `failed` or `inactive` array is rejected with `422 Unprocessable Entity`.
- [ ] Failure at any step after `pvcreate` attempts cleanup and leaves no orphaned PV/VG state.
