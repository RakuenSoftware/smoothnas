# Proposal: mdadm Tiering Infrastructure — Ordered PV Allocation and Segment Verification

**Status:** Pending
**Part of:** mdadm-tiering-infrastructure (Step 12 of 14)
**Depends on:** mdadm-tiering-infra-04-data-model

---

## Problem

If `lvcreate` or `lvextend` is called without explicit PV ordering, LVM may distribute extents across all PVs in the VG simultaneously. On a pool with NVMe + SSD + HDD tiers this means every write hits all three devices, negating the performance benefit of faster tiers. This was the "all-tier write" performance bug that motivated the allocation ordering requirement.

---

## Specification

### lvcreate and lvextend calling convention

Every call to `lvcreate` or `lvextend` on a pool LV must:

1. Collect all `assigned` Tier rows for the pool, sorted by `rank` ascending (rank 1 = fastest).
2. Extract `pv_device` from each row in that order.
3. Pass the device paths as positional arguments — never using `-i` (striping) or `-m` (mirroring):

```
lvcreate -l 100%FREE -n data --type linear tier-{name} /dev/md0 /dev/md1 /dev/md2
```

```
lvextend -l +100%FREE tier-{name}/data /dev/md_new
```

For `lvextend` during array assignment (Step 6), only the newly added PV is passed. Since new extents append after existing segments, the rank order of the full LV is preserved.

For `lvextend` during auto-expansion (Step 14), only the resized PV is passed.

### Segment verification algorithm

After every `lvcreate` or `lvextend`, run:

```
lvs -o seg_pe_ranges,devices tier-{name}/data
```

Parse the output into a list of segments ordered by PE offset. Walk the list and assert that each segment's PV device maps to a Tier row with a `rank` that is greater than or equal to the previous segment's rank. (Equal rank is acceptable when the same PV appears in multiple contiguous segments.)

If the assertion fails for any segment:
- Log the violation at ERROR level with the full segment list.
- Set pool `state = 'error'` and `error_reason = 'segment_order_violation'`.
- Do not mount or serve the volume until the violation is resolved.

Store the verification result (pass/fail) and timestamp in memory; expose it via `GET /api/tiers/{name}/map` (Step 10). Verification also re-runs at every boot-time reconciliation scan (Step 13).

---

## Acceptance Criteria

- [ ] `lvcreate` and `lvextend` always pass PV device paths in rank order.
- [ ] The `-i` (striping) flag is never passed.
- [ ] Segment verification runs after every LV create or extend.
- [ ] Out-of-order segments set pool `state = 'error'` with `error_reason = 'segment_order_violation'` and block mounting.
- [ ] Verification result and timestamp are exposed via `GET /api/tiers/{name}/map`.
- [ ] Verification re-runs during boot-time reconciliation.
