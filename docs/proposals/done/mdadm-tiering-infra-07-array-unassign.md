# Proposal: mdadm Tiering Infrastructure — DELETE /api/tiers/{name}/tiers/{tier_name} (Array Unassignment)

**Status:** Done
**Date:** 2026-04-09
**Part of:** mdadm-tiering-infrastructure (Step 7 of 14)
**Depends on:** mdadm-tiering-infra-06-array-assign

---

## Problem

An array assigned to a tier slot may need to be removed — for example, to decommission a disk set or reassign it. Unassignment must be blocked if the PV has allocated extents, because removing an in-use PV from the VG would corrupt the LV.

---

## Specification

### Request

`DELETE /api/tiers/{name}/tiers/{tier_name}`

No request body.

### Validation

1. Pool must exist.
2. Tier slot must be in `assigned` state (not `empty`, `degraded`, or `missing`).
3. Pool must not be in `destroying` state.
4. Run `pvdisplay -c {pv_device}` and check the allocated PE count. If any extents are allocated, reject with `422 Unprocessable Entity`:
   ```json
   { "error": "PV has allocated extents; migrate data off this tier before unassigning" }
   ```

### Unassignment sequence

1. Run `vgreduce tier-{name} {device}`.
2. Run `pvremove {device}`.
3. Update Tier row: clear `array_id`, `pv_device`, set `state = 'empty'`.
4. Re-evaluate pool state:
   - If at least one slot remains `assigned`, pool stays in its current state.
   - If no slots remain `assigned` (the LV no longer has any backing PVs), pool returns to `state = 'provisioning'`. The LV and VG still exist but are empty.

### Response

**200 OK** — returns the updated pool object.

---

## Acceptance Criteria

- [ ] Unassignment is rejected with `422` if the PV has any allocated extents.
- [ ] Unassignment is rejected with `409` if the pool is in `destroying` state.
- [ ] Successful unassignment removes the PV from the VG and clears `array_id`, `pv_device` on the Tier row.
- [ ] Tier slot returns to `empty` state.
- [ ] Pool state is re-evaluated after unassignment; reverts to `provisioning` if no slots remain assigned.
